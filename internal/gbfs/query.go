package gbfs

import (
	"math"
	"sort"
	"time"
)

// Assignment is copied into an itinerary leg. It deliberately contains all
// information a client needs to identify/rent the resource and all stable
// keys the tracker needs to revalidate it against later snapshots.
type Assignment struct {
	Feed       string `json:"feed"`
	SystemName string `json:"system_name,omitempty"`

	VehicleID     string `json:"vehicle_id,omitempty"`
	VehicleTypeID string `json:"vehicle_type_id"`
	FormFactor    string `json:"form_factor"`
	Propulsion    string `json:"propulsion_type"`
	Class         Class  `json:"class"`

	PickupStationID string  `json:"pickup_station_id,omitempty"`
	HomeStationID   string  `json:"home_station_id,omitempty"`
	PickupName      string  `json:"pickup_name,omitempty"`
	PickupLat       float64 `json:"pickup_lat"`
	PickupLon       float64 `json:"pickup_lon"`

	DropoffStationID string  `json:"dropoff_station_id,omitempty"`
	DropoffName      string  `json:"dropoff_name,omitempty"`
	DropoffLat       float64 `json:"dropoff_lat"`
	DropoffLon       float64 `json:"dropoff_lon"`

	ReturnConstraint string     `json:"return_constraint"`
	CurrentRangeM    float64    `json:"current_range_m,omitempty"`
	RangeKnown       bool       `json:"range_known"`
	BatteryPercent   float64    `json:"battery_percent,omitempty"`
	BatteryKnown     bool       `json:"battery_known"`
	RequiredRangeM   float64    `json:"required_range_m,omitempty"`
	RentalURIs       RentalURIs `json:"rental_uris,omitempty"`
}

func (a Assignment) Motorized() bool { return a.Propulsion != "" && a.Propulsion != "human" }

// Pickups returns fresh, rentable resources near a point ordered by walking
// distance. Station pools are returned once per available vehicle type;
// free-floating vehicles retain their individual IDs and battery/range.
func (s *Snapshot) Pickups(class Class, lat, lon, radiusM float64, now time.Time) []Assignment {
	if s == nil {
		return nil
	}
	// Availability is an observation made now even when the requested rental
	// starts after a transit leg. Geofence schedules, on the other hand, must
	// still be evaluated at the planned rental time in `now`.
	observedAt := availabilityTime(now)
	type ranked struct {
		a Assignment
		d float64
	}
	var out []ranked
	for _, f := range s.Feeds {
		if !f.Fresh(observedAt) {
			continue
		}
		stationExact := map[string]map[string]bool{}
		for _, v := range f.Vehicles {
			if endpointStale(f.VehicleUpdatedAt, f, observedAt) {
				continue
			}
			typ, ok := f.VehicleTypes[v.TypeID]
			if !ok {
				typ = f.VehicleTypes[defaultTypeID]
			}
			if !classMatches(class, typ) || v.Reserved || v.Disabled || entityStale(v.LastReport, f, observedAt) {
				continue
			}
			vlat, vlon := v.Lat, v.Lon
			var st Station
			if v.StationID != "" {
				st = f.Stations[v.StationID]
				if endpointStale(f.StationUpdatedAt, f, observedAt) || !st.Installed || !st.Renting ||
					entityStale(st.LastReport, f, observedAt) {
					continue
				}
				if !validCoord(vlat, vlon) {
					vlat, vlon = st.Lat, st.Lon
				}
				if stationExact[v.StationID] == nil {
					stationExact[v.StationID] = map[string]bool{}
				}
				stationExact[v.StationID][v.TypeID] = true
			}
			if !validCoord(vlat, vlon) {
				continue
			}
			d := distanceM(lat, lon, vlat, vlon)
			if d > radiusM || !f.pointAllows(v.TypeID, vlat, vlon, now, ruleStart) {
				continue
			}
			// A motorized vehicle without current_range_meters (or a usable
			// battery percentage + max_range_meters fallback) cannot be
			// promised for a trip safely.
			if typ.Motorized() && !v.RangeKnown {
				continue
			}
			uris := v.RentalURIs
			if emptyURIs(uris) && v.StationID != "" {
				uris = st.RentalURIs
			}
			out = append(out, ranked{a: Assignment{Feed: f.Name, SystemName: f.SystemName,
				VehicleID: v.ID, VehicleTypeID: typ.ID, FormFactor: typ.FormFactor,
				Propulsion: typ.Propulsion, Class: typ.Class(), PickupStationID: v.StationID,
				HomeStationID: v.HomeStationID,
				PickupName:    st.Name, PickupLat: vlat, PickupLon: vlon,
				ReturnConstraint: inferConstraint(typ.ReturnConstraint, v.StationID),
				CurrentRangeM:    v.RangeM, RangeKnown: v.RangeKnown,
				BatteryPercent: v.BatteryPercent, BatteryKnown: v.BatteryKnown, RentalURIs: uris}, d: d})
		}

		for _, st := range f.Stations {
			if endpointStale(f.StationUpdatedAt, f, observedAt) {
				continue
			}
			if !st.Installed || !st.Renting || st.Vehicles <= 0 || entityStale(st.LastReport, f, observedAt) ||
				distanceM(lat, lon, st.Lat, st.Lon) > radiusM {
				continue
			}
			counts := st.VehicleCounts
			if len(counts) == 0 {
				counts = inferredCounts(f, st.Vehicles)
			}
			for typeID, count := range counts {
				if count <= 0 || stationExact[st.ID][typeID] ||
					!f.pointAllows(typeID, st.Lat, st.Lon, now, ruleStart) {
					continue
				}
				typ, ok := f.VehicleTypes[typeID]
				if !ok || !classMatches(class, typ) {
					continue
				}
				// Aggregate station counts carry no per-vehicle battery. Human
				// vehicles are safe; motorized ones require vehicle_status.
				if typ.Motorized() {
					continue
				}
				d := distanceM(lat, lon, st.Lat, st.Lon)
				out = append(out, ranked{a: Assignment{Feed: f.Name, SystemName: f.SystemName,
					VehicleTypeID: typ.ID, FormFactor: typ.FormFactor, Propulsion: typ.Propulsion,
					Class: typ.Class(), PickupStationID: st.ID, PickupName: st.Name,
					PickupLat: st.Lat, PickupLon: st.Lon,
					ReturnConstraint: inferConstraint(typ.ReturnConstraint, st.ID), RentalURIs: st.RentalURIs}, d: d})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].d < out[j].d })
	res := make([]Assignment, len(out))
	for i := range out {
		res[i] = out[i].a
	}
	return res
}

// Dropoffs expands a pickup into legal return points near the requested end.
// A free-floating result has an empty DropoffStationID and uses the exact end
// coordinate; station-constrained results require a currently accepting dock.
func (s *Snapshot) Dropoffs(a Assignment, lat, lon, radiusM float64, now time.Time) []Assignment {
	if s == nil {
		return nil
	}
	f := s.Feeds[a.Feed]
	observedAt := availabilityTime(now)
	if f == nil || !f.Fresh(observedAt) {
		return nil
	}
	constraint := a.ReturnConstraint
	var out []Assignment
	stationRequired := f.stationOnly(a.VehicleTypeID, lat, lon, now)
	if constraint == "free_floating" || constraint == "hybrid" {
		if f.pointAllows(a.VehicleTypeID, lat, lon, now, ruleEnd) && !stationRequired {
			cp := a
			cp.DropoffLat, cp.DropoffLon = lat, lon
			out = append(out, cp)
		}
	}
	if constraint == "free_floating" && !stationRequired {
		return out
	}
	if endpointStale(f.StationUpdatedAt, f, observedAt) {
		return out
	}
	for _, st := range f.Stations {
		home := a.HomeStationID
		if home == "" {
			home = a.PickupStationID
		}
		if constraint == "roundtrip_station" && st.ID != home {
			continue
		}
		if !stationAccepts(st, a.VehicleTypeID, f, observedAt) ||
			!f.pointAllows(a.VehicleTypeID, st.Lat, st.Lon, now, ruleEnd) ||
			distanceM(lat, lon, st.Lat, st.Lon) > radiusM {
			continue
		}
		cp := a
		cp.DropoffStationID, cp.DropoffName = st.ID, st.Name
		cp.DropoffLat, cp.DropoffLon = st.Lat, st.Lon
		out = append(out, cp)
	}
	sort.Slice(out, func(i, j int) bool {
		return distanceM(lat, lon, out[i].DropoffLat, out[i].DropoffLon) <
			distanceM(lat, lon, out[j].DropoffLat, out[j].DropoffLon)
	})
	return out
}

type CheckResult struct {
	Available      bool
	Reason         string
	CurrentRangeM  float64
	BatteryPercent float64
}

// Check revalidates the exact assignment captured in a leg. reserveRatio and
// reserveM are applied to the network distance stamped by the planner.
func (s *Snapshot) Check(a Assignment, now time.Time, reserveRatio, reserveM float64) CheckResult {
	if s == nil {
		return CheckResult{Reason: "no_gbfs_snapshot"}
	}
	f := s.Feeds[a.Feed]
	observedAt := availabilityTime(now)
	if f == nil || !f.Fresh(observedAt) {
		return CheckResult{Reason: "gbfs_feed_stale"}
	}
	typ, ok := f.VehicleTypes[a.VehicleTypeID]
	if !ok {
		return CheckResult{Reason: "vehicle_type_removed"}
	}
	result := CheckResult{CurrentRangeM: a.CurrentRangeM, BatteryPercent: a.BatteryPercent}
	if a.VehicleID != "" {
		if endpointStale(f.VehicleUpdatedAt, f, observedAt) {
			return CheckResult{Reason: "gbfs_feed_stale"}
		}
		v, ok := f.Vehicles[a.VehicleID]
		if !ok {
			return CheckResult{Reason: "vehicle_unavailable"}
		}
		if v.TypeID != "" && v.TypeID != a.VehicleTypeID {
			return CheckResult{Reason: "vehicle_type_changed"}
		}
		if v.Reserved {
			return CheckResult{Reason: "vehicle_reserved"}
		}
		if v.Disabled || entityStale(v.LastReport, f, observedAt) {
			return CheckResult{Reason: "vehicle_disabled_or_stale"}
		}
		if a.PickupStationID != "" {
			if endpointStale(f.StationUpdatedAt, f, observedAt) {
				return CheckResult{Reason: "gbfs_feed_stale"}
			}
			st, stationOK := f.Stations[a.PickupStationID]
			if !stationOK || v.StationID != a.PickupStationID || !st.Installed || !st.Renting ||
				entityStale(st.LastReport, f, observedAt) {
				return CheckResult{Reason: "pickup_station_unavailable"}
			}
		}
		vlat, vlon := v.Lat, v.Lon
		if v.StationID != "" && !validCoord(vlat, vlon) {
			if st, ok := f.Stations[v.StationID]; ok {
				vlat, vlon = st.Lat, st.Lon
			}
		}
		if validCoord(vlat, vlon) && distanceM(vlat, vlon, a.PickupLat, a.PickupLon) > 75 {
			return CheckResult{Reason: "vehicle_moved"}
		}
		result.CurrentRangeM, result.BatteryPercent = v.RangeM, v.BatteryPercent
		if typ.Motorized() {
			if !v.RangeKnown {
				return CheckResult{Reason: "battery_range_unknown"}
			}
			required := a.RequiredRangeM*(1+reserveRatio) + reserveM
			if v.RangeM < required {
				return CheckResult{Reason: "battery_range_insufficient", CurrentRangeM: v.RangeM, BatteryPercent: v.BatteryPercent}
			}
		}
	} else {
		if endpointStale(f.StationUpdatedAt, f, observedAt) {
			return CheckResult{Reason: "gbfs_feed_stale"}
		}
		st, ok := f.Stations[a.PickupStationID]
		if !ok || !st.Installed || !st.Renting || entityStale(st.LastReport, f, observedAt) || stationCount(st, a.VehicleTypeID, f) <= 0 {
			return CheckResult{Reason: "pickup_station_empty"}
		}
	}
	if !f.pointAllows(a.VehicleTypeID, a.PickupLat, a.PickupLon, now, ruleStart) {
		return CheckResult{Reason: "pickup_no_longer_allowed", CurrentRangeM: result.CurrentRangeM, BatteryPercent: result.BatteryPercent}
	}
	if a.DropoffStationID != "" {
		if endpointStale(f.StationUpdatedAt, f, observedAt) {
			return CheckResult{Reason: "gbfs_feed_stale", CurrentRangeM: result.CurrentRangeM, BatteryPercent: result.BatteryPercent}
		}
		st, ok := f.Stations[a.DropoffStationID]
		if !ok || !stationAccepts(st, a.VehicleTypeID, f, observedAt) ||
			!f.pointAllows(a.VehicleTypeID, st.Lat, st.Lon, now, ruleEnd) {
			return CheckResult{Reason: "dropoff_station_unavailable", CurrentRangeM: result.CurrentRangeM, BatteryPercent: result.BatteryPercent}
		}
	} else if !f.pointAllows(a.VehicleTypeID, a.DropoffLat, a.DropoffLon, now, ruleEnd) ||
		f.stationOnly(a.VehicleTypeID, a.DropoffLat, a.DropoffLon, now) {
		return CheckResult{Reason: "dropoff_no_longer_allowed", CurrentRangeM: result.CurrentRangeM, BatteryPercent: result.BatteryPercent}
	}
	result.Available = true
	return result
}

type ruleField int

const (
	ruleStart ruleField = iota
	ruleEnd
	ruleThrough
)

func (f *Feed) pointAllows(typeID string, lat, lon float64, now time.Time, field ruleField) bool {
	for _, z := range f.Zones {
		if (!z.Start.IsZero() && now.Before(z.Start)) || (!z.End.IsZero() && now.After(z.End)) || !pointInZone(z, lat, lon) {
			continue
		}
		if v, ok := ruleBool(z.Rules, typeID, field); ok {
			return v // first polygon defining this restriction wins
		}
	}
	if v, ok := ruleBool(f.GlobalRules, typeID, field); ok {
		return v
	}
	return true
}

func ruleBool(rules []Rule, typeID string, field ruleField) (bool, bool) {
	for _, r := range rules {
		if !ruleMatches(r, typeID) {
			continue
		}
		var v *bool
		switch field {
		case ruleStart:
			v = r.StartAllowed
		case ruleEnd:
			v = r.EndAllowed
		case ruleThrough:
			v = r.ThroughAllowed
		}
		if v != nil {
			return *v, true
		}
	}
	return false, false
}

func (f *Feed) stationOnly(typeID string, lat, lon float64, now time.Time) bool {
	for _, z := range f.Zones {
		if (!z.Start.IsZero() && now.Before(z.Start)) || (!z.End.IsZero() && now.After(z.End)) || !pointInZone(z, lat, lon) {
			continue
		}
		if v, ok := stationOnlyRule(z.Rules, typeID); ok {
			return v
		}
	}
	v, _ := stationOnlyRule(f.GlobalRules, typeID)
	return v
}

func stationOnlyRule(rules []Rule, typeID string) (bool, bool) {
	for _, r := range rules {
		if ruleMatches(r, typeID) && r.StationParking != nil {
			return *r.StationParking, true
		}
	}
	return false, false
}

// PathAllowed checks every supplied [lat,lon] point against ride-through
// rules. The engine passes the actual routed street geometry, not a beeline.
func (s *Snapshot) PathAllowed(feed, typeID string, points [][2]float64, now time.Time) bool {
	if s == nil || s.Feeds[feed] == nil {
		return false
	}
	f := s.Feeds[feed]
	for i, p := range points {
		if !f.pointAllows(typeID, p[0], p[1], now, ruleThrough) {
			return false
		}
		if i == 0 {
			continue
		}
		prev := points[i-1]
		steps := int(math.Ceil(distanceM(prev[0], prev[1], p[0], p[1]) / 25))
		for k := 1; k < steps; k++ {
			frac := float64(k) / float64(steps)
			lat, lon := prev[0]+(p[0]-prev[0])*frac, prev[1]+(p[1]-prev[1])*frac
			if !f.pointAllows(typeID, lat, lon, now, ruleThrough) {
				return false
			}
		}
	}
	return true
}

// MaximumSpeedKPH resolves the GBFS geofence speed rule at a point. Zero
// means no operator-specified cap.
func (s *Snapshot) MaximumSpeedKPH(feed, typeID string, lat, lon float64, now time.Time) float64 {
	if s == nil || s.Feeds[feed] == nil {
		return 0
	}
	f := s.Feeds[feed]
	for _, z := range f.Zones {
		if (!z.Start.IsZero() && now.Before(z.Start)) || (!z.End.IsZero() && now.After(z.End)) || !pointInZone(z, lat, lon) {
			continue
		}
		if v, ok := maximumSpeedRule(z.Rules, typeID); ok {
			return v
		}
	}
	v, _ := maximumSpeedRule(f.GlobalRules, typeID)
	return v
}

func maximumSpeedRule(rules []Rule, typeID string) (float64, bool) {
	for _, r := range rules {
		if ruleMatches(r, typeID) && r.MaximumSpeedKPH > 0 {
			return r.MaximumSpeedKPH, true
		}
	}
	return 0, false
}

func classMatches(want Class, typ VehicleType) bool {
	return want == ClassAny || want == "" || typ.Class() == want
}

func inferredCounts(f *Feed, total int) map[string]int {
	if len(f.VehicleTypes) == 1 {
		for id := range f.VehicleTypes {
			return map[string]int{id: total}
		}
	}
	return nil // multiple types but no breakdown: never guess the vehicle
}

func stationCount(st Station, typeID string, f *Feed) int {
	if len(st.VehicleCounts) > 0 {
		return st.VehicleCounts[typeID]
	}
	return inferredCounts(f, st.Vehicles)[typeID]
}

func stationAccepts(st Station, typeID string, f *Feed, now time.Time) bool {
	if !st.Installed || !st.Returning || entityStale(st.LastReport, f, now) {
		return false
	}
	if len(st.DockCounts) > 0 {
		return st.DockCounts[typeID] > 0
	}
	return !st.DocksKnown || st.Docks > 0
}

func entityStale(t time.Time, f *Feed, now time.Time) bool {
	return !t.IsZero() && f.MaxAge > 0 && now.Sub(t) > f.MaxAge
}

func endpointStale(t time.Time, f *Feed, now time.Time) bool {
	if t.IsZero() {
		return f.MaxAge > 0 && now.Sub(f.FetchedAt) > f.MaxAge
	}
	return f.MaxAge > 0 && now.Sub(t) > f.MaxAge
}

// availabilityTime prevents a future pickup time (for example, after a
// 40-minute bus ride) from aging a current GBFS observation by 40 minutes.
// The tracker will continue to compare successive snapshots against real
// wall time until the rental starts.
func availabilityTime(planned time.Time) time.Time {
	now := time.Now()
	if planned.After(now) {
		return now
	}
	return planned
}

func inferConstraint(value, stationID string) string {
	if value != "" {
		return value
	}
	if stationID != "" {
		return "any_station"
	}
	return "free_floating"
}

func ruleMatches(r Rule, typeID string) bool {
	if len(r.VehicleTypeIDs) == 0 {
		return true
	}
	for _, id := range r.VehicleTypeIDs {
		if id == typeID {
			return true
		}
	}
	return false
}

func emptyURIs(u RentalURIs) bool { return u.Android == "" && u.IOS == "" && u.Web == "" }

func distanceM(lat1, lon1, lat2, lon2 float64) float64 {
	const radius = 6371000.0
	p1, p2 := lat1*math.Pi/180, lat2*math.Pi/180
	dp, dl := (lat2-lat1)*math.Pi/180, (lon2-lon1)*math.Pi/180
	a := math.Sin(dp/2)*math.Sin(dp/2) + math.Cos(p1)*math.Cos(p2)*math.Sin(dl/2)*math.Sin(dl/2)
	return radius * 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
}
