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

// TestGPSFusion drives a session with client position fixes (protocol 3):
//  1. walking with GPS → walk-progress events with live distance, and the
//     street leg completes early when the rider reaches the boarding stop;
//  2. after first reaching the stop, fresh fixes moving in order along the
//     board→alight shape and >100 m away → boarded even with no vehicle
//     position and no Passed confirmation;
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

	// Stop arrival now emits waiting immediately, followed by zero remaining
	// distance. Do not consume both while expecting a separate delayed handoff.
	waitFor(t, sink, "immediate waiting at reached stop", 5*time.Second, func(ev map[string]any) bool {
		return ev["type"] == "progress" && ev["status"] == "waiting" && ev["distance_to_stop_m"] == float64(0)
	})

	// 2) shape-only boarding fallback: the feed keeps reporting the trip but
	// has neither VehiclePosition nor a passed stop. The rider has already
	// reached SA; ordered fresh fixes now move away along A1's own shape. This
	// must become riding rather than left_stop even though schedule/RT still
	// make the engine believe the vehicle is waiting at SA.
	tripNum := rideOf(t, it)[len("test:"):]
	srv.set(onTime("A1", "A2", "B1"))
	go func() {
		// Two ordered fixes cross 100m after the persistence window, without
		// a later pump update racing with the rode-past stage below.
		for _, p := range []struct {
			lonE7 int32
			hold  time.Duration
		}{
			{oLon + 7000, 6 * time.Second},
			{oLon + 14500, 0},
		} {
			target.Store(&[2]float64{saLat, float64(p.lonE7) / 1e7})
			if p.hold > 0 {
				time.Sleep(p.hold)
			}
		}
	}()
	deadline := time.After(45 * time.Second)
	for {
		select {
		case ev := <-sink:
			t.Logf("event: %s %v", ev["type"], summarize(ev))
			if ev["type"] == "warning" && ev["code"] == "left_stop" {
				t.Fatalf("shape-coherent boarding triggered left_stop: %v", ev)
			}
			if ev["type"] == "reroute" && ev["reason"] == "left_stop" {
				t.Fatalf("shape-coherent boarding rerouted from the stop: %v", ev)
			}
			if ev["type"] == "progress" && ev["status"] == "riding" {
				goto boarded
			}
		case <-deadline:
			t.Fatal("timed out waiting for shape-only boarding")
		}
	}

boarded:

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
	// Only one stop remains in this fixture: a safe replacement requiring
	// two stops of notice does not exist. Keep the occupied bus and search,
	// never replace it with a walk from the rider's mid-road GPS position.
	waitFor(t, sink, "safe recovery search", 30*time.Second, func(ev map[string]any) bool {
		if ev["type"] == "reroute" {
			t.Fatalf("rerouted without two downstream stops: %v", ev)
		}
		return ev["type"] == "warning" && ev["code"] == "stayed_on_vehicle"
	})
}

// TestGPSRapidPickupBeforeWalkHandoff covers a bus arriving while the access
// walk has not reached its planned arrival time. Reaching SA must immediately
// switch to waiting, then ordered shape movement confirms boarding without
// VehiclePosition or TripUpdate confirmation, and without a missed/reroute.
func TestGPSRapidPickupBeforeWalkHandoff(t *testing.T) {
	if testing.Short() {
		t.Skip("wall-clock rapid-pickup E2E (~15s)")
	}
	w := buildWorld(t, worldOpts{midStopShape: true})
	srv := newRTServer()
	defer srv.Close()
	mgr := newManager(t, w, srv)
	w.e.Cfg.Routing.WalkSpeedKmh = 2.0

	// No VehiclePosition and no Passed confirmation: only the rider's ordered
	// shape evidence may promote the upcoming ride.
	srv.set(onTime("A1", "A2", "B1"))
	mgr.Start()
	waitVersion(t, mgr, 1)

	fromLat, fromLon, toLat, toLon := w.odMid()
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
	if len(it.Legs) < 2 || it.Legs[0].Mode == "transit" || it.Legs[1].Mode != "transit" {
		t.Fatalf("fixture must start access-walk -> transit, got %+v", it.Legs)
	}

	tracker := &track.Tracker{E: w.e, Mgr: mgr, Cfg: w.e.Cfg, Log: testLogger(), Tick: 100 * time.Millisecond}
	sink := make(chanSink, 128)
	fixes := make(chan track.Fix, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go tracker.Run(ctx, it.ID, sink, fixes)

	waitFor(t, sink, "hello", 2*time.Second, func(ev map[string]any) bool {
		return ev["type"] == "hello"
	})

	saLat, saLon := float64(oLat+1000)/1e7, float64(oLon+1000)/1e7
	var target atomic.Pointer[[2]float64]
	target.Store(&[2]float64{saLat, saLon})
	go func() {
		tk := time.NewTicker(300 * time.Millisecond)
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

	// One second is safely below atStopHold (10s), but enough for gpsStopGuard
	// to latch that the rider really reached this ride's boarding stop.
	time.Sleep(time.Second)
	go func() {
		for _, p := range []struct {
			lonE7 int32
			hold  time.Duration
		}{
			{oLon + 12000, 3 * time.Second},
			{oLon + 30000, 4 * time.Second}, // >200m from SA, ordered and persisted
		} {
			target.Store(&[2]float64{saLat, float64(p.lonE7) / 1e7})
			time.Sleep(p.hold)
		}
	}()

	deadline := time.After(20 * time.Second)
	for {
		select {
		case ev := <-sink:
			t.Logf("event: %s %v", ev["type"], summarize(ev))
			if ev["type"] == "progress" && ev["status"] == "waiting" && ev["leg_index"] != float64(1) {
				t.Fatalf("stop arrival did not skip the access walk: %v", ev)
			}
			if ev["type"] == "warning" {
				if code := ev["code"]; code == "too_slow" || code == "left_stop" || code == "missed_connection" {
					t.Fatalf("rapid pickup emitted %v before boarding: %v", code, ev)
				}
			}
			if ev["type"] == "reroute" {
				t.Fatalf("rapid pickup rerouted instead of boarding: %v", ev)
			}
			if ev["type"] == "progress" && ev["status"] == "riding" {
				if ev["leg_index"] != float64(1) || ev["boarded"] != true {
					t.Fatalf("invalid direct riding transition: %v", ev)
				}
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for direct walking-to-riding promotion")
		}
	}
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

// TestGPSShapeMismatchKeepsLeftStopGuard is the conservative half of the
// shape-only boarding fallback. Reaching the stop is latched, but fixes that
// then move away perpendicular to the ride shape are not boarding evidence:
// the ordinary persisted left_stop guard must remain armed.
func TestGPSShapeMismatchKeepsLeftStopGuard(t *testing.T) {
	if testing.Short() {
		t.Skip("wall-clock GPS guard E2E (~25s)")
	}
	w := buildWorld(t, worldOpts{midStopShape: true})
	srv := newRTServer()
	defer srv.Close()
	mgr := newManager(t, w, srv)

	// Make walk-back unambiguously infeasible as soon as the rider is outside
	// the wander radius; persistence is still the production 10-second hold.
	w.e.Cfg.Track.BoardBuffer = 10 * time.Minute
	w.e.Cfg.Routing.WalkSpeedKmh = 2.0
	srv.set(onTime("A1", "A2", "B1"))
	mgr.Start()
	waitVersion(t, mgr, 1)

	fromLat, fromLon, toLat, toLon := w.odMid()
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

	tracker := &track.Tracker{E: w.e, Mgr: mgr, Cfg: w.e.Cfg, Log: testLogger(), Tick: 100 * time.Millisecond}
	sink := make(chanSink, 128)
	fixes := make(chan track.Fix, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go tracker.Run(ctx, resp.LiveItineraries[0].ID, sink, fixes)

	waitFor(t, sink, "hello", 2*time.Second, func(ev map[string]any) bool {
		return ev["type"] == "hello"
	})

	saLat, saLon := float64(oLat+1000)/1e7, float64(oLon+1000)/1e7
	var target atomic.Pointer[[2]float64]
	target.Store(&[2]float64{saLat, saLon})
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

	waitFor(t, sink, "waiting after reaching stop", 25*time.Second, func(ev map[string]any) bool {
		return ev["type"] == "progress" && ev["status"] == "waiting"
	})
	// About 545 m north of SA, while A1's shape runs east-west.
	target.Store(&[2]float64{float64(oLat+50000) / 1e7, saLon})
	waitFor(t, sink, "left_stop for shape-incoherent fixes", 30*time.Second, func(ev map[string]any) bool {
		return ev["type"] == "warning" && ev["code"] == "left_stop"
	})
}
