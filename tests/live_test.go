package tests

import (
	"testing"

	"gotransit/internal/engine"
	"gotransit/internal/rt"
)

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
