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
	"syscall"
	"time"
	_ "time/tzdata" // feeds pick their own timezone; never depend on the host

	"gotransit/internal/boot"
	"gotransit/internal/config"
)

const exampleConfig = `# gotransit.toml — the whole configuration. Anything omitted uses defaults.

listen   = ":8080"
# debug_ui = false     # map debug interface at / (default: on)

# [cache]              # optional persistent source tier: GTFS ZIPs stay
# dir = "/var/cache/gotransit"   # file-backed and the initial OSM PBF becomes
                       # a smaller restart/update image. Default: off,
                       # fully ephemeral.

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
# headers = ["x-api-key: secret"]   # attached to every fetch for this feed

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

# Optional shared mobility. Point each entry at gbfs.json; GoTransit discovers
# v1.x, v2.x and v3.x station/vehicle/type/geofence endpoints automatically.
# [[gbfs]]
# name = "city-bikes"
# url  = "https://operator.example/gbfs.json"
# language = "en"       # v1.x only; falls back to en/first language
# poll = "20s"
# max_age = "5m"        # fail closed when realtime availability is older
# headers = ["Authorization: Bearer secret"]

# [realtime]                    # live itineraries + tracking thresholds
# reroute_min_saving    = "5m"  # only push a reroute when it saves this much
# rt_confirm_lead       = "10m" # warn if a leg is still schedule-only this close
# live_first_leg_within = "45m" # live plans: first bus must be RT-confirmed and near
# live_horizon          = "1h"  # ...and every leg in this window must be live
# cancel_blind          = "3m"  # no-trace-after-terminus-departure suspicion window
# (rail-like routes stay trackable by timetable when no vehicle position exists)

# [[gtfs]]             # local feeds are used in place, never deleted,
# name = "mio"         # and reloaded when the file's mtime changes
# url  = "/dati/mio_gtfs.zip"

# [routing]            # defaults shown; uncomment to change
# walk_speed_kmh  = 4.8
# bike_speed_kmh  = 15.0
# scooter_speed_kmh = 20.0
# max_walk_access = "12m"
# max_bike_access = "18m"
# max_transfers   = 4
# transfer_slack  = "90s"
# rail_entry_buffer    = "60s"
# rail_transfer_buffer = "90s"
# transfer_radius_m = 400
# snap_radius_m     = 300
# bike_transit_min_saving = "5m"
# shared_pickup_walk  = "8m"
# shared_dropoff_walk = "6m"
# shared_unlock_time  = "45s"
# shared_park_time    = "30s"
# shared_candidates   = 12
# battery_reserve_ratio = 0.15
# battery_reserve_m     = 500
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

	rt := boot.New(cfg, log)

	// the HTTP server is up immediately; /v1/health says 503 until ready
	httpSrv := &http.Server{Addr: cfg.Listen, Handler: rt.Srv.Handler()}
	go func() {
		log.Info("gotransit listening", "addr", cfg.Listen, "debug_ui", cfg.DebugUI)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("http server", "err", err)
			os.Exit(1)
		}
	}()

	go rt.Run()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Info("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	httpSrv.Shutdown(ctx)
}
