package tests

import (
	"testing"
	"time"

	"gotransit/internal/rt"
)

// The RT overlay through the full pipeline: encode → poll → decode → project.
func TestOverlaySemantics(t *testing.T) {
	w := buildWorld(t, worldOpts{})
	srv := newRTServer()
	defer srv.Close()
	mgr := newManager(t, w, srv)

	trip, ok := w.tt.TripIdx["test:A1"]
	if !ok {
		t.Fatal("trip A1 missing")
	}

	// delay propagation: departure delay at SA holds for the SB arrival
	srv.set(&rt.Feed{Trips: []rt.TripRT{{
		TripID: "A1",
		STUs:   []rt.STU{{Seq: 1, StopID: "SA", ArrDelay: rt.Absent, DepDelay: 120}},
	}}})
	mgr.Start()
	waitVersion(t, mgr, 1)
	o := w.tt.RT()
	if !o.TripHasRT(trip) {
		t.Fatal("A1 should have RT")
	}
	if got := w.tt.TripDep(trip, 0) - w.tt.ScheduledDep(trip, 0); got != 120 {
		t.Errorf("dep delta = %d want 120", got)
	}
	if got := w.tt.TripArr(trip, 1) - w.tt.ScheduledArr(trip, 1); got != 120 {
		t.Errorf("propagated arr delta = %d want 120", got)
	}

	// absolute-time STU + passed inference (arrival already in the past)
	past := time.Now().Unix() - 30
	srv.set(&rt.Feed{Trips: []rt.TripRT{{
		TripID: "A1",
		STUs: []rt.STU{
			{Seq: 1, StopID: "SA", ArrDelay: rt.Absent, DepDelay: 60},
			{Seq: 2, StopID: "SB", ArrDelay: rt.Absent, DepDelay: rt.Absent, ArrTime: past, DepTime: past},
		},
	}}})
	waitVersion(t, mgr, 2)
	o = w.tt.RT()
	if o.TripPassed(trip) != 1 {
		t.Errorf("passed = %d want 1 (arrival in the past)", o.TripPassed(trip))
	}

	// cancellation + skipped stop + vehicle in a mixed feed
	srv.set(&rt.Feed{
		Trips: []rt.TripRT{
			{TripID: "B1", Cancelled: true},
			{TripID: "A2", STUs: []rt.STU{{Seq: 2, StopID: "SB", ArrDelay: rt.Absent, DepDelay: rt.Absent, Skipped: true}}},
		},
		Vehicles: []rt.VehicleRT{{TripID: "A2", Lat: 41.905, Lon: 12.507, CurrentSeq: 2, Status: 2}},
	})
	waitVersion(t, mgr, 3)
	o = w.tt.RT()
	b1 := w.tt.TripIdx["test:B1"]
	a2 := w.tt.TripIdx["test:A2"]
	if !w.tt.TripSkipped(b1) {
		t.Error("B1 should be cancelled")
	}
	if !w.tt.StopSkipped(a2, 1) {
		t.Error("A2 must skip SB")
	}
	lat, lon, pos, status, ok := o.Vehicle(a2)
	if !ok || pos != 1 || status != 2 || lat == 0 || lon == 0 {
		t.Errorf("vehicle = %d,%d pos=%d status=%d ok=%v", lat, lon, pos, status, ok)
	}
	if o.TripPassed(a2) != 0 {
		t.Errorf("vehicle in transit to pos1 → passed=0, got %d", o.TripPassed(a2))
	}

	// stats surface matched/cancelled counts
	stats := mgr.Stats()
	if len(stats) != 1 || stats[0].Matched < 2 || stats[0].Cancelled != 1 {
		t.Errorf("stats = %+v", stats)
	}
}
