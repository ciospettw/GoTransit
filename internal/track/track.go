// Package track is the "engine that never leaves you alone": one session per
// tracked journey, driven purely by the wall clock and GTFS-RT (no GPS).
// If the feed says the user's vehicle passed their stop, we trust it and move
// the virtual user forward; every RT change re-evaluates the rest of the
// journey and pushes deltas, warnings and reroutes over the WebSocket.
package track

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"gotransit/internal/config"
	"gotransit/internal/engine"
	"gotransit/internal/rt"
	"gotransit/internal/transit"
)

// Sink delivers events to the client (the API adapts a WebSocket).
type Sink interface {
	Send(event any) error
}

// Tracker spawns sessions.
type Tracker struct {
	E    *engine.Engine
	Mgr  *rt.Manager // nil → monitor-only (no RT feeds configured)
	Cfg  *config.Config
	Log  *slog.Logger
	Tick time.Duration // clock re-evaluation cadence (default 5s)
}

// Events (type field first so clients can switch on it).
type evHello struct {
	Type      string           `json:"type"` // "hello"
	Mode      string           `json:"mode"` // "live" | "monitor"
	Itinerary engine.Itinerary `json:"itinerary"`
}
type evDelay struct {
	Type   string    `json:"type"` // "delay"
	Arrive time.Time `json:"arrive"`
	DeltaS int       `json:"arrive_delta_s"` // vs the original promise
	Legs   []legTime `json:"legs"`
}
type legTime struct {
	Index    int       `json:"index"`
	Depart   time.Time `json:"depart"`
	Arrive   time.Time `json:"arrive"`
	DelayS   int       `json:"delay_s"`
	Realtime bool      `json:"realtime"`
}
type evProgress struct {
	Type     string `json:"type"`   // "progress"
	Status   string `json:"status"` // walking | waiting | riding | done
	LegIndex int    `json:"leg_index"`
	Boarded  bool   `json:"boarded,omitempty"`
	Passed   *place `json:"passed_stop,omitempty"`
}
type place struct {
	StopID string `json:"stop_id"`
	Name   string `json:"name"`
}
type evReroute struct {
	Type    string `json:"type"`   // "reroute"
	Reason  string `json:"reason"` // cancelled | missed_connection | better_arrival | unconfirmed_trip | stop_skipped
	SavingS int    `json:"saving_s,omitempty"`
	Message string `json:"message"`
	// indices into Itinerary.Legs that are new or different vs the plan they
	// replace — the client knows exactly what changed
	ChangedLegs []int            `json:"changed_legs"`
	Itinerary   engine.Itinerary `json:"itinerary"`
}
type evWarning struct {
	Type     string `json:"type"` // "warning"
	Code     string `json:"code"` // no_rt_signal | possibly_cancelled
	LegIndex int    `json:"leg_index"`
	Message  string `json:"message"`
}
type evVehicle struct {
	Type      string  `json:"type"` // "vehicle"
	LegIndex  int     `json:"leg_index"`
	TripID    string  `json:"trip_id"`
	Route     string  `json:"route,omitempty"`
	Lat       float64 `json:"lat,omitempty"`
	Lon       float64 `json:"lon,omitempty"`
	Status    string  `json:"status"`         // stopped_at | incoming_at | in_transit_to | unknown
	Stop      *place  `json:"stop,omitempty"` // the stop it is at / heading to
	StopsAway int     `json:"stops_away"`     // to your boarding stop (or alighting, once on board)
	DelayS    int     `json:"delay_s"`
	Boarded   bool    `json:"boarded"`
}
type evArrived struct {
	Type   string    `json:"type"` // "arrived"
	Arrive time.Time `json:"arrive"`
}
type evError struct {
	Type    string `json:"type"` // "error"
	Message string `json:"message"`
}

// session state
type session struct {
	t    *Tracker
	sink Sink
	req  engine.Request

	it        engine.Itinerary
	origArr   time.Time // first promise, for arrive_delta_s
	liveMode  bool
	legIdx    int  // first uncompleted leg
	boarded   bool // for the current transit leg
	lastEmit  map[int]legTime
	lastVeh   evVehicle // dedupe for vehicle events
	lastRR    time.Time // better-arrival reroute cooldown
	lastTry   time.Time // infeasibility replan attempt throttle
	warned    map[string]bool
	arrivedAt time.Time
}

// Run drives one tracking session until arrival, error or ctx cancellation.
func (t *Tracker) Run(ctx context.Context, itID string, sink Sink) error {
	cached, ok := t.E.LookupItinerary(itID)
	if !ok {
		sink.Send(evError{"error", "unknown or expired itinerary id; re-plan and reconnect"})
		return fmt.Errorf("unknown itinerary %s", itID)
	}
	s := &session{
		t: t, sink: sink, req: cached.Req,
		it: cached.It, origArr: cached.It.Arrive,
		liveMode: cached.It.Live,
		lastEmit: map[int]legTime{}, warned: map[string]bool{},
	}
	mode := "monitor"
	if s.liveMode {
		mode = "live"
	}
	sink.Send(evHello{"hello", mode, s.it})
	if done, err := s.evaluate(time.Now()); err == nil && done {
		sink.Send(evArrived{"arrived", s.arrivedAt})
		return nil
	}

	cadence := t.Tick
	if cadence <= 0 {
		cadence = 5 * time.Second
	}
	tick := time.NewTicker(cadence)
	defer tick.Stop()
	for {
		var changed <-chan struct{}
		if t.Mgr != nil {
			changed = t.Mgr.Changed()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		case <-tick.C:
		}
		done, err := s.evaluate(time.Now())
		if err != nil {
			sink.Send(evError{"error", err.Error()})
			return err
		}
		if done {
			sink.Send(evArrived{"arrived", s.arrivedAt})
			return nil
		}
	}
}

// ---- per-cycle evaluation -----------------------------------------------------

func (s *session) evaluate(now time.Time) (bool, error) {
	tb := s.t.E.TTBundle()
	if tb == nil {
		return false, fmt.Errorf("engine restarting")
	}
	tt := tb.TT
	o := tt.RT()

	s.advance(tt, o, now)
	if s.legIdx >= len(s.it.Legs) {
		if s.arrivedAt.IsZero() {
			s.arrivedAt = now
		}
		return true, nil
	}

	// refresh RT-adjusted times of the remaining plan and check feasibility
	times, feas := s.refreshTimes(tt, o, now)
	s.emitDelays(times, now)
	s.emitVehicle(tt, o)

	if !feas.ok {
		s.t.Log.Debug("infeasible", "reason", feas.reason, "legIdx", s.legIdx, "boarded", s.boarded)
		return false, s.reroute(tt, o, now, feas.reason, feas.message, 0)
	}

	// schedule-only guard + cancellation blindness on the next boarding
	if warn := s.scheduleGuard(tt, o, now); warn != nil {
		if !s.warned[warn.Code+fmt.Sprint(warn.LegIndex)] {
			s.warned[warn.Code+fmt.Sprint(warn.LegIndex)] = true
			s.sink.Send(*warn)
		}
		if s.liveMode {
			// try to move the user onto RT-confirmed legs
			if err := s.reroute(tt, o, now, "unconfirmed_trip",
				"next trip has no realtime signal; rerouting onto confirmed service", 10*time.Minute); err == nil {
				return false, nil
			}
		}
	}

	// opportunistic improvement: only when it truly pays (≥ min saving) and
	// not more often than once a minute — povero utente.
	if now.Sub(s.lastRR) >= time.Minute && s.rerouteAllowed(tt) {
		s.tryBetterArrival(tt, o, now)
	}
	return false, nil
}

// advance moves the virtual user along the plan using clock + RT confirmations.
func (s *session) advance(tt *transit.Timetable, o *transit.RTOverlay, now time.Time) {
	for s.legIdx < len(s.it.Legs) {
		leg := &s.it.Legs[s.legIdx]
		if leg.Mode != "transit" {
			if now.After(leg.Arrive) {
				s.legIdx++
				s.boarded = false
				continue
			}
			return
		}
		r, ok := resolveRide(tt, leg)
		if !ok { // timetable swapped and trip vanished: treat as done by clock
			if now.After(leg.Arrive.Add(90 * time.Second)) {
				s.legIdx++
				continue
			}
			return
		}
		base := rideBase(tt, r, leg.Depart)
		depRT := base.Add(time.Duration(tt.TripDep(r.trip, r.board)) * time.Second)
		arrRT := base.Add(time.Duration(tt.TripArr(r.trip, r.alight)) * time.Second)
		passed := o.TripPassed(r.trip)

		if !s.boarded {
			if passed >= int16(r.board) || (passed < 0 && now.After(depRT.Add(90*time.Second))) {
				s.boarded = true
				s.sink.Send(evProgress{Type: "progress", Status: "riding", LegIndex: s.legIdx, Boarded: true})
			} else {
				return // still waiting at the stop
			}
		}
		if passed >= int16(r.alight) || now.After(arrRT.Add(90*time.Second)) {
			s.legIdx++
			s.boarded = false
			s.sink.Send(evProgress{Type: "progress", Status: "alighted", LegIndex: s.legIdx - 1})
			// the vehicle may have run early/late: re-anchor the following
			// street leg to the actual alighting moment
			if s.legIdx < len(s.it.Legs) && s.it.Legs[s.legIdx].Mode != "transit" {
				nl := &s.it.Legs[s.legIdx]
				at := arrRT
				if now.After(at) {
					at = now
				}
				dur := time.Duration(nl.DurationS) * time.Second
				nl.Depart, nl.Arrive = at, at.Add(dur)
			}
			continue
		}
		return // riding
	}
}

// feasibility of the remaining plan
type feasibility struct {
	ok      bool
	reason  string
	message string
}

// refreshTimes recomputes RT-adjusted leg times and checks the chain.
func (s *session) refreshTimes(tt *transit.Timetable, o *transit.RTOverlay, now time.Time) ([]legTime, feasibility) {
	var out []legTime
	feas := feasibility{ok: true}
	cursor := now
	for i := s.legIdx; i < len(s.it.Legs); i++ {
		leg := &s.it.Legs[i]
		if leg.Mode != "transit" {
			dur := time.Duration(leg.DurationS) * time.Second
			dep := cursor
			if i == s.legIdx && now.Before(leg.Depart) {
				dep = leg.Depart // not started yet per plan
			}
			cursor = dep.Add(dur)
			out = append(out, legTime{Index: i, Depart: dep, Arrive: cursor})
			continue
		}
		r, ok := resolveRide(tt, leg)
		if !ok {
			out = append(out, legTime{Index: i, Depart: leg.Depart, Arrive: leg.Arrive})
			cursor = leg.Arrive
			continue
		}
		if tt.TripSkipped(r.trip) {
			feas = feasibility{false, "cancelled", "a trip on your route was cancelled"}
		}
		if tt.StopSkipped(r.trip, r.board) || tt.StopSkipped(r.trip, r.alight) {
			feas = feasibility{false, "stop_skipped", "the vehicle will skip one of your stops"}
		}
		base := rideBase(tt, r, leg.Depart)
		depRT := base.Add(time.Duration(tt.TripDep(r.trip, r.board)) * time.Second)
		arrRT := base.Add(time.Duration(tt.TripArr(r.trip, r.alight)) * time.Second)
		delay := int(tt.TripDep(r.trip, r.board)) - int(tt.ScheduledDep(r.trip, r.board))

		if i == s.legIdx && s.boarded {
			// already on it: only the arrival matters
		} else {
			// A connection counts as missed ONLY when GTFS-RT confirms the
			// vehicle already cleared the boarding stop before the rider
			// could be there. Predicted departures are not enough: near the
			// start of a run (vehicle held at its terminus, delay not yet
			// propagated) the clock slides past the scheduled time and a
			// prediction-based check produces false "missed_connection"
			// reroutes for a bus that has not even left.
			if feas.ok && cursor.After(depRT) && o.TripPassed(r.trip) >= int16(r.board) {
				feas = feasibility{false, "missed_connection", "your connection already left its stop"}
			}
			if depRT.After(cursor) {
				cursor = depRT
			}
		}
		cursor = arrRT
		out = append(out, legTime{Index: i, Depart: depRT, Arrive: arrRT,
			DelayS: delay, Realtime: o.TripHasRT(r.trip)})
	}
	return out, feas
}

// emitVehicle streams the live position of the bus/train the user rides or
// is about to board: where it is, which stop it is at/approaching, how many
// stops from the user, and its delay — even before the user is on it.
func (s *session) emitVehicle(tt *transit.Timetable, o *transit.RTOverlay) {
	for i := s.legIdx; i < len(s.it.Legs); i++ {
		leg := &s.it.Legs[i]
		if leg.Mode != "transit" {
			continue
		}
		r, ok := resolveRide(tt, leg)
		if !ok {
			return
		}
		lat, lon, pos, status, ok := o.Vehicle(r.trip)
		if !ok {
			return // no vehicle entity for this trip (metro etc.)
		}
		stops := tt.PatternStops(tt.PatternOfTrip(r.trip))
		ev := evVehicle{
			Type: "vehicle", LegIndex: i, TripID: leg.TripID, Boarded: i == s.legIdx && s.boarded,
			Status: map[int8]string{0: "incoming_at", 1: "stopped_at", 2: "in_transit_to"}[status],
		}
		if ev.Status == "" {
			ev.Status = "unknown"
		}
		if leg.Route != nil {
			ev.Route = leg.Route.ShortName
		}
		if pos >= 0 && int(pos) < len(stops) {
			st := stops[pos]
			ev.Stop = &place{StopID: tt.StopID[st], Name: tt.StopName[st]}
			if lat == 0 && lon == 0 { // no GPS: pin the vehicle at its stop
				lat, lon = tt.StopLat[st], tt.StopLon[st]
			}
			if ev.Boarded {
				ev.StopsAway = int(r.alight) - int(pos)
			} else {
				ev.StopsAway = int(r.board) - int(pos)
			}
			if ev.StopsAway < 0 {
				ev.StopsAway = 0
			}
		}
		ev.Lat = float64(lat) / 1e7
		ev.Lon = float64(lon) / 1e7
		if ev.Boarded {
			ev.DelayS = int(tt.TripArr(r.trip, r.alight)) - int(tt.ScheduledArr(r.trip, r.alight))
		} else {
			ev.DelayS = int(tt.TripDep(r.trip, r.board)) - int(tt.ScheduledDep(r.trip, r.board))
		}
		if ev.Lat == s.lastVeh.Lat && ev.Lon == s.lastVeh.Lon &&
			ev.StopsAway == s.lastVeh.StopsAway && ev.DelayS == s.lastVeh.DelayS &&
			ev.Status == s.lastVeh.Status && ev.Boarded == s.lastVeh.Boarded {
			return // unchanged
		}
		s.lastVeh = ev
		s.sink.Send(ev)
		return // only the nearest relevant vehicle
	}
}

// emitDelays pushes a delay event when any leg time moved ≥30s.
func (s *session) emitDelays(times []legTime, now time.Time) {
	if len(times) == 0 {
		return
	}
	changed := false
	for _, lt := range times {
		prev, seen := s.lastEmit[lt.Index]
		if !seen || absDur(lt.Depart.Sub(prev.Depart)) >= 30*time.Second ||
			absDur(lt.Arrive.Sub(prev.Arrive)) >= 30*time.Second {
			changed = true
		}
		s.lastEmit[lt.Index] = lt
	}
	if !changed {
		return
	}
	arr := times[len(times)-1].Arrive
	s.sink.Send(evDelay{
		Type: "delay", Arrive: arr,
		DeltaS: int(arr.Sub(s.origArr).Seconds()), Legs: times,
	})
}

// scheduleGuard flags upcoming schedule-only boardings (and likely-cancelled
// trips that should already be under way but left no RT trace).
func (s *session) scheduleGuard(tt *transit.Timetable, o *transit.RTOverlay, now time.Time) *evWarning {
	for i := s.legIdx; i < len(s.it.Legs); i++ {
		leg := &s.it.Legs[i]
		if leg.Mode != "transit" || (i == s.legIdx && s.boarded) {
			continue
		}
		if leg.Route != nil && engine.IsMetroType(leg.Route.Type) {
			continue // metro: always considered live, never emits RT entities
		}
		r, ok := resolveRide(tt, leg)
		if !ok || o.TripHasRT(r.trip) {
			continue
		}
		if s.t.Mgr == nil || !s.t.Mgr.FeedFresh(int(tt.TripFeed[r.trip]), 5*time.Minute) {
			continue // this operator has no live coverage at all: nothing to infer
		}
		base := rideBase(tt, r, leg.Depart)
		// CANCELED often only appears ~2 min after terminus departure: a trip
		// that should already be rolling with zero RT trace is suspicious
		terminusDep := base.Add(time.Duration(tt.ScheduledDep(r.trip, 0)) * time.Second)
		if now.After(terminusDep.Add(s.t.Cfg.Realtime.CancelBlind)) {
			return &evWarning{"warning", "possibly_cancelled", i,
				"this trip should already be under way but has no realtime trace; it may have been cancelled"}
		}
		if time.Until(leg.Depart) <= s.t.Cfg.Realtime.ConfirmLead {
			return &evWarning{"warning", "no_rt_signal", i,
				"no realtime signal for this trip yet; it cannot be confirmed"}
		}
		return nil // only guard the nearest unconfirmed boarding
	}
	return nil
}

// rerouteAllowed: reroutes are only legal on realtime ground — never displace
// a user standing on schedule-only legs (monitor mode rule).
func (s *session) rerouteAllowed(tt *transit.Timetable) bool {
	if s.liveMode {
		return true
	}
	for i := s.legIdx; i < len(s.it.Legs); i++ {
		leg := &s.it.Legs[i]
		if leg.Mode == "transit" {
			if leg.Route != nil && engine.IsMetroType(leg.Route.Type) {
				return true // metro counts as live ground
			}
			r, ok := resolveRide(tt, leg)
			return ok && tt.RT().TripHasRT(r.trip)
		}
	}
	return false
}

// tryBetterArrival replans from the virtual position and reroutes when the
// gain clears realtime.reroute_min_saving.
func (s *session) tryBetterArrival(tt *transit.Timetable, o *transit.RTOverlay, now time.Time) {
	curArr, ok := s.currentArrival()
	if !ok {
		return
	}
	best, ok := s.replan(tt, o, now)
	if !ok {
		return
	}
	saving := curArr.Sub(best.Arrive)
	if saving < s.t.Cfg.Realtime.RerouteMinSaving {
		return
	}
	s.lastRR = now
	s.switchTo(best, "better_arrival",
		fmt.Sprintf("a faster option appeared: arrive %s earlier", saving.Round(time.Minute)), int(saving.Seconds()))
}

// reroute handles infeasibility. A broken plan is replaced no matter what —
// live or monitor mode, the engine never leaves the user without a way
// forward. Replans keep retrying (throttled) until an alternative exists.
func (s *session) reroute(tt *transit.Timetable, o *transit.RTOverlay, now time.Time, reason, msg string, tolerance time.Duration) error {
	// tell the client immediately WHY the plan broke, before the fix arrives
	if !s.warned["why_"+reason+fmt.Sprint(s.legIdx)] {
		s.warned["why_"+reason+fmt.Sprint(s.legIdx)] = true
		s.sink.Send(evWarning{"warning", reason, s.legIdx, msg})
	}
	if now.Sub(s.lastTry) < 10*time.Second {
		return nil // gentle retry pacing; the next cycle tries again
	}
	s.lastTry = now
	best, ok := s.replan(tt, o, now)
	if !ok {
		if !s.warned["noalt_"+reason] {
			s.warned["noalt_"+reason] = true
			s.sink.Send(evWarning{"warning", reason, s.legIdx, msg + "; still searching for an alternative"})
		}
		return nil // keep retrying on subsequent cycles
	}
	if tolerance > 0 {
		if cur, ok := s.currentArrival(); ok && best.Arrive.After(cur.Add(tolerance)) {
			return nil // alternative too costly for a soft reroute
		}
	}
	saving := 0
	if cur, ok := s.currentArrival(); ok {
		saving = int(cur.Sub(best.Arrive).Seconds())
	}
	s.lastRR = now
	s.switchTo(best, reason, msg, saving)
	return nil
}

func (s *session) switchTo(it engine.Itinerary, reason, msg string, saving int) {
	s.t.Log.Debug("switchTo", "reason", reason, "legs", len(it.Legs), "arrive", it.Arrive, "id", it.ID)
	changed := changedLegs(s.it.Legs[min(s.legIdx, len(s.it.Legs)):], it.Legs)
	s.it = it
	s.legIdx = 0
	s.boarded = false
	s.lastEmit = map[int]legTime{}
	s.lastVeh = evVehicle{}
	s.warned = map[string]bool{} // fresh plan, fresh guard state
	s.liveMode = it.Live || s.liveMode
	s.sink.Send(evReroute{Type: "reroute", Reason: reason, SavingS: saving, Message: msg,
		ChangedLegs: changed, Itinerary: it})
}

// legSignature identifies a leg across plans: same ride (trip + board/alight)
// or same walk (endpoints, rounded).
func legSignature(l *engine.Leg) string {
	if l.Mode == "transit" {
		return "t|" + l.TripID + "|" + l.From.StopID + "|" + l.To.StopID
	}
	return fmt.Sprintf("%s|%.4f,%.4f|%.4f,%.4f", l.Mode, l.From.Lat, l.From.Lon, l.To.Lat, l.To.Lon)
}

// changedLegs lists indices of new-plan legs absent from the old remainder.
func changedLegs(old, new_ []engine.Leg) []int {
	seen := map[string]bool{}
	for i := range old {
		seen[legSignature(&old[i])] = true
	}
	changed := []int{}
	for i := range new_ {
		if !seen[legSignature(&new_[i])] {
			changed = append(changed, i)
		}
	}
	return changed
}

// currentArrival is the RT-adjusted arrival of the current plan.
func (s *session) currentArrival() (time.Time, bool) {
	last := s.lastEmit[len(s.it.Legs)-1]
	if !last.Arrive.IsZero() {
		return last.Arrive, true
	}
	if len(s.it.Legs) > 0 {
		return s.it.Legs[len(s.it.Legs)-1].Arrive, true
	}
	return time.Time{}, false
}

// replan computes the best alternative from the user's virtual position.
func (s *session) replan(tt *transit.Timetable, o *transit.RTOverlay, now time.Time) (engine.Itinerary, bool) {
	leg := &s.it.Legs[s.legIdx]
	dLat, dLon := s.req.ToLat, s.req.ToLon
	if dLat == 0 && dLon == 0 { // onboard-replan cached reqs keep the dest too
		last := s.it.Legs[len(s.it.Legs)-1]
		dLat, dLon = last.To.Lat, last.To.Lon
	}

	if leg.Mode == "transit" && s.boarded {
		// on the vehicle: seed every downstream stop with its RT arrival —
		// staying on longer, hopping off early, switching lines: all in play
		r, ok := resolveRide(tt, leg)
		if !ok {
			return engine.Itinerary{}, false
		}
		base := rideBase(tt, r, leg.Depart)
		seeds := map[int32]time.Time{}
		stops := tt.PatternStops(tt.PatternOfTrip(r.trip))
		from := int(r.board) + 1
		if p := o.TripPassed(r.trip); int(p)+1 > from {
			from = int(p) + 1
		}
		for pos := from; pos < int(tt.TripLen(r.trip)); pos++ {
			if tt.StopSkipped(r.trip, uint16(pos)) {
				continue
			}
			at := base.Add(time.Duration(tt.TripArr(r.trip, uint16(pos)))*time.Second + 15*time.Second)
			if at.Before(now) {
				continue
			}
			seeds[stops[pos]] = at
		}
		its, err := s.t.E.PlanFromStops(seeds, dLat, dLon, now, 3)
		if err != nil || len(its) == 0 {
			return engine.Itinerary{}, false
		}
		return pickLive(its, s.liveMode), true
	}

	// walking or waiting: replan from the virtual point along the plan
	lat, lon := s.virtualPoint(now)
	req := s.req
	req.FromLat, req.FromLon = lat, lon
	req.ToLat, req.ToLon = dLat, dLon
	req.Mode = "transit"
	req.When = now
	req.ArriveBy = false
	req.Num = 3
	resp, err := s.t.E.Plan(req)
	if err != nil || len(resp.Itineraries) == 0 {
		return engine.Itinerary{}, false
	}
	return pickLive(resp.Itineraries, s.liveMode), true
}

// pickLive prefers RT-confirmed alternatives when tracking live.
func pickLive(its []engine.Itinerary, wantLive bool) engine.Itinerary {
	if wantLive {
		for _, it := range its {
			if it.Live {
				return it
			}
		}
	}
	return its[0]
}

// virtualPoint estimates where the user is along the current street leg
// (or pins them at the stop while waiting).
func (s *session) virtualPoint(now time.Time) (float64, float64) {
	leg := &s.it.Legs[s.legIdx]
	if leg.Mode == "transit" {
		return leg.From.Lat, leg.From.Lon // waiting at the stop
	}
	frac := 0.0
	if d := leg.Arrive.Sub(leg.Depart); d > 0 {
		frac = float64(now.Sub(leg.Depart)) / float64(d)
	}
	if frac <= 0 {
		return leg.From.Lat, leg.From.Lon
	}
	if frac >= 1 {
		return leg.To.Lat, leg.To.Lon
	}
	// interpolate linearly between endpoints (plenty for a replan snap)
	return leg.From.Lat + (leg.To.Lat-leg.From.Lat)*frac,
		leg.From.Lon + (leg.To.Lon-leg.From.Lon)*frac
}

// ---- ride resolution ------------------------------------------------------------

type ride struct {
	trip          uint32
	board, alight uint16
}

// resolveRide maps a leg (string ids) onto the current timetable snapshot.
func resolveRide(tt *transit.Timetable, leg *engine.Leg) (ride, bool) {
	trip, ok := tt.TripIdx[leg.TripID]
	if !ok {
		return ride{}, false
	}
	stops := tt.PatternStops(tt.PatternOfTrip(trip))
	board, alight := -1, -1
	for pos, st := range stops {
		if board < 0 && tt.StopID[st] == leg.From.StopID {
			board = pos
			continue
		}
		if board >= 0 && tt.StopID[st] == leg.To.StopID {
			alight = pos
			break
		}
	}
	if board < 0 || alight < 0 {
		return ride{}, false
	}
	return ride{trip: trip, board: uint16(board), alight: uint16(alight)}, true
}

// rideBase recovers the ride's service-day midnight: the base whose scheduled
// departure lands closest to the leg's assembled departure.
func rideBase(tt *transit.Timetable, r ride, legDepart time.Time) time.Time {
	local := legDepart.In(tt.TZ)
	mid := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, tt.TZ)
	sched := time.Duration(tt.ScheduledDep(r.trip, r.board)) * time.Second
	best, bestDiff := mid, absDur(mid.Add(sched).Sub(legDepart))
	for _, cand := range []time.Time{mid.AddDate(0, 0, -1), mid.AddDate(0, 0, 1)} {
		if d := absDur(cand.Add(sched).Sub(legDepart)); d < bestDiff {
			best, bestDiff = cand, d
		}
	}
	return best
}

func absDur(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}
