package rt

import (
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"gotransit/internal/transit"
)

// Source is one operator's GTFS-RT endpoints, tied to a static feed index.
type Source struct {
	FeedIdx          int
	Name             string
	TripUpdates      string
	VehiclePositions string
	Poll             time.Duration
	Insecure         bool
}

// Manager polls every source, projects the union onto the current timetable
// as an immutable RTOverlay, and broadcasts a version bump to whoever tracks.
type Manager struct {
	Log     *slog.Logger
	Sources []Source
	TT      func() *transit.Timetable // current snapshot getter

	mu      sync.Mutex
	tu      map[int]*Feed // last decoded trip updates per source
	vp      map[int]*Feed
	seen    map[string]string // url → content sha (change detection)
	stats   map[int]*SourceStats
	version atomic.Uint64
	notify  chan struct{}
}

// SourceStats reports one source's health (exposed in /v1/status).
type SourceStats struct {
	Name       string `json:"name"`
	Trips      int    `json:"trips"`
	Vehicles   int    `json:"vehicles"`
	Matched    int    `json:"matched_trips"`
	Unmatched  int    `json:"unmatched_trips"`
	Cancelled  int    `json:"cancelled"`
	FeedTime   string `json:"feed_time,omitempty"`
	LastPoll   string `json:"last_poll,omitempty"`
	LastChange string `json:"last_change,omitempty"`
	Errors     int    `json:"errors"`
	LastError  string `json:"last_error,omitempty"`
}

// NewManager wires sources to a timetable getter.
func NewManager(log *slog.Logger, sources []Source, tt func() *transit.Timetable) *Manager {
	m := &Manager{
		Log: log, Sources: sources, TT: tt,
		tu: map[int]*Feed{}, vp: map[int]*Feed{},
		seen: map[string]string{}, stats: map[int]*SourceStats{},
		notify: make(chan struct{}),
	}
	for _, s := range sources {
		m.stats[s.FeedIdx] = &SourceStats{Name: s.Name}
	}
	return m
}

// Start launches one polling loop per source.
func (m *Manager) Start() {
	for i := range m.Sources {
		go m.loop(m.Sources[i])
	}
}

// Version returns the current overlay version (0 = none yet).
func (m *Manager) Version() uint64 { return m.version.Load() }

// Changed returns a channel closed at the next overlay change.
func (m *Manager) Changed() <-chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.notify
}

// Stats snapshots per-source health.
func (m *Manager) Stats() []SourceStats {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]SourceStats, 0, len(m.stats))
	for _, s := range m.Sources {
		out = append(out, *m.stats[s.FeedIdx])
	}
	return out
}

// FeedFresh reports whether a static feed has live coverage right now:
// a configured source whose last feed timestamp is recent.
func (m *Manager) FeedFresh(feedIdx int, maxAge time.Duration) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	f := m.tu[feedIdx]
	if f == nil {
		return false
	}
	return time.Since(time.Unix(int64(f.Timestamp), 0)) <= maxAge
}

func (m *Manager) loop(src Source) {
	// first fetch immediately, then tick
	m.pollOnce(src)
	tick := time.NewTicker(src.Poll)
	defer tick.Stop()
	for range tick.C {
		m.pollOnce(src)
	}
}

func (m *Manager) pollOnce(src Source) {
	changed := false
	st := m.stats[src.FeedIdx]
	for _, u := range []struct {
		url string
		vp  bool
	}{{src.TripUpdates, false}, {src.VehiclePositions, true}} {
		if u.url == "" {
			continue
		}
		data, err := fetch(u.url, src.Insecure)
		if err != nil {
			m.mu.Lock()
			st.Errors++
			st.LastError = err.Error()
			m.mu.Unlock()
			continue
		}
		sum := fmt.Sprintf("%x", sha256.Sum256(data))
		m.mu.Lock()
		same := m.seen[u.url] == sum
		m.seen[u.url] = sum
		st.LastPoll = time.Now().Format(time.RFC3339)
		m.mu.Unlock()
		if same {
			continue
		}
		f, err := Decode(data)
		if err != nil {
			m.mu.Lock()
			st.Errors++
			st.LastError = err.Error()
			m.mu.Unlock()
			m.Log.Warn("gtfs-rt decode failed", "source", src.Name, "url", u.url, "err", err)
			continue
		}
		m.mu.Lock()
		if u.vp {
			m.vp[src.FeedIdx] = f
		} else {
			m.tu[src.FeedIdx] = f
		}
		st.LastChange = time.Now().Format(time.RFC3339)
		m.mu.Unlock()
		changed = true
	}
	if changed {
		m.Rebuild()
	}
}

// Rebuild projects the latest feeds onto the CURRENT timetable and swaps the
// overlay in. Also called after a timetable swap (static GTFS/OSM update).
func (m *Manager) Rebuild() {
	tt := m.TT()
	if tt == nil {
		return
	}
	m.mu.Lock()
	tu, vp := m.tu, m.vp
	ver := m.version.Load() + 1
	o := buildOverlay(tt, m.Sources, tu, vp, m.stats, time.Now(), ver)
	m.mu.Unlock()

	tt.SetRT(o)
	m.version.Store(ver)

	m.mu.Lock()
	close(m.notify)
	m.notify = make(chan struct{})
	m.mu.Unlock()
}

func fetch(url string, insecure bool) ([]byte, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	if insecure {
		client.Transport = http.DefaultTransport
	}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 64<<20))
}
