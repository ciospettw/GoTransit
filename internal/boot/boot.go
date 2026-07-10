// Package boot wires a full engine runtime — graph, timetable, GTFS-RT,
// tracking, updater — from a config. cmd/gotransit is its one-line caller;
// alternative binaries (private integrations, test harnesses) embed the same
// sequence and add their own routes and hooks around it.
package boot

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"runtime/debug"
	"sync/atomic"
	"time"

	"gotransit/internal/api"
	"gotransit/internal/config"
	"gotransit/internal/engine"
	"gotransit/internal/graph"
	"gotransit/internal/rt"
	"gotransit/internal/track"
	"gotransit/internal/transit"
	"gotransit/internal/updater"
)

// Runtime holds the wired pieces. Create with New, then run Run (blocking —
// call it in a goroutine and serve Srv.Handler() immediately: /v1/health
// answers 503 until the boot completes).
type Runtime struct {
	Cfg *config.Config
	Log *slog.Logger
	E   *engine.Engine
	Up  *updater.Updater
	Srv *api.Server

	// OnTimetableSwap runs after every post-boot timetable swap (static GTFS
	// or OSM update), after the RT overlay has been re-projected.
	OnTimetableSwap func()
	// OnReady runs once, when the boot sequence completes.
	OnReady func()

	rtMgr atomic.Pointer[rt.Manager]
}

// New wires engine, updater and API server for cfg.
func New(cfg *config.Config, log *slog.Logger) *Runtime {
	e := engine.New(cfg)
	return &Runtime{
		Cfg: cfg,
		Log: log,
		E:   e,
		Up:  updater.New(e, cfg, log),
		Srv: &api.Server{E: e, Log: log, DebugUI: cfg.DebugUI},
	}
}

// RTManager returns the GTFS-RT manager, or nil before boot completes or
// when no source is configured.
func (r *Runtime) RTManager() *rt.Manager { return r.rtMgr.Load() }

// retryForever runs fn with capped exponential backoff: a set-and-forget
// daemon must survive flaky sources and rate limits at boot time too.
func retryForever(log *slog.Logger, what string, fn func() error) {
	backoff := 15 * time.Second
	for attempt := 1; ; attempt++ {
		err := fn()
		if err == nil {
			return
		}
		log.Warn("boot step failed, retrying", "step", what, "attempt", attempt,
			"retry_in", backoff.String(), "err", err)
		time.Sleep(backoff)
		if backoff *= 2; backoff > 5*time.Minute {
			backoff = 5 * time.Minute
		}
	}
}

// Run brings the engine up: download → parse → destroy → poll. It blocks
// until the boot sequence finishes; pollers keep running in the background.
func (r *Runtime) Run() {
	e, up, srv, cfg, log := r.E, r.Up, r.Srv, r.Cfg, r.Log
	t0 := time.Now()

	// ---- street graph ----
	pbfPath := ""
	ephemeralPBF := false
	if cfg.OSMLocal() {
		pbfPath = config.LocalPath(cfg.OSM.URL)
		log.Info("using local OSM extract (kept in place)", "path", pbfPath)
	} else {
		log.Info("downloading OSM extract to a temp file", "url", cfg.OSM.URL)
		retryForever(log, "osm download", func() error {
			p, n, err := updater.FetchToTemp(cfg.OSM.URL, cfg.OSM.AllowInsecure)
			if err != nil {
				return err
			}
			pbfPath, ephemeralPBF = p, true
			log.Info("OSM extract downloaded", "MB", n/1e6)
			return nil
		})
	}
	g, src, st, err := graph.BuildFromPBF(pbfPath, 0)
	if ephemeralPBF {
		os.Remove(pbfPath) // parse done: destroy — nothing stays on disk
	}
	if err != nil {
		log.Error("graph build failed", "err", err)
		os.Exit(1)
	}
	log.Info("street graph built", "stats", st.String(), "osm_seq", g.ReplicationSeq)
	if ephemeralPBF {
		log.Info("temp PBF deleted — the graph source now lives compressed in RAM")
	}
	blob, err := src.EncodeStore()
	if err != nil {
		log.Error("graph source encode failed", "err", err)
		os.Exit(1)
	}
	up.SetGraphSource(blob)
	log.Info("graph source held in RAM for live osc updates", "MB", len(blob)/1e6)
	e.SetGraph(g)

	// ---- GTFS feeds ----
	for _, f := range cfg.Feeds {
		if f.Local() {
			info, err := os.Stat(config.LocalPath(f.URL))
			if err != nil {
				log.Error("local GTFS missing", "feed", f.Name, "err", err)
				os.Exit(1)
			}
			up.MarkLocalFeed(f.Name, info.ModTime())
			log.Info("using local GTFS (kept in place, reloaded on change)", "feed", f.Name)
			continue
		}
		log.Info("downloading GTFS into memory", "feed", f.Name, "url", f.URL)
		retryForever(log, "gtfs download "+f.Name, func() error {
			res, err := updater.FetchBytesCond(f.URL, f.AllowInsecure, "", "", "", f.Headers)
			if err != nil {
				return err
			}
			up.InstallFeedZip(f.Name, res)
			log.Info("GTFS in memory", "feed", f.Name, "MB", len(res.Data)/1e6, "etag", res.ETag != "")
			return nil
		})
	}

	feeds, err := up.LoadFeeds()
	if err != nil {
		log.Error("GTFS parse failed", "err", err)
		os.Exit(1)
	}
	tt, cst, err := transit.Compile(feeds, g,
		cfg.Routing.WalkSpeedKmh, cfg.Routing.TransferRadiusM, cfg.Routing.SnapRadiusM)
	if err != nil {
		log.Error("timetable compile failed", "err", err)
		os.Exit(1)
	}
	e.SetTimetable(tt)
	log.Info("timetable ready", "stats", cst.String(), "tz", tt.TZ.String())
	e.LogExclusions(func(format string, a ...any) { log.Warn(fmt.Sprintf(format, a...)) })

	// ---- GTFS-Realtime: pollers + live tracking ----
	var sources []rt.Source
	for i, f := range cfg.Feeds {
		if f.RTTripUpdates == "" && f.RTVehiclePositions == "" {
			continue
		}
		sources = append(sources, rt.Source{
			FeedIdx: i, Name: f.Name,
			TripUpdates: f.RTTripUpdates, VehiclePositions: f.RTVehiclePositions,
			Poll: f.RTPoll, Insecure: f.AllowInsecure, Headers: f.Headers,
		})
	}
	if len(sources) > 0 {
		mgr := rt.NewManager(log, sources, func() *transit.Timetable {
			if tb := e.TTBundle(); tb != nil {
				return tb.TT
			}
			return nil
		})
		e.RTStats = func() any { return mgr.Stats() }
		up.OnSwap = func() { // re-project RT onto every fresh timetable
			mgr.Rebuild()
			if r.OnTimetableSwap != nil {
				r.OnTimetableSwap()
			}
		}
		tracker := &track.Tracker{E: e, Mgr: mgr, Cfg: cfg, Log: log}
		srv.Track = func(ctx context.Context, id string, sink api.TrackSink) error {
			return tracker.Run(ctx, id, sink)
		}
		mgr.Start()
		r.rtMgr.Store(mgr)
		log.Info("gtfs-rt live", "sources", len(sources))
	} else {
		up.OnSwap = func() {
			if r.OnTimetableSwap != nil {
				r.OnTimetableSwap()
			}
		}
		tracker := &track.Tracker{E: e, Cfg: cfg, Log: log}
		srv.Track = func(ctx context.Context, id string, sink api.TrackSink) error {
			return tracker.Run(ctx, id, sink)
		}
		log.Info("no gtfs-rt sources configured: tracking runs in schedule-only monitor mode")
	}

	up.Start()
	debug.FreeOSMemory() // build transients go back to the OS immediately
	log.Info("engine ready — fully in RAM, zero files, updates run themselves",
		"boot", time.Since(t0).Round(time.Millisecond))
	if r.OnReady != nil {
		r.OnReady()
	}
}
