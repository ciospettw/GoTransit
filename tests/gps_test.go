package tests

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"gotransit/internal/engine"
	"gotransit/internal/rt"
	"gotransit/internal/track"
)

// TestGPSFusion drives a session with client position fixes:
//  1. walking with GPS → walk-progress events with live distance, and the
//     street leg completes early when the rider reaches the boarding stop;
//  2. sustained co-location with a vehicle confirmedly past the boarding
//     stop → boarded, before any Passed/clock confirmation;
//  3. the feed confirms the vehicle cleared the alight stop while the rider
//     stays co-located → missed_alight deviation (they did not get off).
func TestGPSFusion(t *testing.T) {
	if testing.Short() {
		t.Skip("wall-clock GPS fusion E2E (~2 min: persistence tolerances are real)")
	}
	w := buildWorld(t, worldOpts{})
	srv := newRTServer()
	defer srv.Close()
	mgr := newManager(t, w, srv)

	srv.set(onTime("A1", "A2", "B1"))
	mgr.Start()
	waitVersion(t, mgr, 1)

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

	tracker := &track.Tracker{E: w.e, Mgr: mgr, Cfg: w.e.Cfg, Log: testLogger(), Tick: 100 * time.Millisecond}
	sink := make(chanSink, 256)
	fixes := make(chan track.Fix, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go tracker.Run(ctx, it.ID, sink, fixes)

	waitFor(t, sink, "hello", 2*time.Second, func(ev map[string]any) bool {
		return ev["type"] == "hello"
	})

	// fix pump: streams the current target position every 400ms, accurate GPS
	saLat, saLon := float64(oLat+1000)/1e7, float64(oLon+1000)/1e7
	sbLat, sbLon := float64(oLat+1000)/1e7, float64(oLon+3*step-1000)/1e7
	var target atomic.Pointer[[2]float64]
	target.Store(&[2]float64{saLat, saLon}) // the rider is at the boarding stop
	go func() {
		tk := time.NewTicker(400 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tk.C:
				p := target.Load()
				select {
				case fixes <- track.Fix{Lat: p[0], Lon: p[1], AccuracyM: 10, At: time.Now()}:
				default:
				}
			}
		}
	}()

	// 1) walk progress: live distance while the walk leg is current, then the
	// street leg completes early (rider at the stop) → explicit waiting phase
	waitFor(t, sink, "walk progress with distance", 10*time.Second, func(ev map[string]any) bool {
		if ev["type"] != "progress" {
			return false
		}
		_, hasDist := ev["distance_to_stop_m"]
		return hasDist
	})
	waitFor(t, sink, "early street-leg completion → waiting at stop", 25*time.Second, func(ev map[string]any) bool {
		return ev["type"] == "progress" && ev["status"] == "waiting"
	})

	// 2) boarding: the vehicle moves past the boarding stop (RT-confirmed) and
	// the rider moves with it — riding starts, co-location corroborating
	tripNum := rideOf(t, it)[len("test:"):]
	rolling := onTime("A1", "A2", "B1")
	rolling.Vehicles = append(rolling.Vehicles, rt.VehicleRT{
		TripID: tripNum, CurrentSeq: 2, Status: 2, // in_transit_to SB
	})
	srv.set(rolling)
	target.Store(&[2]float64{sbLat, sbLon}) // rider moves with the vehicle

	waitFor(t, sink, "boarding", 60*time.Second, func(ev map[string]any) bool {
		return ev["type"] == "progress" && ev["status"] == "riding"
	})
	// let SB fixes actually flow through the pump (400ms cadence) before the
	// feed declares the alight stop passed — like a real rider's stream
	time.Sleep(2 * time.Second)

	// 3) missed alight: the feed confirms the trip cleared the alight stop
	// (early run: negative delays put both stop times in the past) while the
	// rider stays co-located with the vehicle → deviation, not a silent alight
	past := onTime("A2", "B1")
	past.Trips = append(past.Trips, rt.TripRT{
		TripID: tripNum,
		STUs: []rt.STU{
			{Seq: 1, StopID: "SA", ArrDelay: -600, DepDelay: -600},
			{Seq: 2, StopID: "SB", ArrDelay: -660, DepDelay: -660},
		},
	})
	past.Vehicles = append(past.Vehicles, rt.VehicleRT{
		TripID: tripNum, CurrentSeq: 2, Status: 1, // stopped at SB
	})
	srv.set(past)

	dev := waitFor(t, sink, "missed_alight deviation", 90*time.Second, func(ev map[string]any) bool {
		return ev["type"] == "deviation"
	})
	if dev["kind"] != "missed_alight" {
		t.Fatalf("deviation kind = %v, want missed_alight", dev["kind"])
	}
	if stop, ok := dev["expected_stop"].(map[string]any); !ok || stop["name"] == "" {
		t.Fatalf("deviation must carry the expected stop, got %v", dev["expected_stop"])
	}
}
