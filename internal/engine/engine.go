// Package engine ties the street graph and the timetable together behind
// atomic pointers: queries grab a consistent snapshot, live updates build a
// fresh snapshot and swap it in — zero downtime, in-flight queries unharmed.
package engine

import (
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"gotransit/internal/config"
	"gotransit/internal/graph"
	"gotransit/internal/transit"
)

// GraphBundle pairs a graph with search-state pools sized for it.
type GraphBundle struct {
	G    *graph.Graph
	near sync.Pool
	road sync.Pool
}

func NewGraphBundle(g *graph.Graph) *GraphBundle {
	b := &GraphBundle{G: g}
	b.near.New = func() any { return graph.NewNearSearch(g.NumNodes()) }
	b.road.New = func() any { return graph.NewRoadSearch(g.NumNodes()) }
	return b
}

func (b *GraphBundle) Near() *graph.NearSearch     { return b.near.Get().(*graph.NearSearch) }
func (b *GraphBundle) PutNear(s *graph.NearSearch) { b.near.Put(s) }
func (b *GraphBundle) Road() *graph.RoadSearch     { return b.road.Get().(*graph.RoadSearch) }
func (b *GraphBundle) PutRoad(s *graph.RoadSearch) { b.road.Put(s) }

// TTBundle pairs a timetable with RAPTOR state pools sized for it.
type TTBundle struct {
	TT  *transit.Timetable
	rap sync.Pool
}

func NewTTBundle(tt *transit.Timetable) *TTBundle {
	b := &TTBundle{TT: tt}
	b.rap.New = func() any { return transit.NewRaptor(tt) }
	return b
}

func (b *TTBundle) Raptor() *transit.Raptor     { return b.rap.Get().(*transit.Raptor) }
func (b *TTBundle) PutRaptor(r *transit.Raptor) { b.rap.Put(r) }

// Engine is the live routing engine.
type Engine struct {
	Cfg *config.Config

	gb atomic.Pointer[GraphBundle]
	tb atomic.Pointer[TTBundle]

	// RTStats/RTVersion are wired by the realtime manager (nil-safe).
	RTStats   func() any
	RTChanged func() <-chan struct{}

	// planned-itinerary cache: tokens handed to clients for /v1/track
	itMu  sync.Mutex
	itins map[string]*CachedItinerary
	itSeq uint64

	Started time.Time
	// status counters
	GraphSwaps   atomic.Int64
	TTSwaps      atomic.Int64
	Queries      atomic.Int64
	LastGTFSSync atomic.Value // time.Time
	LastOSMSync  atomic.Value // time.Time
}

// CachedItinerary is a planned itinerary remembered for tracking.
type CachedItinerary struct {
	It      Itinerary
	Req     Request
	Created time.Time
}

// New creates an engine (graph/timetable installed separately during boot).
func New(cfg *config.Config) *Engine {
	return &Engine{Cfg: cfg, Started: time.Now(), itins: map[string]*CachedItinerary{}}
}

// SetGraph installs a new street graph (zero-downtime swap).
func (e *Engine) SetGraph(g *graph.Graph) {
	e.gb.Store(NewGraphBundle(g))
	e.GraphSwaps.Add(1)
}

// SetTimetable installs a new timetable (zero-downtime swap).
func (e *Engine) SetTimetable(tt *transit.Timetable) {
	e.tb.Store(NewTTBundle(tt))
	e.TTSwaps.Add(1)
}

// GraphBundle returns the current graph snapshot (nil before boot).
func (e *Engine) GraphBundle() *GraphBundle { return e.gb.Load() }

// TTBundle returns the current timetable snapshot (nil before boot).
func (e *Engine) TTBundle() *TTBundle { return e.tb.Load() }

// Ready reports whether both graph and timetable are installed.
func (e *Engine) Ready() bool { return e.gb.Load() != nil && e.tb.Load() != nil }

// Timezone is the transit network's timezone (from the GTFS agency). Every
// query is interpreted and answered in this zone, no matter what timezone
// the client or the host machine sit in.
func (e *Engine) Timezone() *time.Location {
	if tb := e.tb.Load(); tb != nil && tb.TT.TZ != nil {
		return tb.TT.TZ
	}
	return time.UTC
}

// Status is the /v1/status payload.
type Status struct {
	Uptime        string        `json:"uptime"`
	HeapMB        float64       `json:"heap_mb"`
	Queries       int64         `json:"queries"`
	Graph         GraphStatus   `json:"graph"`
	Transit       TransitStatus `json:"transit"`
	Realtime      any           `json:"realtime,omitempty"`
	Excluded      any           `json:"excluded_routes,omitempty"`
	UnsnappedStop int           `json:"stops_without_street_access"`
}

type GraphStatus struct {
	Nodes          int    `json:"nodes"`
	Edges          int    `json:"edges"`
	ReplicationSeq int64  `json:"osm_replication_seq"`
	ReplicationURL string `json:"osm_replication_url,omitempty"`
	Swaps          int64  `json:"live_swaps"`
	LastSync       string `json:"last_sync,omitempty"`
}

type TransitStatus struct {
	Feeds     []string `json:"feeds"`
	Stops     int      `json:"stops"`
	Patterns  int      `json:"patterns"`
	Trips     int      `json:"trips"`
	StopTimes int      `json:"stop_times"`
	Transfers int      `json:"transfers"`
	Swaps     int64    `json:"live_swaps"`
	LastSync  string   `json:"last_sync,omitempty"`
}

// Status snapshots engine health.
func (e *Engine) Status() Status {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	st := Status{
		Uptime:  time.Since(e.Started).Round(time.Second).String(),
		HeapMB:  float64(ms.HeapAlloc) / 1e6,
		Queries: e.Queries.Load(),
	}
	if gb := e.gb.Load(); gb != nil {
		st.Graph = GraphStatus{
			Nodes: gb.G.NumNodes(), Edges: gb.G.NumEdges(),
			ReplicationSeq: gb.G.ReplicationSeq, ReplicationURL: gb.G.ReplicationURL,
			Swaps: e.GraphSwaps.Load(),
		}
		if t, ok := e.LastOSMSync.Load().(time.Time); ok {
			st.Graph.LastSync = t.Format(time.RFC3339)
		}
	}
	if tb := e.tb.Load(); tb != nil {
		tt := tb.TT
		st.Transit = TransitStatus{
			Feeds: tt.Feeds, Stops: tt.NumStops(), Patterns: tt.NumPatterns(),
			Trips: tt.NumTrips(), StopTimes: len(tt.Arr), Transfers: len(tt.XferTo),
			Swaps: e.TTSwaps.Load(),
		}
		if t, ok := e.LastGTFSSync.Load().(time.Time); ok {
			st.Transit.LastSync = t.Format(time.RFC3339)
		}
		if len(tt.Excluded.Routes) > 0 {
			st.Excluded = tt.Excluded.Routes
		}
		st.UnsnappedStop = tt.Excluded.Unsnapped
	}
	if e.RTStats != nil {
		st.Realtime = e.RTStats()
	}
	return st
}

// LogExclusions prints the coverage report loudly, as required: routes whose
// shapes/stops leave the imported graph are not routed, and the operator must
// know at startup.
func (e *Engine) LogExclusions(logf func(format string, a ...any)) {
	tb := e.tb.Load()
	if tb == nil {
		return
	}
	ex := tb.TT.Excluded
	if ex.Trips == 0 {
		logf("coverage: all trips fit the imported OSM graph")
		return
	}
	logf("coverage: EXCLUDED %d trips on %d routes — their shapes/stops leave the imported OSM graph; no routing on them", ex.Trips, len(ex.Routes))
	max := len(ex.Routes)
	if max > 12 {
		max = 12
	}
	for _, er := range ex.Routes[:max] {
		logf("  - %s route %q (%s): %d trips excluded (%s)", er.Feed, er.Short, er.RouteID, er.Trips, er.Reason)
	}
	if len(ex.Routes) > max {
		logf("  - ... and %d more routes (full list in /v1/status)", len(ex.Routes)-max)
	}
	if ex.Unsnapped > 0 {
		logf("coverage: %d stops have no street within snap radius (no walk access, still rideable-through)", ex.Unsnapped)
	}
}
