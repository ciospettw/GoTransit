package track

// GPS fusion. The client MAY stream position fixes over /v1/track; they are
// treated as EVIDENCE, never as truth. Every inference here tolerates the
// whole error budget of the real world — GTFS-RT propagation lag, vehicle
// GPS error, user GPS error, fixes arriving late or not at all — by
// requiring persistence (a condition must hold across consecutive fixes for
// a minimum duration) and corroboration (user evidence + vehicle evidence
// must agree) before anything user-visible happens. A deviation is only
// declared when it is effectively certain; in doubt, the GPS-free virtual
// rider keeps governing and nothing changes.

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
	confirmOff   = 35 * time.Second // separation persistence → left the vehicle
	atStopHold   = 10 * time.Second // persistence to advance a walk leg early
	missedGrace  = 45 * time.Second // passed-your-stop + still co-located persistence
	offRouteBase = 300.0            // far from every plan anchor
	offRouteDur  = 90 * time.Second // ...for this long → off route
	walkDistStep = 30.0             // walk-progress re-emit threshold (meters)
)

type gpsState struct {
	cur Fix
	has bool

	// persistence anchors: zero time = condition not currently holding
	nearStopSince time.Time
	withVehSince  time.Time
	awayVehSince  time.Time
	missedSince   time.Time
	offRouteSince time.Time

	lastWalkDist float64 // last emitted distance_to_stop_m (-1 = none)
	deviated     map[string]bool
}

func newGPSState() gpsState { return gpsState{lastWalkDist: -1, deviated: map[string]bool{}} }

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
	g.cur = f
	g.has = true
}

// fresh reports whether current evidence is usable at all.
func (g *gpsState) fresh(now time.Time) bool {
	return g.has && now.Sub(g.cur.At) <= fixStale
}

// radius inflates a base threshold with the fix's own uncertainty.
func (g *gpsState) radius(base float64) float64 {
	return base + math.Min(g.cur.AccuracyM, fixAccCap)
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

// resetLegAnchors clears the persistence anchors when the current leg (or the
// whole plan) changes: evidence never carries over across legs.
func (g *gpsState) resetLegAnchors() {
	g.nearStopSince = time.Time{}
	g.withVehSince = time.Time{}
	g.awayVehSince = time.Time{}
	g.missedSince = time.Time{}
	g.offRouteSince = time.Time{}
	g.lastWalkDist = -1
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
// plan. Every branch demands corroboration + persistence: the GTFS-RT may lag,
// both GPS traces may err, fixes may arrive late — in doubt, no deviation.
func (s *session) gpsDeviation(tt *transit.Timetable, o *transit.RTOverlay, now time.Time) *deviation {
	if !s.gps.fresh(now) || s.legIdx >= len(s.it.Legs) {
		return nil
	}
	leg := &s.it.Legs[s.legIdx]

	if leg.Mode == "transit" && s.boarded {
		r, ok := resolveRide(tt, leg)
		if !ok {
			return nil
		}
		passed := o.TripPassed(r.trip)
		co := s.gpsWithVehicle(tt, o, r, now, 0)

		// missed_alight: the feed CONFIRMS the vehicle cleared the alight stop
		// while the rider is still measurably on board. Certainty = the bus is
		// strictly beyond the stop, or at it for missedGrace with the rider
		// co-located throughout.
		key := fmt.Sprintf("missed_alight/%d", s.legIdx)
		beyond := passed > int16(r.alight)
		heldAt := hold(&s.gps.missedSince, co && passed >= int16(r.alight), now, missedGrace)
		if co && (beyond || heldAt) && !s.gps.deviated[key] {
			s.gps.deviated[key] = true
			return &deviation{evDeviation{
				Type: "deviation", Kind: "missed_alight", LegIndex: s.legIdx,
				ExpectedStop: &place{StopID: leg.To.StopID, Name: leg.To.Name},
				Message:      "you did not get off at your stop; recomputing from the vehicle's next stops",
			}}
		}

		// left_vehicle_early: sustained clear separation from a vehicle that
		// has NOT yet reached the alight stop, and the rider is not simply
		// standing at the planned stop.
		key = fmt.Sprintf("left_early/%d", s.legIdx)
		if vLat, vLon, vok := s.vehiclePos(tt, o, r); vok && passed >= 0 && passed < int16(r.alight) {
			sep := s.gps.distTo(vLat, vLon) >= s.gps.radius(offVehBase)+vehGPSErr
			if hold(&s.gps.awayVehSince, sep, now, confirmOff) &&
				s.gps.distTo(leg.To.Lat, leg.To.Lon) > s.gps.radius(nearStopBase) &&
				!s.gps.deviated[key] {
				s.gps.deviated[key] = true
				s.boarded = false // they are on foot: replans anchor to their fix
				return &deviation{evDeviation{
					Type: "deviation", Kind: "left_vehicle_early", LegIndex: s.legIdx,
					ExpectedStop: &place{StopID: leg.To.StopID, Name: leg.To.Name},
					Message:      "you left the vehicle before your stop; recomputing from your position",
				}}
			}
		}
		return nil
	}

	// off_route: while on foot (or waiting), sustained distance from EVERY
	// plan anchor — leg endpoints, expected position, final destination.
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

// emitWalkProgress streams "how far to go" while on foot or waiting, so the
// client can render live meters-to-stop. Re-emits on ≥walkDistStep changes.
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
