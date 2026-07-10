package engine

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"gotransit/internal/graph"
)

// IsMetroType: metro/subway route types run frequently and reliably but
// emit no VehiclePosition/TripUpdate entities — they count as live without
// an RT signal (GTFS type 1 and extended urban-rail types 400-404).
func IsMetroType(t int) bool { return t == 1 || (t >= 400 && t <= 404) }

// legLiveEligible: RT-confirmed, or a metro leg (always considered live).
func legLiveEligible(l *Leg) bool {
	return l.Realtime || (l.Route != nil && IsMetroType(l.Route.Type))
}

// annotateLive stamps each itinerary with the strict liveness rule:
//   - the FIRST transit leg must be RT-covered (or metro) and depart within
//     realtime.live_first_leg_within (the user's very first bus is certain);
//   - every transit leg departing within realtime.live_horizon must be
//     RT-covered (or metro); later legs may still be schedule-only.
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

// PlanFromStops runs a transit plan whose sources are stops with known
// absolute arrival times — the "user is on board / at a stop" replan.
// Itineraries start directly at a boarding stop (no access leg).
func (e *Engine) PlanFromStops(seeds map[int32]time.Time, tLatF, tLonF float64, when time.Time, num int) ([]Itinerary, error) {
	if !e.Ready() {
		return nil, fmt.Errorf("engine not ready")
	}
	gb, tb := e.GraphBundle(), e.TTBundle()
	tt := tb.TT
	tLat, tLon := int32(tLatF*1e7), int32(tLonF*1e7)

	local := when.In(tt.TZ)
	base := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, tt.TZ)

	acc := accessSet{sec: map[int32]uint32{}, anchor: map[int32]int32{}, mode: "none"}
	for s, at := range seeds {
		rel := at.Sub(base)
		if rel < 0 {
			continue
		}
		acc.sec[s] = uint32(rel.Seconds())
	}
	if len(acc.sec) == 0 {
		return nil, fmt.Errorf("no usable seeds")
	}
	egr := e.reachStops(gb, tb, tLat, tLon, graph.ModeFoot,
		graph.SpeedFactor(e.Cfg.Routing.WalkSpeedKmh),
		uint32(e.Cfg.Routing.MaxWalkAccess.Seconds()*10), "walk")
	if len(egr.sec) == 0 {
		return nil, fmt.Errorf("no stops reachable around the destination")
	}
	req := Request{ToLat: tLatF, ToLon: tLonF, Mode: "transit", When: when, Num: num}
	its := e.runRaptor(gb, tb, req, when, 0, 0, tLat, tLon, &acc, &egr)
	its = dedupeRank(its, num)
	e.annotateLive(its, when)
	e.remember(its, req)
	return its, nil
}
