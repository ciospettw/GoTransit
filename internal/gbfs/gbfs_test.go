package gbfs

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestDiscoveryV1LanguageAndRelativeURLs(t *testing.T) {
	b := []byte(`{"last_updated":1,"ttl":0,"data":{"it":{"feeds":[{"name":"station_status","url":"status.json"}]},"en":{"feeds":[{"name":"free_bike_status","url":"bikes.json"}]}}}`)
	version, feeds, err := parseDiscovery(b, "it")
	if err != nil {
		t.Fatal(err)
	}
	if version != "" || len(feeds) != 1 || feeds[0].Name != "station_status" || feeds[0].URL != "status.json" {
		t.Fatalf("unexpected discovery: version=%q feeds=%+v", version, feeds)
	}
}

func TestVehicleTypeBatteryRangeAndStationCapacity(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	stamp := now.Unix()
	docs := map[string][]byte{
		"vehicle_types": []byte(`{"last_updated":"` + now.Format(time.RFC3339) + `","ttl":30,"version":"3.0","data":{"vehicle_types":[` +
			`{"vehicle_type_id":"ebike","form_factor":"bicycle","propulsion_type":"electric_assist","max_range_meters":40000,"return_constraint":"any_station"},` +
			`{"vehicle_type_id":"scoot","form_factor":"scooter_standing","propulsion_type":"electric","max_range_meters":20000,"return_constraint":"free_floating"}]}}`),
		"station_information": []byte(`{"last_updated":"` + now.Format(time.RFC3339) + `","ttl":30,"data":{"stations":[` +
			`{"station_id":"A","name":[{"text":"Alpha","language":"en"}],"lat":41.9,"lon":12.5},` +
			`{"station_id":"B","name":[{"text":"Beta","language":"en"}],"lat":41.91,"lon":12.51}]}}`),
		"station_status": []byte(fmt.Sprintf(`{"last_updated":"%s","ttl":30,"data":{"stations":[`+
			`{"station_id":"A","is_installed":true,"is_renting":true,"is_returning":true,"num_vehicles_available":1,"num_docks_available":0,"vehicle_types_available":[{"vehicle_type_id":"ebike","count":1}],"last_reported":"%s"},`+
			`{"station_id":"B","is_installed":true,"is_renting":true,"is_returning":true,"num_vehicles_available":0,"num_docks_available":2,"vehicle_docks_available":[{"vehicle_type_ids":["ebike"],"count":2}],"last_reported":"%s"}]}}`, now.Format(time.RFC3339), now.Format(time.RFC3339), now.Format(time.RFC3339))),
		"vehicle_status": []byte(fmt.Sprintf(`{"last_updated":"%s","ttl":10,"data":{"vehicles":[`+
			`{"vehicle_id":"E1","vehicle_type_id":"ebike","station_id":"A","is_reserved":false,"is_disabled":false,"battery_percentage":25,"last_reported":"%s"},`+
			`{"vehicle_id":"S1","vehicle_type_id":"scoot","lat":41.9002,"lon":12.5002,"is_reserved":false,"is_disabled":false,"current_range_meters":9000,"last_reported":"%s"}]}}`, now.Format(time.RFC3339), now.Format(time.RFC3339), now.Format(time.RFC3339))),
	}
	f, err := parseFeed("demo", "3.0", docs, now, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	s := &Snapshot{Feeds: map[string]*Feed{"demo": f}}
	bikes := s.Pickups(ClassBicycle, 41.9, 12.5, 500, now)
	if len(bikes) != 1 || bikes[0].VehicleID != "E1" || !bikes[0].RangeKnown || bikes[0].CurrentRangeM != 10000 {
		t.Fatalf("battery fallback/type selection failed: %+v", bikes)
	}
	// The aggregate electric count at A is not returned as a second,
	// battery-unknown promise; E1 is the only safe assignment.
	scooters := s.Pickups(ClassScooter, 41.9, 12.5, 500, now)
	if len(scooters) != 1 || scooters[0].VehicleID != "S1" {
		t.Fatalf("scooter classification failed: %+v", scooters)
	}
	drops := s.Dropoffs(bikes[0], 41.91, 12.51, 100, now)
	if len(drops) != 1 || drops[0].DropoffStationID != "B" {
		t.Fatalf("expected accepting station B, got %+v", drops)
	}
	a := drops[0]
	a.RequiredRangeM = 8000 // 8km + 15% + 500m = 9.7km, within 10km.
	if got := s.Check(a, now, .15, 500); !got.Available {
		t.Fatalf("expected safe battery range, got %+v", got)
	}
	a.RequiredRangeM = 9000
	if got := s.Check(a, now, .15, 500); got.Available || got.Reason != "battery_range_insufficient" {
		t.Fatalf("expected range rejection, got %+v", got)
	}
	_ = stamp
}

func TestGeofenceV2AndV3Rules(t *testing.T) {
	now := time.Now().UTC()
	f := &Feed{Name: "z", FetchedAt: now, DataUpdatedAt: now, MaxAge: time.Minute,
		VehicleTypes: map[string]VehicleType{"bike": {ID: "bike", FormFactor: "bicycle", Propulsion: "human"}}}
	doc := []byte(`{"last_updated":"` + now.Format(time.RFC3339) + `","ttl":30,"version":"3.0","data":{` +
		`"geofencing_zones":{"type":"FeatureCollection","features":[{"type":"Feature","geometry":{"type":"MultiPolygon","coordinates":[[[[12.0,41.0],[13.0,41.0],[13.0,42.0],[12.0,42.0],[12.0,41.0]]]]},"properties":{"rules":[{"vehicle_type_ids":["bike"],"ride_start_allowed":true,"ride_end_allowed":false,"ride_through_allowed":true}]}}]},` +
		`"global_rules":[{"ride_start_allowed":true,"ride_end_allowed":true,"ride_through_allowed":true}]}}`)
	var newest time.Time
	var ttl int64
	if err := parseGeofencing(f, doc, &newest, &ttl); err != nil {
		t.Fatal(err)
	}
	if !f.pointAllows("bike", 41.5, 12.5, now, ruleStart) || f.pointAllows("bike", 41.5, 12.5, now, ruleEnd) {
		t.Fatal("v3 start/end rules were not applied")
	}
	if !f.pointAllows("bike", 43, 12.5, now, ruleEnd) {
		t.Fatal("global rule should apply outside the polygon")
	}
	legacy := rawRule{RideAllowed: boolPtr(false), ThroughAllowed: boolPtr(true)}.normalized()
	if legacy.StartAllowed == nil || *legacy.StartAllowed || legacy.EndAllowed == nil || *legacy.EndAllowed ||
		legacy.ThroughAllowed == nil || !*legacy.ThroughAllowed {
		t.Fatalf("v2 ride_allowed normalization failed: %+v", legacy)
	}
}

func TestManagerAtomicRealtimeDisappearance(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	var available atomic.Bool
	available.Store(true)
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/gbfs.json":
			fmt.Fprintf(w, `{"last_updated":%d,"ttl":0,"version":"2.3","data":{"feeds":[{"name":"free_bike_status","url":"%s/vehicles.json"}]}}`, now.Unix(), server.URL)
		case "/vehicles.json":
			bikes := "[]"
			if available.Load() {
				bikes = fmt.Sprintf(`[{"bike_id":"B1","lat":41.9,"lon":12.5,"is_reserved":false,"is_disabled":false,"last_reported":%d}]`, now.Unix())
			}
			fmt.Fprintf(w, `{"last_updated":%d,"ttl":0,"version":"2.3","data":{"bikes":%s}}`, now.Unix(), bikes)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)), []Source{{Name: "live", URL: server.URL + "/gbfs.json", MaxAge: time.Hour}})
	if err := m.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	pickups := m.Snapshot().Pickups(ClassBicycle, 41.9, 12.5, 100, now)
	if len(pickups) != 1 {
		t.Fatalf("expected initial vehicle, got %+v", pickups)
	}
	changed := m.Changed()
	available.Store(false)
	if err := m.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-changed:
	default:
		t.Fatal("snapshot swap did not signal Changed")
	}
	a := pickups[0]
	a.DropoffLat, a.DropoffLon = 41.91, 12.51
	if got := m.Snapshot().Check(a, now, .15, 500); got.Available || !strings.Contains(got.Reason, "unavailable") {
		t.Fatalf("disappeared vehicle remained available: %+v", got)
	}
}

func TestFutureRentalUsesCurrentAvailabilityButFutureGeofence(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	future := now.Add(40 * time.Minute)
	f := &Feed{Name: "future", FetchedAt: now, DataUpdatedAt: now, VehicleUpdatedAt: now,
		MaxAge:       5 * time.Minute,
		VehicleTypes: map[string]VehicleType{"bike": {ID: "bike", FormFactor: "bicycle", Propulsion: "human"}},
		Vehicles:     map[string]Vehicle{"B": {ID: "B", TypeID: "bike", Lat: 41.9, Lon: 12.5, LastReport: now}},
		Stations:     map[string]Station{}}
	startDenied := false
	f.Zones = []Zone{{
		Start: future.Add(-time.Minute),
		Polygons: [][][][2]float64{{{
			{12.49, 41.89}, {12.51, 41.89}, {12.51, 41.91}, {12.49, 41.91}, {12.49, 41.89},
		}}},
		Rules: []Rule{{VehicleTypeIDs: []string{"bike"}, StartAllowed: &startDenied}},
	}}
	s := &Snapshot{At: now, Feeds: map[string]*Feed{"future": f}}
	if got := s.Pickups(ClassBicycle, 41.9, 12.5, 100, now); len(got) != 1 {
		t.Fatalf("current pickup unexpectedly unavailable: %+v", got)
	}
	if got := s.Pickups(ClassBicycle, 41.9, 12.5, 100, future); len(got) != 0 {
		t.Fatalf("future geofence was ignored: %+v", got)
	}
	// Removing the scheduled restriction proves that the same five-minute-old
	// limit does not falsely reject a rental merely because it follows transit.
	f.Zones = nil
	if got := s.Pickups(ClassBicycle, 41.9, 12.5, 100, future); len(got) != 1 {
		t.Fatalf("future pickup treated current observation as stale: %+v", got)
	}
}

func TestStationVehiclesObeyStartAndEndGeofences(t *testing.T) {
	now := time.Now().UTC()
	no := false
	zone := Zone{Polygons: [][][][2]float64{{{
		{12.49, 41.89}, {12.52, 41.89}, {12.52, 41.92}, {12.49, 41.92}, {12.49, 41.89},
	}}}, Rules: []Rule{{VehicleTypeIDs: []string{"bike"}, StartAllowed: &no}}}
	f := &Feed{Name: "stationed", FetchedAt: now, DataUpdatedAt: now,
		StationUpdatedAt: now, MaxAge: time.Minute,
		VehicleTypes: map[string]VehicleType{"bike": {ID: "bike", FormFactor: "bicycle", Propulsion: "human"}},
		Stations: map[string]Station{
			"A": {ID: "A", Lat: 41.9, Lon: 12.5, Installed: true, Renting: true, Returning: true,
				Vehicles: 1, Docks: 1, DocksKnown: true, VehicleCounts: map[string]int{"bike": 1}},
		}, Zones: []Zone{zone}, Vehicles: map[string]Vehicle{}}
	s := &Snapshot{At: now, Feeds: map[string]*Feed{"stationed": f}}
	if got := s.Pickups(ClassBicycle, 41.9, 12.5, 100, now); len(got) != 0 {
		t.Fatalf("station pickup ignored ride_start_allowed=false: %+v", got)
	}
	allow := true
	f.Zones[0].Rules[0].StartAllowed = &allow
	f.Zones[0].Rules[0].EndAllowed = &no
	pickups := s.Pickups(ClassBicycle, 41.9, 12.5, 100, now)
	if len(pickups) != 1 {
		t.Fatalf("expected pickup after start became allowed: %+v", pickups)
	}
	if got := s.Dropoffs(pickups[0], 41.9, 12.5, 100, now); len(got) != 0 {
		t.Fatalf("station dropoff ignored ride_end_allowed=false: %+v", got)
	}
}

func boolPtr(v bool) *bool { return &v }
