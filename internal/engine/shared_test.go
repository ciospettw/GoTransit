package engine

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
	"gotransit/internal/gbfs"
	"gotransit/internal/graph"
)

func TestSharedDirectUsesRealtimeVehicleAndRentalMetadata(t *testing.T) {
	const (
		baseLat = int32(419000000)
		baseLon = int32(125000000)
	)
	now := time.Now().UTC().Truncate(time.Second)
	var available atomic.Bool
	available.Store(true)
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/gbfs.json":
			fmt.Fprintf(w, `{"last_updated":%d,"ttl":0,"version":"2.3","data":{"feeds":[{"name":"free_bike_status","url":"%s/vehicles.json"}]}}`, now.Unix(), srv.URL)
		case "/vehicles.json":
			bikes := "[]"
			if available.Load() {
				bikes = fmt.Sprintf(`[{"bike_id":"bike-1","lat":41.9,"lon":12.5,"is_reserved":false,"is_disabled":false,"last_reported":%d}]`, now.Unix())
			}
			fmt.Fprintf(w, `{"last_updated":%d,"ttl":0,"version":"2.3","data":{"bikes":%s}}`, now.Unix(), bikes)
		}
	}))
	defer srv.Close()

	mgr := gbfs.NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)), []gbfs.Source{{
		Name: "share", URL: srv.URL + "/gbfs.json", MaxAge: time.Hour,
	}})
	if err := mgr.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	e := New(cfg)
	e.SetGraph(graph.SyntheticGrid(3, 3, baseLat, baseLon, 10000))
	e.GBFS = mgr

	it, ok := e.planSharedDirect(baseLat, baseLon, baseLat+20000, baseLon+20000, now, gbfs.ClassBicycle)
	if !ok {
		t.Fatal("expected a direct shared-bike route")
	}
	var rental *gbfs.Assignment
	for i := range it.Legs {
		if it.Legs[i].Rental != nil {
			rental = it.Legs[i].Rental
			if it.Legs[i].Mode != "bike" || it.Legs[i].DistanceM <= 0 {
				t.Fatalf("invalid rental leg: %+v", it.Legs[i])
			}
		}
	}
	if rental == nil || rental.VehicleID != "bike-1" || rental.RequiredRangeM <= 0 {
		t.Fatalf("missing rental metadata: %+v", rental)
	}

	available.Store(false)
	if err := mgr.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.planSharedDirect(baseLat, baseLon, baseLat+20000, baseLon+20000, now, gbfs.ClassBicycle); ok {
		t.Fatal("route still assigned a vehicle after it disappeared from realtime GBFS")
	}
}
