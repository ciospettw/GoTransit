// Package gbfs loads General Bikeshare Feed Specification datasets and
// exposes immutable, atomically-swapped availability snapshots.
package gbfs

import (
	"sort"
	"strings"
	"time"
)

// Class is the routing-level vehicle family. GBFS form factors remain
// available on VehicleType; Class deliberately groups version-specific names
// such as scooter (v2.1) and scooter_standing (v2.3+).
type Class string

const (
	ClassBicycle Class = "bicycle"
	ClassScooter Class = "scooter"
	ClassAny     Class = "any"
)

func ParseClass(s string) Class {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "bike", "bicycle", "cargo_bicycle", "cargo-bike":
		return ClassBicycle
	case "scooter", "scooter_standing", "scooter_seated":
		return ClassScooter
	case "any", "shared":
		return ClassAny
	default:
		return Class(s)
	}
}

func classOf(form string) Class {
	switch strings.ToLower(form) {
	case "bicycle", "cargo_bicycle", "bike":
		return ClassBicycle
	case "scooter", "scooter_standing", "scooter_seated":
		return ClassScooter
	default:
		return Class(form)
	}
}

type RentalURIs struct {
	Android string `json:"android,omitempty"`
	IOS     string `json:"ios,omitempty"`
	Web     string `json:"web,omitempty"`
}

type VehicleType struct {
	ID               string
	Name             string
	FormFactor       string
	Propulsion       string
	MaxRangeM        float64
	ReturnConstraint string
}

func (v VehicleType) Class() Class { return classOf(v.FormFactor) }
func (v VehicleType) Motorized() bool {
	return v.Propulsion != "" && v.Propulsion != "human"
}

type Station struct {
	ID, Name string
	Lat, Lon float64
	Capacity int

	Installed bool
	Renting   bool
	Returning bool
	Vehicles  int
	Docks     int
	// DocksKnown distinguishes an omitted num_docks_available (unlimited
	// virtual station) from an explicitly full station.
	DocksKnown bool
	LastReport time.Time

	VehicleCounts map[string]int
	DockCounts    map[string]int
	RentalURIs    RentalURIs
}

type Vehicle struct {
	ID, TypeID, StationID, HomeStationID string
	Lat, Lon                             float64
	Reserved, Disabled                   bool
	RangeM                               float64
	RangeKnown                           bool
	BatteryPercent                       float64
	BatteryKnown                         bool
	LastReport                           time.Time
	RentalURIs                           RentalURIs
}

// Rule is the normalized GBFS v2/v3 geofencing rule. Nil booleans mean that
// the producer omitted the field and therefore supplied no restriction.
type Rule struct {
	VehicleTypeIDs  []string
	StartAllowed    *bool
	EndAllowed      *bool
	ThroughAllowed  *bool
	StationParking  *bool
	MaximumSpeedKPH float64
}

type Zone struct {
	// Polygons uses GeoJSON order: polygon -> ring -> point [lon,lat]. The
	// first ring is the exterior and later rings are holes.
	Polygons [][][][2]float64
	Rules    []Rule
	Start    time.Time
	End      time.Time
}

type Feed struct {
	Name, SystemName, Version string
	FetchedAt                 time.Time
	DataUpdatedAt             time.Time
	StationUpdatedAt          time.Time
	VehicleUpdatedAt          time.Time
	ExpiresAt                 time.Time
	MaxAge                    time.Duration
	Stations                  map[string]Station
	Vehicles                  map[string]Vehicle
	VehicleTypes              map[string]VehicleType
	Zones                     []Zone
	GlobalRules               []Rule
	LastError                 string
}

func (f *Feed) Fresh(now time.Time) bool {
	if f == nil || f.FetchedAt.IsZero() {
		return false
	}
	availabilityAt := f.StationUpdatedAt
	if f.VehicleUpdatedAt.After(availabilityAt) {
		availabilityAt = f.VehicleUpdatedAt
	}
	if availabilityAt.IsZero() {
		availabilityAt = f.DataUpdatedAt
	}
	if f.MaxAge > 0 && !availabilityAt.IsZero() && now.Sub(availabilityAt) > f.MaxAge {
		return false
	}
	return true
}

type Snapshot struct {
	Version uint64
	At      time.Time
	Feeds   map[string]*Feed
}

// Status is JSON-safe operational information for /v1/status.
type Status struct {
	Version uint64       `json:"version"`
	Feeds   []FeedStatus `json:"feeds"`
}

type FeedStatus struct {
	Name         string `json:"name"`
	GBFSVersion  string `json:"gbfs_version,omitempty"`
	System       string `json:"system,omitempty"`
	Updated      string `json:"updated,omitempty"`
	Fresh        bool   `json:"fresh"`
	Stations     int    `json:"stations"`
	Vehicles     int    `json:"vehicles"`
	VehicleTypes int    `json:"vehicle_types"`
	Error        string `json:"error,omitempty"`
}

func (s *Snapshot) Status(now time.Time) Status {
	out := Status{}
	if s == nil {
		return out
	}
	out.Version = s.Version
	for _, f := range s.Feeds {
		st := FeedStatus{Name: f.Name, GBFSVersion: f.Version, System: f.SystemName,
			Fresh: f.Fresh(now), Stations: len(f.Stations), Vehicles: len(f.Vehicles),
			VehicleTypes: len(f.VehicleTypes), Error: f.LastError}
		if !f.DataUpdatedAt.IsZero() {
			st.Updated = f.DataUpdatedAt.Format(time.RFC3339)
		}
		out.Feeds = append(out.Feeds, st)
	}
	sort.Slice(out.Feeds, func(i, j int) bool { return out.Feeds[i].Name < out.Feeds[j].Name })
	return out
}
