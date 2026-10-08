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
	"gotransit/internal/gbfs"
	"gotransit/internal/graph"
	"gotransit/internal/memrelease"
	"gotransit/internal/stats"
	"gotransit/internal/transit"
)

// GraphBundle pairs a graph with search-state pools sized for it.
type GraphBundle struct {
	G    *graph.Graph
	near scratchPool[*graph.NearSearch]
	road scratchPool[*graph.RoadSearch]
}

func NewGraphBundle(g *graph.Graph) *GraphBundle {
	b := &GraphBundle{G: g}
	b.near.new = func() *graph.NearSearch { return graph.NewNearSearch(g.NumNodes()) }
	b.near.size = func(s *graph.NearSearch) uint64 { return s.MemoryBytes() }
	b.road.new = func() *graph.RoadSearch { return graph.NewRoadSearch(g.NumNodes()) }
	b.road.size = func(s *graph.RoadSearch) uint64 { return s.MemoryBytes() }
	return b
}

func (b *GraphBundle) Near() *graph.NearSearch     { return b.near.get() }
func (b *GraphBundle) PutNear(s *graph.NearSearch) { b.near.put(s) }
func (b *GraphBundle) Road() *graph.RoadSearch     { return b.road.get() }
func (b *GraphBundle) PutRoad(s *graph.RoadSearch) { b.road.put(s) }

// TTBundle pairs a timetable with RAPTOR state pools sized for it.
type TTBundle struct {
	TT  *transit.Timetable
	rap scratchPool[*transit.Raptor]
}

func NewTTBundle(tt *transit.Timetable) *TTBundle {
	b := &TTBundle{TT: tt}
	b.rap.new = func() *transit.Raptor { return transit.NewRaptor(tt) }
	b.rap.size = func(r *transit.Raptor) uint64 { return r.MemoryBytes() }
	return b
}

func (b *TTBundle) Raptor() *transit.Raptor     { return b.rap.get() }
func (b *TTBundle) PutRaptor(r *transit.Raptor) { b.rap.put(r) }

// Unlike sync.Pool, hot states survive idle-memory collection. Their count is
// derived from their real byte size and the controller's live memory budget.
type scratchPool[T any] struct {
	mu   sync.Mutex
	new  func() T
	size func(T) uint64
	max  func(uint64) int // test seam; nil uses the process memory controller
	idle []T
}

func (p *scratchPool[T]) get() T {
	p.mu.Lock()
	n := len(p.idle)
	if n > 0 {
		value := p.idle[n-1]
		var zero T
		p.idle[n-1] = zero
		p.idle = p.idle[:n-1]
		p.mu.Unlock()
		return value
	}
	p.mu.Unlock()
	return p.new()
}

func (p *scratchPool[T]) put(value T) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.idle) < p.limit(p.size(value)) {
		p.idle = append(p.idle, value)
	}
}

func (p *scratchPool[T]) limit(bytes uint64) int {
	if p.max != nil {
		return p.max(bytes)
	}
	return memrelease.IdleCacheEntries(bytes)
}

func (p *scratchPool[T]) trim(pressure bool) uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	limit := 0
	if !pressure && len(p.idle) > 0 {
		limit = p.limit(p.size(p.idle[0]))
	}
	var dropped uint64
	for len(p.idle) > limit {
		i := len(p.idle) - 1
		dropped += p.size(p.idle[i])
		var zero T
		p.idle[i] = zero
		p.idle = p.idle[:i]
	}
	return dropped
}

// Engine is the live routing engine.
type Engine struct {
	Cfg *config.Config

	// Stats supplies learned delay distributions to the probability model
	// (risk.go). Nil-safe: without it the analytic fallback still runs.
	Stats stats.Provider

	gb atomic.Pointer[GraphBundle]
	tb atomic.Pointer[TTBundle]

	// RTStats/RTVersion are wired by the realtime manager (nil-safe).
	RTStats   func() any
	RTChanged func() <-chan struct{}
	// GBFS is the live shared-mobility source used by bike/scooter+transit.
	// Nil preserves the legacy personal-bike behavior for deployments that
	// do not configure any [[gbfs]] system.
	GBFS             *gbfs.Manager
	GBFSChanged      func() <-chan struct{}
	SharedConfigured bool

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
	return &Engine{Cfg: cfg, Started: time.Now(), itins: map[string]*CachedItinerary{},
		SharedConfigured: len(cfg.GBFS) > 0}
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

// TrimScratch applies the current byte budget to the active immutable bundles.
// It is called immediately before the runtime scavenges idle pages.
func (e *Engine) TrimScratch(pressure bool) uint64 {
	var dropped uint64
	if b := e.gb.Load(); b != nil {
		dropped += b.near.trim(pressure)
		dropped += b.road.trim(pressure)
	}
	if b := e.tb.Load(); b != nil {
		dropped += b.rap.trim(pressure)
	}
	return dropped
}

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
	Uptime         string            `json:"uptime"`
	HeapMB         float64           `json:"heap_mb"`
	MemoryControl  memrelease.Status `json:"memory_control"`
	Queries        int64             `json:"queries"`
	Graph          GraphStatus       `json:"graph"`
	Transit        TransitStatus     `json:"transit"`
	Realtime       any               `json:"realtime,omitempty"`
	SharedMobility any               `json:"shared_mobility,omitempty"`
	Excluded       any               `json:"excluded_routes,omitempty"`
	UnsnappedStop  int               `json:"stops_without_street_access"`
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
		Uptime:        time.Since(e.Started).Round(time.Second).String(),
		HeapMB:        float64(ms.HeapAlloc) / 1e6,
		MemoryControl: memrelease.Snapshot(),
		Queries:       e.Queries.Load(),
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
	if e.GBFS != nil {
		st.SharedMobility = e.GBFS.Status()
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
