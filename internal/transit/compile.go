package transit

import (
	"fmt"
	"sort"
	"time"
	"unsafe"

	"gotransit/internal/geo"
	"gotransit/internal/graph"
	"gotransit/internal/gtfs"
)

// CompileStats reports what Compile did.
type CompileStats struct {
	Stops, Patterns, Trips, StopTimes int
	Transfers                         int
	Excluded                          ExclusionReport
	Elapsed                           time.Duration
}

func (s *CompileStats) String() string {
	return fmt.Sprintf("timetable: %d stops, %d patterns, %d trips, %d stop_times, %d transfers, %d trips excluded (coverage) in %v",
		s.Stops, s.Patterns, s.Trips, s.StopTimes, s.Transfers, s.Excluded.Trips, s.Elapsed.Round(time.Millisecond))
}

// coverage margin: shapes may wiggle slightly past the extract bounding box
// (bridges over border rivers etc.) without truly leaving coverage.
const coverageMarginM = 2000

// Compile merges feeds into one timetable, validating coverage against g.
func Compile(feeds []*gtfs.Feed, g *graph.Graph, walkSpeedKmh float64, transferRadiusM, snapRadiusM int) (*Timetable, *CompileStats, error) {
	t0 := time.Now()
	tt := &Timetable{}
	st := &CompileStats{}

	tz, err := time.LoadLocation(pickTZ(feeds))
	if err != nil {
		tz = time.FixedZone("CET", 3600)
	}
	tt.TZ = tz

	// ---- stops, routes, services, headsigns, shapes: merge with offsets ----
	type feedOff struct{ stop, route, svc, head, shape int32 }
	offs := make([]feedOff, len(feeds))
	var agencyIdx = map[string]int32{}
	for fi, f := range feeds {
		offs[fi] = feedOff{
			stop: int32(len(tt.StopID)), route: int32(len(tt.Routes)),
			svc: int32(len(tt.Services)), head: int32(len(tt.Headsigns)),
			shape: int32(len(tt.ShpFirst)),
		}
		tt.Feeds = append(tt.Feeds, f.Name)
		for i := range f.Stops {
			s := &f.Stops[i]
			tt.StopID = append(tt.StopID, f.Name+":"+s.ID)
			tt.StopCode = append(tt.StopCode, s.Code)
			tt.StopName = append(tt.StopName, s.Name)
			tt.StopLat = append(tt.StopLat, s.Lat)
			tt.StopLon = append(tt.StopLon, s.Lon)
			tt.StopFeed = append(tt.StopFeed, uint8(fi))
		}
		for i := range f.Routes {
			r := &f.Routes[i]
			ai, ok := agencyIdx[r.Agency]
			if !ok {
				ai = int32(len(tt.Agencies))
				tt.Agencies = append(tt.Agencies, r.Agency)
				agencyIdx[r.Agency] = ai
			}
			_ = ai
			tt.Routes = append(tt.Routes, RouteMeta{
				GTFSID: r.ID, Short: r.Short, Long: r.Long, Color: r.Color,
				TextColor: r.TextColor, Agency: r.Agency, Feed: f.Name, Type: r.Type,
			})
		}
		tt.Services = append(tt.Services, f.Services...)
		tt.Headsigns = append(tt.Headsigns, f.Headsigns...)
		// shapes → flat arrays with cumulative distance
		for i := range f.Shapes {
			sh := &f.Shapes[i]
			tt.ShpFirst = append(tt.ShpFirst, uint32(len(tt.ShpLat)))
			var cum float64
			for k := range sh.Lat {
				if k > 0 {
					cum += geo.Dist(sh.Lat[k-1], sh.Lon[k-1], sh.Lat[k], sh.Lon[k])
				}
				tt.ShpLat = append(tt.ShpLat, sh.Lat[k])
				tt.ShpLon = append(tt.ShpLon, sh.Lon[k])
				tt.ShpCumDm = append(tt.ShpCumDm, uint32(cum*10))
			}
		}
	}
	tt.ShpFirst = append(tt.ShpFirst, uint32(len(tt.ShpLat)))

	// ---- coverage validation --------------------------------------------------
	bbox := g.BBox
	stopInside := make([]bool, len(tt.StopID))
	for i := range tt.StopID {
		stopInside[i] = bbox.Contains(tt.StopLat[i], tt.StopLon[i], coverageMarginM)
	}
	shapeInside := make([]bool, len(tt.ShpFirst)-1)
	for si := range shapeInside {
		inside := true
		for k := tt.ShpFirst[si]; k < tt.ShpFirst[si+1]; k++ {
			if !bbox.Contains(tt.ShpLat[k], tt.ShpLon[k], coverageMarginM) {
				inside = false
				break
			}
		}
		shapeInside[si] = inside
	}

	// ---- trips → patterns ------------------------------------------------------
	type patKey = string
	patIdx := map[patKey]int32{}
	type patAccum struct {
		route int32
		stops []int32
		shape int32
		trips []int32 // global trip ids, sorted later
	}
	var pats []patAccum

	type tripRec struct {
		pat      int32
		firstDep uint32
		service  int32
		headsign int32
		shape    int32
		id       string
		feed     int32
		stOff    uint32 // into feed arrays
		stCnt    uint32
	}
	var trips []tripRec
	exclByRoute := map[int32]*ExcludedRoute{}
	exclude := func(globalRoute int32, f *gtfs.Feed, r int32, reason string) {
		er := exclByRoute[globalRoute]
		if er == nil {
			er = &ExcludedRoute{Feed: f.Name, RouteID: f.Routes[r].ID, Short: f.Routes[r].Short, Reason: reason}
			exclByRoute[globalRoute] = er
		}
		er.Trips++
		tt.Excluded.Trips++
	}

	for fi, f := range feeds {
		off := offs[fi]
		for ti := range f.Trips {
			tr := &f.Trips[ti]
			lo, hi := f.TripSTOff[ti], f.TripSTOff[ti+1]
			if hi-lo < 2 {
				continue
			}
			globalRoute := off.route + tr.RouteIdx
			// coverage: all stops and the shape must stay on the graph
			ok := true
			for k := lo; k < hi; k++ {
				gs := off.stop + f.STStop[k]
				if !stopInside[gs] {
					ok = false
					break
				}
			}
			if !ok {
				exclude(globalRoute, f, tr.RouteIdx, "stops outside imported OSM graph")
				continue
			}
			shape := int32(-1)
			if tr.ShapeIdx >= 0 {
				shape = off.shape + tr.ShapeIdx
				if !shapeInside[shape] {
					exclude(globalRoute, f, tr.RouteIdx, "shape outside imported OSM graph")
					continue
				}
			}
			// pattern key: route + stop sequence bytes
			stops := f.STStop[lo:hi]
			key := patternKey(globalRoute, off.stop, stops)
			pi, seen := patIdx[key]
			if !seen {
				pi = int32(len(pats))
				patIdx[key] = pi
				gstops := make([]int32, len(stops))
				for i, s := range stops {
					gstops[i] = off.stop + s
				}
				pats = append(pats, patAccum{route: globalRoute, stops: gstops, shape: shape})
			} else if pats[pi].shape < 0 && shape >= 0 {
				pats[pi].shape = shape
			}
			trips = append(trips, tripRec{
				pat: pi, firstDep: f.STDep[lo], service: off.svc + tr.ServiceIdx,
				headsign: off.head + tr.HeadsignIdx, shape: shape,
				id: tr.ID, feed: int32(fi), stOff: lo, stCnt: hi - lo,
			})
			pats[pi].trips = append(pats[pi].trips, int32(len(trips)-1))
		}
	}
	for _, er := range exclByRoute {
		tt.Excluded.Routes = append(tt.Excluded.Routes, *er)
	}
	sort.Slice(tt.Excluded.Routes, func(i, j int) bool {
		return tt.Excluded.Routes[i].Trips > tt.Excluded.Routes[j].Trips
	})

	// ---- freeze patterns and trips ----------------------------------------------
	tt.PatFirstStop = make([]uint32, 0, len(pats)+1)
	tt.PatFirstTrip = make([]uint32, 0, len(pats)+1)
	totalST := 0
	for pi := range pats {
		totalST += len(pats[pi].stops) * len(pats[pi].trips)
	}
	tt.Arr = make([]uint32, 0, totalST)
	tt.Dep = make([]uint32, 0, totalST)

	for pi := range pats {
		p := &pats[pi]
		tt.PatFirstStop = append(tt.PatFirstStop, uint32(len(tt.PatStops)))
		tt.PatStops = append(tt.PatStops, p.stops...)
		tt.PatFirstTrip = append(tt.PatFirstTrip, uint32(len(tt.TripService)))
		tt.PatRoute = append(tt.PatRoute, p.route)
		tt.PatShape = append(tt.PatShape, p.shape)
		sort.Slice(p.trips, func(a, b int) bool {
			return trips[p.trips[a]].firstDep < trips[p.trips[b]].firstDep
		})
		for _, gti := range p.trips {
			tr := &trips[gti]
			f := feeds[tr.feed]
			tt.TripService = append(tt.TripService, tr.service)
			tt.TripHeadsign = append(tt.TripHeadsign, tr.headsign)
			tt.TripID = append(tt.TripID, tr.id)
			tt.TripFeed = append(tt.TripFeed, uint8(tr.feed))
			tt.TripTimeOff = append(tt.TripTimeOff, uint32(len(tt.Arr)))
			tt.Arr = append(tt.Arr, f.STArr[tr.stOff:tr.stOff+tr.stCnt]...)
			tt.Dep = append(tt.Dep, f.STDep[tr.stOff:tr.stOff+tr.stCnt]...)
			tt.Seq = append(tt.Seq, f.STSeq[tr.stOff:tr.stOff+tr.stCnt]...)
		}
	}
	tt.PatFirstStop = append(tt.PatFirstStop, uint32(len(tt.PatStops)))
	tt.PatFirstTrip = append(tt.PatFirstTrip, uint32(len(tt.TripService)))

	// ---- stop → patterns CSR ------------------------------------------------------
	cnt := make([]uint32, len(tt.StopID)+1)
	for pi := range pats {
		for _, s := range pats[pi].stops {
			cnt[s+1]++
		}
	}
	for i := 1; i < len(cnt); i++ {
		cnt[i] += cnt[i-1]
	}
	tt.StopFirstPat = cnt
	tt.StopPat = make([]uint32, cnt[len(cnt)-1])
	tt.StopPatPos = make([]uint16, cnt[len(cnt)-1])
	fill := make([]uint32, len(tt.StopID))
	for pi := range pats {
		for pos, s := range pats[pi].stops {
			at := tt.StopFirstPat[s] + fill[s]
			fill[s]++
			tt.StopPat[at] = uint32(pi)
			tt.StopPatPos[at] = uint16(pos)
		}
	}

	// ---- shape projection: pattern stop → shape point index ------------------------
	tt.PatShapeIdx = make([]uint32, len(tt.PatStops))
	for pi := range pats {
		shape := tt.PatShape[pi]
		if shape < 0 {
			continue
		}
		lo, hi := tt.ShpFirst[shape], tt.ShpFirst[shape+1]
		stops := tt.PatternStops(uint32(pi))
		cursor := lo
		for k, s := range stops {
			bestIdx, bestD := cursor, 1e18
			for j := cursor; j < hi; j++ {
				d := geo.Dist(tt.StopLat[s], tt.StopLon[s], tt.ShpLat[j], tt.ShpLon[j])
				if d < bestD {
					bestD, bestIdx = d, j
				}
			}
			tt.PatShapeIdx[tt.PatFirstStop[pi]+uint32(k)] = bestIdx
			cursor = bestIdx
		}
	}

	// ---- GTFS-RT matching index -----------------------------------------------------
	tt.TripIdx = make(map[string]uint32, len(tt.TripID))
	for i, id := range tt.TripID {
		tt.TripIdx[tt.Feeds[tt.TripFeed[i]]+":"+id] = uint32(i)
	}

	// ---- snap stops & compute transfers on the street graph -----------------------
	computeSnaps(tt, g, snapRadiusM)
	computeTransfers(tt, g, walkSpeedKmh, transferRadiusM)

	st.Stops = len(tt.StopID)
	st.Patterns = len(pats)
	st.Trips = len(tt.TripService)
	st.StopTimes = len(tt.Arr)
	st.Transfers = len(tt.XferTo)
	st.Excluded = tt.Excluded
	st.Elapsed = time.Since(t0)
	return tt, st, nil
}

func pickTZ(feeds []*gtfs.Feed) string {
	for _, f := range feeds {
		if f.TZ != "" {
			return f.TZ
		}
	}
	return "Europe/Rome"
}

// patternKey builds a compact map key from route and the raw stop id slice.
func patternKey(route int32, stopOff int32, stops []int32) string {
	buf := make([]byte, 8+len(stops)*4)
	putI32 := func(o int, v int32) {
		buf[o] = byte(v)
		buf[o+1] = byte(v >> 8)
		buf[o+2] = byte(v >> 16)
		buf[o+3] = byte(v >> 24)
	}
	putI32(0, route)
	putI32(4, stopOff)
	for i, s := range stops {
		putI32(8+i*4, s)
	}
	return unsafe.String(&buf[0], len(buf))
}
