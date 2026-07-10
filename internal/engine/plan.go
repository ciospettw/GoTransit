package engine

import (
	"fmt"
	"sort"
	"time"

	"gotransit/internal/geo"
	"gotransit/internal/graph"
	"gotransit/internal/transit"
)

// Request is one routing query.
type Request struct {
	FromLat, FromLon float64
	ToLat, ToLon     float64
	Mode             string    // transit | bike_transit | bike | car | walk
	When             time.Time // departure, or arrival when ArriveBy
	ArriveBy         bool
	Num              int
	Live             bool // also return the strictly RT-covered subset
}

// Response is the itinerary set.
type Response struct {
	Itineraries []Itinerary `json:"itineraries"`
	// with live=true: itineraries whose near-term transit legs are all
	// RT-confirmed (first leg live and within the live window)
	LiveItineraries []Itinerary `json:"live_itineraries,omitempty"`
	Note            string      `json:"note,omitempty"`
}

type Itinerary struct {
	ID        string    `json:"id,omitempty"` // token for /v1/track
	Live      bool      `json:"live"`
	Depart    time.Time `json:"depart"`
	Arrive    time.Time `json:"arrive"`
	DurationS int       `json:"duration_s"`
	Transfers int       `json:"transfers"`
	Legs      []Leg     `json:"legs"`

	sig string // dedupe signature
}

type Place struct {
	Name   string  `json:"name,omitempty"`
	Lat    float64 `json:"lat"`
	Lon    float64 `json:"lon"`
	StopID string  `json:"stop_id,omitempty"`
	Code   string  `json:"code,omitempty"`
}

type RouteRef struct {
	ID        string `json:"id"`
	ShortName string `json:"short_name,omitempty"`
	LongName  string `json:"long_name,omitempty"`
	Color     string `json:"color,omitempty"`
	TextColor string `json:"text_color,omitempty"`
	Agency    string `json:"agency,omitempty"`
	Type      int    `json:"type"`
}

type StopTime struct {
	ID     string    `json:"id"`
	Code   string    `json:"code,omitempty"`
	Name   string    `json:"name"`
	Lat    float64   `json:"lat"`
	Lon    float64   `json:"lon"`
	Arrive time.Time `json:"arrive"`
	Depart time.Time `json:"depart"`
}

type Step struct {
	Kind      string  `json:"kind"`
	Modifier  string  `json:"modifier,omitempty"`
	Name      string  `json:"name,omitempty"`
	DistanceM int     `json:"distance_m"`
	DurationS int     `json:"duration_s"`
	Lat       float64 `json:"lat"`
	Lon       float64 `json:"lon"`
}

type Leg struct {
	Mode      string     `json:"mode"` // walk | bike | car | transit
	From      Place      `json:"from"`
	To        Place      `json:"to"`
	Depart    time.Time  `json:"depart"`
	Arrive    time.Time  `json:"arrive"`
	DurationS int        `json:"duration_s"`
	DistanceM int        `json:"distance_m"`
	Polyline  string     `json:"polyline,omitempty"`
	Route     *RouteRef  `json:"route,omitempty"`
	TripID    string     `json:"trip_id,omitempty"`
	Headsign  string     `json:"headsign,omitempty"`
	Stops     []StopTime `json:"stops,omitempty"`
	Steps     []Step     `json:"steps,omitempty"`
	Realtime  bool       `json:"realtime,omitempty"` // trip has live GTFS-RT data
	DelayS    int        `json:"delay_s,omitempty"`  // departure delay vs schedule
}

// Plan answers a routing request against the current snapshots.
func (e *Engine) Plan(req Request) (*Response, error) {
	if !e.Ready() {
		return nil, fmt.Errorf("engine still starting up")
	}
	e.Queries.Add(1)
	if req.Num <= 0 {
		req.Num = e.Cfg.Routing.MaxItineraries
	}
	fromLat, fromLon := int32(req.FromLat*1e7), int32(req.FromLon*1e7)
	toLat, toLon := int32(req.ToLat*1e7), int32(req.ToLon*1e7)

	switch req.Mode {
	case "car", "bike", "walk":
		it, err := e.planRoad(req.Mode, fromLat, fromLon, toLat, toLon, req.When, req.ArriveBy)
		if err != nil {
			return nil, err
		}
		return &Response{Itineraries: []Itinerary{*it}}, nil
	case "transit", "", "bike_transit", "bike+transit":
		bike := req.Mode == "bike_transit" || req.Mode == "bike+transit"
		return e.planTransit(req, fromLat, fromLon, toLat, toLon, bike)
	default:
		return nil, fmt.Errorf("unknown mode %q (use transit, bike_transit, bike, car, walk)", req.Mode)
	}
}

// ---- road modes ---------------------------------------------------------------

func (e *Engine) planRoad(mode string, fLat, fLon, tLat, tLon int32, when time.Time, arriveBy bool) (*Itinerary, error) {
	gb := e.GraphBundle()
	g := gb.G
	var m graph.Mode
	var sf uint32
	var eps float64
	switch mode {
	case "car":
		m, sf = graph.ModeCar, 0
		eps = 1.2
		if e.Cfg.Routing.CarHeuristic == "exact" {
			eps = 1.0
		}
	case "bike":
		m, sf = graph.ModeBike, graph.SpeedFactor(e.Cfg.Routing.BikeSpeedKmh)
		eps = 1.1
	default:
		m, sf = graph.ModeFoot, graph.SpeedFactor(e.Cfg.Routing.WalkSpeedKmh)
		eps = 1.05
	}
	snapR := float64(e.Cfg.Routing.SnapRadiusM)
	if mode == "car" {
		snapR *= 2 // driveable streets can be farther from the door
	}
	snF, okF := g.SnapPoint(fLat, fLon, m, snapR)
	snT, okT := g.SnapPoint(tLat, tLon, m, snapR)
	if !okF {
		return nil, fmt.Errorf("origin is too far from a %s-accessible street", mode)
	}
	if !okT {
		return nil, fmt.Errorf("destination is too far from a %s-accessible street", mode)
	}

	rs := gb.Road()
	defer gb.PutRoad(rs)
	res := rs.Route(g, srcSeeds(g, snF, m, sf), dstSeeds(g, snT, m, sf), tLat, tLon, m, sf, eps, 6<<20)
	if !res.Found {
		return nil, fmt.Errorf("no %s route found", mode)
	}

	leg := e.roadLeg(g, mode, m, sf, res.Edges, snF, snT, fLat, fLon, tLat, tLon)
	dur := time.Duration(res.Ds) * 100 * time.Millisecond
	depart := when.In(e.Timezone()) // answers always speak the network's timezone
	if arriveBy {
		depart = depart.Add(-dur)
	}
	leg.Depart = depart
	leg.Arrive = depart.Add(dur)
	leg.DurationS = int(dur.Seconds())
	it := &Itinerary{
		Depart: leg.Depart, Arrive: leg.Arrive,
		DurationS: leg.DurationS, Legs: []Leg{leg},
	}
	return it, nil
}

// srcSeeds/dstSeeds convert a snap into directed search seeds with the cost
// of the partial edge.
func srcSeeds(g *graph.Graph, sn graph.Snap, m graph.Mode, sf uint32) []graph.Seed {
	var out []graph.Seed
	if sn.Fwd >= 0 && g.Allowed(uint32(sn.Fwd), m) {
		out = append(out, graph.Seed{Node: sn.V, Ds: partialDs(g, uint32(sn.Fwd), sn.AlongV+sn.PerpM, m, sf)})
	}
	if sn.Bwd >= 0 && g.Allowed(uint32(sn.Bwd), m) {
		out = append(out, graph.Seed{Node: sn.U, Ds: partialDs(g, uint32(sn.Bwd), sn.AlongU+sn.PerpM, m, sf)})
	}
	if len(out) == 0 { // e.g. foot on a mode-mixed edge: fall back to both ends
		out = append(out,
			graph.Seed{Node: sn.U, Ds: partialDs(g, uint32(max32(sn.Fwd, 0)), sn.AlongU+sn.PerpM, m, sf)},
			graph.Seed{Node: sn.V, Ds: partialDs(g, uint32(max32(sn.Fwd, 0)), sn.AlongV+sn.PerpM, m, sf)})
	}
	return out
}

func dstSeeds(g *graph.Graph, sn graph.Snap, m graph.Mode, sf uint32) []graph.Seed {
	var out []graph.Seed
	if sn.Fwd >= 0 && g.Allowed(uint32(sn.Fwd), m) {
		out = append(out, graph.Seed{Node: sn.U, Ds: partialDs(g, uint32(sn.Fwd), sn.AlongU+sn.PerpM, m, sf)})
	}
	if sn.Bwd >= 0 && g.Allowed(uint32(sn.Bwd), m) {
		out = append(out, graph.Seed{Node: sn.V, Ds: partialDs(g, uint32(sn.Bwd), sn.AlongV+sn.PerpM, m, sf)})
	}
	if len(out) == 0 {
		out = append(out,
			graph.Seed{Node: sn.U, Ds: partialDs(g, uint32(max32(sn.Fwd, 0)), sn.AlongU+sn.PerpM, m, sf)},
			graph.Seed{Node: sn.V, Ds: partialDs(g, uint32(max32(sn.Fwd, 0)), sn.AlongV+sn.PerpM, m, sf)})
	}
	return out
}

func partialDs(g *graph.Graph, e uint32, meters float64, m graph.Mode, sf uint32) uint32 {
	if m == graph.ModeCar {
		v := uint32(g.EdgeSpeed[e])
		if v == 0 {
			v = 30
		}
		return uint32(meters*36) / v
	}
	return (uint32(meters) * sf) >> 16
}

func max32(a, b int32) int32 {
	if a > b {
		return a
	}
	return b
}

// ---- transit ------------------------------------------------------------------

// accessSet is one access/egress computation: stop → (seconds, anchor node).
type accessSet struct {
	sec    map[int32]uint32
	anchor map[int32]int32
	mode   string
}

func (e *Engine) planTransit(req Request, fLat, fLon, tLat, tLon int32, bikeAllowed bool) (*Response, error) {
	gb, tb := e.GraphBundle(), e.TTBundle()
	r := e.Cfg.Routing

	sfWalk := graph.SpeedFactor(r.WalkSpeedKmh)
	sfBike := graph.SpeedFactor(r.BikeSpeedKmh)

	// access/egress walking sets (always)
	walkAcc := e.reachStops(gb, tb, fLat, fLon, graph.ModeFoot, sfWalk, uint32(r.MaxWalkAccess.Seconds()*10), "walk")
	walkEgr := e.reachStops(gb, tb, tLat, tLon, graph.ModeFoot, sfWalk, uint32(r.MaxWalkAccess.Seconds()*10), "walk")
	if len(walkAcc.sec) == 0 && !bikeAllowed {
		return nil, fmt.Errorf("no stops reachable on foot from the origin (max %s)", r.MaxWalkAccess)
	}
	if len(walkEgr.sec) == 0 && !bikeAllowed {
		return nil, fmt.Errorf("no stops reachable on foot around the destination (max %s)", r.MaxWalkAccess)
	}

	var bikeAcc, bikeEgr *accessSet
	if bikeAllowed {
		ba := e.reachStops(gb, tb, fLat, fLon, graph.ModeBike, sfBike, uint32(r.MaxBikeAccess.Seconds()*10), "bike")
		be := e.reachStops(gb, tb, tLat, tLon, graph.ModeBike, sfBike, uint32(r.MaxBikeAccess.Seconds()*10), "bike")
		bikeAcc, bikeEgr = &ba, &be
	}

	when := req.When
	if req.ArriveBy {
		return e.planTransitArriveBy(req, fLat, fLon, tLat, tLon, walkAcc, walkEgr, bikeAcc, bikeEgr)
	}

	variants := []struct {
		acc, egr *accessSet
		tag      string
	}{{&walkAcc, &walkEgr, "walk/walk"}}
	if bikeAllowed {
		variants = append(variants,
			struct {
				acc, egr *accessSet
				tag      string
			}{bikeAcc, &walkEgr, "bike/walk"},
			struct {
				acc, egr *accessSet
				tag      string
			}{&walkAcc, bikeEgr, "walk/bike"},
		)
	}

	var all []Itinerary
	var walkBestArr time.Time
	for vi, v := range variants {
		if len(v.acc.sec) == 0 || len(v.egr.sec) == 0 {
			continue
		}
		its := e.runRaptor(gb, tb, req, when, fLat, fLon, tLat, tLon, v.acc, v.egr)
		if vi == 0 {
			for _, it := range its {
				if walkBestArr.IsZero() || it.Arrive.Before(walkBestArr) {
					walkBestArr = it.Arrive
				}
			}
			all = append(all, its...)
			continue
		}
		// bike realism: a bike variant must beat the classic plan clearly
		for _, it := range its {
			if walkBestArr.IsZero() || it.Arrive.Add(r.BikeTransitMinSaving).Before(walkBestArr) ||
				it.Arrive.Add(r.BikeTransitMinSaving).Equal(walkBestArr) {
				all = append(all, it)
			}
		}
	}

	// bike+transit also offers the honest comparison: just ride the bike
	if bikeAllowed {
		if direct, err := e.planRoad("bike", fLat, fLon, tLat, tLon, when, false); err == nil {
			directDur := time.Duration(direct.DurationS) * time.Second
			include := directDur <= 45*time.Minute
			if !walkBestArr.IsZero() && direct.Arrive.After(walkBestArr.Add(10*time.Minute)) {
				include = false
			}
			if include {
				all = append(all, *direct)
			}
		}
	}

	if len(all) == 0 {
		return nil, fmt.Errorf("no transit itinerary found")
	}
	all = dedupeRank(all, req.Num)
	e.annotateLive(all, when)
	e.remember(all, req)
	resp := &Response{Itineraries: all}
	if req.Live {
		for _, it := range all {
			if it.Live {
				resp.LiveItineraries = append(resp.LiveItineraries, it)
			}
		}
		if len(resp.LiveItineraries) == 0 {
			resp.Note = "no fully RT-confirmed itinerary right now; static itineraries only"
		}
	}
	return resp, nil
}

// planTransitArriveBy: latest departure whose arrival stays ≤ the deadline,
// found by binary search over forward runs (RAPTOR runs are ~ms).
func (e *Engine) planTransitArriveBy(req Request, fLat, fLon, tLat, tLon int32, walkAcc, walkEgr accessSet, bikeAcc, bikeEgr *accessSet) (*Response, error) {
	gb, tb := e.GraphBundle(), e.TTBundle()
	deadline := req.When

	arrivalFor := func(dep time.Time) (time.Time, bool) {
		its := e.runRaptor(gb, tb, req, dep, fLat, fLon, tLat, tLon, &walkAcc, &walkEgr)
		var best time.Time
		for _, it := range its {
			if best.IsZero() || it.Arrive.Before(best) {
				best = it.Arrive
			}
		}
		return best, !best.IsZero()
	}

	// bracket: start from a plausible departure and widen until feasible
	beeline := geo.Dist(fLat, fLon, tLat, tLon)
	est := time.Duration(beeline/5) * time.Second // ~18 km/h effective transit speed
	if est < 20*time.Minute {
		est = 20 * time.Minute
	}
	lo := deadline.Add(-est - 30*time.Minute)
	for tries := 0; tries < 4; tries++ {
		if arr, ok := arrivalFor(lo); ok && !arr.After(deadline) {
			break
		}
		lo = lo.Add(-90 * time.Minute)
	}
	hi := deadline
	if arr, ok := arrivalFor(lo); !ok || arr.After(deadline) {
		return nil, fmt.Errorf("no itinerary arrives by %s", deadline.Format(time.RFC3339))
	}
	for hi.Sub(lo) > time.Minute {
		mid := lo.Add(hi.Sub(lo) / 2)
		if arr, ok := arrivalFor(mid); ok && !arr.After(deadline) {
			lo = mid
		} else {
			hi = mid
		}
	}

	req2 := req
	req2.ArriveBy = false
	req2.When = lo
	resp, err := e.planTransit(req2, fLat, fLon, tLat, tLon, bikeAcc != nil)
	if err != nil {
		return nil, err
	}
	kept := resp.Itineraries[:0]
	for _, it := range resp.Itineraries {
		if !it.Arrive.After(deadline) {
			kept = append(kept, it)
		}
	}
	if len(kept) == 0 {
		return nil, fmt.Errorf("no itinerary arrives by %s", deadline.Format(time.RFC3339))
	}
	resp.Itineraries = kept
	return resp, nil
}

// reachStops runs a bounded street search and harvests stop seeds.
func (e *Engine) reachStops(gb *GraphBundle, tb *TTBundle, lat, lon int32, m graph.Mode, sf uint32, maxDs uint32, tag string) accessSet {
	g, tt := gb.G, tb.TT
	out := accessSet{sec: map[int32]uint32{}, anchor: map[int32]int32{}, mode: tag}
	sn, ok := g.SnapPoint(lat, lon, m, float64(e.Cfg.Routing.SnapRadiusM))
	if !ok {
		return out
	}
	ns := gb.Near()
	defer gb.PutNear(ns)
	ns.Run(g, srcSeeds(g, sn, m, sf), m, sf, maxDs)
	for _, n := range ns.Touched() {
		d, _ := ns.Dist(n)
		i, _ := findNS(tt.NSNode, n)
		for ; i < len(tt.NSNode) && tt.NSNode[i] == n; i++ {
			s := tt.NSStop[i]
			total := d + uint32(tt.NSExtra[i])
			if total > maxDs {
				continue
			}
			secs := (total + 5) / 10
			if cur, ok := out.sec[s]; !ok || secs < cur {
				out.sec[s] = secs
				out.anchor[s] = n
			}
		}
	}
	return out
}

func findNS(nodes []int32, n int32) (int, bool) {
	lo, hi := 0, len(nodes)
	for lo < hi {
		mid := (lo + hi) / 2
		if nodes[mid] < n {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo, lo < len(nodes) && nodes[lo] == n
}

// runRaptor executes one RAPTOR query and assembles full itineraries.
func (e *Engine) runRaptor(gb *GraphBundle, tb *TTBundle, req Request, when time.Time,
	fLat, fLon, tLat, tLon int32, acc, egr *accessSet) []Itinerary {

	tt := tb.TT
	local := when.In(tt.TZ)
	base := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, tt.TZ)
	depSec := uint32(local.Sub(base).Seconds())
	prev := base.AddDate(0, 0, -1)

	q := transit.Query{
		Date:     dateInt(base),
		Weekday:  base.Weekday(),
		PrevDate: dateInt(prev), PrevWeekday: prev.Weekday(),
		MaxTransfers: e.Cfg.Routing.MaxTransfers,
		SlackSec:     uint32(e.Cfg.Routing.TransferSlack.Seconds()),
	}
	for s, sec := range acc.sec {
		q.Sources = append(q.Sources, transit.StopSeed{Stop: s, Sec: depSec + sec})
	}
	for s, sec := range egr.sec {
		q.Targets = append(q.Targets, transit.StopSeed{Stop: s, Sec: sec})
	}

	rap := tb.Raptor()
	journeys := rap.Plan(q)
	tb.PutRaptor(rap)

	var out []Itinerary
	for _, j := range journeys {
		if it, ok := e.assemble(gb, tb, j, base, depSec, fLat, fLon, tLat, tLon, acc, egr); ok {
			out = append(out, it)
		}
	}
	return out
}

func dateInt(t time.Time) uint32 {
	return uint32(t.Year()*10000 + int(t.Month())*100 + t.Day())
}

// dedupeRank sorts by arrival then trims duplicates and caps the count.
func dedupeRank(its []Itinerary, num int) []Itinerary {
	sort.Slice(its, func(i, j int) bool {
		if !its[i].Arrive.Equal(its[j].Arrive) {
			return its[i].Arrive.Before(its[j].Arrive)
		}
		return its[i].Transfers < its[j].Transfers
	})
	seen := map[string]bool{}
	out := its[:0]
	for _, it := range its {
		if seen[it.sig] {
			continue
		}
		seen[it.sig] = true
		out = append(out, it)
		if len(out) >= num {
			break
		}
	}
	return out
}
