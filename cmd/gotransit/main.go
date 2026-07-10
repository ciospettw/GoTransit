// gotransit — a featherweight, runtime-updating public transport router.
// Ephemeral by design: download, parse, destroy, poll. Remote data lives in
// RAM, nothing is kept on disk. Local files are used in place, never touched.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"
	_ "time/tzdata" // feeds pick their own timezone; never depend on the host

	"gotransit/internal/api"
	"gotransit/internal/config"
	"gotransit/internal/engine"
	"gotransit/internal/graph"
	"gotransit/internal/rt"
	"gotransit/internal/track"
	"gotransit/internal/transit"
	"gotransit/internal/updater"
)

const exampleConfig = `# gotransit.toml — the whole configuration. Anything omitted uses defaults.

listen   = ":8080"
# debug_ui = false     # map debug interface at / (default: on)

[osm]
# Geofabrik is the supported OSM host: its -updates/ osmChange stream keeps
# the street graph fresh at runtime with zero downtime. A local path (or
# file://) works too and is never deleted; anything else imports once but
# receives no live updates.
url  = "https://download.geofabrik.de/europe/italy/centro-latest.osm.pbf"
poll = "6h"

[[gtfs]]
name = "roma"
url  = "https://romamobilita.it/sites/default/files/rome_static_gtfs.zip"
# poll = "1m"          # default: every minute, via ETag conditional requests
rt_trip_updates      = "https://romamobilita.it/sites/default/files/rome_rtgtfs_trip_updates_feed.pb"
rt_vehicle_positions = "https://romamobilita.it/sites/default/files/rome_rtgtfs_vehicle_positions_feed.pb"
# rt_poll = "20s"

[[gtfs]]
name = "cotral"
url  = "http://travel.mob.cotralspa.it:7777/GTFS/GTFS_COTRAL.zip"
allow_insecure = true  # plain http must be opted into, explicitly
rt_trip_updates      = "https://proxy.busone.app/cotral/gtfs-rt/trip-updates.pb"
rt_vehicle_positions = "https://proxy.busone.app/cotral/gtfs-rt/vehicle-positions.pb"

[[gtfs]]
name = "trenitalia"
url  = "https://proxy.busone.app/trenitalia/gtfs.zip"
rt_trip_updates      = "https://proxy.busone.app/trenitalia/gtfs-rt/trip-updates.pb"
rt_vehicle_positions = "https://proxy.busone.app/trenitalia/gtfs-rt/vehicle-positions.pb"

# [realtime]                    # live itineraries + tracking thresholds
# reroute_min_saving    = "5m"  # only push a reroute when it saves this much
# rt_confirm_lead       = "10m" # warn if a leg is still schedule-only this close
# live_first_leg_within = "45m" # live plans: first bus must be RT-confirmed and near
# live_horizon          = "1h"  # ...and every leg in this window must be live
# cancel_blind          = "3m"  # no-trace-after-terminus-departure suspicion window
# (metro route types are always considered live: no VP/TU expected from them)

# [[gtfs]]             # local feeds are used in place, never deleted,
# name = "mio"         # and reloaded when the file's mtime changes
# url  = "/dati/mio_gtfs.zip"

# [routing]            # defaults shown; uncomment to change
# walk_speed_kmh  = 4.8
# bike_speed_kmh  = 15.0
# max_walk_access = "12m"
# max_bike_access = "18m"
# max_transfers   = 4
# transfer_slack  = "90s"
# transfer_radius_m = 400
# snap_radius_m     = 300
# bike_transit_min_saving = "5m"
# car_heuristic   = "fast"   # "exact" for provably optimal car routes
# max_itineraries = 4
`

func main() {
	cfgPath := flag.String("c", "gotransit.toml", "config file")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if flag.Arg(0) == "init" {
		if _, err := os.Stat(*cfgPath); err == nil {
			log.Error("refusing to overwrite existing config", "path", *cfgPath)
			os.Exit(1)
		}
		if err := os.WriteFile(*cfgPath, []byte(exampleConfig), 0o644); err != nil {
			log.Error("write failed", "err", err)
			os.Exit(1)
		}
		fmt.Printf("wrote %s — edit it, then run: gotransit -c %s\n", *cfgPath, *cfgPath)
		return
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Error("config", "err", err)
		log.Info("hint: run \"gotransit init\" to create a starter gotransit.toml")
		os.Exit(1)
	}

	e := engine.New(cfg)
	up := updater.New(e, cfg, log)
	srv := &api.Server{E: e, Log: log, DebugUI: cfg.DebugUI}

	// the HTTP server is up immediately; /v1/health says 503 until ready
	httpSrv := &http.Server{Addr: cfg.Listen, Handler: srv.Handler()}
	go func() {
		log.Info("gotransit listening", "addr", cfg.Listen, "debug_ui", cfg.DebugUI)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("http server", "err", err)
			os.Exit(1)
		}
	}()

	go boot(e, up, srv, cfg, log)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Info("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	httpSrv.Shutdown(ctx)
}

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

// boot brings the engine up: download → parse → destroy → poll.
func boot(e *engine.Engine, up *updater.Updater, srv *api.Server, cfg *config.Config, log *slog.Logger) {
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
			res, err := updater.FetchBytesCond(f.URL, f.AllowInsecure, "", "", "")
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
			Poll: f.RTPoll, Insecure: f.AllowInsecure,
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
		up.OnSwap = mgr.Rebuild // re-project RT onto every fresh timetable
		tracker := &track.Tracker{E: e, Mgr: mgr, Cfg: cfg, Log: log}
		srv.Track = func(ctx context.Context, id string, sink api.TrackSink) error {
			return tracker.Run(ctx, id, sink)
		}
		mgr.Start()
		log.Info("gtfs-rt live", "sources", len(sources))
	} else {
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
}
