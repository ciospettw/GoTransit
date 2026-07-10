// Package transit compiles GTFS feeds into the flat timetable RAPTOR scans:
// stop-pattern-trip arrays, walking transfers computed on the street graph,
// shapes sliced per pattern. Immutable after compile; live updates build a
// fresh Timetable and swap a pointer.
package transit

import (
	"sync"
	"sync/atomic"
	"time"

	"gotransit/internal/gtfs"
)

// Timetable is the compiled multi-feed transit network.
type Timetable struct {
	// stops (global across feeds)
	StopID   []string // "feed:gtfs_stop_id"
	StopCode []string
	StopName []string
	StopLat  []int32
	StopLon  []int32
	StopFeed []uint8

	// patterns: unique (route, stop sequence); trips grouped per pattern
	PatFirstStop []uint32 // CSR into PatStops/PatShapeIdx
	PatStops     []int32
	PatShapeIdx  []uint32 // shape point index of each pattern stop (when shaped)
	PatFirstTrip []uint32 // CSR into trip arrays
	PatRoute     []int32
	PatShape     []int32 // -1 = no shape

	// trips, grouped by pattern and sorted by first departure
	TripService  []int32
	TripHeadsign []int32
	TripID       []string
	TripFeed     []uint8
	TripTimeOff  []uint32 // into Arr/Dep

	// stop times: seconds since service-day midnight (may exceed 86400)
	Arr []uint32
	Dep []uint32
	Seq []uint16 // original GTFS stop_sequence, parallel to Arr/Dep (GTFS-RT matching)

	// "feed:trip_id" → trip index (GTFS-RT matching)
	TripIdx map[string]uint32

	// stop → (pattern, position) adjacency
	StopFirstPat []uint32
	StopPat      []uint32
	StopPatPos   []uint16

	// precomputed walking transfers (street-network distances)
	XferFirst []uint32
	XferTo    []int32
	XferDs    []uint16

	// display metadata
	Routes    []RouteMeta
	Headsigns []string
	Agencies  []string

	// service calendars
	Services []gtfs.Service
	TZ       *time.Location

	// shapes
	ShpFirst []uint32
	ShpLat   []int32
	ShpLon   []int32
	ShpCumDm []uint32 // cumulative decimeters

	// stop → street graph attachment (computed against a specific graph)
	StopSnap []StopSnap

	// reverse anchoring: graph node → stops anchored there (sorted by node),
	// used to harvest stop seeds out of an access/egress street search.
	NSNode  []int32
	NSStop  []int32
	NSExtra []uint16 // deciseconds from the node to the stop

	Feeds    []string
	Excluded ExclusionReport

	// GTFS-RT hook: a future overlay holds per-trip delay/cancel state; the
	// scan reads through it when present. Swapped atomically, never mutated.
	rt atomic.Pointer[RTOverlay]

	svcCache sync.Map // dateKey(int) → []uint64 active-service bitset
}

// RouteMeta is display info for one GTFS route.
type RouteMeta struct {
	GTFSID    string
	Short     string
	Long      string
	Color     string
	TextColor string
	Agency    string
	Feed      string
	Type      int
}

// StopSnap anchors a stop onto the street graph for access/egress/transfers.
type StopSnap struct {
	NodeU, NodeV int32 // -1 when the stop could not be snapped
	DsU, DsV     uint16
	PerpM        uint16
}

// ExclusionReport lists what coverage validation cut, per requirement:
// shapes leaving the imported graph exclude their trips categorically.
type ExclusionReport struct {
	Trips     int
	Routes    []ExcludedRoute
	Unsnapped int // stops with no street within snap radius (kept, but no access)
}

type ExcludedRoute struct {
	Feed    string
	RouteID string
	Short   string
	Trips   int
	Reason  string
}

// RTOverlay projects GTFS-RT onto the timetable: per-trip-per-stop time
// deltas, cancellations, skipped stops and passed-position tracking.
// Immutable — each RT poll builds a fresh one and swaps the pointer, so the
// RAPTOR hot path reads it without locks.
type RTOverlay struct {
	// per trip: offset into the flattened per-stop arrays, or -1 (no data)
	TripOff []int32
	Skip    []bool // cancelled trips
	HasRT   []bool // any live signal (trip update or vehicle) for this trip

	// flattened per-stop deltas in seconds (aligned with the trip's stops)
	ArrDelta []int32
	DepDelta []int32
	StopSkip []bool // SKIPPED stops: no boarding/alighting

	// Passed[t] = highest pattern position the vehicle has confirmedly
	// passed or is stopped at (-1 unknown); drives virtual user tracking.
	Passed []int16

	// live vehicle, per trip (from VehiclePosition entities):
	// position (0,0 = unknown), the pattern position it is at/heading to
	// (-1 unknown) and the raw status (0 incoming, 1 stopped, 2 in transit).
	VehLat    []int32
	VehLon    []int32
	VehPos    []int16
	VehStatus []int8

	// feed timestamps (unix) per static feed index; 0 = feed absent/stale
	FeedTime []uint64

	Version uint64 // monotonically increasing, for change notifications
}

// Vehicle returns the live vehicle of a trip, if any (position and/or the
// pattern stop it is at — some operators send only one of the two).
func (o *RTOverlay) Vehicle(trip uint32) (latE7, lonE7 int32, pos int16, status int8, ok bool) {
	if o == nil || int(trip) >= len(o.VehPos) {
		return 0, 0, -1, -1, false
	}
	if o.VehPos[trip] < 0 && o.VehLat[trip] == 0 && o.VehLon[trip] == 0 {
		return 0, 0, -1, -1, false
	}
	return o.VehLat[trip], o.VehLon[trip], o.VehPos[trip], o.VehStatus[trip], true
}

// SetRT atomically installs (or clears) the realtime overlay.
func (tt *Timetable) SetRT(o *RTOverlay) { tt.rt.Store(o) }

// RT returns the current overlay (nil when no realtime data).
func (tt *Timetable) RT() *RTOverlay { return tt.rt.Load() }

func (tt *Timetable) rtOverlay() *RTOverlay { return tt.rt.Load() }

// rtIdx returns the flattened index for (trip, pos), or -1.
func (o *RTOverlay) rtIdx(trip uint32, pos uint16) int32 {
	if o == nil || int(trip) >= len(o.TripOff) {
		return -1
	}
	off := o.TripOff[trip]
	if off < 0 {
		return -1
	}
	return off + int32(pos)
}

// TripHasRT reports whether the trip has any live signal.
func (o *RTOverlay) TripHasRT(trip uint32) bool {
	return o != nil && int(trip) < len(o.HasRT) && o.HasRT[trip]
}

// TripPassed returns the highest confirmed-passed position, or -1.
func (o *RTOverlay) TripPassed(trip uint32) int16 {
	if o == nil || int(trip) >= len(o.Passed) {
		return -1
	}
	return o.Passed[trip]
}

// NumStops returns the global stop count.
func (tt *Timetable) NumStops() int { return len(tt.StopID) }

// NumPatterns returns the pattern count.
func (tt *Timetable) NumPatterns() int { return len(tt.PatRoute) }

// NumTrips returns the trip count.
func (tt *Timetable) NumTrips() int { return len(tt.TripService) }

// PatternStops returns the stop sequence of pattern p.
func (tt *Timetable) PatternStops(p uint32) []int32 {
	return tt.PatStops[tt.PatFirstStop[p]:tt.PatFirstStop[p+1]]
}

// PatternTrips returns the trip index range of pattern p.
func (tt *Timetable) PatternTrips(p uint32) (uint32, uint32) {
	return tt.PatFirstTrip[p], tt.PatFirstTrip[p+1]
}

// TripArr / TripDep read times through the RT overlay when installed.
func (tt *Timetable) TripArr(trip uint32, pos uint16) uint32 {
	v := tt.Arr[tt.TripTimeOff[trip]+uint32(pos)]
	if o := tt.rtOverlay(); o != nil {
		if i := o.rtIdx(trip, pos); i >= 0 {
			return addDelta(v, o.ArrDelta[i])
		}
	}
	return v
}

func (tt *Timetable) TripDep(trip uint32, pos uint16) uint32 {
	v := tt.Dep[tt.TripTimeOff[trip]+uint32(pos)]
	if o := tt.rtOverlay(); o != nil {
		if i := o.rtIdx(trip, pos); i >= 0 {
			return addDelta(v, o.DepDelta[i])
		}
	}
	return v
}

func addDelta(v uint32, d int32) uint32 {
	nv := int64(v) + int64(d)
	if nv < 0 {
		return 0
	}
	return uint32(nv)
}

// TripSkipped reports RT cancellation of the whole trip.
func (tt *Timetable) TripSkipped(trip uint32) bool {
	o := tt.rtOverlay()
	return o != nil && int(trip) < len(o.Skip) && o.Skip[trip]
}

// StopSkipped reports an RT SKIPPED stop: no boarding, no alighting there.
func (tt *Timetable) StopSkipped(trip uint32, pos uint16) bool {
	o := tt.rtOverlay()
	if o == nil {
		return false
	}
	i := o.rtIdx(trip, pos)
	return i >= 0 && o.StopSkip[i]
}

// TripSeqPos maps an original GTFS stop_sequence to the pattern position.
func (tt *Timetable) TripSeqPos(trip uint32, seq uint16) (uint16, bool) {
	lo := tt.TripTimeOff[trip]
	var hi uint32
	if int(trip)+1 < len(tt.TripTimeOff) {
		hi = tt.TripTimeOff[trip+1]
	} else {
		hi = uint32(len(tt.Seq))
	}
	for i := lo; i < hi; i++ {
		if tt.Seq[i] == seq {
			return uint16(i - lo), true
		}
	}
	return 0, false
}

// TripLen returns the number of stops of a trip.
func (tt *Timetable) TripLen(trip uint32) uint16 {
	lo := tt.TripTimeOff[trip]
	if int(trip)+1 < len(tt.TripTimeOff) {
		return uint16(tt.TripTimeOff[trip+1] - lo)
	}
	return uint16(uint32(len(tt.Arr)) - lo)
}

// ScheduledArr / ScheduledDep read the static schedule, bypassing the RT
// overlay (deltas are computed against these).
func (tt *Timetable) ScheduledArr(trip uint32, pos uint16) uint32 {
	return tt.Arr[tt.TripTimeOff[trip]+uint32(pos)]
}

func (tt *Timetable) ScheduledDep(trip uint32, pos uint16) uint32 {
	return tt.Dep[tt.TripTimeOff[trip]+uint32(pos)]
}

// PatternOfTrip recovers the pattern owning a trip (trips are grouped).
func (tt *Timetable) PatternOfTrip(trip uint32) uint32 {
	lo, hi := 0, len(tt.PatFirstTrip)-1
	for lo < hi-1 {
		mid := (lo + hi) / 2
		if tt.PatFirstTrip[mid] <= trip {
			lo = mid
		} else {
			hi = mid
		}
	}
	return uint32(lo)
}

// Transfers returns the precomputed footpaths out of stop s.
func (tt *Timetable) Transfers(s int32) ([]int32, []uint16) {
	lo, hi := tt.XferFirst[s], tt.XferFirst[s+1]
	return tt.XferTo[lo:hi], tt.XferDs[lo:hi]
}

// PatternsOf returns the patterns serving stop s with s's position in each.
func (tt *Timetable) PatternsOf(s int32) ([]uint32, []uint16) {
	lo, hi := tt.StopFirstPat[s], tt.StopFirstPat[s+1]
	return tt.StopPat[lo:hi], tt.StopPatPos[lo:hi]
}

// ActiveServices returns (building if needed) the service bitset for a date.
func (tt *Timetable) ActiveServices(date uint32, wd time.Weekday) []uint64 {
	if v, ok := tt.svcCache.Load(date); ok {
		return v.([]uint64)
	}
	bits := make([]uint64, (len(tt.Services)+63)/64)
	for i := range tt.Services {
		if tt.Services[i].ActiveOn(date, wd) {
			bits[i/64] |= 1 << (i % 64)
		}
	}
	tt.svcCache.Store(date, bits)
	return bits
}

// ServiceActive tests a bitset.
func ServiceActive(bits []uint64, svc int32) bool {
	return bits[svc/64]&(1<<(svc%64)) != 0
}
