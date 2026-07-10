// Package tests hosts the whole test suite, black-box style, against the
// engine's exported APIs. Unit and synthetic tests run everywhere; the
// real-data suite activates when GOTRANSIT_TEST_DATA points to a directory
// holding osm.pbf / gtfs-*.zip captures.
package tests

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"gotransit/internal/config"
	"gotransit/internal/engine"
	"gotransit/internal/graph"
	"gotransit/internal/gtfs"
	"gotransit/internal/rt"
	"gotransit/internal/transit"
)

const (
	oLat = int32(419000000)
	oLon = int32(125000000)
	step = int32(45000)
)

// world is the synthetic universe: a street grid with two stops ~1.1 km
// apart, a fast line A (two runs), a slower line B, all anchored to "now".
type world struct {
	g    *graph.Graph
	tt   *transit.Timetable
	e    *engine.Engine
	base time.Time
	now  time.Time
}

// worldOpts tweaks the fixture.
type worldOpts struct {
	metroInsteadOfA bool // line A becomes a metro (route type 1)
}

func buildWorld(t *testing.T, opts worldOpts) *world {
	t.Helper()
	g := graph.SyntheticGrid(4, 4, oLat, oLon, step)

	tz, _ := time.LoadLocation("Europe/Rome")
	now := time.Now().In(tz)
	base := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, tz)
	sec := func(d time.Duration) uint32 { return uint32(now.Add(d).Sub(base).Seconds()) }

	f := &gtfs.Feed{Name: "test", TZ: "Europe/Rome", StopIdx: map[string]int32{}}
	addStop := func(id string, lat, lon int32) {
		f.StopIdx[id] = int32(len(f.Stops))
		f.Stops = append(f.Stops, gtfs.Stop{ID: id, Name: "Stop " + id, Lat: lat, Lon: lon, OK: true})
	}
	addStop("SA", oLat+1000, oLon+1000)
	addStop("SB", oLat+1000, oLon+3*step-1000)

	typeA := 3
	if opts.metroInsteadOfA {
		typeA = 1 // metro: always considered live
	}
	f.Routes = []gtfs.Route{{ID: "A", Short: "A", Type: typeA, Color: "FF0000"}, {ID: "B", Short: "B", Type: 3, Color: "0000FF"}}
	f.RouteIdx = map[string]int32{"A": 0, "B": 1}
	f.Services = []gtfs.Service{{Mask: 0x7F, Start: 20200101, End: 20301231}}
	f.ServiceIdx = map[string]int32{"S": 0}
	f.ShapeIdx = map[string]int32{}
	f.Headsigns = []string{""}

	addTrip := func(id string, route int32, dep, arr uint32) {
		f.Trips = append(f.Trips, gtfs.Trip{RouteIdx: route, ServiceIdx: 0, ShapeIdx: -1, ID: id})
		f.TripSTOff = append(f.TripSTOff, uint32(len(f.STArr)))
		f.STArr = append(f.STArr, dep, arr)
		f.STDep = append(f.STDep, dep, arr)
		f.STStop = append(f.STStop, f.StopIdx["SA"], f.StopIdx["SB"])
		f.STSeq = append(f.STSeq, 1, 2)
	}
	addTrip("A1", 0, sec(4*time.Minute), sec(10*time.Minute))
	addTrip("A2", 0, sec(14*time.Minute), sec(20*time.Minute))
	addTrip("B1", 1, sec(6*time.Minute), sec(12*time.Minute))
	f.TripSTOff = append(f.TripSTOff, uint32(len(f.STArr)))

	tt, _, err := transit.Compile([]*gtfs.Feed{f}, g, 4.8, 400, 300)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.OSM.URL = "synthetic"
	cfg.Feeds = []config.Feed{{Name: "test", URL: "synthetic"}}
	e := engine.New(cfg)
	e.SetGraph(g)
	e.SetTimetable(tt)
	return &world{g: g, tt: tt, e: e, base: base, now: now}
}

// worldODs returns the from/to coordinates near SA and SB.
func (w *world) od() (fromLat, fromLon, toLat, toLon float64) {
	return float64(oLat+2000) / 1e7, float64(oLon+2000) / 1e7,
		float64(oLat+2000) / 1e7, float64(oLon+3*step-2000) / 1e7
}

// ---- mutable GTFS-RT server -----------------------------------------------------

type rtServer struct {
	*httptest.Server
	payload atomic.Pointer[[]byte]
	stamp   atomic.Uint64
}

func newRTServer() *rtServer {
	s := &rtServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p := s.payload.Load(); p != nil {
			w.Write(*p)
		}
	}))
	s.stamp.Store(uint64(time.Now().Unix()))
	return s
}

func (s *rtServer) set(f *rt.Feed) {
	f.Timestamp = s.stamp.Add(1)
	b := rt.Encode(f)
	s.payload.Store(&b)
}

// onTime builds a feed with on-time updates for the given trips.
func onTime(trips ...string) *rt.Feed {
	f := &rt.Feed{}
	for _, id := range trips {
		f.Trips = append(f.Trips, rt.TripRT{
			TripID: id, DelaySec: 0, HasDelay: true,
			STUs: []rt.STU{{Seq: 1, StopID: "SA", ArrDelay: 0, DepDelay: 0}},
		})
	}
	return f
}

func newManager(t *testing.T, w *world, srv *rtServer) *rt.Manager {
	t.Helper()
	mgr := rt.NewManager(testLogger(), []rt.Source{{
		FeedIdx: 0, Name: "test", TripUpdates: srv.URL, Poll: 50 * time.Millisecond,
	}}, func() *transit.Timetable { return w.tt })
	return mgr
}

func waitVersion(t *testing.T, mgr *rt.Manager, min uint64) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for mgr.Version() < min {
		if time.Now().After(deadline) {
			t.Fatalf("rt manager never reached version %d", min)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// ---- event sink ------------------------------------------------------------------

type chanSink chan map[string]any

func (c chanSink) Send(ev any) error {
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	var m map[string]any
	json.Unmarshal(b, &m)
	select {
	case c <- m:
	case <-time.After(2 * time.Second):
		return fmt.Errorf("sink full")
	}
	return nil
}

func waitFor(t *testing.T, ch chanSink, what string, timeout time.Duration, pred func(map[string]any) bool) map[string]any {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case ev := <-ch:
			t.Logf("event: %s %v", ev["type"], summarize(ev))
			if pred(ev) {
				return ev
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
			return nil
		}
	}
}

func summarize(ev map[string]any) string {
	switch ev["type"] {
	case "reroute":
		return fmt.Sprint(ev["reason"], " saving=", ev["saving_s"])
	case "delay":
		return fmt.Sprint("arrive_delta_s=", ev["arrive_delta_s"])
	case "progress":
		return fmt.Sprint(ev["status"], " leg=", ev["leg_index"])
	case "warning":
		return fmt.Sprint(ev["code"])
	case "vehicle":
		name := ""
		if m, ok := ev["stop"].(map[string]any); ok {
			name = fmt.Sprint(m["name"])
		}
		return fmt.Sprint(ev["status"], " @", name, " away=", ev["stops_away"], " delay=", ev["delay_s"])
	}
	return ""
}

func firstRideTrip(ev map[string]any) string {
	it, _ := ev["itinerary"].(map[string]any)
	legs, _ := it["legs"].([]any)
	for _, l := range legs {
		leg := l.(map[string]any)
		if leg["mode"] == "transit" {
			return leg["trip_id"].(string)
		}
	}
	return ""
}

func rideOf(t *testing.T, it engine.Itinerary) string {
	t.Helper()
	for _, l := range it.Legs {
		if l.Mode == "transit" {
			return l.TripID
		}
	}
	t.Fatal("no transit leg")
	return ""
}

// ---- misc ------------------------------------------------------------------------

func testDataDir() string { return os.Getenv("GOTRANSIT_TEST_DATA") }

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

func hhmm(s uint32) string {
	return fmt.Sprintf("%02d:%02d:%02d", s/3600%24, s/60%60, s%60)
}
