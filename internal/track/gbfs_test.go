package track

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"gotransit/internal/config"
	"gotransit/internal/engine"
	"gotransit/internal/gbfs"
)

func TestTrackerRejectsDisappearedFutureRental(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	var available atomic.Bool
	available.Store(true)
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/gbfs.json" {
			fmt.Fprintf(w, `{"last_updated":%d,"ttl":0,"version":"2.3","data":{"feeds":[{"name":"free_bike_status","url":"%s/vehicles"}]}}`, now.Unix(), srv.URL)
			return
		}
		bikes := "[]"
		if available.Load() {
			bikes = fmt.Sprintf(`[{"bike_id":"future-bike","lat":41.9,"lon":12.5,"is_reserved":false,"is_disabled":false,"last_reported":%d}]`, now.Unix())
		}
		fmt.Fprintf(w, `{"last_updated":%d,"ttl":0,"version":"2.3","data":{"bikes":%s}}`, now.Unix(), bikes)
	}))
	defer srv.Close()

	mgr := gbfs.NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)), []gbfs.Source{{
		Name: "share", URL: srv.URL + "/gbfs.json", MaxAge: time.Hour,
	}})
	if err := mgr.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	pickups := mgr.Snapshot().Pickups(gbfs.ClassBicycle, 41.9, 12.5, 100, now)
	if len(pickups) != 1 {
		t.Fatalf("pickups: %+v", pickups)
	}
	a := pickups[0]
	a.DropoffLat, a.DropoffLon, a.RequiredRangeM = 41.91, 12.51, 1000
	cfg := config.Default()
	e := engine.New(cfg)
	e.GBFS = mgr
	s := &session{t: &Tracker{E: e, Cfg: cfg, Log: slog.Default()}, legIdx: 0,
		it: engine.Itinerary{Legs: []engine.Leg{{Mode: "walk", Depart: now, Arrive: now.Add(time.Minute)},
			{Mode: "bike", Depart: now.Add(time.Minute), Arrive: now.Add(5 * time.Minute), Rental: &a}}},
		rentalStarted: map[int]bool{}}
	if _, _, _, unavailable := s.sharedUnavailable(now); unavailable {
		t.Fatal("available future rental was rejected")
	}
	available.Store(false)
	if err := mgr.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	idx, reason, _, unavailable := s.sharedUnavailable(now.Add(time.Second))
	if !unavailable || idx != 1 || reason != "vehicle_unavailable" {
		t.Fatalf("got idx=%d reason=%q unavailable=%v", idx, reason, unavailable)
	}
}

func TestPickBestPrefersEarlierBusOverSlowerSharedVehicle(t *testing.T) {
	now := time.Now()
	shared := engine.Itinerary{Live: true, Arrive: now.Add(20 * time.Minute),
		Legs: []engine.Leg{{Mode: "bike", DurationS: 1200, Rental: &gbfs.Assignment{Feed: "share"}}}}
	bus := engine.Itinerary{Live: false, Arrive: now.Add(12 * time.Minute),
		Legs: []engine.Leg{{Mode: "transit", Realtime: false}}}
	got := pickBest([]engine.Itinerary{shared, bus}, true)
	if len(got.Legs) == 0 || got.Legs[0].Mode != "transit" {
		t.Fatalf("slower shared vehicle displaced earlier bus: %+v", got)
	}
}
