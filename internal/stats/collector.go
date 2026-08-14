// Collector: campiona i delta del RTOverlay a ogni rebuild, registrando SOLO
// osservazioni "settled" — fermate che il feed conferma superate (Passed).
// I delta predetti oscillano a ogni poll: contarli produrrebbe distribuzioni
// avvelenate dal double-counting. La transizione prev→next più il watermark
// per-trip garantiscono UN campione per passaggio reale.
package stats

import (
	"sync"
	"time"

	"gotransit/internal/transit"
)

// Collector implements rt.OverlayObserver, feeding a Store.
type Collector struct {
	Store *Store
	// SampleStride: record every Nth passed stop (1 = all). Bounds volume on
	// very dense networks without biasing the distribution.
	SampleStride int

	mu        sync.Mutex
	watermark map[uint32]int16 // trip → highest recorded pattern pos
	tt        *transit.Timetable
	day       int // local YearDay the watermark belongs to
}

// NewCollector wires a collector to a store.
func NewCollector(st *Store) *Collector {
	return &Collector{Store: st, SampleStride: 1, watermark: map[uint32]int16{}}
}

// ObserveOverlay records the newly settled (trip, stop) passages of next
// relative to prev. Called by rt.Manager after each overlay rebuild.
func (c *Collector) ObserveOverlay(tt *transit.Timetable, prev, next *transit.RTOverlay) {
	if c == nil || c.Store == nil || tt == nil || next == nil {
		return
	}
	now := time.Now().In(tt.TZ)
	day := DayTypeOf(now)

	c.mu.Lock()
	defer c.mu.Unlock()
	// New timetable or new service day: the watermark no longer applies.
	if c.tt != tt || c.day != now.YearDay() {
		c.watermark = map[uint32]int16{}
		c.tt = tt
		c.day = now.YearDay()
	}

	stride := c.SampleStride
	if stride < 1 {
		stride = 1
	}

	for t := range next.Passed {
		trip := uint32(t)
		np := next.Passed[t]
		if np < 0 || !next.HasRT[t] || next.TripOff[t] < 0 {
			continue
		}
		// Start after both the previous overlay's confirmation and our own
		// watermark: feed flaps (prev resets to -1) must not re-count.
		from := int16(0)
		if prev != nil && t < len(prev.Passed) && prev.Passed[t] >= 0 {
			from = prev.Passed[t] + 1
		}
		if wm, ok := c.watermark[trip]; ok && wm+1 > from {
			from = wm + 1
		}
		if np < from {
			continue
		}

		pat := tt.PatternOfTrip(trip)
		rm := tt.Routes[tt.PatRoute[pat]]
		route := rm.Feed + ":" + rm.GTFSID

		off := next.TripOff[t]
		n := int32(tt.TripLen(trip))
		for pos := from; pos <= np; pos++ {
			if int32(pos) >= n {
				break
			}
			if stride > 1 && int(pos)%stride != 0 {
				continue
			}
			if next.StopSkip[off+int32(pos)] {
				continue
			}
			schedDep := tt.ScheduledDep(trip, uint16(pos))
			hour := int(schedDep/3600) % 24
			c.Store.Observe(Arrival, route, day, hour, float64(next.ArrDelta[off+int32(pos)]), now)
			c.Store.Observe(Departure, route, day, hour, float64(next.DepDelta[off+int32(pos)]), now)
		}
		c.watermark[trip] = np
	}
}
