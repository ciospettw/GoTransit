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
	Type string `json:"type"` // "hello"
	Mode string `json:"mode"` // "live" | "monitor"
	// Protocol 3: dual GPS/virtual tracking plus schedule-assumed rail
	// segments, signalled additively on progress events. The server derives ALL
	// stop-related states from streamed positions (or runs pure virtual
	// when none arrive) and may emit risk / connection_risk / too_slow /
	// left_stop / stayed_on_vehicle events.
	Protocol  int              `json:"protocol"`
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
	// with client GPS: live meters left to the target of the current phase
	// (leg endpoint while walking, the boarding stop while waiting).
	// Pointer: 0 m is meaningful (you are there), absent = no GPS.
	DistM          *int   `json:"distance_to_stop_m,omitempty"`
	TrackingSource string `json:"tracking_source,omitempty"` // schedule_assumed when position is unavailable
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
	Code     string `json:"code"` // no_rt_signal | possibly_cancelled | position_unavailable
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

// evDeviation reports a GPS-confirmed departure from the plan; it is always
// followed by a reroute (same kind as reason) carrying the recovery plan.
type evDeviation struct {
	Type         string `json:"type"` // "deviation"
	Kind         string `json:"kind"` // missed_alight | left_vehicle_early | off_route
	LegIndex     int    `json:"leg_index"`
	ExpectedStop *place `json:"expected_stop,omitempty"` // where the plan said to alight
	Message      string `json:"message"`
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

	it       engine.Itinerary
	origArr  time.Time // first promise, for arrive_delta_s
	liveMode bool
	legIdx   int           // first uncompleted leg
	boarded  bool          // for the current transit leg
	atStop   *engine.Place // GPS-confirmed stop; unaffected by subsequent jitter
	// boardedByShape keeps stale vehicle data from triggering opportunistic
	// replans until the feed catches up with the inferred boarding.
	boardedByShape bool
	lastEmit       map[int]legTime
	lastVeh        evVehicle // dedupe for vehicle events
	lastRR         time.Time // better-arrival reroute cooldown
	lastTry        time.Time // infeasibility replan attempt throttle
	warned         map[string]bool
	// positionUnavailableWarned survives reroutes: the rider only needs the
	// rail-position caveat once per tracking session.
	positionUnavailableWarned bool
	arrivedAt                 time.Time
	opaque                    bool               // ignore incoming fixes inside schedule-assumed rail blocks
	railReadyAt               map[int]time.Time  // fixed station-arrival/change anchors, by outgoing rail leg
	gps                       gpsState           // client position evidence (optional)
	risk                      map[int]*riskState // live connection-risk hysteresis, per leg
}

// Run drives one tracking session until arrival, error or ctx cancellation.
// fixes MAY be nil: without client positions the virtual rider governs alone.
func (t *Tracker) Run(ctx context.Context, itID string, sink Sink, fixes <-chan Fix) error {
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
		gps: newGPSState(),
	}
	s.resetRailReady()
	if len(s.it.Legs) > 0 {
		s.boarded = s.it.Legs[0].Boarded
	}
	mode := "monitor"
	if s.liveMode {
		mode = "live"
	}
	sink.Send(evHello{"hello", mode, 3, s.it})
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
		case f, ok := <-fixes:
			if !ok {
				fixes = nil // client stopped sending: back to virtual-only
				continue
			}
			if !s.opaque {
				s.gps.update(f, time.Now())
			}
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

	s.arriveAtBoardingStop(tt, o, now)
	s.ensureCurrentRailReady(tt, now)
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
	s.emitVehicle(tt, o, now)
	opaque := s.opaqueAt(tt, o, now, s.legIdx)
	s.setOpaque(opaque)
	if !opaque {
		s.emitWalkProgress(tt, now)
	}

	// GPS silence after fixes were flowing: tell the client once and keep
	// governing with the virtual rider (protocol falls back to virtual).
	if !opaque && s.gps.has && now.Sub(s.gps.cur.At) > s.t.Cfg.Track.GPSStaleAfter {
		if !s.warned["gps_lost"] {
			s.warned["gps_lost"] = true
			s.sink.Send(evWarning{"warning", "gps_lost", s.legIdx,
				"no recent position fixes; tracking continues on schedule and realtime data only"})
		}
	} else if !opaque && s.gps.fresh(now) {
		s.warned["gps_lost"] = false
	}

	// GPS-confirmed deviations outrank plan feasibility: the rider already
	// IS somewhere else, the plan must follow them
	if !opaque {
		if dev := s.gpsDeviation(tt, o, now); dev != nil {
			s.sink.Send(dev.ev)
			return false, s.reroute(tt, o, now, dev.ev.Kind, dev.ev.Message, 0)
		}

		// proactive stop guard: too slow to reach the boarding stop / walked
		// away beyond recovery → reroute BEFORE the bus is actually missed
		if act := s.gpsStopGuard(tt, now); act != nil {
			return false, s.reroute(tt, o, now, act.reason, act.message, 0)
		}
	}

	if !feas.ok {
		if s.shapeBoardingInProgress(tt, now) && feas.reason == "missed_connection" {
			return false, nil
		}
		s.t.Log.Debug("infeasible", "reason", feas.reason, "legIdx", s.legIdx, "boarded", s.boarded)
		return false, s.reroute(tt, o, now, feas.reason, feas.message, 0)
	}

	// probabilistic look-ahead: score every remaining connection with the
	// freshly RT-adjusted times and act BEFORE a tight one is actually lost
	if s.evaluateRisk(tt, o, now, times) {
		return false, nil
	}

	// schedule-only guard + cancellation blindness on the next boarding
	if warn := s.scheduleGuard(tt, o, now); warn != nil {
		if !s.warned[warn.Code+fmt.Sprint(warn.LegIndex)] {
			s.warned[warn.Code+fmt.Sprint(warn.LegIndex)] = true
			s.sink.Send(*warn)
		}
		if s.liveMode && now.Sub(s.lastRR) >= betterArrivalCooldown && !s.shapeBoardingInProgress(tt, now) {
			// try to move the user onto RT-confirmed legs
			if err := s.reroute(tt, o, now, "unconfirmed_trip",
				"next trip has no realtime signal; rerouting onto confirmed service", 10*time.Minute); err == nil {
				return false, nil
			}
		}
	}

	// opportunistic improvement: only when it truly pays (≥ min saving), not
	// more often than once every 3 minutes after ANY reroute, and never
	// moments before a boarding the rider is already committed to — povero
	// utente, non si fanno 300 reroute né switch all'ultimo secondo.
	if now.Sub(s.lastRR) >= betterArrivalCooldown && !s.boardedByShape && s.rerouteAllowed(tt) &&
		!s.boardingImminent(now) && !s.shapeBoardingInProgress(tt, now) {
		s.tryBetterArrival(tt, o, now)
	}
	return false, nil
}

// shapeBoardingInProgress protects the short evidence window between leaving
// a reached stop along the planned shape and being promoted to riding. During
// that window neither left_stop nor an opportunistic "better arrival" may
// replace the ride the user is demonstrably boarding.
func (s *session) shapeBoardingInProgress(tt *transit.Timetable, now time.Time) bool {
	if !s.gps.fresh(now) {
		return false
	}
	for i := s.legIdx; i < len(s.it.Legs); i++ {
		leg := &s.it.Legs[i]
		if leg.Mode != "transit" {
			continue
		}
		if i == s.legIdx && s.boarded {
			return false
		}
		r, ok := resolveRide(tt, leg)
		if !ok {
			return false
		}
		following, _ := s.gpsShapeBoarding(tt, r, i, now)
		return following
	}
	return false
}

// betterArrivalCooldown paces opportunistic reroutes; infeasibility reroutes
// (cancelled, missed, deviations) are never delayed by it.
const betterArrivalCooldown = 3 * time.Minute

// boardingImminent reports whether the next boarding is ≤3 minutes away:
// proposing a different bus while the rider watches theirs pull in is churn,
// not help.
func (s *session) boardingImminent(now time.Time) bool {
	for i := s.legIdx; i < len(s.it.Legs); i++ {
		leg := &s.it.Legs[i]
		if leg.Mode != "transit" {
			continue
		}
		if i == s.legIdx && s.boarded {
			return false // already riding: switches only via downstream stops
		}
		dep := leg.Depart
		if lt, ok := s.lastEmit[i]; ok && !lt.Depart.IsZero() {
			dep = lt.Depart // RT-adjusted
		}
		return dep.Sub(now) <= 3*time.Minute && dep.After(now.Add(-time.Minute))
	}
	return false
}

// advance moves the virtual user along the plan using clock + RT
// confirmations, refined (never contradicted) by client GPS evidence.
func (s *session) advance(tt *transit.Timetable, o *transit.RTOverlay, now time.Time) {
	for s.legIdx < len(s.it.Legs) {
		leg := &s.it.Legs[s.legIdx]
		if leg.Mode != "transit" {
			// Fast pickup: the rider may reach the boarding stop and leave on
			// the vehicle before atStopHold has completed the access walk. The
			// reached-stop latch belongs to the upcoming ride, so shape evidence
			// can atomically skip the synthetic waiting phase and promote that
			// ride without letting feasibility report a missed departure first.
			if s.boardNextRideFromAccessShape(tt, o, now) {
				return
			}
			// GPS: reaching the leg's endpoint early beats the planned clock
			// (arriving early at a stop only ever helps)
			atEnd := false
			if !s.opaqueAt(tt, o, now, s.legIdx) && s.gps.fresh(now) {
				atEnd = s.gps.distTo(leg.To.Lat, leg.To.Lon) <= nearStopBase
			}
			if now.After(leg.Arrive) || atEnd {
				completedAt := leg.Arrive
				if atEnd {
					// GPS-confirmed arrival is the real station-entry anchor.
					// A clock completion keeps the itinerary's absolute arrival so
					// evaluator cadence never steals buffer time from the rider.
					completedAt = now
				}
				s.anchorRailAfterStreet(tt, s.legIdx, completedAt)
				s.legIdx++
				s.boarded = false
				s.boardedByShape = false
				s.gps.resetLegAnchors()
				s.emitPhase(tt, o, now) // the client must always know the current leg
				continue
			}
			return
		}
		r, ok := resolveRide(tt, leg)
		if !ok && railLikeLeg(tt, leg) {
			// A timetable swap may remove the trip while the cached rail leg is
			// still being tracked. Its itinerary clock remains the safest
			// schedule assumption; importantly, it has no legacy +90s lag.
			s.setOpaque(true)
			s.warnPositionUnavailable()
			if s.railDepartureTooEarly(s.legIdx, leg.Depart) {
				return
			}
			if !s.boarded {
				if now.Before(leg.Depart) {
					return
				}
				s.boarded = true
				s.boardedByShape = false
				s.sendProgress("riding", s.legIdx, true, "schedule_assumed")
			}
			if now.Before(leg.Arrive) {
				return
			}
			s.completeAlight(tt, o, now, leg.Arrive, "schedule_assumed")
			continue
		}
		if !ok { // timetable swapped and road trip vanished: clock fallback
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
		assumed := s.scheduleAssumedLeg(tt, o, now, leg)
		if tt.TripSkipped(r.trip) || tt.StopSkipped(r.trip, r.board) || tt.StopSkipped(r.trip, r.alight) {
			return // refreshTimes reports and reroutes before virtual progress
		}
		if railLikeLeg(tt, leg) && s.railDepartureTooEarly(s.legIdx, depRT) {
			// refreshTimes emits the missed-connection reason and triggers the
			// replan. Never promote an impossible schedule-assumed boarding first.
			return
		}

		if assumed {
			s.setOpaque(true)
			s.warnPositionUnavailable()
			// The planner already reserved the station-entry/platform-change
			// margin. With no usable VehiclePosition, board and alight exactly
			// on the TripUpdate-adjusted clock — never with the generic +90s
			// virtual-rider lag used for unconfirmed road vehicles.
			if !s.boarded {
				if now.Before(depRT) {
					return
				}
				s.boarded = true
				s.boardedByShape = false
				s.gps.resetLegAnchors()
				s.sendProgress("riding", s.legIdx, true, "schedule_assumed")
			}
			if now.Before(arrRT) {
				return
			}
			s.completeAlight(tt, o, now, arrRT, "schedule_assumed")
			continue
		}

		if !s.boarded {
			_, shapeBoarded := s.gpsShapeBoarding(tt, r, s.legIdx, now)
			// GPS: sustained co-location with the tracked vehicle confirms
			// boarding before the feed's Passed does — but only once the
			// vehicle is confirmedly past the boarding stop: standing at the
			// stop next to a dwelling bus must never count as being on it
			if shapeBoarded || passed >= int16(r.board) || (passed < 0 && now.After(depRT.Add(90*time.Second))) ||
				(s.vehicleBeyond(tt, o, r, int(r.board)) && s.gpsWithVehicle(tt, o, r, now, confirmOn)) {
				s.boarded = true
				s.boardedByShape = shapeBoarded
				s.atStop = nil
				s.gps.resetLegAnchors()
				s.sendProgress("riding", s.legIdx, true, "")
			} else {
				return // still waiting at the stop
			}
		}
		// GPS alight assist: the rider is measurably AT the alight stop while
		// the vehicle is confirmedly there or beyond → they got off, even if
		// the feed's Passed has not caught up yet.
		gpsAlighted := s.gps.fresh(now) &&
			s.vehicleBeyond(tt, o, r, int(r.alight)-1) &&
			hold(&s.gps.nearStopSince,
				s.gps.distTo(leg.To.Lat, leg.To.Lon) <= nearStopBase, now, atStopHold)
		if passed >= int16(r.alight) || now.After(arrRT.Add(90*time.Second)) || gpsAlighted {
			// A stale ETA cannot put a GPS-observed rider on foot while they
			// are still following this ride far from its alighting stop.
			if !gpsAlighted && s.gps.fresh(now) && s.gps.distTo(leg.To.Lat, leg.To.Lon) > nearStopBase {
				if passed < int16(r.alight) {
					return // an overdue prediction alone is not evidence of alighting
				}
				if seg, ok := shapeBetween(tt, r); ok && seg.within(tt, s.gps.cur.Lat, s.gps.cur.Lon, s.gps.radius(offVehBase)) {
					return
				}
			}
			// GPS veto: the feed says the vehicle cleared the stop, but the
			// rider is still measurably ON it → hold; the rode-past check
			// (gps.go) decides once the evidence is conclusive
			if !gpsAlighted && s.gpsWithVehicle(tt, o, r, now, 0) {
				return
			}
			s.completeAlight(tt, o, now, arrRT, "")
			continue
		}
		return // riding
	}
}

// boardNextRideFromAccessShape handles a vehicle that picks the rider up
// during the short persistence window which still leaves the access walk as
// the current leg. Only an immediately following, normally GPS-trackable ride
// is eligible; schedule-assumed rail keeps its dedicated clock semantics.
func (s *session) boardNextRideFromAccessShape(tt *transit.Timetable, o *transit.RTOverlay, now time.Time) bool {
	next := s.legIdx + 1
	if next >= len(s.it.Legs) || s.it.Legs[next].Mode != "transit" || !s.gps.fresh(now) {
		return false
	}
	leg := &s.it.Legs[next]
	if s.scheduleAssumedLeg(tt, o, now, leg) {
		return false
	}
	r, ok := resolveRide(tt, leg)
	if !ok {
		return false
	}
	_, shapeBoarded := s.gpsShapeBoarding(tt, r, next, now)
	if !shapeBoarded {
		return false
	}
	s.legIdx = next
	s.boarded = true
	s.boardedByShape = true
	s.atStop = nil
	s.gps.resetLegAnchors()
	s.sendProgress("riding", next, true, "")
	return true
}

// emitPhase tells the client which leg is now current and in which phase —
// sent at every leg transition so the UI is never left guessing.
func (s *session) emitPhase(tt *transit.Timetable, o *transit.RTOverlay, now time.Time) {
	if s.legIdx >= len(s.it.Legs) {
		return
	}
	status := "walking"
	if s.it.Legs[s.legIdx].Mode == "transit" {
		status = "waiting"
	}
	source := ""
	if s.opaqueAt(tt, o, now, s.legIdx) {
		source = "schedule_assumed"
		s.warnPositionUnavailable()
	}
	s.sendProgress(status, s.legIdx, s.boarded, source)
}

func (s *session) sendProgress(status string, legIdx int, boarded bool, source string) {
	s.sink.Send(evProgress{
		Type: "progress", Status: status, LegIndex: legIdx, Boarded: boarded,
		TrackingSource: source,
	})
}

// completeAlight advances from a transit leg and re-anchors the following
// walking phase to the actual (possibly TripUpdate-adjusted) arrival.
func (s *session) completeAlight(tt *transit.Timetable, o *transit.RTOverlay, now, arrRT time.Time, source string) {
	completed := s.legIdx
	s.anchorRailAfterAlight(tt, completed, arrRT)
	s.legIdx++
	s.boarded = false
	s.boardedByShape = false
	s.atStop = nil
	s.gps.resetLegAnchors()
	s.sendProgress("alighted", completed, false, source)
	// The handoff is explicit so clients never hang on the alight phase.
	s.emitPhase(tt, o, now)
	if s.legIdx < len(s.it.Legs) && s.it.Legs[s.legIdx].Mode != "transit" {
		nl := &s.it.Legs[s.legIdx]
		at := arrRT
		if now.After(at) {
			at = now
		}
		dur := time.Duration(nl.DurationS) * time.Second
		nl.Depart, nl.Arrive = at, at.Add(dur)
	}
}

// resetRailReady starts a fresh per-plan set of immutable readiness anchors.
// BoardReadyAt is only present when an onboard replan starts after an incoming
// rail ride that is intentionally absent from the replacement itinerary.
func (s *session) resetRailReady() {
	s.railReadyAt = map[int]time.Time{}
	for i := range s.it.Legs {
		if at := s.it.Legs[i].BoardReadyAt; !at.IsZero() {
			s.railReadyAt[i] = at
		}
	}
}

func (s *session) setRailReady(idx int, at time.Time) {
	if idx < 0 || idx >= len(s.it.Legs) || at.IsZero() {
		return
	}
	if s.railReadyAt == nil {
		s.railReadyAt = map[int]time.Time{}
	}
	if _, fixed := s.railReadyAt[idx]; !fixed {
		s.railReadyAt[idx] = at
	}
}

func (s *session) ensureCurrentRailReady(tt *transit.Timetable, now time.Time) {
	if s.boarded || s.legIdx < 0 || s.legIdx >= len(s.it.Legs) ||
		!railLikeLeg(tt, &s.it.Legs[s.legIdx]) {
		return
	}
	if _, fixed := s.railReadyAt[s.legIdx]; fixed {
		return
	}
	// Normal plans install this when their access/transfer leg completes, and
	// onboard plans carry BoardReadyAt. This fallback covers legacy cached or
	// hand-built itineraries without turning "now + 60s" into a sliding timer.
	s.setRailReady(s.legIdx, now.Add(s.railEntryBuffer()))
}

func (s *session) railDepartureTooEarly(idx int, depart time.Time) bool {
	ready, ok := s.railReadyAt[idx]
	return ok && depart.Before(ready)
}

func (s *session) railEntryBuffer() time.Duration {
	if s.t != nil && s.t.Cfg != nil {
		return s.t.Cfg.Routing.RailEntryBuffer
	}
	return time.Minute
}

func (s *session) railTransferBuffer() time.Duration {
	if s.t != nil && s.t.Cfg != nil {
		return s.t.Cfg.Routing.RailTransferBuffer
	}
	return 90 * time.Second
}

func (s *session) nextTransitIndex(from int) int {
	for i := from; i < len(s.it.Legs); i++ {
		if s.it.Legs[i].Mode == "transit" {
			return i
		}
	}
	return -1
}

// anchorRailAfterStreet applies the one-minute station-entry allowance from
// the actual GPS arrival (or the stable virtual arrival). A rail-to-rail
// anchor is installed earlier, at alighting, because its 90 seconds include
// the intervening footpath rather than starting after it.
func (s *session) anchorRailAfterStreet(tt *transit.Timetable, streetIdx int, arrived time.Time) {
	next := streetIdx + 1
	if next >= len(s.it.Legs) || !railLikeLeg(tt, &s.it.Legs[next]) {
		return
	}
	if _, fixed := s.railReadyAt[next]; fixed {
		return
	}
	for i := streetIdx - 1; i >= 0; i-- {
		if s.it.Legs[i].Mode != "transit" {
			continue
		}
		if railLikeLeg(tt, &s.it.Legs[i]) {
			// Defensive fallback for cached/legacy itineraries. New sessions
			// receive the precise RT-adjusted anchor in completeAlight.
			s.setRailReady(next, s.it.Legs[i].Arrive.Add(s.railTransferBuffer()))
			return
		}
		break
	}
	s.setRailReady(next, arrived.Add(s.railEntryBuffer()))
}

// anchorRailAfterAlight starts a rail-to-rail change at the incoming train's
// actual/assumed arrival. The fixed target survives its walking leg and later
// RT refreshes, so the 90-second margin cannot slide on every evaluator tick.
func (s *session) anchorRailAfterAlight(tt *transit.Timetable, completed int, arrived time.Time) {
	if completed < 0 || completed >= len(s.it.Legs) {
		return
	}
	next := s.nextTransitIndex(completed + 1)
	if next < 0 || !railLikeLeg(tt, &s.it.Legs[next]) {
		return
	}
	if railLikeLeg(tt, &s.it.Legs[completed]) {
		s.setRailReady(next, arrived.Add(s.railTransferBuffer()))
		return
	}
	// With no street leg between a bus and rail, station entry begins at the
	// alight itself. Otherwise the intervening walk installs the anchor.
	if next == completed+1 {
		s.setRailReady(next, arrived.Add(s.railEntryBuffer()))
	}
}

const vehiclePositionMaxAge = 5 * time.Minute

// scheduleAssumedLeg identifies a rail-like ride without a usable fresh
// VehiclePosition. TripUpdates are intentionally irrelevant to this choice:
// they still adjust TripDep/TripArr and therefore the assumed clock.
func (s *session) scheduleAssumedLeg(tt *transit.Timetable, o *transit.RTOverlay, now time.Time, leg *engine.Leg) bool {
	if !railLikeLeg(tt, leg) {
		return false
	}
	r, ok := resolveRide(tt, leg)
	if !ok || o == nil {
		return true
	}
	if _, _, _, _, ok := o.Vehicle(r.trip); !ok {
		return true
	}
	stamp := o.VehicleTime(r.trip)
	if stamp == 0 {
		// Production overlays substitute the persisted feed-receipt time. A
		// zero here can only be an unverifiable hand-built/legacy overlay, which
		// must fail closed rather than remain fresh forever.
		return true
	}
	return now.Sub(time.Unix(int64(stamp), 0)) > vehiclePositionMaxAge
}

func railLikeLeg(tt *transit.Timetable, leg *engine.Leg) bool {
	if leg == nil || leg.Mode != "transit" {
		return false
	}
	if leg.Route != nil {
		return transit.IsRailLikeRouteType(leg.Route.Type)
	}
	r, ok := resolveRide(tt, leg)
	if !ok {
		return false
	}
	pattern := tt.PatternOfTrip(r.trip)
	route := tt.PatRoute[pattern]
	return route >= 0 && int(route) < len(tt.Routes) && transit.IsRailLikeRouteType(tt.Routes[route].Type)
}

// opaqueAt spans a schedule-assumed rail ride and an in-station walking
// transfer between rail rides. User GPS is deliberately ignored throughout
// this block: an absent/jittering underground fix is not evidence of a
// deviation and must not trigger a stop guard or gps_lost warning.
func (s *session) opaqueAt(tt *transit.Timetable, o *transit.RTOverlay, now time.Time, idx int) bool {
	if idx < 0 || idx >= len(s.it.Legs) {
		return false
	}
	if s.it.Legs[idx].Mode == "transit" {
		return s.scheduleAssumedLeg(tt, o, now, &s.it.Legs[idx])
	}
	prev, next := -1, -1
	for i := idx - 1; i >= 0; i-- {
		if s.it.Legs[i].Mode == "transit" {
			prev = i
			break
		}
	}
	for i := idx + 1; i < len(s.it.Legs); i++ {
		if s.it.Legs[i].Mode == "transit" {
			next = i
			break
		}
	}
	if prev < 0 || next < 0 || !railLikeLeg(tt, &s.it.Legs[prev]) || !railLikeLeg(tt, &s.it.Legs[next]) {
		return false
	}
	return s.scheduleAssumedLeg(tt, o, now, &s.it.Legs[prev]) ||
		s.scheduleAssumedLeg(tt, o, now, &s.it.Legs[next])
}

func (s *session) warnPositionUnavailable() {
	if s.positionUnavailableWarned {
		return
	}
	s.positionUnavailableWarned = true
	s.sink.Send(evWarning{
		Type: "warning", Code: "position_unavailable", LegIndex: s.legIdx,
		Message: "You're entering a section where your position may be unavailable. Guidance will continue automatically using the timetable and realtime service updates.",
	})
}

func (s *session) setOpaque(opaque bool) {
	if opaque && !s.opaque {
		// A pre-entry fix must never reappear as "current" at the far end of
		// the tunnel, and fixes received while opaque are discarded by Run.
		s.gps = newGPSState()
	}
	s.opaque = opaque
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
	var priorTransit, priorRail bool
	var priorRailArr time.Time
	for i := s.legIdx; i < len(s.it.Legs); i++ {
		leg := &s.it.Legs[i]
		if leg.Mode != "transit" {
			dur := time.Duration(leg.DurationS) * time.Second
			if i == s.legIdx {
				// The current street leg already started: its original arrival is
				// an absolute countdown anchor. Re-adding the full duration at
				// every tick made the ETA slide forward forever.
				dep, arrive := leg.Depart, leg.Arrive
				if now.Before(dep) {
					arrive = dep.Add(dur)
				} else if arrive.Before(now) {
					arrive = now
				}
				cursor = arrive
				out = append(out, legTime{Index: i, Depart: dep, Arrive: arrive})
				continue
			}
			dep := cursor
			cursor = dep.Add(dur)
			out = append(out, legTime{Index: i, Depart: dep, Arrive: cursor})
			continue
		}
		r, ok := resolveRide(tt, leg)
		if !ok {
			if railLikeLeg(tt, leg) && !(i == s.legIdx && s.boarded) &&
				s.railDepartureTooEarly(i, leg.Depart) && feas.ok {
				feas = feasibility{false, "missed_connection", "not enough time remains to enter or change rail service"}
			}
			out = append(out, legTime{Index: i, Depart: leg.Depart, Arrive: leg.Arrive})
			cursor = leg.Arrive
			priorTransit, priorRail, priorRailArr = true, railLikeLeg(tt, leg), leg.Arrive
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
			ready := cursor
			if railLikeLeg(tt, leg) {
				if fixed, ok := s.railReadyAt[i]; ok {
					if fixed.After(ready) {
						ready = fixed
					}
				} else if priorTransit && priorRail {
					if changeReady := priorRailArr.Add(s.railTransferBuffer()); changeReady.After(ready) {
						ready = changeReady
					}
				} else if i == s.legIdx {
					// A direct/legacy rail itinerary has no preceding street leg
					// from which to capture arrival. Anchor once now; never rebuild
					// this deadline on later ticks.
					s.setRailReady(i, now.Add(s.railEntryBuffer()))
					ready = s.railReadyAt[i]
				} else {
					ready = ready.Add(s.railEntryBuffer())
				}
				if feas.ok && depRT.Before(ready) {
					feas = feasibility{false, "missed_connection", "not enough time remains to enter or change rail service"}
				}
			}
			// A connection counts as missed ONLY when GTFS-RT confirms the
			// vehicle already cleared the boarding stop before the rider
			// could be there. Predicted departures are not enough: near the
			// start of a run (vehicle held at its terminus, delay not yet
			// propagated) the clock slides past the scheduled time and a
			// prediction-based check produces false "missed_connection"
			// reroutes for a bus that has not even left.
			if feas.ok && ready.After(depRT) && o.TripPassed(r.trip) >= int16(r.board) {
				feas = feasibility{false, "missed_connection", "your connection already left its stop"}
			}
			if depRT.After(cursor) {
				cursor = depRT
			}
		}
		cursor = arrRT
		priorTransit = true
		priorRail = railLikeLeg(tt, leg)
		priorRailArr = arrRT
		out = append(out, legTime{Index: i, Depart: depRT, Arrive: arrRT,
			DelayS: delay, Realtime: o.TripHasRT(r.trip)})
	}
	return out, feas
}

// emitVehicle streams the live position of the bus/train the user rides or
// is about to board: where it is, which stop it is at/approaching, how many
// stops from the user, and its delay — even before the user is on it.
func (s *session) emitVehicle(tt *transit.Timetable, o *transit.RTOverlay, now time.Time) {
	for i := s.legIdx; i < len(s.it.Legs); i++ {
		leg := &s.it.Legs[i]
		if leg.Mode != "transit" {
			continue
		}
		if s.scheduleAssumedLeg(tt, o, now, leg) {
			return // never expose a stale rail position during clock-based guidance
		}
		r, ok := resolveRide(tt, leg)
		if !ok {
			return
		}
		lat, lon, pos, status, ok := o.Vehicle(r.trip)
		if !ok {
			return // no vehicle entity for this trip (rail-like services etc.)
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
		if leg.Route != nil && engine.IsRailLikeType(leg.Route.Type) {
			continue // rail-like: eligible for schedule-assumed tracking
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
			if leg.Route != nil && engine.IsRailLikeType(leg.Route.Type) {
				return true // rail-like service counts as live ground
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
		if sameRides(s.it.Legs[s.legIdx:], best.Legs) {
			return nil // do not repeatedly announce the same unconfirmed option
		}
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
	if len(it.Legs) == 0 {
		return
	}
	wasBoarded, byShape := s.boarded, s.boardedByShape
	if wasBoarded && (!it.Legs[0].Boarded || it.Legs[0].TripID != s.it.Legs[s.legIdx].TripID) {
		return // every replacement path must preserve the occupied vehicle
	}
	s.t.Log.Debug("switchTo", "reason", reason, "legs", len(it.Legs), "arrive", it.Arrive, "id", it.ID)
	changed := changedLegs(s.it.Legs[min(s.legIdx, len(s.it.Legs)):], it.Legs)
	s.it = it
	s.legIdx = 0
	s.boarded = it.Legs[0].Boarded
	s.boardedByShape = s.boarded && byShape
	if s.boarded || it.Legs[0].Mode != "transit" || s.reachedPlace() == nil {
		s.atStop = nil
	}
	s.lastEmit = map[int]legTime{}
	s.lastVeh = evVehicle{}
	s.warned = map[string]bool{} // fresh plan, fresh guard state
	s.risk = map[int]*riskState{}
	s.opaque = false
	s.resetRailReady()
	s.gps.resetLegAnchors()
	s.gps.deviated = map[string]bool{} // fresh plan, fresh deviation slate
	s.liveMode = it.Live || s.liveMode
	s.sink.Send(evReroute{Type: "reroute", Reason: reason, SavingS: saving, Message: msg,
		ChangedLegs: changed, Itinerary: it})
	if s.boarded {
		s.sendProgress("riding", 0, true, "")
	} else if s.reachedPlace() != nil {
		s.gps.latchReachedStop(s.boardingKey(0))
		s.sendProgress("waiting", 0, false, "")
	}
}

func sameRides(a, b []engine.Leg) bool {
	keys := func(legs []engine.Leg) string {
		key := ""
		for i := range legs {
			if legs[i].Mode == "transit" {
				key += legSignature(&legs[i]) + ";"
			}
		}
		return key
	}
	return keys(a) == keys(b)
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
		from := s.firstAlightPosition(tt, o, r, now)
		for pos := from; pos < int(tt.TripLen(r.trip)); pos++ {
			if tt.StopSkipped(r.trip, uint16(pos)) {
				continue
			}
			at := base.Add(time.Duration(tt.TripArr(r.trip, uint16(pos))) * time.Second)
			if at.Before(now) {
				continue
			}
			seeds[stops[pos]] = at
		}
		source := engine.StopPlanSource{
			PriorRail:  railLikeLeg(tt, leg),
			ReadySlack: s.t.Cfg.Routing.TransferSlack,
			Onboard:    leg,
		}
		its, err := s.t.E.PlanFromStops(seeds, dLat, dLon, now, 12, source)
		if err != nil || len(its) == 0 {
			return engine.Itinerary{}, false // keep riding while searching
		}
		return s.pickFeasible(its, now)
	}
	if stop := s.reachedPlace(); stop != nil {
		for id, stopID := range tt.StopID {
			if stopID != stop.StopID {
				continue
			}
			its, err := s.t.E.PlanFromStops(map[int32]time.Time{int32(id): now}, dLat, dLon, now, 12, engine.StopPlanSource{})
			if err == nil {
				return s.pickFeasible(its, now)
			}
		}
		return engine.Itinerary{}, false
	}

	// walking or waiting: replan from the rider's real position when the
	// client streams GPS, else from the virtual point along the plan
	lat, lon := s.replanPoint(now)
	req := s.req
	req.FromLat, req.FromLon = lat, lon
	req.ToLat, req.ToLon = dLat, dLon
	req.Mode = "transit"
	req.When = now
	req.ArriveBy = false
	req.Num = 12
	resp, err := s.t.E.Plan(req)
	if err != nil || len(resp.Itineraries) == 0 {
		// No transit journey from here — typically the destination is now
		// within walking distance (any ride would arrive later than just
		// walking, so RAPTOR prunes everything). The engine never strands
		// the rider: hand them a plain walk when it is a reasonable one.
		return s.walkFallback(now, dLat, dLon)
	}
	return s.pickFeasible(resp.Itineraries, now)
}

// Give a rider two stops of notice. GPS shape progress can be ahead of stale
// TripUpdates, so those updates must never seed a stop already behind them.
func (s *session) firstAlightPosition(tt *transit.Timetable, o *transit.RTOverlay, r ride, now time.Time) int {
	passed := max(int(r.board), int(o.TripPassed(r.trip)))
	positionKnown := o.TripPassed(r.trip) >= 0
	if _, _, pos, status, ok := o.Vehicle(r.trip); ok && pos >= 0 {
		at := int(pos) - 1
		if status == 1 { // dwelling at this stop: two subsequent stops of notice
			at = int(pos)
		}
		passed = max(passed, at)
		positionKnown = true
	}
	if s.gps.fresh(now) {
		full := r
		full.alight = tt.TripLen(r.trip) - 1
		if seg, ok := shapeBetween(tt, full); ok {
			along, distance, ok := seg.project(tt, s.gps.cur.Lat, s.gps.cur.Lon)
			if ok && distance <= s.gps.radius(offVehBase) {
				positionKnown = true
				pat := tt.PatternOfTrip(r.trip)
				for pos := int(r.board) + 1; pos <= int(full.alight); pos++ {
					idx := tt.PatShapeIdx[tt.PatFirstStop[pat]+uint32(pos)]
					stopAlong := float64(tt.ShpCumDm[idx]-tt.ShpCumDm[seg.lo]) / 10
					if along >= stopAlong {
						passed = max(passed, pos)
					}
				}
			}
		}
	}
	if !positionKnown {
		base := rideBase(tt, r, s.it.Legs[s.legIdx].Depart)
		for pos := int(r.board) + 1; pos < int(tt.TripLen(r.trip)); pos++ {
			if !base.Add(time.Duration(tt.TripDep(r.trip, uint16(pos))) * time.Second).After(now) {
				passed = pos
			}
		}
	}
	return passed + 2
}

func (s *session) pickFeasible(its []engine.Itinerary, now time.Time) (engine.Itinerary, bool) {
	valid := make([]engine.Itinerary, 0, len(its))
	for _, it := range its {
		it.Legs = append([]engine.Leg(nil), it.Legs...) // cached plans remain immutable
		s.t.E.AssessRisk(&it, now)
		ok := len(it.Legs) > 0
		for _, leg := range it.Legs {
			if leg.Mode == "transit" && !leg.Boarded && leg.CatchProb < 0.8 {
				ok = false
				break
			}
		}
		if ok {
			valid = append(valid, it)
		}
	}
	if len(valid) == 0 {
		return engine.Itinerary{}, false
	}
	return pickBest(valid, s.liveMode), true
}

// walkFallback offers a plain walking itinerary from the rider's position
// when no transit replan exists. Capped at 30 minutes: beyond that, keeping
// the search alive beats sending someone on an absurd hike.
func (s *session) walkFallback(now time.Time, dLat, dLon float64) (engine.Itinerary, bool) {
	if s.boarded {
		return engine.Itinerary{}, false
	}
	lat, lon := s.replanPoint(now)
	req := s.req
	req.FromLat, req.FromLon = lat, lon
	req.ToLat, req.ToLon = dLat, dLon
	req.Mode = "walk"
	req.When = now
	req.ArriveBy = false
	if resp, err := s.t.E.Plan(req); err == nil && len(resp.Itineraries) > 0 &&
		resp.Itineraries[0].DurationS <= 30*60 {
		return resp.Itineraries[0], true
	}
	return engine.Itinerary{}, false
}

// pickBest chooses the reroute target with three rules, in order:
//  1. realtime confidence — never move a rider onto a schedule-only bus when
//     an RT-confirmed (trip updates) alternative exists; delays are already
//     folded into the times, so a late-but-catchable bus competes fairly;
//  2. earlier arrival (beyond a 60s tie window);
//  3. less walking — the same vehicle at a nearer stop beats a farther stop
//     with an earlier vehicle ETA: you walk less and arrive when you arrive.
func pickBest(its []engine.Itinerary, wantLive bool) engine.Itinerary {
	class := func(it *engine.Itinerary) int {
		if wantLive && it.Live {
			return 0
		}
		for i := range it.Legs {
			if it.Legs[i].Mode == "transit" {
				if it.Legs[i].Realtime {
					return 1
				}
				return 2
			}
		}
		return 1 // no transit at all: nothing needing confirmation
	}
	walk := func(it *engine.Itinerary) int {
		total := 0
		for i := range it.Legs {
			if it.Legs[i].Mode != "transit" {
				total += it.Legs[i].DurationS
			}
		}
		return total
	}
	best := 0
	for i := 1; i < len(its); i++ {
		a, b := &its[best], &its[i]
		ca, cb := class(a), class(b)
		// expected arrivals (risk-adjusted): a nominally-faster plan built on
		// a fragile connection must not win the reroute
		ea, eb := a.ExpectedArrive(), b.ExpectedArrive()
		switch {
		case cb < ca:
			best = i
		case cb > ca:
		case eb.Before(ea.Add(-60 * time.Second)):
			best = i
		case ea.Before(eb.Add(-60 * time.Second)):
		case walk(b) < walk(a):
			best = i
		}
	}
	return its[best]
}

// replanPoint is where replans start from: the real GPS fix when fresh,
// otherwise the plan-based estimate.
func (s *session) replanPoint(now time.Time) (float64, float64) {
	if stop := s.reachedPlace(); stop != nil {
		return stop.Lat, stop.Lon
	}
	if s.gps.fresh(now) {
		return s.gps.cur.Lat, s.gps.cur.Lon
	}
	return s.virtualPoint(now)
}

// virtualPoint estimates where the user is along the current street leg
// (or pins them at the stop while waiting). Deliberately plan-based: the
// off_route detector compares the GPS against THIS expectation.
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
