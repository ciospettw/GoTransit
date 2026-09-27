package engine

import (
	"fmt"
	"sort"
	"time"

	"gotransit/internal/gbfs"
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
	// SharedVehicle selects the GBFS class for multimodal requests:
	// bicycle (default), scooter, or any.
	SharedVehicle string
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

	// Feasibility is the product of the per-leg catch probabilities (risk.go):
	// the chance the whole chain of connections works out. RiskLevel is the
	// worst per-leg level ("ok" | "warn" | "risk"). Informational only —
	// risky itineraries are ranked down, never hidden.
	Feasibility float64 `json:"feasibility,omitempty"`
	RiskLevel   string  `json:"risk_level,omitempty"`

	sig    string    // dedupe signature
	expArr time.Time // expected arrival incl. miss costs; zero = use Arrive
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
	Mode      string     `json:"mode"` // walk | bike | scooter | car | transit
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
	Boarded   bool       `json:"boarded,omitempty"`  // retained onboard leg in a live replan
	// BoardReadyAt is internal tracking metadata. It is populated when an
	// onboard replan starts directly at a rail transfer, whose incoming-arrival
	// anchor would otherwise be absent from the returned itinerary.
	BoardReadyAt time.Time        `json:"-"`
	Rental       *gbfs.Assignment `json:"rental,omitempty"`

	// Transit legs only: probability the rider makes THIS boarding given the
	// connection slack and the delay distributions (risk.go). RiskLevel is
	// the discretized label ("ok" ≥ risk_ok, "warn" ≥ risk_warn, else "risk").
	CatchProb float64 `json:"catch_prob,omitempty"`
	RiskLevel string  `json:"risk_level,omitempty"`
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
	case "transit", "":
		req.SharedVehicle = ""
		return e.planTransit(req, fromLat, fromLon, toLat, toLon, false, "")
	case "bike_transit", "bike+transit":
		if req.SharedVehicle == "" {
			req.SharedVehicle = "bicycle"
		}
		class := gbfs.ParseClass(req.SharedVehicle)
		if class != gbfs.ClassBicycle && class != gbfs.ClassScooter && class != gbfs.ClassAny {
			return nil, fmt.Errorf("unknown shared vehicle class %q (use bicycle, scooter, or any)", req.SharedVehicle)
		}
		return e.planTransit(req, fromLat, fromLon, toLat, toLon, true, class)
	case "scooter_transit", "scooter+transit", "shared_transit":
		if !e.SharedConfigured && e.GBFS == nil {
			return nil, fmt.Errorf("mode %q requires at least one [[gbfs]] feed", req.Mode)
		}
		if req.SharedVehicle == "" {
			if req.Mode == "shared_transit" {
				req.SharedVehicle = "any"
			} else {
				req.SharedVehicle = "scooter"
			}
		}
		class := gbfs.ParseClass(req.SharedVehicle)
		if class != gbfs.ClassBicycle && class != gbfs.ClassScooter && class != gbfs.ClassAny {
			return nil, fmt.Errorf("unknown shared vehicle class %q (use bicycle, scooter, or any)", req.SharedVehicle)
		}
		return e.planTransit(req, fromLat, fromLon, toLat, toLon, true, class)
	case "personal_bike_transit":
		req.SharedVehicle = ""
		return e.planTransit(req, fromLat, fromLon, toLat, toLon, true, "")
	default:
		return nil, fmt.Errorf("unknown mode %q (use transit, bike_transit, scooter_transit, personal_bike_transit, bike, car, walk)", req.Mode)
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
	case "bike", "scooter":
		speed := e.Cfg.Routing.BikeSpeedKmh
		if mode == "scooter" {
			speed = e.Cfg.Routing.ScooterSpeedKmh
		}
		m, sf = graph.ModeBike, graph.SpeedFactor(speed)
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

	leg := e.roadLeg(g, mode, m, sf, res.Edges, res.EndSeed, snF, snT, fLat, fLon, tLat, tLon)
	dur := time.Duration(res.Ds) * 100 * time.Millisecond
	depart := when.In(e.Timezone()) // answers always speak the network's timezone
	if arriveBy {
		depart = depart.Add(-dur)
	}
	leg.Depart = depart
	leg.Arrive = depart.Add(dur)
	leg.DurationS = int(dur.Seconds())
	reconcileLegSteps(&leg)
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
	return fixedSpeedDs(uint32(meters), sf)
}

func fixedSpeedDs(meters uint32, sf uint32) uint32 {
	return uint32((uint64(meters) * uint64(sf)) >> 16)
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
	sec          map[int32]uint32
	anchor       map[int32]int32
	mode         string
	priorRailArr map[int32]uint32 // actual incoming-rail arrival by source stop
	absolute     bool             // sec values are already seconds since midnight
	shared       map[int32]sharedPlan
}

func (e *Engine) planTransit(req Request, fLat, fLon, tLat, tLon int32, bikeAllowed bool, sharedClass gbfs.Class) (*Response, error) {
	gb, tb := e.GraphBundle(), e.TTBundle()
	r := e.Cfg.Routing
	useShared := sharedClass != "" && (e.SharedConfigured || e.GBFS != nil)

	sfWalk := graph.SpeedFactor(r.WalkSpeedKmh)
	rideSpeed := r.BikeSpeedKmh
	if sharedClass == gbfs.ClassScooter {
		rideSpeed = r.ScooterSpeedKmh
	} else if sharedClass == gbfs.ClassAny && r.ScooterSpeedKmh > rideSpeed {
		rideSpeed = r.ScooterSpeedKmh
	}
	sfBike := graph.SpeedFactor(rideSpeed)

	// access/egress walking sets (always)
	walkAcc := e.reachStops(gb, tb, fLat, fLon, graph.ModeFoot, sfWalk, uint32(r.MaxWalkAccess.Seconds()*10), "walk")
	walkEgr := e.reachStops(gb, tb, tLat, tLon, graph.ModeFoot, sfWalk, uint32(r.MaxWalkAccess.Seconds()*10), "walk")
	if len(walkAcc.sec) == 0 && !bikeAllowed {
		return nil, fmt.Errorf("no stops reachable on foot from the origin (max %s)", r.MaxWalkAccess)
	}
	if len(walkEgr.sec) == 0 && !bikeAllowed {
		return nil, fmt.Errorf("no stops reachable on foot around the destination (max %s)", r.MaxWalkAccess)
	}

	when := req.When
	var bikeAcc, bikeEgr *accessSet
	if bikeAllowed {
		ba := e.reachStops(gb, tb, fLat, fLon, graph.ModeBike, sfBike, uint32(r.MaxBikeAccess.Seconds()*10), "bike")
		be := e.reachStops(gb, tb, tLat, tLon, graph.ModeBike, sfBike, uint32(r.MaxBikeAccess.Seconds()*10), "bike")
		if useShared {
			if e.GBFS != nil {
				ba = e.sharedAccessSet(gb, tb, ba, fLat, fLon, true, sharedClass, whenOrNow(req.When))
				be = e.sharedAccessSet(gb, tb, be, tLat, tLon, false, sharedClass, whenOrNow(req.When))
			} else {
				ba = accessSet{sec: map[int32]uint32{}, anchor: map[int32]int32{}, mode: "shared", shared: map[int32]sharedPlan{}}
				be = accessSet{sec: map[int32]uint32{}, anchor: map[int32]int32{}, mode: "shared", shared: map[int32]sharedPlan{}}
			}
		}
		bikeAcc, bikeEgr = &ba, &be
	}

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

	// Multimodal planning always includes the honest direct-ride comparison.
	// With GBFS this is another checked rental, never an imaginary personal bike.
	if bikeAllowed {
		if useShared {
			if e.GBFS != nil { // nil while GBFS boots: walk/transit remains
				if direct, ok := e.planSharedDirect(fLat, fLon, tLat, tLon, when, sharedClass); ok &&
					(walkBestArr.IsZero() || !direct.Arrive.After(walkBestArr.Add(10*time.Minute))) {
					all = append(all, direct)
				}
			}
		} else if direct, err := e.planRoad("bike", fLat, fLon, tLat, tLon, when, false); err == nil {
			if direct.DurationS <= 45*60 && (walkBestArr.IsZero() || !direct.Arrive.After(walkBestArr.Add(10*time.Minute))) {
				all = append(all, *direct)
			}
		}
	}

	if len(all) == 0 {
		return nil, fmt.Errorf("no transit itinerary found")
	}
	e.annotateRisk(all, when)
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
		var best time.Time
		variants := [][2]*accessSet{{&walkAcc, &walkEgr}}
		if bikeAcc != nil && bikeEgr != nil {
			variants = append(variants, [2]*accessSet{bikeAcc, &walkEgr}, [2]*accessSet{&walkAcc, bikeEgr})
		}
		for _, variant := range variants {
			if len(variant[0].sec) == 0 || len(variant[1].sec) == 0 {
				continue
			}
			for _, it := range e.runRaptor(gb, tb, req, dep, fLat, fLon, tLat, tLon, variant[0], variant[1]) {
				if best.IsZero() || it.Arrive.Before(best) {
					best = it.Arrive
				}
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
	sharedClass := gbfs.Class("")
	if (e.GBFS != nil || e.SharedConfigured) && req.SharedVehicle != "" {
		sharedClass = gbfs.ParseClass(req.SharedVehicle)
	}
	resp, err := e.planTransit(req2, fLat, fLon, tLat, tLon, bikeAcc != nil, sharedClass)
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
			total := d + fixedSpeedDs(uint32(tt.NSExtraM[i]), sf)
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
		MaxTransfers:    e.Cfg.Routing.MaxTransfers,
		SlackSec:        uint32(e.Cfg.Routing.TransferSlack.Seconds()),
		RailEntrySec:    uint32(e.Cfg.Routing.RailEntryBuffer.Seconds()),
		RailTransferSec: uint32(e.Cfg.Routing.RailTransferBuffer.Seconds()),
	}
	for s, sec := range acc.sec {
		at := depSec + sec
		if acc.absolute {
			at = sec
		}
		seed := transit.StopSeed{Stop: s, Sec: at}
		if priorArr, ok := acc.priorRailArr[s]; ok {
			seed.PriorRail = true
			seed.PriorRailArrSec = priorArr
		}
		q.Sources = append(q.Sources, seed)
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

// dedupeRank sorts by EXPECTED arrival (nominal arrival + Σ (1−P)·missCost
// over the connections, stamped by annotateRisk; zero expArr = plain arrival),
// then trims duplicates and caps the count. A fast itinerary with fragile
// connections loses to a robust one arriving slightly later — but it stays
// in the list, labeled, for the rider to choose.
func dedupeRank(its []Itinerary, num int) []Itinerary {
	eff := func(it *Itinerary) time.Time {
		if it.expArr.IsZero() {
			return it.Arrive
		}
		return it.expArr
	}
	sort.Slice(its, func(i, j int) bool {
		ei, ej := eff(&its[i]), eff(&its[j])
		if !ei.Equal(ej) {
			return ei.Before(ej)
		}
		if its[i].Transfers != its[j].Transfers {
			return its[i].Transfers < its[j].Transfers
		}
		return its[i].Arrive.Before(its[j].Arrive)
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
