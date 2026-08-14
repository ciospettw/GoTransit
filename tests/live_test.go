package tests

import (
	"context"
	"testing"
	"time"

	"gotransit/internal/engine"
	"gotransit/internal/rt"
	"gotransit/internal/track"
	"gotransit/internal/transit"
)

func TestRailLikeRouteTypes(t *testing.T) {
	for _, routeType := range []int{1, 2, 12, 100, 117, 400, 405} {
		if !transit.IsRailLikeRouteType(routeType) {
			t.Errorf("route type %d should be rail-like", routeType)
		}
	}
	for _, routeType := range []int{0, 3, 4, 11, 99, 118, 399, 406} {
		if transit.IsRailLikeRouteType(routeType) {
			t.Errorf("route type %d should not be rail-like", routeType)
		}
	}
}

// The live rule end to end through Plan: schedule-only buses are not live,
// RT-confirmed buses are, and metro is live by definition (no VP/TU exist).
func TestLiveRuleThroughPlan(t *testing.T) {
	fromLat, fromLon, toLat, toLon := (&world{}).od()

	plan := func(w *world) engine.Itinerary {
		t.Helper()
		resp, err := w.e.Plan(engine.Request{
			FromLat: fromLat, FromLon: fromLon, ToLat: toLat, ToLon: toLon,
			Mode: "transit", When: w.now, Live: true, Num: 3,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(resp.Itineraries) == 0 {
			t.Fatal("no itineraries")
		}
		return resp.Itineraries[0]
	}

	// 1) buses without any RT: not live
	wBus := buildWorld(t, worldOpts{})
	if it := plan(wBus); it.Live {
		t.Error("schedule-only bus itinerary must NOT be live")
	}

	// 2) buses with RT coverage: live
	srv := newRTServer()
	defer srv.Close()
	mgr := newManager(t, wBus, srv)
	srv.set(onTime("A1", "A2", "B1"))
	mgr.Start()
	waitVersion(t, mgr, 1)
	it := plan(wBus)
	if !it.Live {
		t.Error("RT-covered bus itinerary must be live")
	}
	for _, l := range it.Legs {
		if l.Mode == "transit" && !l.Realtime {
			t.Error("transit leg should carry realtime=true")
		}
	}

	// 3) metro without RT: live by definition, realtime flag stays honest
	wMetro := buildWorld(t, worldOpts{metroInsteadOfA: true})
	it = plan(wMetro)
	if !it.Live {
		t.Error("metro itinerary must be live even without RT")
	}
	for _, l := range it.Legs {
		if l.Mode == "transit" && l.Realtime {
			t.Error("metro leg must not fake realtime=true")
		}
	}
	_ = rt.Absent // package kept for symmetry with other files
}

func TestScheduleAssumedRailTracking(t *testing.T) {
	w := buildWorld(t, worldOpts{metroInsteadOfA: true})
	fromLat, fromLon, toLat, toLon := w.od()
	resp, err := w.e.Plan(engine.Request{
		FromLat: fromLat, FromLon: fromLon, ToLat: toLat, ToLon: toLon,
		Mode: "transit", When: w.now, Live: true, Num: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Itineraries) == 0 {
		t.Fatal("no rail itinerary")
	}
	it := resp.Itineraries[0]
	cached, ok := w.e.LookupItinerary(it.ID)
	if !ok {
		t.Fatal("planned itinerary was not cached")
	}

	rideIdx := -1
	for i := range cached.It.Legs {
		if cached.It.Legs[i].Mode == "transit" {
			rideIdx = i
			break
		}
	}
	if rideIdx < 0 || cached.It.Legs[rideIdx].Route == nil ||
		!transit.IsRailLikeRouteType(cached.It.Legs[rideIdx].Route.Type) {
		t.Fatal("planned itinerary has no rail-like ride")
	}
	trip := w.tt.TripIdx[cached.It.Legs[rideIdx].TripID]
	tlen := int(w.tt.TripLen(trip))
	now := time.Now().In(w.tt.TZ)
	nowSec := uint32(now.Sub(w.base).Seconds())
	depSec, arrSec := nowSec+1, nowSec+2

	// A TripUpdate moves the synthetic trip close to now. A cached, stale
	// VehiclePosition is also present: its age must remain independent from
	// the fresh TU/feed timestamp and force schedule-assumed tracking.
	o := &transit.RTOverlay{
		TripOff:   make([]int32, w.tt.NumTrips()),
		Skip:      make([]bool, w.tt.NumTrips()),
		HasRT:     make([]bool, w.tt.NumTrips()),
		Passed:    make([]int16, w.tt.NumTrips()),
		VehLat:    make([]int32, w.tt.NumTrips()),
		VehLon:    make([]int32, w.tt.NumTrips()),
		VehPos:    make([]int16, w.tt.NumTrips()),
		VehStatus: make([]int8, w.tt.NumTrips()),
		VehTime:   make([]uint64, w.tt.NumTrips()),
		FeedTime:  []uint64{uint64(now.Unix())},
	}
	for i := range o.TripOff {
		o.TripOff[i], o.Passed[i], o.VehPos[i], o.VehStatus[i] = -1, -1, -1, -1
	}
	o.TripOff[trip] = 0
	o.HasRT[trip] = true
	o.VehLat[trip], o.VehLon[trip] = int32(fromLat*1e7), int32(fromLon*1e7)
	o.VehPos[trip], o.VehStatus[trip] = 0, 1
	o.VehTime[trip] = uint64(now.Add(-10 * time.Minute).Unix())
	o.ArrDelta = make([]int32, tlen)
	o.DepDelta = make([]int32, tlen)
	o.StopSkip = make([]bool, tlen)
	o.DepDelta[0] = int32(depSec) - int32(w.tt.ScheduledDep(trip, 0))
	o.ArrDelta[tlen-1] = int32(arrSec) - int32(w.tt.ScheduledArr(trip, uint16(tlen-1)))
	w.tt.SetRT(o)

	// Skip the tiny access leg and make egress instantaneous: the test then
	// exercises the rail clock without wall-clock minutes of fixture travel.
	for i := 0; i < rideIdx; i++ {
		cached.It.Legs[i].Depart = now.Add(-3 * time.Minute)
		cached.It.Legs[i].Arrive = now.Add(-2 * time.Minute)
		cached.It.Legs[i].DurationS = 0
	}
	for i := rideIdx + 1; i < len(cached.It.Legs); i++ {
		cached.It.Legs[i].DurationS = 0
	}
	w.e.Cfg.Track.GPSStaleAfter = 100 * time.Millisecond

	tracker := &track.Tracker{E: w.e, Cfg: w.e.Cfg, Log: testLogger(), Tick: 20 * time.Millisecond}
	sink := make(chanSink, 64)
	fixes := make(chan track.Fix, 2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- tracker.Run(ctx, it.ID, sink, fixes) }()

	hello := waitFor(t, sink, "protocol 3 hello", 2*time.Second, func(ev map[string]any) bool {
		return ev["type"] == "hello"
	})
	if hello["protocol"] != float64(3) {
		t.Fatalf("protocol = %v, want 3", hello["protocol"])
	}
	// A fix becomes stale while underground. gps_lost must remain suspended
	// until the schedule-assumed rail block has completed.
	fixes <- track.Fix{Lat: fromLat, Lon: fromLon, AccuracyM: 10, At: time.Now()}

	warnings := 0
	sawRiding := false
	sawAlighted := false
	deadline := time.After(6 * time.Second)
	for !sawAlighted {
		select {
		case ev := <-sink:
			switch ev["type"] {
			case "vehicle":
				t.Fatal("stale rail VehiclePosition leaked into schedule-assumed guidance")
			case "warning":
				if ev["code"] == "gps_lost" {
					t.Fatal("gps_lost emitted inside schedule-assumed rail block")
				}
				if ev["code"] == "position_unavailable" {
					warnings++
				}
			case "progress":
				if ev["status"] == "riding" {
					sawRiding = true
					if ev["tracking_source"] != "schedule_assumed" {
						t.Fatalf("riding source = %v", ev["tracking_source"])
					}
				}
				if ev["status"] == "alighted" {
					sawAlighted = true
					if ev["tracking_source"] != "schedule_assumed" {
						t.Fatalf("alighting source = %v", ev["tracking_source"])
					}
				}
			}
		case <-deadline:
			t.Fatal("schedule-assumed rail progression timed out")
		}
	}
	if !sawRiding {
		t.Fatal("no schedule-assumed boarding progress")
	}
	if warnings != 1 {
		t.Fatalf("position_unavailable warnings = %d, want 1", warnings)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("tracking ended with %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("tracking did not finish after schedule-assumed alighting")
	}
}
