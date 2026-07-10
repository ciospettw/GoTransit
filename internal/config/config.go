// Package config loads gotransit.toml. Every tuning knob has a sane default:
// a minimal config is just the OSM URL and one GTFS feed. One file, no JSON.
package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gotransit/internal/toml"
)

// Feed is one GTFS static source: a remote URL (polled with ETag, downloaded
// into memory, never written to disk) or a local file path (kept, reloaded
// when its mtime changes).
type Feed struct {
	Name          string // short identifier, used in stop ids ("roma:70431")
	URL           string
	AllowInsecure bool          // permit plain http / invalid TLS, must be explicit
	Poll          time.Duration // how often to check for changes

	// GTFS-Realtime endpoints (optional; enable live itineraries + tracking)
	RTTripUpdates      string
	RTVehiclePositions string
	RTPoll             time.Duration
}

// Local reports whether the source is a filesystem path.
func (f Feed) Local() bool { return IsLocal(f.URL) }

// IsLocal detects filesystem sources: file:// URLs or plain paths.
func IsLocal(u string) bool {
	if strings.HasPrefix(u, "file://") {
		return true
	}
	return !strings.Contains(u, "://")
}

// LocalPath strips the file:// scheme.
func LocalPath(u string) string { return strings.TrimPrefix(u, "file://") }

// Config is the whole engine configuration.
type Config struct {
	Listen  string
	DebugUI bool // map-based debug interface at /

	OSM struct {
		URL           string
		Poll          time.Duration
		AllowInsecure bool
	}

	Feeds []Feed

	Realtime struct {
		// a reroute is only pushed when it beats the current plan by this much
		RerouteMinSaving time.Duration
		// a scheduled leg without any RT signal this close to its departure
		// triggers a warning and a reroute attempt onto live alternatives
		ConfirmLead time.Duration
		// live itineraries: the first transit leg must be RT-covered and
		// depart within this window...
		LiveFirstLeg time.Duration
		// ...and every transit leg departing within this horizon must be live
		LiveHorizon time.Duration
		// a trip that should have left its terminus this long ago with no RT
		// trace is treated as possibly cancelled (CANCELED often shows up
		// only ~2 min after scheduled departure)
		CancelBlind time.Duration
	}

	Routing struct {
		WalkSpeedKmh float64
		BikeSpeedKmh float64

		MaxWalkAccess   time.Duration // max walk to/from a stop
		MaxBikeAccess   time.Duration // max ride to/from a stop (bike+transit)
		MaxTransfers    int
		TransferSlack   time.Duration // minimum time to change vehicles
		TransferRadiusM int           // stop-to-stop footpath precompute radius (network meters)
		SnapRadiusM     int           // max distance from a point to the street graph

		// bike+transit realism: a bike access/egress variant is only offered
		// if it beats the walk-based plan by at least this much.
		BikeTransitMinSaving time.Duration
		// bike legs feel more expensive than the raw ride time (parking,
		// locking, effort): multiplied cost used when comparing to walking.
		BikeCostFactor float64

		CarHeuristic string // "fast" (weighted A*, ~few % from optimal) or "exact"

		MaxItineraries int
	}
}

// Default returns the configuration defaults (documented in gotransit.toml).
func Default() *Config {
	c := &Config{}
	c.Listen = ":8080"
	c.DebugUI = true
	c.OSM.Poll = 6 * time.Hour
	c.Routing.WalkSpeedKmh = 4.8
	c.Routing.BikeSpeedKmh = 15
	c.Routing.MaxWalkAccess = 12 * time.Minute
	c.Routing.MaxBikeAccess = 18 * time.Minute
	c.Routing.MaxTransfers = 4
	c.Routing.TransferSlack = 90 * time.Second
	c.Routing.TransferRadiusM = 400
	c.Routing.SnapRadiusM = 300
	c.Routing.BikeTransitMinSaving = 5 * time.Minute
	c.Routing.BikeCostFactor = 1.25
	c.Routing.CarHeuristic = "fast"
	c.Routing.MaxItineraries = 4
	c.Realtime.RerouteMinSaving = 5 * time.Minute
	c.Realtime.ConfirmLead = 10 * time.Minute
	c.Realtime.LiveFirstLeg = 45 * time.Minute
	c.Realtime.LiveHorizon = time.Hour
	c.Realtime.CancelBlind = 3 * time.Minute
	return c
}

// DefaultFeedPoll is how often remote GTFS feeds are checked (ETag
// conditional requests, so a no-change poll is a handful of bytes).
const DefaultFeedPoll = time.Minute

// Load reads and validates a TOML config file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parse(data)
}

func parse(data []byte) (*Config, error) {
	t, err := toml.Parse(data)
	if err != nil {
		return nil, err
	}
	c := Default()
	c.Listen = t.Str("listen", c.Listen)
	c.DebugUI = t.Bool("debug_ui", c.DebugUI)

	c.OSM.URL = t.Str("osm.url", "")
	c.OSM.AllowInsecure = t.Bool("osm.allow_insecure", false)
	if c.OSM.Poll, err = t.Dur("osm.poll", c.OSM.Poll); err != nil {
		return nil, err
	}

	for i, ft := range t.Tables("gtfs") {
		f := Feed{
			Name:               ft.Str("name", ""),
			URL:                ft.Str("url", ""),
			AllowInsecure:      ft.Bool("allow_insecure", false),
			RTTripUpdates:      ft.Str("rt_trip_updates", ""),
			RTVehiclePositions: ft.Str("rt_vehicle_positions", ""),
		}
		if f.Poll, err = ft.Dur("poll", DefaultFeedPoll); err != nil {
			return nil, err
		}
		if f.RTPoll, err = ft.Dur("rt_poll", 20*time.Second); err != nil {
			return nil, err
		}
		if f.URL == "" {
			return nil, fmt.Errorf("config: [[gtfs]] #%d has no url", i+1)
		}
		if f.Name == "" {
			return nil, fmt.Errorf("config: [[gtfs]] #%d (%s) has no name", i+1, f.URL)
		}
		f.Name = strings.ToLower(f.Name)
		if strings.ContainsAny(f.Name, " :/\\") {
			return nil, fmt.Errorf("config: feed name %q must be a short slug (no spaces, colons or slashes)", f.Name)
		}
		c.Feeds = append(c.Feeds, f)
	}

	rt := t.Table("realtime")
	if c.Realtime.RerouteMinSaving, err = rt.Dur("reroute_min_saving", c.Realtime.RerouteMinSaving); err != nil {
		return nil, err
	}
	if c.Realtime.ConfirmLead, err = rt.Dur("rt_confirm_lead", c.Realtime.ConfirmLead); err != nil {
		return nil, err
	}
	if c.Realtime.LiveFirstLeg, err = rt.Dur("live_first_leg_within", c.Realtime.LiveFirstLeg); err != nil {
		return nil, err
	}
	if c.Realtime.LiveHorizon, err = rt.Dur("live_horizon", c.Realtime.LiveHorizon); err != nil {
		return nil, err
	}
	if c.Realtime.CancelBlind, err = rt.Dur("cancel_blind", c.Realtime.CancelBlind); err != nil {
		return nil, err
	}

	r := t.Table("routing")
	c.Routing.WalkSpeedKmh = r.Float("walk_speed_kmh", c.Routing.WalkSpeedKmh)
	c.Routing.BikeSpeedKmh = r.Float("bike_speed_kmh", c.Routing.BikeSpeedKmh)
	if c.Routing.MaxWalkAccess, err = r.Dur("max_walk_access", c.Routing.MaxWalkAccess); err != nil {
		return nil, err
	}
	if c.Routing.MaxBikeAccess, err = r.Dur("max_bike_access", c.Routing.MaxBikeAccess); err != nil {
		return nil, err
	}
	c.Routing.MaxTransfers = int(r.Int("max_transfers", int64(c.Routing.MaxTransfers)))
	if c.Routing.TransferSlack, err = r.Dur("transfer_slack", c.Routing.TransferSlack); err != nil {
		return nil, err
	}
	c.Routing.TransferRadiusM = int(r.Int("transfer_radius_m", int64(c.Routing.TransferRadiusM)))
	c.Routing.SnapRadiusM = int(r.Int("snap_radius_m", int64(c.Routing.SnapRadiusM)))
	if c.Routing.BikeTransitMinSaving, err = r.Dur("bike_transit_min_saving", c.Routing.BikeTransitMinSaving); err != nil {
		return nil, err
	}
	c.Routing.BikeCostFactor = r.Float("bike_cost_factor", c.Routing.BikeCostFactor)
	c.Routing.CarHeuristic = r.Str("car_heuristic", c.Routing.CarHeuristic)
	c.Routing.MaxItineraries = int(r.Int("max_itineraries", int64(c.Routing.MaxItineraries)))

	return c, validate(c)
}

func validate(c *Config) error {
	if c.OSM.URL == "" {
		return fmt.Errorf("config: [osm] url is required (a Geofabrik .osm.pbf URL, or a local path)")
	}
	if len(c.Feeds) == 0 {
		return fmt.Errorf("config: at least one [[gtfs]] feed is required")
	}
	seen := map[string]bool{}
	for _, f := range c.Feeds {
		if seen[f.Name] {
			return fmt.Errorf("config: duplicate feed name %q", f.Name)
		}
		seen[f.Name] = true
		if strings.HasPrefix(f.URL, "http://") && !f.AllowInsecure {
			return fmt.Errorf("config: feed %q uses plain http; set allow_insecure = true to accept it", f.Name)
		}
		if f.Local() {
			if _, err := os.Stat(LocalPath(f.URL)); err != nil {
				return fmt.Errorf("config: feed %q: local file not found: %s", f.Name, LocalPath(f.URL))
			}
		}
	}
	if strings.HasPrefix(c.OSM.URL, "http://") && !c.OSM.AllowInsecure {
		return fmt.Errorf("config: [osm] url uses plain http; set allow_insecure = true to accept it")
	}
	if IsLocal(c.OSM.URL) {
		if _, err := os.Stat(LocalPath(c.OSM.URL)); err != nil {
			return fmt.Errorf("config: [osm] local file not found: %s", LocalPath(c.OSM.URL))
		}
	}
	if h := c.Routing.CarHeuristic; h != "fast" && h != "exact" {
		return fmt.Errorf("config: routing.car_heuristic must be \"fast\" or \"exact\", got %q", h)
	}
	if c.Routing.WalkSpeedKmh <= 0 || c.Routing.BikeSpeedKmh <= 0 {
		return fmt.Errorf("config: speeds must be positive")
	}
	if c.Routing.MaxTransfers < 0 || c.Routing.MaxTransfers > 8 {
		return fmt.Errorf("config: max_transfers must be between 0 and 8")
	}
	return nil
}

// OSMLocal reports whether the OSM source is a filesystem path.
func (c *Config) OSMLocal() bool { return IsLocal(c.OSM.URL) }

// IsGeofabrik reports whether the OSM URL points at Geofabrik, the only host
// for which .osc (osmChange) live updates are supported. Local extracts that
// embed a Geofabrik replication URL in their header update too.
func (c *Config) IsGeofabrik() bool {
	return strings.Contains(c.OSM.URL, "download.geofabrik.de/")
}

// GeofabrikUpdatesURL derives the replication directory from a
// ".../<region>-latest.osm.pbf" URL: ".../<region>-updates/".
func (c *Config) GeofabrikUpdatesURL() string {
	u := c.OSM.URL
	for _, suf := range []string{"-latest.osm.pbf", ".osm.pbf"} {
		if strings.HasSuffix(u, suf) {
			return strings.TrimSuffix(u, suf) + "-updates/"
		}
	}
	return ""
}
