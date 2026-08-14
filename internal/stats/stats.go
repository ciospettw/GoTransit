// Package stats accumulates per-route delay distributions from GTFS-RT and
// answers probability queries ("will the rider make this connection?").
//
// Collection is OPT-IN ([stats] enabled = true): a deployment that already
// mines its realtime data elsewhere runs with the collector off and the
// engine falls back to the analytic model in predict.go — probabilities are
// always available, the learned histograms only sharpen them.
//
// Design: fixed-bin histograms keyed by (route, day type, hour), with lazy
// exponential decay (half-life ~2 weeks) so recent disruption dominates and
// stale patterns fade. Everything is in-memory; snapshot.go persists via gob.
// Zero external dependencies, like the rest of the engine.
package stats

import (
	"math"
	"sync"
	"time"
)

// NumBins is the fixed histogram width. Bin 0 is "very early", the last bin
// is "very late"; edges are seconds of delay relative to schedule.
const NumBins = 32

// binEdges are the 31 boundaries between the 32 bins (seconds). Log-ish grid:
// dense around zero where connections are decided, coarse in the long tail.
var binEdges = [NumBins - 1]float64{
	-300, -180, -120, -90, -60, -45, -30, -15,
	0, 15, 30, 45, 60, 90, 120, 150,
	180, 240, 300, 360, 420, 480, 600, 720,
	900, 1080, 1320, 1620, 1980, 2400, 3000,
}

// binMid returns a representative value for bin i (midpoint; clamped tails).
func binMid(i int) float64 {
	switch {
	case i <= 0:
		return binEdges[0] - 60
	case i >= NumBins-1:
		return binEdges[NumBins-2] + 300
	default:
		return (binEdges[i-1] + binEdges[i]) / 2
	}
}

// binOf maps a delay (seconds) to its bin.
func binOf(d float64) int {
	for i, e := range binEdges {
		if d < e {
			return i
		}
	}
	return NumBins - 1
}

// Hist is an immutable normalized snapshot of a delay distribution, safe to
// pass around and query without locks.
type Hist struct {
	P      [NumBins]float64 // probabilities, sum ≈ 1
	Weight float64          // decayed observation weight behind it
}

// Mean returns the expected delay in seconds.
func (h *Hist) Mean() float64 {
	m := 0.0
	for i, p := range h.P {
		m += p * binMid(i)
	}
	return m
}

// CDF returns P(delay ≤ x) with linear interpolation inside the bin.
func (h *Hist) CDF(x float64) float64 {
	acc := 0.0
	for i := 0; i < NumBins; i++ {
		lo, hi := binLo(i), binHi(i)
		if x >= hi {
			acc += h.P[i]
			continue
		}
		if x <= lo {
			return acc
		}
		return acc + h.P[i]*(x-lo)/(hi-lo)
	}
	return 1
}

func binLo(i int) float64 {
	if i == 0 {
		return binEdges[0] - 120 // nominal open tail
	}
	return binEdges[i-1]
}

func binHi(i int) float64 {
	if i == NumBins-1 {
		return binEdges[NumBins-2] + 600
	}
	return binEdges[i]
}

// DayType buckets service days: weekday / saturday / sunday-holiday.
type DayType uint8

const (
	Weekday DayType = 0
	Saturday DayType = 1
	Sunday   DayType = 2
	// AnyDay / AnyHour are aggregate keys used in the fallback chain.
	AnyDay  DayType = 255
	AnyHour uint8   = 255
)

// DayTypeOf classifies a local time.
func DayTypeOf(t time.Time) DayType {
	switch t.Weekday() {
	case time.Saturday:
		return Saturday
	case time.Sunday:
		return Sunday
	default:
		return Weekday
	}
}

// Kind separates arrival-delay from departure-delay distributions.
type Kind uint8

const (
	Arrival   Kind = 0
	Departure Kind = 1
)

type key struct {
	Route string
	Day   DayType
	Hour  uint8
	Kind  Kind
}

type bucket struct {
	Counts  [NumBins]float32
	Weight  float32
	LastUpd int64 // unix seconds, anchor for lazy decay
}

// Provider supplies delay distributions to the probability model. The engine
// depends on this interface (nil = analytic only); Store is the built-in
// implementation, an external data pipeline can plug its own.
type Provider interface {
	// Dist returns the delay distribution for boarding/alighting the given
	// route around the given local hour, walking the fallback chain
	// (route+day+hour → route+day → route → global). ok = false when even
	// the global distribution has too little data to be trusted.
	Dist(kind Kind, routeID string, day DayType, hour int) (Hist, bool)
}

// Store is the built-in histogram store.
type Store struct {
	mu      sync.RWMutex
	buckets map[key]*bucket

	// HalfLife controls the lazy exponential decay (default 14 days).
	HalfLife time.Duration
	// MinSamples is the decayed weight below which a bucket is not trusted.
	MinSamples float64

	dirty bool // set by Observe, cleared by snapshots
}

// NewStore creates an empty store with the given tuning (zero values → defaults).
func NewStore(halfLife time.Duration, minSamples float64) *Store {
	if halfLife <= 0 {
		halfLife = 14 * 24 * time.Hour
	}
	if minSamples <= 0 {
		minSamples = 20
	}
	return &Store{
		buckets:    map[key]*bucket{},
		HalfLife:   halfLife,
		MinSamples: minSamples,
	}
}

func (s *Store) decayFactor(last, now int64) float64 {
	if last == 0 || now <= last {
		return 1
	}
	elapsed := float64(now - last)
	return math.Exp2(-elapsed / s.HalfLife.Seconds())
}

// Observe records one settled delay observation (seconds vs schedule).
// hour is the scheduled local hour of the event. Every observation also
// feeds the aggregate buckets of the fallback chain.
func (s *Store) Observe(kind Kind, routeID string, day DayType, hour int, delaySec float64, now time.Time) {
	if routeID == "" {
		return
	}
	bin := binOf(delaySec)
	nowU := now.Unix()
	keys := [4]key{
		{routeID, day, uint8(hour), kind},
		{routeID, day, AnyHour, kind},
		{routeID, AnyDay, AnyHour, kind},
		{"", AnyDay, AnyHour, kind}, // global
	}
	s.mu.Lock()
	for _, k := range keys {
		b := s.buckets[k]
		if b == nil {
			b = &bucket{}
			s.buckets[k] = b
		}
		f := float32(s.decayFactor(b.LastUpd, nowU))
		if f != 1 {
			for i := range b.Counts {
				b.Counts[i] *= f
			}
			b.Weight *= f
		}
		b.Counts[bin]++
		b.Weight++
		b.LastUpd = nowU
	}
	s.dirty = true
	s.mu.Unlock()
}

// Dist implements Provider with the documented fallback chain.
func (s *Store) Dist(kind Kind, routeID string, day DayType, hour int) (Hist, bool) {
	if s == nil {
		return Hist{}, false
	}
	now := time.Now().Unix()
	chain := [4]key{
		{routeID, day, uint8(hour), kind},
		{routeID, day, AnyHour, kind},
		{routeID, AnyDay, AnyHour, kind},
		{"", AnyDay, AnyHour, kind},
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, k := range chain {
		b := s.buckets[k]
		if b == nil {
			continue
		}
		w := float64(b.Weight) * s.decayFactor(b.LastUpd, now)
		if w < s.MinSamples {
			continue
		}
		var h Hist
		total := 0.0
		for i := range b.Counts {
			total += float64(b.Counts[i])
		}
		if total <= 0 {
			continue
		}
		for i := range b.Counts {
			h.P[i] = float64(b.Counts[i]) / total
		}
		h.Weight = w
		return h, true
	}
	return Hist{}, false
}

// Len returns the number of live buckets (for /v1/status introspection).
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.buckets)
}
