package tests

import (
	"testing"

	"gotransit/internal/engine"
	"gotransit/internal/geo"
	"gotransit/internal/graph"
	"gotransit/internal/gtfs"
	"gotransit/internal/transit"
)

func TestStopConnectorTimingUsesConfiguredWalkSpeed(t *testing.T) {
	compile := func(speed float64) *transit.Timetable {
		t.Helper()
		g := graph.SyntheticGrid(2, 2, oLat, oLon, step)
		// Both stops project near the middle of the same street edge. Their
		// transfer is therefore dominated by the two stop↔anchor connectors.
		f := &gtfs.Feed{
			Name: "walk", TZ: "Europe/Rome",
			Stops: []gtfs.Stop{
				{ID: "A", Name: "A", Lat: oLat + 1000, Lon: oLon + step/2, OK: true},
				{ID: "B", Name: "B", Lat: oLat + 2000, Lon: oLon + step/2, OK: true},
			},
		}
		tt, _, err := transit.Compile([]*gtfs.Feed{f}, g, speed, 1000, 300)
		if err != nil {
			t.Fatal(err)
		}
		return tt
	}

	transferDs := func(tt *transit.Timetable) uint16 {
		t.Helper()
		to, ds := tt.Transfers(0)
		for i := range to {
			if to[i] == 1 {
				return ds[i]
			}
		}
		t.Fatal("synthetic stop transfer not found")
		return 0
	}

	slow := compile(3.0)
	fast := compile(6.0)
	slowDs, fastDs := transferDs(slow), transferDs(fast)
	if slowDs < fastDs*19/10 || slowDs > fastDs*21/10 {
		t.Fatalf("connector timing does not follow configured speed: 3 km/h=%d ds, 6 km/h=%d ds", slowDs, fastDs)
	}
	if slow.StopSnap[0].MetersU == 0 || slow.StopSnap[0].MetersU != fast.StopSnap[0].MetersU {
		t.Fatalf("stop connector must be stable metres, slow=%+v fast=%+v", slow.StopSnap[0], fast.StopSnap[0])
	}
	if len(slow.NSExtraM) == 0 {
		t.Fatal("reverse stop index did not retain connector metres")
	}
}

func TestWalkingLegGeometryAndStepTotals(t *testing.T) {
	w := buildWorld(t, worldOpts{})
	fromLat, fromLon, toLat, toLon := w.od()
	resp, err := w.e.Plan(engine.Request{
		FromLat: fromLat, FromLon: fromLon, ToLat: toLat, ToLon: toLon,
		Mode: "walk", When: w.now, Num: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Itineraries) != 1 || len(resp.Itineraries[0].Legs) != 1 {
		t.Fatalf("unexpected walking response: %+v", resp)
	}
	leg := resp.Itineraries[0].Legs[0]
	assertStepTotals(t, leg)

	lats, lons := geo.DecodePolyline(leg.Polyline)
	if len(lats) < 4 {
		t.Fatalf("walking polyline omits connector geometry: %d points", len(lats))
	}
	wantFLat, wantFLon := int32(fromLat*1e7), int32(fromLon*1e7)
	wantTLat, wantTLon := int32(toLat*1e7), int32(toLon*1e7)
	if abs32(lats[0]-wantFLat) > 100 || abs32(lons[0]-wantFLon) > 100 {
		t.Fatalf("polyline starts at (%d,%d), want (%d,%d)", lats[0], lons[0], wantFLat, wantFLon)
	}
	last := len(lats) - 1
	if abs32(lats[last]-wantTLat) > 100 || abs32(lons[last]-wantTLon) > 100 {
		t.Fatalf("polyline ends at (%d,%d), want (%d,%d)", lats[last], lons[last], wantTLat, wantTLon)
	}

	// Access and egress use the precompiled stop attachments rather than the
	// two request snaps. They must expose the same complete instruction totals.
	transitResp, err := w.e.Plan(engine.Request{
		FromLat: fromLat, FromLon: fromLon, ToLat: toLat, ToLon: toLon,
		Mode: "transit", When: w.now, Num: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	walkLegs := 0
	for _, transitLeg := range transitResp.Itineraries[0].Legs {
		if transitLeg.Mode == "walk" {
			walkLegs++
			assertStepTotals(t, transitLeg)
		}
	}
	if walkLegs < 2 {
		t.Fatalf("transit itinerary should contain access and egress walks: %+v", transitResp.Itineraries[0].Legs)
	}
}

func assertStepTotals(t *testing.T, leg engine.Leg) {
	t.Helper()
	if len(leg.Steps) < 2 || leg.Steps[0].Kind != "depart" || leg.Steps[len(leg.Steps)-1].Kind != "arrive" {
		t.Fatalf("incomplete walking instructions: %+v", leg.Steps)
	}
	distance, duration := 0, 0
	for _, step := range leg.Steps {
		distance += step.DistanceM
		duration += step.DurationS
	}
	if distance != leg.DistanceM || duration != leg.DurationS {
		t.Fatalf("step totals (%dm, %ds) != leg (%dm, %ds): %+v",
			distance, duration, leg.DistanceM, leg.DurationS, leg.Steps)
	}
}

func abs32(v int32) int32 {
	if v < 0 {
		return -v
	}
	return v
}
