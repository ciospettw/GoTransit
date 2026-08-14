package track

// GPS fusion — protocollo "il client non calcola niente". The client MAY
// stream raw position fixes over /v1/track; the server derives ONLY the
// stop-related states from them:
//
//   WALKING_TO_STOP  too-slow check: can the rider still reach the boarding
//                    stop before the (RT-adjusted) departure? If not →
//                    proactive reroute ("too_slow") from their position.
//   AT_STOP          wander tolerance: within stop_wander_radius (~150 m,
//                    GPS jitter / coffee at the bar) nothing happens; beyond
//                    it ordered movement along the planned board→alight shape
//                    can confirm boarding even without fresh vehicle data;
//                    otherwise a background walk-back computation decides
//                    whether they can still make it — only when they can't →
//                    reroute ("left_stop").
//   ONBOARD          NO GPS inference while riding, with ONE exception:
//                    "stayed on the line" — the rider is >1 km from the
//                    expected alight stop AND their recent fixes follow the
//                    pattern shape BEYOND that stop → they never got off →
//                    deviation "stayed_on_vehicle" + onboard replan from the
//                    vehicle's downstream stops.
//   ALIGHT           near the alight stop + vehicle context confirms the
//                    rider got off (assists the feed-based virtual rider).
//
// Fixes are EVIDENCE, never truth: every inference requires persistence
// (a condition holding across consecutive fixes) and corroboration (user
// evidence + vehicle/feed evidence agreeing). In doubt, the GPS-free
// virtual rider governs and nothing changes. Without any fixes the session
// runs in pure virtual mode — same engine, zero position knowledge.

import (
	"fmt"
	"math"
	"time"

	"gotransit/internal/transit"
)

// Fix is one client position sample.
type Fix struct {
	Lat       float64
	Lon       float64
	AccuracyM float64   // reported horizontal accuracy; 0 = unknown
	HeadingD  float64   // course over ground, degrees; <0 = unknown
	SpeedMS   float64   // ground speed, m/s; <0 = unknown
	At        time.Time // client timestamp if plausible, else receive time
}

// Tolerances. Base radii grow with the fix's reported accuracy (capped), so
// a poor GPS day only makes the system MORE conservative, never trigger-happy.
const (
	fixStale     = 45 * time.Second // older fixes are ignored: virtual rider governs
	fixAccMax    = 150.0            // meters; fixes worse than this are discarded
	fixAccCap    = 60.0             // how much of the accuracy inflates radii
	vehGPSErr    = 30.0             // typical bus AVL error, always budgeted
	nearStopBase = 40.0             // "you are at the stop"
	onVehBase    = 55.0             // co-located with the tracked vehicle
	offVehBase   = 130.0            // clearly separated from the vehicle
	confirmOn    = 20 * time.Second // co-location persistence → boarded
	atStopHold   = 10 * time.Second // persistence to advance a walk leg early
	offRouteBase = 300.0            // far from every plan anchor (walk legs)
	offRouteDur  = 90 * time.Second // ...for this long → off route
	walkDistStep = 30.0             // walk-progress re-emit threshold (meters)
	guardHold    = 10 * time.Second // persistence for too_slow / left_stop

	// Shape-only boarding fallback. Once the rider has reached the stop, a
	// sequence of fresh fixes progressing board -> alight inside this
	// corridor is stronger evidence than an absent/stale vehicle position.
	shapeBoardAwayM       = 200.0
	shapeBoardCorridorM   = 55.0
	shapeBoardProgressM   = 30.0
	shapeBoardStepM       = 5.0
	shapeBoardBacktrackM  = 35.0
	shapeBoardHold        = 5 * time.Second
	shapeBoardMovingFixes = 2
	// detour factor: street distance ≈ 1.3 × beeline when walking back
	walkDetour = 1.3
	// fix history kept for the shape-following check
	fixHistLen = 6
)

type gpsState struct {
	cur Fix
	has bool

	// hist holds the last accepted fixes, most recent last (cur included).
	hist []Fix
	// speedEMA tracks observed ground speed (m/s) across fixes.
	speedEMA float64

	// persistence anchors: zero time = condition not currently holding
	nearStopSince time.Time
	withVehSince  time.Time
	offRouteSince time.Time
	tooSlowSince  time.Time
	leftStopSince time.Time

	// reachedStops is a persistent, per-ride latch. Reaching a boarding stop
	// is a historical fact and must survive the walk -> wait handoff (whose
	// generic anchor reset used to erase the lone reachedStop bool).
	reachedStops map[string]time.Time
	shapeBoard   map[string]*shapeBoardState

	lastWalkDist float64 // last emitted distance_to_stop_m (-1 = none)
	deviated     map[string]bool
}

// shapeBoardState consumes each fix at most once and remembers ordered
// progress along one ride's board -> alight shape portion.
type shapeBoardState struct {
	lastAt           time.Time
	lastProgress     float64
	evidenceProgress float64
	haveLast         bool
	movingSince      time.Time
	movingFrom       float64
	movingFixes      int
	onShape          bool
	awayM            float64
	accuracyM        float64
}

func newGPSState() gpsState {
	return gpsState{
		lastWalkDist: -1,
		deviated:     map[string]bool{},
		reachedStops: map[string]time.Time{},
		shapeBoard:   map[string]*shapeBoardState{},
	}
}

// update ingests a fix; implausible ones are dropped whole.
func (g *gpsState) update(f Fix, now time.Time) {
	if f.Lat == 0 && f.Lon == 0 {
		return
	}
	if f.AccuracyM > fixAccMax {
		return
	}
	if f.At.IsZero() || f.At.After(now.Add(30*time.Second)) {
		f.At = now // timestamp assente o implausibile: vale l'ora di ricezione
	}
	if now.Sub(f.At) > fixStale {
		return // esplicitamente vecchio (consegna in ritardo): non è evidenza
	}
	// never let an older fix replace a newer one (out-of-order delivery)
	if g.has && f.At.Before(g.cur.At) {
		return
	}
	// observed speed: reported by the client, else derived from displacement
	if g.has {
		dt := f.At.Sub(g.cur.At).Seconds()
		if dt >= 1 && dt <= 60 {
			v := f.SpeedMS
			if v <= 0 || !(v < 50) { // absent, zero-valued or absurd: derive
				v = distMeters(g.cur.Lat, g.cur.Lon, f.Lat, f.Lon) / dt
			}
			if v < 50 {
				g.speedEMA = 0.7*g.speedEMA + 0.3*v
			}
		}
	}
	g.cur = f
	g.has = true
	g.hist = append(g.hist, f)
	if len(g.hist) > fixHistLen {
		g.hist = g.hist[len(g.hist)-fixHistLen:]
	}
}

// fresh reports whether current evidence is usable at all.
func (g *gpsState) fresh(now time.Time) bool {
	return g.has && now.Sub(g.cur.At) <= fixStale
}

// radius inflates a base threshold with the fix's own uncertainty.
func (g *gpsState) radius(base float64) float64 {
	return base + math.Min(g.cur.AccuracyM, fixAccCap)
}

// walkSpeed is the rider's usable walking pace for ETA math: the observed
// EMA clamped to plausible walking bounds (never below 1.1 m/s so a rider
// standing still is not instantly declared late).
func (g *gpsState) walkSpeed() float64 {
	return math.Min(math.Max(g.speedEMA, 1.1), 2.2)
}

// hold updates a persistence anchor: sets it when cond starts holding,
// clears it when it stops, and reports whether it has held for at least d.
func hold(anchor *time.Time, cond bool, now time.Time, d time.Duration) bool {
	if !cond {
		*anchor = time.Time{}
		return false
	}
	if anchor.IsZero() {
		*anchor = now
	}
	return now.Sub(*anchor) >= d
}

// distMeters is the haversine distance.
func distMeters(aLat, aLon, bLat, bLon float64) float64 {
	const R = 6371e3
	p1 := aLat * math.Pi / 180
	p2 := bLat * math.Pi / 180
	dp := (bLat - aLat) * math.Pi / 180
	dl := (bLon - aLon) * math.Pi / 180
	h := math.Sin(dp/2)*math.Sin(dp/2) + math.Cos(p1)*math.Cos(p2)*math.Sin(dl/2)*math.Sin(dl/2)
	return R * 2 * math.Atan2(math.Sqrt(h), math.Sqrt(1-h))
}

func (g *gpsState) distTo(lat, lon float64) float64 {
	return distMeters(g.cur.Lat, g.cur.Lon, lat, lon)
}

// resetLegAnchors clears transient persistence anchors when the current leg
// changes. reachedStops and shapeBoard deliberately survive: both are keyed
// per ride, so evidence cannot leak to another leg, while the fact that the
// rider reached a stop survives the walk -> waiting handoff.
func (g *gpsState) resetLegAnchors() {
	g.nearStopSince = time.Time{}
	g.withVehSince = time.Time{}
	g.offRouteSince = time.Time{}
	g.tooSlowSince = time.Time{}
	g.leftStopSince = time.Time{}
	g.lastWalkDist = -1
}

// boardingKey identifies a concrete boarding independently of its position in
// the itinerary. A compatible reroute may move the same ride to another leg,
// change its alight stop, or refresh its RT-adjusted departure; all of those
// must retain the reached-stop fact. A tracking session cannot span two
// service-day instances of the same namespaced trip, so trip + stop is stable.
func (s *session) boardingKey(legIdx int) string {
	if legIdx < 0 || legIdx >= len(s.it.Legs) {
		return ""
	}
	leg := &s.it.Legs[legIdx]
	return fmt.Sprintf("%s/%s", leg.TripID, leg.From.StopID)
}

func (g *gpsState) latchReachedStop(key string) {
	if key == "" {
		return
	}
	if g.reachedStops == nil {
		g.reachedStops = map[string]time.Time{}
	}
	if _, ok := g.reachedStops[key]; !ok {
		g.reachedStops[key] = g.cur.At
	}
}

// gpsShapeBoarding recognizes the rider moving away on the planned vehicle's
// own shape when Passed/VehiclePosition cannot corroborate boarding. It
// returns following=true as soon as coherent movement exists so left_stop is
// suspended while evidence accumulates; boarded becomes true only beyond
// ~200 m and after sustained ordered progress.
func (s *session) gpsShapeBoarding(tt *transit.Timetable, r ride, legIdx int, now time.Time) (following, boarded bool) {
	if !s.gps.fresh(now) {
		return false, false
	}
	key := s.boardingKey(legIdx)
	reachedAt, reached := s.gps.reachedStops[key]
	if !reached {
		return false, false
	}
	seg, ok := shapeBetween(tt, r)
	if !ok {
		return false, false
	}
	if s.gps.shapeBoard == nil {
		s.gps.shapeBoard = map[string]*shapeBoardState{}
	}
	st := s.gps.shapeBoard[key]
	if st == nil {
		st = &shapeBoardState{}
		s.gps.shapeBoard[key] = st
	}

	// Replay all not-yet-consumed fresh fixes. This makes the detector robust
	// when evaluation skipped one or more samples (RT refresh, slow tick).
	for _, f := range s.gps.hist {
		if f.At.Before(reachedAt) || !f.At.After(st.lastAt) || now.Sub(f.At) > fixStale {
			continue
		}
		progress, shapeDist, projected := seg.project(tt, f.Lat, f.Lon)
		corridor := shapeBoardCorridorM + math.Min(f.AccuracyM, fixAccCap)
		if !projected || shapeDist > corridor {
			// An incoherent current stream never disables the ordinary stop
			// guard. Keep the reached-stop latch, but restart shape evidence.
			*st = shapeBoardState{lastAt: f.At}
			continue
		}

		away := distMeters(f.Lat, f.Lon, s.it.Legs[legIdx].From.Lat, s.it.Legs[legIdx].From.Lon)
		if !st.haveLast || f.At.Sub(st.lastAt) > fixStale {
			*st = shapeBoardState{
				lastAt: f.At, lastProgress: progress, evidenceProgress: progress,
				haveLast: true, onShape: true, awayM: away, accuracyM: f.AccuracyM,
			}
			continue
		}

		delta := progress - st.evidenceProgress
		if delta < -shapeBoardBacktrackM {
			// Following the shape in reverse (or a large GPS jump) contradicts
			// boarding this ride; restart from this fix without losing arrival.
			*st = shapeBoardState{
				lastAt: f.At, lastProgress: progress, evidenceProgress: progress,
				haveLast: true, onShape: true, awayM: away, accuracyM: f.AccuracyM,
			}
			continue
		}
		if delta >= shapeBoardStepM {
			if st.movingSince.IsZero() {
				st.movingSince = st.lastAt
				st.movingFrom = st.evidenceProgress
			}
			st.movingFixes++
			st.evidenceProgress = progress
		}
		st.lastAt = f.At
		st.lastProgress = progress
		st.onShape = true
		st.awayM = away
		st.accuracyM = f.AccuracyM
	}

	if !st.onShape || st.movingSince.IsZero() || st.movingFixes == 0 {
		return false, false
	}
	progressed := st.lastProgress - st.movingFrom
	following = progressed >= shapeBoardProgressM
	if !following {
		return false, false
	}
	clearAway := st.awayM >= shapeBoardAwayM+math.Min(st.accuracyM, fixAccCap)
	boarded = clearAway && st.movingFixes >= shapeBoardMovingFixes &&
		st.lastAt.Sub(st.movingSince) >= shapeBoardHold
	return following, boarded
}

// ---- session-level fusion (evidence + timetable + overlay) -----------------

// vehiclePos returns the tracked vehicle's position in degrees, pinning it at
// its current pattern stop when the feed carries no coordinates.
func (s *session) vehiclePos(tt *transit.Timetable, o *transit.RTOverlay, r ride) (float64, float64, bool) {
	lat, lon, pos, _, ok := o.Vehicle(r.trip)
	if !ok {
		return 0, 0, false
	}
	if lat == 0 && lon == 0 {
		stops := tt.PatternStops(tt.PatternOfTrip(r.trip))
		if pos < 0 || int(pos) >= len(stops) {
			return 0, 0, false
		}
		st := stops[pos]
		return float64(tt.StopLat[st]) / 1e7, float64(tt.StopLon[st]) / 1e7, true
	}
	return float64(lat) / 1e7, float64(lon) / 1e7, true
}

// vehicleBeyond reports whether the vehicle's live position is confirmedly
// past the given pattern position (it is at, or heading to, a later stop).
func (s *session) vehicleBeyond(tt *transit.Timetable, o *transit.RTOverlay, r ride, pos int) bool {
	_, _, vpos, _, ok := o.Vehicle(r.trip)
	return ok && int(vpos) > pos
}

// gpsWithVehicle reports co-location with the tracked vehicle, sustained for
// at least minHold (0 = instantaneous check with the same generous radius).
// The radius budgets BOTH GPS errors: the rider's fix and the bus AVL.
func (s *session) gpsWithVehicle(tt *transit.Timetable, o *transit.RTOverlay, r ride, now time.Time, minHold time.Duration) bool {
	if !s.gps.fresh(now) {
		return false
	}
	vLat, vLon, ok := s.vehiclePos(tt, o, r)
	if !ok {
		return false
	}
	near := s.gps.distTo(vLat, vLon) <= s.gps.radius(onVehBase)+vehGPSErr
	if minHold <= 0 {
		return near
	}
	return hold(&s.gps.withVehSince, near, now, minHold)
}

type deviation struct{ ev evDeviation }

// gpsDeviation detects, with certainty only, that the rider departed from the
// plan. While ONBOARD there is exactly ONE inference (stayed_on_vehicle); on
// foot only the walk-leg off_route survives. Everything else stop-related
// lives in gpsStopGuard.
func (s *session) gpsDeviation(tt *transit.Timetable, o *transit.RTOverlay, now time.Time) *deviation {
	if !s.gps.fresh(now) || s.legIdx >= len(s.it.Legs) {
		return nil
	}
	leg := &s.it.Legs[s.legIdx]

	// ONBOARD: no GPS processing while riding, except the rode-past check.
	if leg.Mode == "transit" && s.boarded {
		if dev := s.rodePastCheck(tt, o, now, s.legIdx, false); dev != nil {
			return dev
		}
		return nil
	}

	// Just alighted (per the virtual rider) but actually still on the bus?
	// Same rode-past check against the PREVIOUS transit leg's shape.
	if prev := s.prevTransitLeg(); prev >= 0 {
		if dev := s.rodePastCheck(tt, o, now, prev, true); dev != nil {
			return dev
		}
	}

	// off_route: WALK legs only — sustained distance from every plan anchor.
	// Waiting legs are governed by gpsStopGuard's wander/walk-back logic.
	if leg.Mode == "transit" {
		return nil
	}
	key := fmt.Sprintf("off_route/%d", s.legIdx)
	if s.gps.deviated[key] {
		return nil
	}
	vLat, vLon := s.virtualPoint(now)
	last := s.it.Legs[len(s.it.Legs)-1]
	minD := math.Min(
		math.Min(s.gps.distTo(leg.From.Lat, leg.From.Lon), s.gps.distTo(leg.To.Lat, leg.To.Lon)),
		math.Min(s.gps.distTo(vLat, vLon), s.gps.distTo(last.To.Lat, last.To.Lon)),
	)
	off := minD > s.gps.radius(offRouteBase)
	if hold(&s.gps.offRouteSince, off, now, offRouteDur) {
		s.gps.deviated[key] = true
		return &deviation{evDeviation{
			Type: "deviation", Kind: "off_route", LegIndex: s.legIdx,
			Message: "you moved away from the planned route; recomputing from your position",
		}}
	}
	return nil
}

// prevTransitLeg returns the index of the most recent completed transit leg
// (looking back from the current one), or -1.
func (s *session) prevTransitLeg() int {
	for i := s.legIdx - 1; i >= 0; i-- {
		if s.it.Legs[i].Mode == "transit" {
			return i
		}
	}
	return -1
}

// rodePastCheck: "sei rimasto sulla linea". Fires only when BOTH hold:
//   - the rider is more than rode_past_dist (default 1 km) from the leg's
//     alight stop, and
//   - their last rode_past_fixes fixes all lie inside the corridor of the
//     pattern shape BEYOND the alight stop (they are following the line).
//
// afterAlight: the virtual rider already advanced past legIdx — roll the
// session back onto the vehicle so the reroute replans from its downstream
// stops ("ok, sei rimasto sulla linea: ricalcolo").
func (s *session) rodePastCheck(tt *transit.Timetable, o *transit.RTOverlay, now time.Time, legIdx int, afterAlight bool) *deviation {
	cfg := s.t.Cfg
	leg := &s.it.Legs[legIdx]
	key := fmt.Sprintf("stayed_on_vehicle/%d", legIdx)
	if s.gps.deviated[key] {
		return nil
	}
	if s.gps.distTo(leg.To.Lat, leg.To.Lon) <= cfg.Track.RodePastDistM {
		return nil
	}
	r, ok := resolveRide(tt, leg)
	if !ok {
		return nil
	}
	seg, ok := shapeBeyond(tt, r)
	if !ok {
		return nil
	}
	need := cfg.Track.RodePastFixes
	if need < 1 {
		need = 1
	}
	if len(s.gps.hist) < need {
		return nil
	}
	corridor := s.gps.radius(offVehBase) + vehGPSErr
	for _, f := range s.gps.hist[len(s.gps.hist)-need:] {
		if !seg.within(tt, f.Lat, f.Lon, corridor) {
			return nil
		}
	}
	s.gps.deviated[key] = true
	if afterAlight {
		s.legIdx = legIdx
		s.boarded = true // back on the vehicle: replan seeds its downstream stops
	}
	return &deviation{evDeviation{
		Type: "deviation", Kind: "stayed_on_vehicle", LegIndex: legIdx,
		ExpectedStop: &place{StopID: leg.To.StopID, Name: leg.To.Name},
		Message:      "you stayed on the line past your stop; recomputing from the vehicle's next stops",
	}}
}

// guardAction is a proactive stop-related reroute decision.
type guardAction struct {
	reason  string // "too_slow" | "left_stop"
	message string
}

// gpsStopGuard runs the WALKING_TO_STOP / AT_STOP background math against the
// next boarding: it NEVER cares where the rider wandered — only whether the
// departure is still reachable. Within stop_wander_radius nothing ever fires
// (GPS jitter, pacing, un caffè al volo).
func (s *session) gpsStopGuard(tt *transit.Timetable, now time.Time) *guardAction {
	if !s.gps.fresh(now) {
		return nil
	}
	cfg := s.t.Cfg

	// next boarding not yet committed to
	bi := -1
	for i := s.legIdx; i < len(s.it.Legs); i++ {
		if s.it.Legs[i].Mode == "transit" {
			if i == s.legIdx && s.boarded {
				return nil // riding: nothing to guard
			}
			bi = i
			break
		}
	}
	if bi < 0 {
		return nil
	}
	leg := &s.it.Legs[bi]
	key := s.boardingKey(bi)
	dep := leg.Depart
	if lt, ok := s.lastEmit[bi]; ok && !lt.Depart.IsZero() {
		dep = lt.Depart // RT-adjusted
	}
	d := s.gps.distTo(leg.From.Lat, leg.From.Lon)
	buffer := cfg.Track.BoardBuffer

	if d <= s.gps.radius(nearStopBase) {
		s.gps.latchReachedStop(key)
		// Seed/refresh ordered shape progress while the rider is still at the
		// stop. That preserves a reliable origin even if several fixes arrive
		// between tracker evaluations just as the vehicle departs.
		if r, ok := resolveRide(tt, leg); ok {
			s.gpsShapeBoarding(tt, r, bi, now)
		}
		s.gps.tooSlowSince = time.Time{}
		s.gps.leftStopSince = time.Time{}
		return nil
	}

	if _, reached := s.gps.reachedStops[key]; reached {
		// The rider reached this stop and is now moving in order along the
		// planned ride's shape. Treat that as an in-progress boarding signal,
		// not as walking away; advance() promotes it to onboard after the
		// stronger distance/persistence threshold is met.
		if r, ok := resolveRide(tt, leg); ok {
			if following, _ := s.gpsShapeBoarding(tt, r, bi, now); following {
				s.gps.leftStopSince = time.Time{}
				return nil
			}
		}
		// AT_STOP, wandered off. Inside the wander radius: waiting, full stop.
		if d <= cfg.Track.StopWanderRadius+math.Min(s.gps.cur.AccuracyM, fixAccCap) {
			s.gps.leftStopSince = time.Time{}
			return nil
		}
		// Beyond it: background walk-back feasibility. Only when the rider
		// can NO LONGER make it back in time does anything happen.
		walkBack := time.Duration(d * walkDetour / s.gps.walkSpeed() * float64(time.Second))
		late := now.Add(walkBack).Add(buffer).After(dep)
		if hold(&s.gps.leftStopSince, late, now, guardHold) {
			s.gps.leftStopSince = time.Time{}
			return &guardAction{"left_stop",
				"you moved away from the stop and can no longer make the departure; recomputing from your position"}
		}
		return nil
	}

	// WALKING_TO_STOP: too-slow check against the RT-adjusted departure.
	eta := time.Duration(d * walkDetour / s.gps.walkSpeed() * float64(time.Second))
	late := now.Add(eta).Add(buffer).After(dep)
	if hold(&s.gps.tooSlowSince, late, now, guardHold) {
		s.gps.tooSlowSince = time.Time{}
		return &guardAction{"too_slow",
			"you can no longer reach the boarding stop in time; recomputing from your position"}
	}
	return nil
}

// emitWalkProgress streams "how far to go" while on foot or waiting, so the
// client can render live meters-to-stop. Re-emits on ≥walkDistStep changes.
// Skips entirely while boarded: no GPS chatter during the ride.
func (s *session) emitWalkProgress(tt *transit.Timetable, now time.Time) {
	if !s.gps.fresh(now) || s.legIdx >= len(s.it.Legs) {
		return
	}
	leg := &s.it.Legs[s.legIdx]
	status := "walking"
	tLat, tLon := leg.To.Lat, leg.To.Lon
	if leg.Mode == "transit" {
		if s.boarded {
			return
		}
		status = "waiting"
		tLat, tLon = leg.From.Lat, leg.From.Lon
	}
	d := s.gps.distTo(tLat, tLon)
	if s.gps.lastWalkDist >= 0 && math.Abs(d-s.gps.lastWalkDist) < walkDistStep {
		return
	}
	s.gps.lastWalkDist = d
	di := int(d)
	s.sink.Send(evProgress{Type: "progress", Status: status, LegIndex: s.legIdx,
		Boarded: false, DistM: &di})
}
