package engine

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"gotransit/internal/gbfs"
	"gotransit/internal/graph"
	"gotransit/internal/transit"
)

// IsRailLikeType reports routes eligible for schedule-assumed tracking when
// no usable VehiclePosition is published.
func IsRailLikeType(t int) bool { return transit.IsRailLikeRouteType(t) }

// IsMetroType is retained for source compatibility. The historical helper is
// intentionally broadened to the full rail-like family.
func IsMetroType(t int) bool { return IsRailLikeType(t) }

// legLiveEligible: RT-confirmed, or rail-like (schedule-assumed when no
// VehiclePosition exists; TripUpdates still adjust its times).
func legLiveEligible(l *Leg) bool {
	return l.Realtime || (l.Route != nil && IsRailLikeType(l.Route.Type))
}

// annotateLive stamps each itinerary with the strict liveness rule:
//   - the FIRST transit leg must be RT-covered (or rail-like) and depart within
//     realtime.live_first_leg_within (the user's very first bus is certain);
//   - every transit leg departing within realtime.live_horizon must be
//     RT-covered (or rail-like); later legs may still be schedule-only.
//
// Itineraries without transit legs (pure bike) are deterministic → live.
func (e *Engine) annotateLive(its []Itinerary, when time.Time) {
	firstWin := e.Cfg.Realtime.LiveFirstLeg
	horizon := e.Cfg.Realtime.LiveHorizon
	for i := range its {
		live := true
		first := true
		for li := range its[i].Legs {
			l := &its[i].Legs[li]
			if l.Mode != "transit" {
				continue
			}
			lead := l.Depart.Sub(when)
			if first {
				if !legLiveEligible(l) || lead > firstWin {
					live = false
				}
				first = false
				continue
			}
			if lead <= horizon && !legLiveEligible(l) {
				live = false
			}
		}
		its[i].Live = live
	}
}

// remember stores itineraries in the tracking cache and stamps their IDs.
func (e *Engine) remember(its []Itinerary, req Request) {
	const ttl = 30 * time.Minute
	const capacity = 4096
	e.itMu.Lock()
	defer e.itMu.Unlock()
	now := time.Now()
	// opportunistic expiry
	if len(e.itins) > capacity {
		for id, c := range e.itins {
			if now.Sub(c.Created) > ttl {
				delete(e.itins, id)
			}
		}
	}
	for i := range its {
		var b [8]byte
		rand.Read(b[:])
		id := "it_" + hex.EncodeToString(b[:])
		its[i].ID = id
		cp := its[i]
		e.itins[id] = &CachedItinerary{It: cp, Req: req, Created: now}
	}
}

// LookupItinerary fetches a planned itinerary by its tracking token.
func (e *Engine) LookupItinerary(id string) (*CachedItinerary, bool) {
	e.itMu.Lock()
	defer e.itMu.Unlock()
	c, ok := e.itins[id]
	if !ok || time.Since(c.Created) > 30*time.Minute {
		return nil, false
	}
	return c, true
}

// StopPlanSource describes how stop seeds were produced. ReadySlack is the
// short allowance between the incoming vehicle's arrival and being ready to
// board another vehicle. When PriorRail is true, RAPTOR measures a following
// rail transfer from the unshifted arrival while bus boardings use only the
// ready time.
type StopPlanSource struct {
	PriorRail  bool
	ReadySlack time.Duration
	// Onboard retains the actual incoming ride, including geometry and arrival
	// uncertainty. Seeds must be reachable downstream stops of this trip.
	Onboard *Leg
	// Mode/SharedVehicle keep bike/scooter+transit active during a live
	// replan from downstream stops. The transit-only variant is still run and
	// competes normally by expected arrival.
	Mode          string
	SharedVehicle string
}

// PlanFromStops runs a transit plan whose sources are stops with known
// absolute vehicle-arrival times — the onboard replan. Itineraries start
// directly at a boarding stop (no access leg).
func (e *Engine) PlanFromStops(seeds map[int32]time.Time, tLatF, tLonF float64, when time.Time, num int, source StopPlanSource) ([]Itinerary, error) {
	if !e.Ready() {
		return nil, fmt.Errorf("engine not ready")
	}
	gb, tb := e.GraphBundle(), e.TTBundle()
	tt := tb.TT
	tLat, tLon := int32(tLatF*1e7), int32(tLonF*1e7)

	local := when.In(tt.TZ)
	base := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, tt.TZ)

	acc := accessSet{
		sec: map[int32]uint32{}, anchor: map[int32]int32{}, mode: "none",
		absolute: true,
	}
	if source.PriorRail {
		acc.priorRailArr = map[int32]uint32{}
	}
	readySlack := source.ReadySlack
	if readySlack < 0 {
		readySlack = 0
	}
	for s, at := range seeds {
		rel := at.Sub(base)
		if rel < 0 {
			continue
		}
		arrSec := uint32(rel.Seconds())
		readySec := uint64(arrSec) + uint64(readySlack/time.Second)
		if readySec >= uint64(^uint32(0)>>1) {
			continue
		}
		acc.sec[s] = uint32(readySec)
		if source.PriorRail {
			acc.priorRailArr[s] = arrSec
		}
	}
	if len(acc.sec) == 0 {
		return nil, fmt.Errorf("no usable seeds")
	}
	walkEgr := e.reachStops(gb, tb, tLat, tLon, graph.ModeFoot,
		graph.SpeedFactor(e.Cfg.Routing.WalkSpeedKmh),
		uint32(e.Cfg.Routing.MaxWalkAccess.Seconds()*10), "walk")
	if len(walkEgr.sec) == 0 && source.SharedVehicle == "" {
		return nil, fmt.Errorf("no stops reachable around the destination")
	}
	reqMode := source.Mode
	if reqMode == "" {
		reqMode = "transit"
	}
	req := Request{ToLat: tLatF, ToLon: tLonF, Mode: reqMode, SharedVehicle: source.SharedVehicle, When: when, Num: num}
	its := e.runRaptor(gb, tb, req, when, 0, 0, tLat, tLon, &acc, &walkEgr)
	var sharedEgr accessSet
	if e.GBFS != nil && source.SharedVehicle != "" {
		class := gbfs.ParseClass(source.SharedVehicle)
		speed := e.Cfg.Routing.BikeSpeedKmh
		if class == gbfs.ClassScooter {
			speed = e.Cfg.Routing.ScooterSpeedKmh
		} else if class == gbfs.ClassAny && e.Cfg.Routing.ScooterSpeedKmh > speed {
			speed = e.Cfg.Routing.ScooterSpeedKmh
		}
		baseEgr := e.reachStops(gb, tb, tLat, tLon, graph.ModeBike,
			graph.SpeedFactor(speed), uint32(e.Cfg.Routing.MaxBikeAccess.Seconds()*10), "bike")
		sharedEgr = e.sharedAccessSet(gb, tb, baseEgr, tLat, tLon, false, class, when)
		if len(sharedEgr.sec) > 0 {
			its = append(its, e.runRaptor(gb, tb, req, when, 0, 0, tLat, tLon, &acc, &sharedEgr)...)
		}
	}
	if source.Onboard != nil {
		// Walking is only possible AFTER alighting at a supplied future stop.
		// RAPTOR requires a ride and therefore cannot return these tails itself.
		for stop, at := range seeds {
			if sec, ok := walkEgr.sec[stop]; ok {
				leg := e.stopStreetLeg(gb, tb, "walk", tLat, tLon, stop, true)
				leg.From, leg.To = stopPlace(tt, stop), Place{Lat: tLatF, Lon: tLonF}
				leg.Depart, leg.Arrive = at, at.Add(time.Duration(sec)*time.Second)
				leg.DurationS = int(sec)
				reconcileLegSteps(&leg)
				its = append(its, Itinerary{Legs: []Leg{leg}, Arrive: leg.Arrive})
			}
			if plan, ok := sharedEgr.shared[stop]; ok {
				legs, valid := e.sharedLegs(plan, tt.StopLat[stop], tt.StopLon[stop], tLat, tLon, at)
				if valid && len(legs) > 0 {
					// retainOnboard uses this private stop anchor to prove that the
					// tail begins at a downstream stop of the vehicle already ridden.
					legs[0].From.StopID = tt.StopID[stop]
					its = append(its, Itinerary{Legs: legs, Depart: legs[0].Depart,
						Arrive: legs[len(legs)-1].Arrive, sig: "onboard-shared|" + rentalSignature(plan.Assignment)})
				}
			}
		}
		retained := its[:0]
		for _, it := range its {
			if e.retainOnboard(tt, &it, source.Onboard, seeds, when) {
				retained = append(retained, it)
			}
		}
		its = retained
	}
	e.annotateRisk(its, when)
	its = dedupeRank(its, num)
	e.annotateLive(its, when)
	e.remember(its, req)
	return its, nil
}

// retainOnboard builds the part a rider cannot teleport past. It also merges
// a continuation on the current trip instead of inventing a second boarding.
func (e *Engine) retainOnboard(tt *transit.Timetable, it *Itinerary, current *Leg, seeds map[int32]time.Time, now time.Time) bool {
	if len(it.Legs) == 0 {
		return false
	}
	trip, ok := tt.TripIdx[current.TripID]
	if !ok {
		return false
	}
	pat := tt.PatternOfTrip(trip)
	board, alight := -1, -1
	var seedAt time.Time
	for pos, stop := range tt.PatternStops(pat) {
		if board < 0 && tt.StopID[stop] == current.From.StopID {
			board = pos
		}
		if at, seeded := seeds[stop]; seeded && pos > board && board >= 0 &&
			tt.StopID[stop] == it.Legs[0].From.StopID && !at.Before(now) {
			alight = pos
			seedAt = at
			break
		}
	}
	if board < 0 || alight <= board {
		return false
	}
	base := seedAt.Add(-time.Duration(tt.TripArr(trip, uint16(alight))) * time.Second)
	tail := it.Legs
	if tail[0].Mode == "transit" && tail[0].TripID == current.TripID {
		found := false
		for pos, stop := range tt.PatternStops(pat) {
			if pos >= alight && tt.StopID[stop] == tail[0].To.StopID {
				alight, found = pos, true
				break
			}
		}
		if !found {
			return false
		}
		tail = tail[1:]
	}
	ride := e.transitLeg(tt, transit.RLeg{Ride: true, Trip: trip, Pattern: pat, Board: uint16(board), Alight: uint16(alight)}, base)
	ride.Boarded = true
	ride.CatchProb, ride.RiskLevel = 1, "ok"
	// The seed's readiness allowance was used by RAPTOR to filter departures.
	// Street legs describe actual movement, so do not count that allowance as
	// additional walking and then subtract it a second time in the risk model.
	at := ride.Arrive
	for i := range tail {
		if tail[i].Mode == "transit" {
			break
		}
		tail[i].Depart = at
		tail[i].Arrive = at.Add(time.Duration(tail[i].DurationS) * time.Second)
		at = tail[i].Arrive
	}
	it.Legs = append([]Leg{ride}, tail...)
	it.Arrive = it.Legs[len(it.Legs)-1].Arrive
	it.Depart = now
	it.DurationS = int(it.Arrive.Sub(now).Seconds())
	it.Transfers = 0
	it.sig = ""
	for _, leg := range it.Legs {
		if leg.Mode == "transit" {
			it.Transfers++
			it.sig += leg.TripID + ":" + leg.To.StopID + ";"
		}
	}
	it.Transfers--
	return true
}
