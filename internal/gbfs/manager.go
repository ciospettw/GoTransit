package gbfs

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Source struct {
	Name          string
	URL           string // gbfs.json auto-discovery endpoint
	Language      string // v1.x language key; falls back to en/first present
	Poll          time.Duration
	MaxAge        time.Duration
	AllowInsecure bool
	Headers       map[string]string
}

type sourceState struct {
	source       Source
	version      string
	links        map[string]string
	discoveredAt time.Time
	docs         map[string][]byte
	staticAt     time.Time
}

// Manager polls all configured systems. Readers only ever observe complete
// snapshots: a half-downloaded GBFS update is never published.
type Manager struct {
	log    *slog.Logger
	client *http.Client

	mu      sync.Mutex
	states  map[string]*sourceState
	feeds   map[string]*Feed
	version uint64
	changed chan struct{}
	snap    atomic.Pointer[Snapshot]
}

func NewManager(log *slog.Logger, sources []Source) *Manager {
	if log == nil {
		log = slog.Default()
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	m := &Manager{log: log, client: &http.Client{Timeout: 20 * time.Second, Transport: tr},
		states: map[string]*sourceState{}, feeds: map[string]*Feed{}, changed: make(chan struct{})}
	for _, src := range sources {
		if src.Poll <= 0 {
			src.Poll = 20 * time.Second
		}
		if src.MaxAge <= 0 {
			src.MaxAge = 5 * time.Minute
		}
		m.states[src.Name] = &sourceState{source: src}
	}
	m.publishLocked()
	return m
}

func (m *Manager) Snapshot() *Snapshot { return m.snap.Load() }

func (m *Manager) Changed() <-chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.changed
}

func (m *Manager) Status() Status { return m.Snapshot().Status(time.Now()) }

// Refresh performs one synchronous refresh of every source. It returns an
// error only when every configured source failed; healthy systems remain
// usable when one operator is down.
func (m *Manager) Refresh(ctx context.Context) error {
	m.mu.Lock()
	states := make([]*sourceState, 0, len(m.states))
	for _, st := range m.states {
		states = append(states, st)
	}
	m.mu.Unlock()
	if len(states) == 0 {
		return nil
	}
	type result struct {
		st   *sourceState
		feed *Feed
		err  error
	}
	results := make(chan result, len(states))
	for _, st := range states {
		go func(st *sourceState) {
			f, err := m.refreshSource(ctx, st)
			results <- result{st: st, feed: f, err: err}
		}(st)
	}
	okCount := 0
	var firstErr error
	for range states {
		r := <-results
		if r.err != nil {
			if firstErr == nil {
				firstErr = r.err
			}
			m.recordError(r.st.source.Name, r.err)
			continue
		}
		okCount++
		m.install(r.feed)
	}
	if okCount == 0 {
		return firstErr
	}
	return nil
}

// Start launches one independent poller per system. A failed operator never
// delays another one, and the last good snapshot remains readable until its
// declared freshness window expires.
func (m *Manager) Start(ctx context.Context) {
	m.mu.Lock()
	states := make([]*sourceState, 0, len(m.states))
	for _, st := range m.states {
		states = append(states, st)
	}
	m.mu.Unlock()
	for _, st := range states {
		go func(st *sourceState) {
			ticker := time.NewTicker(st.source.Poll)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					feed, err := m.refreshSource(ctx, st)
					if err != nil {
						m.log.Warn("GBFS refresh failed", "feed", st.source.Name, "err", err)
						m.recordError(st.source.Name, err)
						continue
					}
					m.install(feed)
				}
			}
		}(st)
	}
}

func (m *Manager) refreshSource(ctx context.Context, st *sourceState) (*Feed, error) {
	// Discovery URLs may change. Re-read hourly, and immediately after a
	// missing/failed link (the caller preserves the last good snapshot).
	stNeedsDiscovery := len(st.links) == 0 || time.Since(st.discoveredAt) >= time.Hour
	if stNeedsDiscovery {
		body, err := m.fetch(ctx, st.source, st.source.URL)
		if err != nil {
			return nil, fmt.Errorf("%s discovery: %w", st.source.Name, err)
		}
		version, links, err := parseDiscovery(body, st.source.Language)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", st.source.Name, err)
		}
		base, err := url.Parse(st.source.URL)
		if err != nil {
			return nil, err
		}
		resolved := map[string]string{}
		for _, link := range links {
			u, err := url.Parse(link.URL)
			if err == nil && link.Name != "" {
				resolved[link.Name] = base.ResolveReference(u).String()
			}
		}
		st.version, st.links, st.discoveredAt = version, resolved, time.Now()
	}
	refreshStatic := len(st.docs) == 0 || stNeedsDiscovery || time.Since(st.staticAt) >= 10*time.Minute

	wanted := []string{"system_information", "vehicle_types", "station_information", "station_status",
		"vehicle_status", "free_bike_status", "geofencing_zones"}
	type fetched struct {
		name string
		body []byte
		err  error
	}
	result := make(chan fetched, len(wanted))
	n := 0
	for _, name := range wanted {
		u := st.links[name]
		if u == "" {
			continue
		}
		dynamic := name == "station_status" || name == "vehicle_status" || name == "free_bike_status"
		if !dynamic && !refreshStatic && len(st.docs[name]) > 0 {
			continue
		}
		n++
		go func(name, u string) {
			body, err := m.fetch(ctx, st.source, u)
			result <- fetched{name: name, body: body, err: err}
		}(name, u)
	}
	docs := make(map[string][]byte, len(st.docs)+n)
	for name, body := range st.docs {
		docs[name] = body
	}
	for i := 0; i < n; i++ {
		r := <-result
		if r.err != nil {
			return nil, fmt.Errorf("%s %s: %w", st.source.Name, r.name, r.err)
		}
		docs[r.name] = r.body
	}
	if len(docs["station_status"]) == 0 && len(docs["vehicle_status"]) == 0 && len(docs["free_bike_status"]) == 0 {
		return nil, fmt.Errorf("%s has neither station_status nor vehicle_status/free_bike_status", st.source.Name)
	}
	feed, err := parseFeed(st.source.Name, st.version, docs, time.Now(), st.source.MaxAge)
	if err != nil {
		return nil, err
	}
	// Commit endpoint bytes only after the whole set parsed successfully.
	st.docs = docs
	if refreshStatic {
		st.staticAt = time.Now()
	}
	return feed, nil
}

func (m *Manager) fetch(ctx context.Context, src Source, rawURL string) ([]byte, error) {
	if strings.HasPrefix(rawURL, "file://") || !strings.Contains(rawURL, "://") {
		path := strings.TrimPrefix(rawURL, "file://")
		if !filepath.IsAbs(path) && strings.HasPrefix(src.URL, "file://") {
			path = filepath.Join(filepath.Dir(strings.TrimPrefix(src.URL, "file://")), path)
		}
		return os.ReadFile(path)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "GoTransit/GBFS")
	for k, v := range src.Headers {
		req.Header.Set(k, v)
	}
	client := m.client
	if src.AllowInsecure {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // explicitly opted into by config
		client = &http.Client{Timeout: m.client.Timeout, Transport: tr}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %s", resp.Status)
	}
	const maxDocument = 32 << 20
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxDocument+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxDocument {
		return nil, fmt.Errorf("document exceeds %d MiB", maxDocument>>20)
	}
	return b, nil
}

func (m *Manager) install(feed *Feed) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.feeds[feed.Name] = feed
	m.publishLocked()
}

func (m *Manager) recordError(name string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	old := m.feeds[name]
	if old == nil {
		old = &Feed{Name: name, Stations: map[string]Station{}, Vehicles: map[string]Vehicle{}, VehicleTypes: map[string]VehicleType{}}
	}
	cp := *old
	cp.LastError = err.Error()
	m.feeds[name] = &cp
	m.publishLocked()
}

func (m *Manager) publishLocked() {
	m.version++
	feeds := make(map[string]*Feed, len(m.feeds))
	for k, v := range m.feeds {
		feeds[k] = v
	}
	m.snap.Store(&Snapshot{Version: m.version, At: time.Now(), Feeds: feeds})
	close(m.changed)
	m.changed = make(chan struct{})
}
