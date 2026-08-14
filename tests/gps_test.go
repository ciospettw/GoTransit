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

// TestGPSFusion drives a session with client position fixes (protocol 2):
//  1. walking with GPS → walk-progress events with live distance, and the
//     street leg completes early when the rider reaches the boarding stop;
//  2. sustained co-location with a vehicle confirmedly past the boarding
//     stop → boarded, before any Passed/clock confirmation;
//  3. the rider does NOT get off at the alight stop and keeps following the
//     line: >rode_past_dist from the stop with consecutive fixes inside the
//     shape corridor beyond it → "stayed_on_vehicle" deviation + reroute
//     from the vehicle's downstream stops. (GPS is otherwise never used to
//     infer anything while riding.)
func TestGPSFusion(t *testing.T) {
	if testing.Short() {
		t.Skip("wall-clock GPS fusion E2E (~2 min: persistence tolerances are real)")
	}
	w := buildWorld(t, worldOpts{midStopShape: true})
	srv := newRTServer()
	defer srv.Close()
	mgr := newManager(t, w, srv)

	// the fixture line is short (~1.1 km): scale the rode-past threshold down
	w.e.Cfg.Track.RodePastDistM = 300
	// slow walking so the ride to SM beats the direct walk (the toy world is
	// tiny; at 4.8 km/h RAPTOR would prune the ride as walk-dominated)
	w.e.Cfg.Routing.WalkSpeedKmh = 2.0

	srv.set(onTime("A1", "A2", "B1"))
	mgr.Start()
	waitVersion(t, mgr, 1)

	fromLat, fromLon, toLat, toLon := w.odMid() // destination near SM: alight midway
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
	// beyond the alight stop SM (at 1.5·step), still on the line's shape
	pastLat, pastLon := float64(oLat+1000)/1e7, float64(oLon+27*step/10)/1e7
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
				case fixes <- track.Fix{Lat: p[0], Lon: p[1], AccuracyM: 10, HeadingD: -1, SpeedMS: -1, At: time.Now()}:
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
		TripID: tripNum, CurrentSeq: 2, Status: 2, // in_transit_to SM
	})
	srv.set(rolling)
	target.Store(&[2]float64{saLat, saLon}) // still at SA while the bus leaves it
	waitFor(t, sink, "boarding", 60*time.Second, func(ev map[string]any) bool {
		return ev["type"] == "progress" && ev["status"] == "riding"
	})

	// 3) stayed on the line: the feed confirms SM passed (early run: negative
	// delays put SA/SM in the past, vehicle heading to SB) while the rider's
	// fixes keep following the shape BEYOND SM, >rode_past_dist from it
	target.Store(&[2]float64{pastLat, pastLon})
	time.Sleep(2 * time.Second) // let beyond-fixes accumulate (rode_past_fixes)
	past := onTime("A2", "B1")
	past.Trips = append(past.Trips, rt.TripRT{
		TripID: tripNum,
		STUs: []rt.STU{
			{Seq: 1, StopID: "SA", ArrDelay: -600, DepDelay: -600},
			{Seq: 2, StopID: "SM", ArrDelay: -660, DepDelay: -660},
		},
	})
	past.Vehicles = append(past.Vehicles, rt.VehicleRT{
		TripID: tripNum, CurrentSeq: 3, Status: 2, // in transit to SB
	})
	srv.set(past)

	dev := waitFor(t, sink, "stayed_on_vehicle deviation", 90*time.Second, func(ev map[string]any) bool {
		return ev["type"] == "deviation"
	})
	if dev["kind"] != "stayed_on_vehicle" {
		t.Fatalf("deviation kind = %v, want stayed_on_vehicle", dev["kind"])
	}
	if stop, ok := dev["expected_stop"].(map[string]any); !ok || stop["name"] == "" {
		t.Fatalf("deviation must carry the expected stop, got %v", dev["expected_stop"])
	}
	waitFor(t, sink, "recovery reroute", 30*time.Second, func(ev map[string]any) bool {
		return ev["type"] == "reroute" && ev["reason"] == "stayed_on_vehicle"
	})
}

// TestGPSStopGuard: the rider is too far from the boarding stop to make the
// departure (walk ETA + buffer past the RT-adjusted departure) → proactive
// "too_slow" warning + reroute from their position, BEFORE the bus is missed.
func TestGPSStopGuard(t *testing.T) {
	if testing.Short() {
		t.Skip("wall-clock GPS guard E2E (~30s)")
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

	// the rider is ~600 m from SA with the departure ~4 minutes out: the walk
	// ETA (detour × distance / pace) plus the board buffer cannot make it
	farLat, farLon := float64(oLat+1000)/1e7, float64(oLon+73000)/1e7
	go func() {
		tk := time.NewTicker(400 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tk.C:
				select {
				case fixes <- track.Fix{Lat: farLat, Lon: farLon, AccuracyM: 10, HeadingD: -1, SpeedMS: -1, At: time.Now()}:
				default:
				}
			}
		}
	}()

	waitFor(t, sink, "too_slow warning", 60*time.Second, func(ev map[string]any) bool {
		return ev["type"] == "warning" && ev["code"] == "too_slow"
	})
	waitFor(t, sink, "too_slow reroute", 60*time.Second, func(ev map[string]any) bool {
		return ev["type"] == "reroute" && ev["reason"] == "too_slow"
	})
}
