package engine

import (
	"fmt"
	"math"
	"time"

	"gotransit/internal/gbfs"
	"gotransit/internal/geo"
)

type sharedPlan struct {
	Assignment gbfs.Assignment
	EstimateS  uint32
}

func whenOrNow(t time.Time) time.Time {
	if t.IsZero() {
		return time.Now()
	}
	return t
}

// sharedAccessSet replaces imaginary door-to-stop bicycle reachability with
// an actual GBFS chain. For access=true point is the query origin and each
// stop is the end; for egress it is the destination and each stop is the
// beginning. The cheap pass uses conservative distance estimates. Only the
// few journeys returned by RAPTOR are then routed exactly in sharedLegs.
func (e *Engine) sharedAccessSet(gb *GraphBundle, tb *TTBundle, base accessSet,
	pointLat, pointLon int32, access bool, class gbfs.Class, at time.Time) accessSet {

	out := accessSet{sec: map[int32]uint32{}, anchor: map[int32]int32{}, mode: "shared", shared: map[int32]sharedPlan{}}
	if e.GBFS == nil || e.GBFS.Snapshot() == nil {
		return out
	}
	tt := tb.TT
	for stop := range base.sec {
		sLat, sLon := tt.StopLat[stop], tt.StopLon[stop]
		fromLat, fromLon, toLat, toLon := sLat, sLon, pointLat, pointLon
		if access {
			fromLat, fromLon, toLat, toLon = pointLat, pointLon, sLat, sLon
		}
		plan, ok := e.estimateShared(fromLat, fromLon, toLat, toLon, class, at)
		if !ok || time.Duration(plan.EstimateS)*time.Second > e.Cfg.Routing.MaxBikeAccess {
			continue
		}
		out.sec[stop] = plan.EstimateS
		out.shared[stop] = plan
	}
	return out
}

func (e *Engine) estimateShared(fLat, fLon, tLat, tLon int32, class gbfs.Class, at time.Time) (sharedPlan, bool) {
	snap := e.GBFS.Snapshot()
	r := e.Cfg.Routing
	walkMPS := r.WalkSpeedKmh / 3.6
	pickupRadius := walkMPS * r.SharedPickupWalk.Seconds()
	dropoffRadius := walkMPS * r.SharedDropoffWalk.Seconds()
	pickups := snap.Pickups(class, e7f(fLat), e7f(fLon), pickupRadius, at)
	if len(pickups) > r.SharedCandidates {
		pickups = pickups[:r.SharedCandidates]
	}
	best := math.Inf(1)
	var chosen gbfs.Assignment
	for _, pickup := range pickups {
		dropoffs := snap.Dropoffs(pickup, e7f(tLat), e7f(tLon), dropoffRadius, at)
		if len(dropoffs) > r.SharedCandidates {
			dropoffs = dropoffs[:r.SharedCandidates]
		}
		for _, a := range dropoffs {
			rideMPS := r.BikeSpeedKmh / 3.6
			if a.Class == gbfs.ClassScooter {
				rideMPS = r.ScooterSpeedKmh / 3.6
			}
			walkA := meters(e7f(fLat), e7f(fLon), a.PickupLat, a.PickupLon) * 1.15
			ride := meters(a.PickupLat, a.PickupLon, a.DropoffLat, a.DropoffLon) * 1.25
			walkB := meters(a.DropoffLat, a.DropoffLon, e7f(tLat), e7f(tLon)) * 1.15
			a.RequiredRangeM = ride
			if !snap.Check(a, at, r.BatteryReserveRatio, r.BatteryReserveM).Available ||
				!snap.PathAllowed(a.Feed, a.VehicleTypeID, [][2]float64{{a.PickupLat, a.PickupLon}, {a.DropoffLat, a.DropoffLon}}, at) {
				continue
			}
			seconds := walkA/walkMPS + ride/rideMPS + walkB/walkMPS +
				r.SharedUnlock.Seconds() + r.SharedPark.Seconds()
			if seconds < best {
				best, chosen = seconds, a
			}
		}
	}
	if math.IsInf(best, 1) {
		return sharedPlan{}, false
	}
	return sharedPlan{Assignment: chosen, EstimateS: uint32(math.Ceil(best))}, true
}

func (e *Engine) planSharedDirect(fLat, fLon, tLat, tLon int32, when time.Time, class gbfs.Class) (Itinerary, bool) {
	plan, ok := e.estimateShared(fLat, fLon, tLat, tLon, class, when)
	if !ok {
		return Itinerary{}, false
	}
	legs, ok := e.sharedLegs(plan, fLat, fLon, tLat, tLon, when)
	if !ok || len(legs) == 0 {
		return Itinerary{}, false
	}
	it := Itinerary{Legs: legs, Depart: legs[0].Depart, Arrive: legs[len(legs)-1].Arrive,
		sig: "shared-direct|" + rentalSignature(plan.Assignment)}
	it.DurationS = int(it.Arrive.Sub(it.Depart).Seconds())
	if it.DurationS > 45*60 {
		return Itinerary{}, false
	}
	return it, true
}

// sharedLegs routes and revalidates the actual walk → rental → walk chain.
// GBFS may update between the estimate and here; the final Check deliberately
// fails the candidate closed in that case.
func (e *Engine) sharedLegs(plan sharedPlan, fLat, fLon, tLat, tLon int32, start time.Time) ([]Leg, bool) {
	a := plan.Assignment
	var legs []Leg
	cursor := start
	appendRoad := func(mode string, aLat, aLon, bLat, bLon int32) (Leg, bool) {
		if geo.Dist(aLat, aLon, bLat, bLon) < 2 {
			return Leg{}, true
		}
		it, err := e.planRoad(mode, aLat, aLon, bLat, bLon, cursor, false)
		if err != nil || len(it.Legs) == 0 {
			return Leg{}, false
		}
		return it.Legs[0], true
	}

	pLat, pLon := i7(a.PickupLat), i7(a.PickupLon)
	dLat, dLon := i7(a.DropoffLat), i7(a.DropoffLon)
	if leg, ok := appendRoad("walk", fLat, fLon, pLat, pLon); !ok {
		return nil, false
	} else if leg.Mode != "" {
		if time.Duration(leg.DurationS)*time.Second > e.Cfg.Routing.SharedPickupWalk {
			return nil, false
		}
		legs = append(legs, leg)
		cursor = leg.Arrive
	}

	mode := "bike"
	if a.Class == gbfs.ClassScooter {
		mode = "scooter"
	}
	ride, ok := appendRoad(mode, pLat, pLon, dLat, dLon)
	if !ok || ride.Mode == "" {
		return nil, false
	}
	a.RequiredRangeM = float64(ride.DistanceM)
	snap := e.GBFS.Snapshot()
	check := snap.Check(a, start, e.Cfg.Routing.BatteryReserveRatio, e.Cfg.Routing.BatteryReserveM)
	if !check.Available {
		return nil, false
	}
	a.CurrentRangeM, a.BatteryPercent = check.CurrentRangeM, check.BatteryPercent
	points := decodePolylinePoints(ride.Polyline)
	if !snap.PathAllowed(a.Feed, a.VehicleTypeID, points, start) {
		return nil, false
	}
	rideKPH := e.Cfg.Routing.BikeSpeedKmh
	if a.Class == gbfs.ClassScooter {
		rideKPH = e.Cfg.Routing.ScooterSpeedKmh
	}
	rideSeconds := geofencedRideSeconds(snap, a, points, start, rideKPH)
	if rideSeconds <= 0 {
		rideSeconds = float64(ride.DurationS)
	}
	overhead := e.Cfg.Routing.SharedUnlock + e.Cfg.Routing.SharedPark
	ride.Depart = cursor
	ride.DurationS = int(math.Ceil(rideSeconds + overhead.Seconds()))
	ride.Arrive = cursor.Add(time.Duration(ride.DurationS) * time.Second)
	ride.Rental = &a
	ride.From = Place{Name: pickupName(a), Lat: a.PickupLat, Lon: a.PickupLon}
	ride.To = Place{Name: dropoffName(a), Lat: a.DropoffLat, Lon: a.DropoffLon}
	legs = append(legs, ride)
	cursor = ride.Arrive

	if leg, ok := appendRoad("walk", dLat, dLon, tLat, tLon); !ok {
		return nil, false
	} else if leg.Mode != "" {
		if time.Duration(leg.DurationS)*time.Second > e.Cfg.Routing.SharedDropoffWalk {
			return nil, false
		}
		leg.Depart = cursor
		leg.Arrive = cursor.Add(time.Duration(leg.DurationS) * time.Second)
		legs = append(legs, leg)
	}
	return legs, true
}

func pickupName(a gbfs.Assignment) string {
	if a.PickupName != "" {
		return a.PickupName
	}
	if a.FormFactor != "" {
		return "Shared " + a.FormFactor
	}
	return "Shared vehicle"
}

func dropoffName(a gbfs.Assignment) string {
	if a.DropoffName != "" {
		return a.DropoffName
	}
	return "Shared vehicle return"
}

func rentalSignature(a gbfs.Assignment) string {
	return fmt.Sprintf("%s|%s|%s|%s|%s", a.Feed, a.VehicleID, a.VehicleTypeID, a.PickupStationID, a.DropoffStationID)
}

func meters(aLat, aLon, bLat, bLon float64) float64 {
	return geo.Dist(i7(aLat), i7(aLon), i7(bLat), i7(bLon))
}

func i7(v float64) int32 { return int32(math.Round(v * 1e7)) }

// decodePolylinePoints decodes the standard Google encoded polyline emitted
// by internal/geo. Sampling every point lets GBFS no-ride-through geofences
// reject an otherwise valid-looking pickup/dropoff pair.
func decodePolylinePoints(encoded string) [][2]float64 {
	lats, lons := geo.DecodePolyline(encoded)
	out := make([][2]float64, len(lats))
	for i := range lats {
		out[i] = [2]float64{e7f(lats[i]), e7f(lons[i])}
	}
	return out
}

func geofencedRideSeconds(snap *gbfs.Snapshot, a gbfs.Assignment, points [][2]float64, at time.Time, defaultKPH float64) float64 {
	seconds := 0.0
	for i := 1; i < len(points); i++ {
		p, q := points[i-1], points[i]
		kph := defaultKPH
		midLat, midLon := (p[0]+q[0])/2, (p[1]+q[1])/2
		if limit := snap.MaximumSpeedKPH(a.Feed, a.VehicleTypeID, midLat, midLon, at); limit > 0 && limit < kph {
			kph = limit
		}
		if kph > 0 {
			seconds += meters(p[0], p[1], q[0], q[1]) / (kph / 3.6)
		}
	}
	return seconds
}
