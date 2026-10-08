package updater

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"gotransit/internal/config"
	"gotransit/internal/engine"
	"gotransit/internal/graph"
	"gotransit/internal/gtfs"
	"gotransit/internal/memrelease"
	"gotransit/internal/osm"
	"gotransit/internal/transit"
)

// feedData is the in-memory life of one GTFS source.
type feedData struct {
	cfg config.Feed

	// remote feeds
	zip     []byte
	path    string
	etag    string
	lastMod string
	sha     string
	warned  bool // logged "server ignores conditionals" once

	// local feeds
	mtime time.Time
}

const graphSourceCache = "graph-source.gts"

// Updater owns live source data. With [cache], immutable source payloads are
// file-backed and loaded only for rebuilds; ephemeral deployments retain them
// in RAM and preserve the original zero-disk behavior.
type Updater struct {
	E   *engine.Engine
	Cfg *config.Config
	Log *slog.Logger

	// OnSwap runs after every timetable swap (e.g. re-project GTFS-RT).
	OnSwap func()

	cache Cache // optional on-disk cache: refreshed zips are persisted for warm restarts

	mu      sync.Mutex
	srcBlob []byte
	srcPath string
	feeds   map[string]*feedData

	rebuildMu chan struct{}
}

// New creates the updater.
func New(e *engine.Engine, cfg *config.Config, log *slog.Logger) *Updater {
	u := &Updater{
		E: e, Cfg: cfg, Log: log,
		cache:     Cache{Dir: cfg.Cache.Dir},
		feeds:     map[string]*feedData{},
		rebuildMu: make(chan struct{}, 1),
	}
	for _, f := range cfg.Feeds {
		u.feeds[f.Name] = &feedData{cfg: f}
	}
	return u
}

// LoadCachedGraphSource installs and decodes a source image when it belongs to
// the configured OSM URL. Corrupt or old images are discarded and rebuilt.
func (u *Updater) LoadCachedGraphSource() (*graph.SrcData, bool) {
	if _, ok := u.cache.Meta(graphSourceCache, u.Cfg.OSM.URL); !ok {
		return nil, false
	}
	path, ok := u.cache.FilePath(graphSourceCache)
	if !ok {
		return nil, false
	}
	src, err := graph.LoadStore(path)
	if err != nil {
		if u.Log != nil {
			u.Log.Warn("cached graph source is invalid; rebuilding from PBF", "err", err)
		}
		u.cache.Remove(graphSourceCache)
		return nil, false
	}
	u.mu.Lock()
	u.srcPath, u.srcBlob = path, nil
	u.mu.Unlock()
	return src, true
}

// DiscardCachedGraphSource removes a derived source that cannot participate in
// the supported live-update path. Its original PBF can then be revalidated.
func (u *Updater) DiscardCachedGraphSource() {
	u.mu.Lock()
	u.srcPath, u.srcBlob = "", nil
	u.mu.Unlock()
	u.cache.Remove(graphSourceCache)
}

// SetGraphSource persists the compact update/restart image when cache is
// available. On disk failure it falls back to the in-memory representation.
func (u *Updater) SetGraphSource(src *graph.SrcData) (bool, error) {
	if u.cache.Enabled() {
		path, err := u.cache.StoreGenerated(graphSourceCache, u.Cfg.OSM.URL, src.SaveStore)
		if err == nil {
			u.mu.Lock()
			u.srcPath, u.srcBlob = path, nil
			u.mu.Unlock()
			return true, nil
		}
		if u.Log != nil {
			u.Log.Warn("graph source cache write failed; retaining it in RAM", "err", err)
		}
	}
	blob, err := src.EncodeStore()
	if err != nil {
		return false, err
	}
	u.mu.Lock()
	u.srcPath, u.srcBlob = "", blob
	u.mu.Unlock()
	return false, nil
}

func (u *Updater) loadGraphSource() (*graph.SrcData, error) {
	u.mu.Lock()
	path, blob := u.srcPath, u.srcBlob
	u.mu.Unlock()
	if path != "" {
		return graph.LoadStore(path)
	}
	if blob == nil {
		return nil, fmt.Errorf("no graph source available")
	}
	return graph.DecodeStore(blob)
}

// InstallFeedZip records a remote feed's zip bytes and validators.
func (u *Updater) InstallFeedZip(name string, res CondResult) {
	u.mu.Lock()
	fd := u.feeds[name]
	fd.zip, fd.path = res.Data, ""
	fd.etag, fd.lastMod, fd.sha = res.ETag, res.LastMod, res.SHA256
	u.mu.Unlock()
}

// InstallFeedFile records an already cached remote ZIP without retaining a
// duplicate byte slice in the heap.
func (u *Updater) InstallFeedFile(name, path string, res CondResult) {
	u.mu.Lock()
	fd := u.feeds[name]
	fd.zip, fd.path = nil, path
	fd.etag, fd.lastMod, fd.sha = res.ETag, res.LastMod, res.SHA256
	u.mu.Unlock()
}

// MarkLocalFeed records a local feed's current mtime.
func (u *Updater) MarkLocalFeed(name string, mtime time.Time) {
	u.mu.Lock()
	u.feeds[name].mtime = mtime
	u.mu.Unlock()
}

// LoadFeeds parses every feed from its selected RAM or file backing.
func (u *Updater) LoadFeeds() ([]*gtfs.Feed, error) {
	var feeds []*gtfs.Feed
	for _, f := range u.Cfg.Feeds {
		var fd *gtfs.Feed
		var err error
		if f.Local() {
			fd, err = gtfs.Load(config.LocalPath(f.URL), f.Name)
		} else {
			u.mu.Lock()
			data, path := u.feeds[f.Name].zip, u.feeds[f.Name].path
			u.mu.Unlock()
			switch {
			case path != "":
				fd, err = gtfs.Load(path, f.Name)
			case data != nil:
				fd, err = gtfs.LoadBytes(data, f.Name)
			default:
				return nil, fmt.Errorf("feed %s: no source data", f.Name)
			}
		}
		if err != nil {
			return nil, fmt.Errorf("feed %s: %w", f.Name, err)
		}
		u.Log.Info(fd.LoadStats)
		feeds = append(feeds, fd)
	}
	return feeds, nil
}

// Start launches all polling loops.
func (u *Updater) Start() {
	for _, f := range u.Cfg.Feeds {
		go u.gtfsLoop(f)
	}
	gb := u.E.GraphBundle()
	replURL := ""
	if gb != nil {
		replURL = gb.G.ReplicationURL
	}
	if strings.Contains(replURL, "download.geofabrik.de") {
		go func() {
			if err := u.syncOSM(replURL); err != nil {
				u.Log.Error("initial osm sync failed", "err", err)
			}
			u.osmLoop(replURL)
		}()
	} else {
		u.Log.Warn("OSM source has no Geofabrik replication stream: .osc live updates UNSUPPORTED, the street graph will age",
			"replication_url", replURL,
			"hint", "import a download.geofabrik.de .osm.pbf to get zero-downtime updates")
	}
}

// ---- GTFS ------------------------------------------------------------------

func (u *Updater) gtfsLoop(f config.Feed) {
	tick := time.NewTicker(f.Poll)
	defer tick.Stop()
	for range tick.C {
		var err error
		if f.Local() {
			err = u.syncLocalFeed(f)
		} else {
			err = u.syncRemoteFeed(f)
		}
		if err != nil {
			u.Log.Error("gtfs sync failed", "feed", f.Name, "err", err)
		}
	}
}

func (u *Updater) syncRemoteFeed(f config.Feed) error {
	u.mu.Lock()
	fd := u.feeds[f.Name]
	etag, lastMod, sha := fd.etag, fd.lastMod, fd.sha
	u.mu.Unlock()

	res, err := FetchBytesCond(f.URL, f.AllowInsecure, etag, lastMod, sha, f.Headers)
	if err != nil {
		return err
	}
	if res.Ignored {
		u.mu.Lock()
		warned := fd.warned
		fd.warned = true
		u.mu.Unlock()
		if !warned {
			u.Log.Warn("gtfs server ignores conditional requests: every poll re-downloads the zip; consider a longer poll",
				"feed", f.Name, "poll", f.Poll.String())
		}
	}
	if !res.Changed {
		u.Log.Debug("gtfs unchanged", "feed", f.Name)
		return nil
	}
	u.Log.Info("gtfs changed, rebuilding timetable in background", "feed", f.Name,
		"bytes", len(res.Data), "download", res.Duration.Round(time.Millisecond))
	cacheName := "gtfs-" + f.Name + ".zip"
	if err := u.cache.StoreBytes(cacheName, f.URL, res.Data, res.ETag, res.LastMod, res.SHA256); err != nil {
		u.Log.Warn("gtfs cache write failed", "feed", f.Name, "err", err)
		u.InstallFeedZip(f.Name, res)
	} else if path, ok := u.cache.FilePath(cacheName); ok {
		u.InstallFeedFile(f.Name, path, res)
	} else {
		u.InstallFeedZip(f.Name, res)
	}
	if err := u.RebuildTimetable(); err != nil {
		return err
	}
	u.E.LastGTFSSync.Store(time.Now())
	return nil
}

func (u *Updater) syncLocalFeed(f config.Feed) error {
	info, err := os.Stat(config.LocalPath(f.URL))
	if err != nil {
		return err
	}
	u.mu.Lock()
	fd := u.feeds[f.Name]
	changed := info.ModTime() != fd.mtime
	fd.mtime = info.ModTime()
	u.mu.Unlock()
	if !changed {
		return nil
	}
	u.Log.Info("local gtfs changed on disk, rebuilding timetable", "feed", f.Name)
	if err := u.RebuildTimetable(); err != nil {
		return err
	}
	u.E.LastGTFSSync.Store(time.Now())
	return nil
}

// RebuildTimetable re-parses every feed and swaps a fresh timetable in.
// The street graph is untouched — no graph rebuild, ever.
func (u *Updater) RebuildTimetable() error {
	u.rebuildMu <- struct{}{}
	defer func() { <-u.rebuildMu }()
	done := memrelease.Begin()
	defer done()

	gb := u.E.GraphBundle()
	if gb == nil {
		return fmt.Errorf("graph not ready")
	}
	t0 := time.Now()
	feeds, err := u.LoadFeeds()
	if err != nil {
		return err
	}
	tt, st, err := transit.Compile(feeds, gb.G,
		u.Cfg.Routing.WalkSpeedKmh, u.Cfg.Routing.TransferRadiusM, u.Cfg.Routing.SnapRadiusM)
	if err != nil {
		return err
	}
	u.E.SetTimetable(tt)
	if u.OnSwap != nil {
		u.OnSwap()
	}
	u.Log.Info("timetable swapped (zero downtime)", "total", time.Since(t0).Round(time.Millisecond), "stats", st.String())
	u.E.LogExclusions(func(format string, a ...any) { u.Log.Warn(fmt.Sprintf(format, a...)) })
	debug.FreeOSMemory() // hand rebuild transients back to the OS right away
	return nil
}

// ---- OSM (Geofabrik osmChange) ------------------------------------------------

func (u *Updater) osmLoop(updatesURL string) {
	tick := time.NewTicker(u.Cfg.OSM.Poll)
	defer tick.Stop()
	for range tick.C {
		if err := u.syncOSM(updatesURL); err != nil {
			u.Log.Error("osm sync failed", "err", err)
		}
	}
}

func (u *Updater) syncOSM(updatesURL string) error {
	gb := u.E.GraphBundle()
	if gb == nil {
		return fmt.Errorf("graph not ready")
	}
	cur := gb.G.ReplicationSeq
	if cur <= 0 {
		return fmt.Errorf("extract carries no replication sequence; live updates impossible")
	}

	stateRaw, err := FetchBytes(updatesURL+"/state.txt", u.Cfg.OSM.AllowInsecure)
	if err != nil {
		return err
	}
	remote, err := osm.ParseStateTxt(stateRaw)
	if err != nil {
		return err
	}
	if remote.Sequence <= cur {
		u.Log.Debug("osm up to date", "seq", cur)
		return nil
	}
	u.Log.Info("osm diffs available", "have", cur, "remote", remote.Sequence)

	// Load the compact source, fold every pending diff in, reassemble and swap.
	done := memrelease.Begin()
	defer done()
	t0 := time.Now()
	src, err := u.loadGraphSource()
	if err != nil {
		return fmt.Errorf("graph source: %w", err)
	}
	for seq := cur + 1; seq <= remote.Sequence; seq++ {
		oscURL := fmt.Sprintf("%s/%s.osc.gz", updatesURL, osm.SeqPath(seq))
		data, err := FetchBytes(oscURL, u.Cfg.OSM.AllowInsecure)
		if err != nil {
			return fmt.Errorf("diff %d: %w", seq, err)
		}
		ch, err := osm.ParseOSCGz(bytes.NewReader(data))
		if err != nil {
			return fmt.Errorf("diff %d: %w", seq, err)
		}
		var ast graph.ApplyStats
		src, ast = src.ApplyChange(ch)
		u.Log.Info(fmt.Sprintf("osc %d %s", seq, ast.String()))
	}
	src.SetReplication(remote.Sequence)

	st := &graph.BuildStats{}
	g := graph.Assemble(src, st)
	if _, err := u.SetGraphSource(src); err != nil {
		return err
	}
	u.E.SetGraph(g)
	debug.FreeOSMemory()
	u.Log.Info("street graph swapped (zero downtime, no PBF re-download)",
		"seq", remote.Sequence, "total", time.Since(t0).Round(time.Millisecond), "stats", st.String())
	u.E.LastOSMSync.Store(time.Now())

	// stop anchoring and transfers were computed against the old graph:
	// recompile the timetable against the new one (seconds, background)
	return u.RebuildTimetable()
}
