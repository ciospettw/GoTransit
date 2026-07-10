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
	"gotransit/internal/osm"
	"gotransit/internal/transit"
)

// feedData is the in-memory life of one GTFS source.
type feedData struct {
	cfg config.Feed

	// remote feeds
	zip     []byte
	etag    string
	lastMod string
	sha     string
	warned  bool // logged "server ignores conditionals" once

	// local feeds
	mtime time.Time
}

// Updater owns the live data: the compressed graph source blob and the feed
// zips, all in RAM. Nothing here ever touches the disk (local sources are
// read in place and never modified).
type Updater struct {
	E   *engine.Engine
	Cfg *config.Config
	Log *slog.Logger

	// OnSwap runs after every timetable swap (e.g. re-project GTFS-RT).
	OnSwap func()

	mu      sync.Mutex
	srcBlob []byte
	feeds   map[string]*feedData

	rebuildMu chan struct{}
}

// New creates the updater.
func New(e *engine.Engine, cfg *config.Config, log *slog.Logger) *Updater {
	u := &Updater{
		E: e, Cfg: cfg, Log: log,
		feeds:     map[string]*feedData{},
		rebuildMu: make(chan struct{}, 1),
	}
	for _, f := range cfg.Feeds {
		u.feeds[f.Name] = &feedData{cfg: f}
	}
	return u
}

// SetGraphSource installs the compressed graph source blob (boot and after
// each osc application).
func (u *Updater) SetGraphSource(blob []byte) {
	u.mu.Lock()
	u.srcBlob = blob
	u.mu.Unlock()
}

// InstallFeedZip records a remote feed's zip bytes and validators.
func (u *Updater) InstallFeedZip(name string, res CondResult) {
	u.mu.Lock()
	fd := u.feeds[name]
	fd.zip = res.Data
	fd.etag, fd.lastMod, fd.sha = res.ETag, res.LastMod, res.SHA256
	u.mu.Unlock()
}

// MarkLocalFeed records a local feed's current mtime.
func (u *Updater) MarkLocalFeed(name string, mtime time.Time) {
	u.mu.Lock()
	u.feeds[name].mtime = mtime
	u.mu.Unlock()
}

// LoadFeeds parses every feed from RAM (remote) or from disk (local).
func (u *Updater) LoadFeeds() ([]*gtfs.Feed, error) {
	var feeds []*gtfs.Feed
	for _, f := range u.Cfg.Feeds {
		var fd *gtfs.Feed
		var err error
		if f.Local() {
			fd, err = gtfs.Load(config.LocalPath(f.URL), f.Name)
		} else {
			u.mu.Lock()
			data := u.feeds[f.Name].zip
			u.mu.Unlock()
			if data == nil {
				return nil, fmt.Errorf("feed %s: no data in memory", f.Name)
			}
			fd, err = gtfs.LoadBytes(data, f.Name)
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
		go u.osmLoop(replURL)
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
	u.InstallFeedZip(f.Name, res)
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

	// decode the in-RAM source, fold every pending diff in, reassemble, swap
	t0 := time.Now()
	u.mu.Lock()
	blob := u.srcBlob
	u.mu.Unlock()
	if blob == nil {
		return fmt.Errorf("no graph source in memory")
	}
	src, err := graph.DecodeStore(blob)
	if err != nil {
		return fmt.Errorf("graph source blob: %w", err)
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
	newBlob, err := src.EncodeStore()
	if err != nil {
		return err
	}
	u.SetGraphSource(newBlob)
	u.E.SetGraph(g)
	debug.FreeOSMemory()
	u.Log.Info("street graph swapped (zero downtime, no PBF, no disk)",
		"seq", remote.Sequence, "total", time.Since(t0).Round(time.Millisecond), "stats", st.String())
	u.E.LastOSMSync.Store(time.Now())

	// stop anchoring and transfers were computed against the old graph:
	// recompile the timetable against the new one (seconds, background)
	return u.RebuildTimetable()
}
