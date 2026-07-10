package rt

import (
	"time"

	"gotransit/internal/transit"
)

// buildOverlay projects decoded feeds onto the timetable. Called with the
// manager lock held; reads tt (immutable) and the feeds (immutable).
func buildOverlay(tt *transit.Timetable, sources []Source, tus, vps map[int]*Feed,
	stats map[int]*SourceStats, now time.Time, version uint64) *transit.RTOverlay {

	n := tt.NumTrips()
	o := &transit.RTOverlay{
		TripOff:   make([]int32, n),
		Skip:      make([]bool, n),
		HasRT:     make([]bool, n),
		Passed:    make([]int16, n),
		VehLat:    make([]int32, n),
		VehLon:    make([]int32, n),
		VehPos:    make([]int16, n),
		VehStatus: make([]int8, n),
		FeedTime:  make([]uint64, len(tt.Feeds)),
		Version:   version,
	}
	for i := range o.TripOff {
		o.TripOff[i] = -1
		o.Passed[i] = -1
		o.VehPos[i] = -1
		o.VehStatus[i] = -1
	}
	nowUnix := now.Unix()

	for _, src := range sources {
		st := stats[src.FeedIdx]
		st.Trips, st.Vehicles, st.Matched, st.Unmatched, st.Cancelled = 0, 0, 0, 0, 0

		// GTFS-RT allows mixed feeds: trips and vehicles are honored from
		// both endpoints alike
		for _, f := range []*Feed{tus[src.FeedIdx], vps[src.FeedIdx]} {
			if f == nil {
				continue
			}
			if src.FeedIdx < len(o.FeedTime) && f.Timestamp > o.FeedTime[src.FeedIdx] {
				o.FeedTime[src.FeedIdx] = f.Timestamp
				st.FeedTime = time.Unix(int64(f.Timestamp), 0).Format(time.RFC3339)
			}
			st.Trips += len(f.Trips)
			st.Vehicles += len(f.Vehicles)
			for i := range f.Trips {
				tu := &f.Trips[i]
				trip, ok := tt.TripIdx[src.Name+":"+tu.TripID]
				if !ok {
					st.Unmatched++
					continue
				}
				st.Matched++
				o.HasRT[trip] = true
				if tu.Cancelled {
					o.Skip[trip] = true
					st.Cancelled++
					continue
				}
				if o.TripOff[trip] < 0 { // first update wins (dedupe across feeds)
					applyTripUpdate(tt, o, trip, tu, nowUnix)
				}
			}
			for i := range f.Vehicles {
				v := &f.Vehicles[i]
				trip, ok := tt.TripIdx[src.Name+":"+v.TripID]
				if !ok {
					continue
				}
				o.HasRT[trip] = true
				if v.Lat != 0 || v.Lon != 0 {
					o.VehLat[trip] = int32(float64(v.Lat) * 1e7)
					o.VehLon[trip] = int32(float64(v.Lon) * 1e7)
				}
				if v.Status != Absent {
					o.VehStatus[trip] = int8(v.Status)
				}
				if v.CurrentSeq == Absent {
					continue
				}
				// the vehicle is at/approaching CurrentSeq: everything
				// strictly before is confirmed passed
				if pos, ok := tt.TripSeqPos(trip, uint16(v.CurrentSeq)); ok {
					o.VehPos[trip] = int16(pos)
					if pos > 0 {
						if p := int16(pos - 1); p > o.Passed[trip] {
							o.Passed[trip] = p
						}
					}
				}
			}
		}
	}
	return o
}

// applyTripUpdate fills per-stop deltas for one trip. GTFS-RT semantics: a
// StopTimeUpdate's delay holds from its stop onward until the next update.
func applyTripUpdate(tt *transit.Timetable, o *transit.RTOverlay, trip uint32, tu *TripRT, nowUnix int64) {
	tlen := int(tt.TripLen(trip))
	off := int32(len(o.ArrDelta))
	o.TripOff[trip] = off
	for i := 0; i < tlen; i++ {
		o.ArrDelta = append(o.ArrDelta, 0)
		o.DepDelta = append(o.DepDelta, 0)
		o.StopSkip = append(o.StopSkip, false)
	}
	arr := o.ArrDelta[off : off+int32(tlen)]
	dep := o.DepDelta[off : off+int32(tlen)]
	skip := o.StopSkip[off : off+int32(tlen)]

	// service-day base epoch for absolute-time STUs
	base := serviceBase(tt, tu.StartDate, nowUnix)

	// prop is the running propagated delay: per GTFS-RT semantics, a stop's
	// delay holds for every following stop until the next explicit update.
	prop := int32(0)
	if tu.HasDelay && tu.DelaySec != Absent {
		prop = tu.DelaySec
	}
	cursor := 0 // fill position
	stops := patternStopsOfTrip(tt, trip)

	for si := range tu.STUs {
		stu := &tu.STUs[si]
		pos := matchSTU(tt, trip, stops, stu, cursor)
		if pos < 0 {
			continue
		}
		// fill the gap [cursor, pos) with the running delay
		for ; cursor < pos; cursor++ {
			arr[cursor], dep[cursor] = prop, prop
		}
		if stu.Skipped {
			skip[pos] = true
		}
		a, d := stuDeltas(tt, trip, uint16(pos), stu, base)
		arrHere := prop
		if a != Absent {
			arrHere = a
		}
		depHere := arrHere
		if d != Absent {
			depHere = d
		}
		arr[pos], dep[pos] = arrHere, depHere
		prop = depHere
		cursor = pos + 1

		// passed inference: a departure (or arrival) already in the past
		evt := stu.DepTime
		if evt == 0 && stu.ArrTime != 0 {
			evt = stu.ArrTime
		}
		if evt == 0 {
			evt = base + int64(tt.ScheduledDep(trip, uint16(pos))) + int64(depHere)
		}
		if evt > 0 && evt <= nowUnix {
			if p := int16(pos); p > o.Passed[trip] {
				o.Passed[trip] = p
			}
		}
	}
	for ; cursor < tlen; cursor++ {
		arr[cursor], dep[cursor] = prop, prop
	}
}

// stuDeltas extracts arrival/departure deltas in seconds, deriving them from
// absolute times when the feed omits explicit delays.
func stuDeltas(tt *transit.Timetable, trip uint32, pos uint16, stu *STU, base int64) (int32, int32) {
	a, d := stu.ArrDelay, stu.DepDelay
	if a == Absent && stu.ArrTime > 0 {
		sched := base + int64(tt.ScheduledArr(trip, pos))
		a = int32(stu.ArrTime - sched)
	}
	if d == Absent && stu.DepTime > 0 {
		sched := base + int64(tt.ScheduledDep(trip, pos))
		d = int32(stu.DepTime - sched)
	}
	return a, d
}

// matchSTU resolves a StopTimeUpdate to a pattern position, preferring the
// original stop_sequence, falling back to stop_id scan from the cursor.
func matchSTU(tt *transit.Timetable, trip uint32, stops []int32, stu *STU, cursor int) int {
	if stu.Seq != Absent {
		if pos, ok := tt.TripSeqPos(trip, uint16(stu.Seq)); ok {
			return int(pos)
		}
	}
	if stu.StopID != "" {
		feed := tt.Feeds[tt.TripFeed[trip]]
		want := feed + ":" + stu.StopID
		for p := cursor; p < len(stops); p++ {
			if tt.StopID[stops[p]] == want {
				return p
			}
		}
	}
	return -1
}

func patternStopsOfTrip(tt *transit.Timetable, trip uint32) []int32 {
	return tt.PatternStops(tt.PatternOfTrip(trip))
}

// serviceBase resolves the trip's service-day midnight as a unix epoch.
func serviceBase(tt *transit.Timetable, startDate string, nowUnix int64) int64 {
	loc := tt.TZ
	if len(startDate) == 8 {
		y := atoi(startDate[0:4])
		mo := atoi(startDate[4:6])
		dd := atoi(startDate[6:8])
		if y > 2000 && mo >= 1 && mo <= 12 && dd >= 1 && dd <= 31 {
			return time.Date(y, time.Month(mo), dd, 0, 0, 0, 0, loc).Unix()
		}
	}
	nowLoc := time.Unix(nowUnix, 0).In(loc)
	return time.Date(nowLoc.Year(), nowLoc.Month(), nowLoc.Day(), 0, 0, 0, 0, loc).Unix()
}

func atoi(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return -1
		}
		n = n*10 + int(c-'0')
	}
	return n
}
