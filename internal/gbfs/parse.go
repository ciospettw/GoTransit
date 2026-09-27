package gbfs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

type document struct {
	LastUpdated json.RawMessage `json:"last_updated"`
	TTL         int64           `json:"ttl"`
	Version     string          `json:"version"`
	Data        json.RawMessage `json:"data"`
}

type feedLink struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

func parseDiscovery(b []byte, language string) (string, []feedLink, error) {
	var doc document
	if err := json.Unmarshal(b, &doc); err != nil {
		return "", nil, fmt.Errorf("decode gbfs.json: %w", err)
	}
	var direct struct {
		Feeds []feedLink `json:"feeds"`
	}
	if json.Unmarshal(doc.Data, &direct) == nil && len(direct.Feeds) > 0 {
		return doc.Version, direct.Feeds, nil
	}
	// GBFS v1.x nests feed links under a language key.
	var localized map[string]json.RawMessage
	if err := json.Unmarshal(doc.Data, &localized); err != nil {
		return "", nil, fmt.Errorf("decode gbfs.json data: %w", err)
	}
	keys := []string{language, "en"}
	for k := range localized {
		keys = append(keys, k)
	}
	seen := map[string]bool{}
	for _, k := range keys {
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		if raw, ok := localized[k]; ok && json.Unmarshal(raw, &direct) == nil && len(direct.Feeds) > 0 {
			return doc.Version, direct.Feeds, nil
		}
	}
	return "", nil, fmt.Errorf("gbfs.json contains no feeds")
}

type rawURIs struct {
	Android string `json:"android"`
	IOS     string `json:"ios"`
	Web     string `json:"web"`
}

func (u rawURIs) value() RentalURIs { return RentalURIs{Android: u.Android, IOS: u.IOS, Web: u.Web} }

type rawVehicleType struct {
	ID               string          `json:"vehicle_type_id"`
	Name             json.RawMessage `json:"name"`
	FormFactor       string          `json:"form_factor"`
	Propulsion       string          `json:"propulsion_type"`
	MaxRangeM        float64         `json:"max_range_meters"`
	ReturnConstraint string          `json:"return_constraint"`
}

type rawStationInfo struct {
	ID         string          `json:"station_id"`
	Name       json.RawMessage `json:"name"`
	Lat        float64         `json:"lat"`
	Lon        float64         `json:"lon"`
	Capacity   int             `json:"capacity"`
	RentalURIs rawURIs         `json:"rental_uris"`
}

type rawCount struct {
	TypeID string `json:"vehicle_type_id"`
	Count  int    `json:"count"`
}

type rawDockCount struct {
	TypeIDs []string `json:"vehicle_type_ids"`
	Count   int      `json:"count"`
}

type rawStationStatus struct {
	ID           string          `json:"station_id"`
	Installed    json.RawMessage `json:"is_installed"`
	Renting      json.RawMessage `json:"is_renting"`
	Returning    json.RawMessage `json:"is_returning"`
	NumVehicles  *int            `json:"num_vehicles_available"`
	NumBikes     *int            `json:"num_bikes_available"`
	NumDocks     *int            `json:"num_docks_available"`
	VehicleTypes []rawCount      `json:"vehicle_types_available"`
	VehicleDocks []rawDockCount  `json:"vehicle_docks_available"`
	LastReported json.RawMessage `json:"last_reported"`
}

type rawVehicle struct {
	VehicleID         string          `json:"vehicle_id"`
	BikeID            string          `json:"bike_id"`
	TypeID            string          `json:"vehicle_type_id"`
	StationID         string          `json:"station_id"`
	HomeStationID     string          `json:"home_station_id"`
	HomeStationLegacy string          `json:"home_station"`
	Lat               float64         `json:"lat"`
	Lon               float64         `json:"lon"`
	Reserved          json.RawMessage `json:"is_reserved"`
	Disabled          json.RawMessage `json:"is_disabled"`
	RangeM            *float64        `json:"current_range_meters"`
	FuelPercent       *float64        `json:"current_fuel_percent"`
	BatteryPercent    *float64        `json:"current_battery_percent"`
	BatteryPercentage *float64        `json:"battery_percentage"`
	LastReported      json.RawMessage `json:"last_reported"`
	RentalURIs        rawURIs         `json:"rental_uris"`
}

func parseFeed(name, version string, docs map[string][]byte, fetched time.Time, maxAge time.Duration) (*Feed, error) {
	f := &Feed{Name: name, Version: version, FetchedAt: fetched, MaxAge: maxAge,
		Stations: map[string]Station{}, Vehicles: map[string]Vehicle{}, VehicleTypes: map[string]VehicleType{}}
	var newest time.Time
	var ttl int64
	read := func(key string, dst any) error {
		b := docs[key]
		if len(b) == 0 {
			return nil
		}
		var doc document
		if err := json.Unmarshal(b, &doc); err != nil {
			return fmt.Errorf("decode %s: %w", key, err)
		}
		updated := parseTimestamp(doc.LastUpdated)
		if updated.After(newest) {
			newest = updated
		}
		switch key {
		case "station_status":
			f.StationUpdatedAt = updated
		case "vehicle_status", "free_bike_status":
			f.VehicleUpdatedAt = updated
		}
		if doc.TTL > 0 && (ttl == 0 || doc.TTL < ttl) {
			ttl = doc.TTL
		}
		if doc.Version != "" && f.Version == "" {
			f.Version = doc.Version
		}
		if dst == nil {
			return nil
		}
		return json.Unmarshal(doc.Data, dst)
	}

	var sys struct {
		Name json.RawMessage `json:"name"`
	}
	if err := read("system_information", &sys); err != nil {
		return nil, err
	}
	f.SystemName = localizedString(sys.Name)

	var types struct {
		VehicleTypes []rawVehicleType `json:"vehicle_types"`
	}
	if err := read("vehicle_types", &types); err != nil {
		return nil, err
	}
	for _, t := range types.VehicleTypes {
		if t.ID == "" {
			continue
		}
		f.VehicleTypes[t.ID] = VehicleType{ID: t.ID, Name: localizedString(t.Name),
			FormFactor: t.FormFactor, Propulsion: t.Propulsion, MaxRangeM: t.MaxRangeM,
			ReturnConstraint: t.ReturnConstraint}
	}
	if len(f.VehicleTypes) == 0 {
		f.VehicleTypes[defaultTypeID] = VehicleType{ID: defaultTypeID, FormFactor: "bicycle", Propulsion: "human"}
	}

	var info struct {
		Stations []rawStationInfo `json:"stations"`
	}
	if err := read("station_information", &info); err != nil {
		return nil, err
	}
	for _, st := range info.Stations {
		if st.ID == "" || !validCoord(st.Lat, st.Lon) {
			continue
		}
		f.Stations[st.ID] = Station{ID: st.ID, Name: localizedString(st.Name), Lat: st.Lat, Lon: st.Lon,
			Capacity: st.Capacity, RentalURIs: st.RentalURIs.value(), VehicleCounts: map[string]int{}, DockCounts: map[string]int{}}
	}

	var status struct {
		Stations []rawStationStatus `json:"stations"`
	}
	if err := read("station_status", &status); err != nil {
		return nil, err
	}
	for _, rs := range status.Stations {
		st, ok := f.Stations[rs.ID]
		if !ok {
			continue // coordinates live in station_information and are required
		}
		st.Installed = flexBoolDefault(rs.Installed, true)
		st.Renting = flexBoolDefault(rs.Renting, false)
		st.Returning = flexBoolDefault(rs.Returning, false)
		if rs.NumVehicles != nil {
			st.Vehicles = *rs.NumVehicles
		} else if rs.NumBikes != nil {
			st.Vehicles = *rs.NumBikes
		}
		if rs.NumDocks != nil {
			st.Docks, st.DocksKnown = *rs.NumDocks, true
		}
		st.LastReport = parseTimestamp(rs.LastReported)
		for _, c := range rs.VehicleTypes {
			st.VehicleCounts[c.TypeID] = c.Count
		}
		for _, c := range rs.VehicleDocks {
			for _, typeID := range c.TypeIDs {
				st.DockCounts[typeID] += c.Count
			}
		}
		f.Stations[rs.ID] = st
	}

	var vehicles struct {
		Vehicles []rawVehicle `json:"vehicles"`
		Bikes    []rawVehicle `json:"bikes"`
	}
	vehicleKey := "vehicle_status"
	if len(docs[vehicleKey]) == 0 {
		vehicleKey = "free_bike_status"
	}
	if err := read(vehicleKey, &vehicles); err != nil {
		return nil, err
	}
	vehicles.Vehicles = append(vehicles.Vehicles, vehicles.Bikes...)
	for _, rv := range vehicles.Vehicles {
		id := rv.VehicleID
		if id == "" {
			id = rv.BikeID
		}
		if id == "" {
			continue
		}
		typeID := rv.TypeID
		if typeID == "" {
			typeID = defaultTypeID
		} else if _, known := f.VehicleTypes[typeID]; !known {
			// Some sparse pre-vehicle_types feeds still emit a type-shaped
			// field. Only fall back when the feed supplied no type catalogue at
			// all; with a real catalogue, an unknown type is left unknown and
			// will fail closed in the query layer.
			if _, legacy := f.VehicleTypes[defaultTypeID]; legacy {
				typeID = defaultTypeID
			}
		}
		home := rv.HomeStationID
		if home == "" {
			home = rv.HomeStationLegacy
		}
		v := Vehicle{ID: id, TypeID: typeID, StationID: rv.StationID, HomeStationID: home,
			Lat: rv.Lat, Lon: rv.Lon, Reserved: flexBoolDefault(rv.Reserved, false),
			Disabled: flexBoolDefault(rv.Disabled, false), LastReport: parseTimestamp(rv.LastReported),
			RentalURIs: rv.RentalURIs.value()}
		if rv.RangeM != nil && *rv.RangeM >= 0 {
			v.RangeM, v.RangeKnown = *rv.RangeM, true
		}
		for _, p := range []*float64{rv.BatteryPercent, rv.BatteryPercentage, rv.FuelPercent} {
			if p != nil && *p >= 0 {
				v.BatteryPercent, v.BatteryKnown = normalizePercent(*p), true
				break
			}
		}
		if !v.RangeKnown && v.BatteryKnown {
			if typ, ok := f.VehicleTypes[typeID]; ok && typ.MaxRangeM > 0 {
				v.RangeM, v.RangeKnown = typ.MaxRangeM*v.BatteryPercent/100, true
			}
		}
		if v.RangeKnown && !v.BatteryKnown {
			if typ, ok := f.VehicleTypes[typeID]; ok && typ.MaxRangeM > 0 {
				v.BatteryPercent, v.BatteryKnown = normalizePercent(v.RangeM/typ.MaxRangeM), true
			}
		}
		f.Vehicles[id] = v
	}

	if err := parseGeofencing(f, docs["geofencing_zones"], &newest, &ttl); err != nil {
		return nil, err
	}
	if newest.IsZero() {
		newest = fetched
	}
	f.DataUpdatedAt = newest
	if ttl > 0 {
		f.ExpiresAt = fetched.Add(time.Duration(ttl) * time.Second)
	}
	return f, nil
}

const defaultTypeID = "__gbfs_default_bicycle"

func localizedString(raw json.RawMessage) string {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var vals []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &vals) == nil && len(vals) > 0 {
		return vals[0].Text
	}
	return ""
}

func parseTimestamp(raw json.RawMessage) time.Time {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return time.Time{}
	}
	var n json.Number
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if dec.Decode(&n) == nil {
		if sec, err := strconv.ParseInt(n.String(), 10, 64); err == nil && sec > 0 {
			return time.Unix(sec, 0)
		}
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if sec, err := strconv.ParseInt(s, 10, 64); err == nil && sec > 0 {
			return time.Unix(sec, 0)
		}
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

func flexBoolDefault(raw json.RawMessage, def bool) bool {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return def
	}
	var b bool
	if json.Unmarshal(raw, &b) == nil {
		return b
	}
	var n int
	if json.Unmarshal(raw, &n) == nil {
		return n != 0
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		switch strings.ToLower(s) {
		case "1", "true", "yes":
			return true
		case "0", "false", "no":
			return false
		}
	}
	return def
}

func normalizePercent(v float64) float64 {
	if v <= 1 {
		v *= 100
	}
	if v > 100 {
		return 100
	}
	return v
}

func validCoord(lat, lon float64) bool {
	return lat >= -90 && lat <= 90 && lon >= -180 && lon <= 180 && !(lat == 0 && lon == 0)
}
