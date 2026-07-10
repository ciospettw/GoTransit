package tests

import (
	"context"
	"testing"
	"time"

	"gotransit/internal/engine"
	"gotransit/internal/rt"
	"gotransit/internal/track"
)

// ---- THE E2E: the engine that never leaves you alone ----------------------------

func TestE2ELiveTracking(t *testing.T) {
	if testing.Short() {
		t.Skip("wall-clock E2E (~2 min)")
	}
	w := buildWorld(t, worldOpts{})
	srv := newRTServer()
	defer srv.Close()

	mgr := newManager(t, w, srv)

	// everything on time and live before planning
	srv.set(onTime("A1", "A2", "B1"))
	mgr.Start()
	waitVersion(t, mgr, 1)

	// ---- plan live: expect the fast A1, flagged live ----
	fromLat, fromLon, toLat, toLon := w.od()
	resp, err := w.e.Plan(engine.Request{
		FromLat: fromLat, FromLon: fromLon, ToLat: toLat, ToLon: toLon,
		Mode: "transit", When: w.now, Live: true, Num: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.LiveItineraries) == 0 {
		t.Fatal("expected live itineraries")
	}
	it := resp.LiveItineraries[0]
	if got := rideOf(t, it); got != "test:A1" {
		t.Fatalf("baseline should ride A1, got %s", got)
	}
	if !it.Live || it.ID == "" {
		t.Fatalf("itinerary not live or without id: %+v", it.ID)
	}

	// ---- track it ----
	tracker := &track.Tracker{E: w.e, Mgr: mgr, Cfg: w.e.Cfg, Log: testLogger(), Tick: 100 * time.Millisecond}
	sink := make(chanSink, 64)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- tracker.Run(ctx, it.ID, sink) }()

	waitFor(t, sink, "hello", 2*time.Second, func(ev map[string]any) bool {
		return ev["type"] == "hello" && ev["mode"] == "live"
	})

	// ---- 1: A1 delayed +9m → delay event, then better_arrival reroute to B1 ----
	delayed := onTime("A2", "B1")
	delayed.Trips = append(delayed.Trips, rt.TripRT{
		TripID: "A1",
		STUs:   []rt.STU{{Seq: 1, StopID: "SA", ArrDelay: 540, DepDelay: 540}},
	})
	srv.set(delayed)

	waitFor(t, sink, "delay event (+9m)", 5*time.Second, func(ev map[string]any) bool {
		if ev["type"] != "delay" {
			return false
		}
		return ev["arrive_delta_s"].(float64) >= 400
	})
	rr := waitFor(t, sink, "better_arrival reroute onto B1", 8*time.Second, func(ev map[string]any) bool {
		return ev["type"] == "reroute" && ev["reason"] == "better_arrival"
	})
	if got := firstRideTrip(rr); got != "test:B1" {
		t.Fatalf("reroute should ride B1, got %s", got)
	}
	if rr["saving_s"].(float64) < 300 {
		t.Fatalf("saving too small: %v", rr["saving_s"])
	}

	// ---- 2: B1 cancelled → infeasibility reroute back to line A ----
	cancelledB := onTime("A2")
	cancelledB.Trips = append(cancelledB.Trips,
		rt.TripRT{TripID: "A1", STUs: []rt.STU{{Seq: 1, StopID: "SA", ArrDelay: 540, DepDelay: 540}}},
		rt.TripRT{TripID: "B1", Cancelled: true},
	)
	srv.set(cancelledB)

	rr2 := waitFor(t, sink, "cancellation reroute", 15*time.Second, func(ev map[string]any) bool {
		return ev["type"] == "reroute" && ev["reason"] == "cancelled"
	})
	if ch, ok := rr2["changed_legs"].([]any); !ok || len(ch) == 0 {
		t.Fatalf("reroute must list changed legs, got %v", rr2["changed_legs"])
	}
	newTrip := firstRideTrip(rr2)
	if it2, ok := rr2["itinerary"].(map[string]any); ok {
		for i, l := range it2["legs"].([]any) {
			lm := l.(map[string]any)
			t.Logf("  rr2 leg %d: %v dur=%vs dist=%vm dep=%v arr=%v", i, lm["mode"], lm["duration_s"], lm["distance_m"], lm["depart"], lm["arrive"])
		}
	}
	if newTrip != "test:A1" && newTrip != "test:A2" {
		t.Fatalf("cancellation reroute should fall back to line A, got %s", newTrip)
	}

	// ---- 3: vehicle passes the boarding stop → boarded ----
	tripNum := newTrip[len("test:"):]
	rolling := onTime("A2")
	rolling.Trips = append(rolling.Trips,
		rt.TripRT{TripID: "A1", STUs: []rt.STU{{Seq: 1, StopID: "SA", ArrDelay: 540, DepDelay: 540}}},
		rt.TripRT{TripID: "B1", Cancelled: true},
	)
	rolling.Vehicles = append(rolling.Vehicles, rt.VehicleRT{
		TripID: tripNum, CurrentSeq: 2, Status: 2, // IN_TRANSIT_TO the second stop
	})
	srv.set(rolling)

	waitFor(t, sink, "boarded", 150*time.Second, func(ev map[string]any) bool {
		return ev["type"] == "progress" && ev["boarded"] == true
	})
	// the live vehicle is streamed: heading to SB, pinned at the stop
	// coordinates (this feed sends no GPS), with the ride's delay
	veh := waitFor(t, sink, "vehicle event", 10*time.Second, func(ev map[string]any) bool {
		return ev["type"] == "vehicle" && ev["status"] == "in_transit_to"
	})
	if veh["stop"] == nil || veh["lat"] == nil {
		t.Fatalf("vehicle event lacks stop/coords: %v", veh)
	}

	// ---- 4: vehicle reaches SB (arrival in the past) → alighted, then arrived ----
	landed := onTime("A2")
	landed.Trips = append(landed.Trips,
		rt.TripRT{TripID: "B1", Cancelled: true},
		rt.TripRT{TripID: tripNum, STUs: []rt.STU{
			{Seq: 1, StopID: "SA", ArrDelay: rt.Absent, DepDelay: 540},
			{Seq: 2, StopID: "SB", ArrDelay: rt.Absent, DepDelay: rt.Absent,
				ArrTime: time.Now().Unix() - 5, DepTime: time.Now().Unix() - 5},
		}},
	)
	srv.set(landed)

	waitFor(t, sink, "alighted", 30*time.Second, func(ev map[string]any) bool {
		return ev["type"] == "progress" && ev["status"] == "alighted"
	})
	// egress is a ~150 m walk re-anchored to the alighting moment
	waitFor(t, sink, "arrived", 4*time.Minute, func(ev map[string]any) bool {
		return ev["type"] == "arrived"
	})
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("session ended with error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("session did not finish after arrival")
	}
}
