package transit

import (
	"time"
)

// StopSeed seeds RAPTOR: for sources, Sec is the arrival time at the stop
// (seconds since query-date midnight, access walk included); for targets,
// Sec is the egress cost from the stop to the destination.
type StopSeed struct {
	Stop int32
	Sec  uint32
}

// Query is one earliest-arrival RAPTOR run.
type Query struct {
	Sources      []StopSeed
	Targets      []StopSeed
	Date         uint32 // YYYYMMDD in the timetable's timezone
	Weekday      time.Weekday
	PrevDate     uint32 // the day before (late-night >24:00 trips)
	PrevWeekday  time.Weekday
	MaxTransfers int
	SlackSec     uint32 // minimum vehicle-change time
}

// Journey is one pareto-optimal result (per number of rides).
type Journey struct {
	Rides  int
	ArrSec uint32
	Legs   []RLeg // travel order: (foot|ride)*
	Target int32  // stop the egress leaves from
}

// RLeg is a ride or an in-network footpath.
type RLeg struct {
	Ride          bool
	Trip          uint32
	Pattern       uint32
	Board, Alight uint16
	DayOff        int32 // seconds shift: -86400 for yesterday's service day
	From, To      int32 // foot
	Sec           uint32
}

const inf = ^uint32(0) >> 1

// parent kinds
const (
	pkNone uint8 = iota
	pkAccess
	pkRide
	pkXfer
)

// Raptor holds reusable per-query state; one instance per concurrent query.
type Raptor struct {
	tt *Timetable

	tauBest []uint32
	tau     [][]uint32
	kind    [][]uint8
	ptrip   [][]uint32
	pboard  [][]uint16
	pfrom   [][]int32
	pday    [][]int32

	marked   []bool
	markList []int32
	patMin   []int32
	patList  []uint32
}

// NewRaptor allocates state for tt.
func NewRaptor(tt *Timetable) *Raptor {
	n := tt.NumStops()
	r := &Raptor{tt: tt}
	r.tauBest = make([]uint32, n)
	r.marked = make([]bool, n)
	r.patMin = make([]int32, tt.NumPatterns())
	for i := range r.patMin {
		r.patMin[i] = -1
	}
	return r
}

func (r *Raptor) round(k int) {
	for len(r.tau) <= k {
		n := r.tt.NumStops()
		r.tau = append(r.tau, make([]uint32, n))
		r.kind = append(r.kind, make([]uint8, n))
		r.ptrip = append(r.ptrip, make([]uint32, n))
		r.pboard = append(r.pboard, make([]uint16, n))
		r.pfrom = append(r.pfrom, make([]int32, n))
		r.pday = append(r.pday, make([]int32, n))
	}
}

// Plan runs RAPTOR and returns the pareto set of journeys (by ride count).
func (r *Raptor) Plan(q Query) []Journey {
	tt := r.tt
	maxRounds := q.MaxTransfers + 1
	r.round(maxRounds)

	for i := range r.tauBest {
		r.tauBest[i] = inf
	}
	clear(r.tau[0])
	for i := range r.tau[0] {
		r.tau[0][i] = inf
	}
	clear(r.kind[0])
	r.markList = r.markList[:0]

	targetExtra := map[int32]uint32{}
	for _, t := range q.Targets {
		if cur, ok := targetExtra[t.Stop]; !ok || t.Sec < cur {
			targetExtra[t.Stop] = t.Sec
		}
	}

	for _, s := range q.Sources {
		if s.Sec < r.tau[0][s.Stop] {
			r.tau[0][s.Stop] = s.Sec
			r.tauBest[s.Stop] = s.Sec
			r.kind[0][s.Stop] = pkAccess
			if !r.marked[s.Stop] {
				r.marked[s.Stop] = true
				r.markList = append(r.markList, s.Stop)
			}
		}
	}

	// service-day layers: yesterday (times shifted -86400) and today
	layers := [2]struct {
		bits []uint64
		off  int32
	}{
		{tt.ActiveServices(q.PrevDate, q.PrevWeekday), -86400},
		{tt.ActiveServices(q.Date, q.Weekday), 0},
	}

	bestArrPerRound := make([]uint32, maxRounds+1)
	bestTargetPerRound := make([]int32, maxRounds+1)
	btUB := inf // best complete arrival so far (target pruning)
	evalTargets := func(k int) {
		bestArrPerRound[k] = inf
		bestTargetPerRound[k] = -1
		for t, extra := range targetExtra {
			if r.tau[k][t] >= inf {
				continue
			}
			if cand := r.tau[k][t] + extra; cand < bestArrPerRound[k] {
				bestArrPerRound[k] = cand
				bestTargetPerRound[k] = t
			}
		}
		if bestArrPerRound[k] < btUB {
			btUB = bestArrPerRound[k]
		}
	}
	evalTargets(0)

	for k := 1; k <= maxRounds; k++ {
		copy(r.tau[k], r.tau[k-1])
		copy(r.kind[k], r.kind[k-1])
		copy(r.ptrip[k], r.ptrip[k-1])
		copy(r.pboard[k], r.pboard[k-1])
		copy(r.pfrom[k], r.pfrom[k-1])
		copy(r.pday[k], r.pday[k-1])

		// queue: patterns serving marked stops, with earliest position
		r.patList = r.patList[:0]
		for _, s := range r.markList {
			r.marked[s] = false
			pats, poss := tt.PatternsOf(s)
			for i, p := range pats {
				if r.patMin[p] == -1 {
					r.patMin[p] = int32(poss[i])
					r.patList = append(r.patList, p)
				} else if int32(poss[i]) < r.patMin[p] {
					r.patMin[p] = int32(poss[i])
				}
			}
		}
		r.markList = r.markList[:0]

		for _, p := range r.patList {
			startPos := uint16(r.patMin[p])
			r.patMin[p] = -1
			for li := range layers {
				r.scanPattern(q, k, p, startPos, layers[li].bits, layers[li].off, &btUB)
			}
		}

		// transfers from stops improved by this round's rides
		newly := r.markList
		for _, s := range newly {
			base := r.tau[k][s]
			if base >= inf || r.kind[k][s] != pkRide {
				continue
			}
			tos, dss := tt.Transfers(s)
			for i, to := range tos {
				cand := base + uint32(dss[i])/10 + uint32(dss[i])%10/5 // ds → sec, rounded
				if cand < r.tau[k][to] && cand < r.tauBest[to] && cand < btUB {
					r.tau[k][to] = cand
					r.tauBest[to] = cand
					r.kind[k][to] = pkXfer
					r.pfrom[k][to] = s
					if !r.marked[to] {
						r.marked[to] = true
						r.markList = append(r.markList, to)
					}
				}
			}
		}
		evalTargets(k)
		if len(r.markList) == 0 {
			for kk := k + 1; kk <= maxRounds; kk++ {
				bestArrPerRound[kk] = inf
				bestTargetPerRound[kk] = -1
			}
			break
		}
	}
	// clean marks for next query
	for _, s := range r.markList {
		r.marked[s] = false
	}
	r.markList = r.markList[:0]

	// pareto extraction: a round with a strictly better arrival than every
	// earlier round is a distinct journey
	var out []Journey
	best := inf
	for k := 0; k <= maxRounds; k++ {
		if bestArrPerRound[k] < best {
			best = bestArrPerRound[k]
			if k == 0 {
				continue // pure-walk journeys are the planner's job
			}
			if j, ok := r.extract(k, bestTargetPerRound[k], bestArrPerRound[k]); ok {
				out = append(out, j)
			}
		}
	}
	return out
}

// scanPattern relaxes one pattern for one service-day layer.
func (r *Raptor) scanPattern(q Query, k int, p uint32, startPos uint16, active []uint64, dayOff int32, btUB *uint32) {
	tt := r.tt
	stops := tt.PatternStops(p)
	tLo, tHi := tt.PatternTrips(p)
	prevTau := r.tau[k-1]

	trip := int64(-1)
	var boardPos uint16
	var boardStop int32

	for pos := startPos; pos < uint16(len(stops)); pos++ {
		s := stops[pos]
		// RT SKIPPED stops: the held trip cannot drop us here
		if trip >= 0 && !tt.StopSkipped(uint32(trip), pos) {
			arr := addDay(tt.TripArr(uint32(trip), pos), dayOff)
			if arr < r.tauBest[s] && arr < *btUB {
				r.tau[k][s] = arr
				r.tauBest[s] = arr
				r.kind[k][s] = pkRide
				r.ptrip[k][s] = uint32(trip)
				r.pboard[k][s] = boardPos
				r.pfrom[k][s] = boardStop
				r.pday[k][s] = dayOff
				if !r.marked[s] {
					r.marked[s] = true
					r.markList = append(r.markList, s)
				}
			}
		}
		// try to board (or catch an earlier trip) at s.
		// invariant: trip is always active and catchable, or -1.
		rt := prevTau[s]
		if rt >= inf {
			continue
		}
		if r.kind[k-1][s] == pkRide {
			rt += q.SlackSec
		}
		usable := func(t int64) bool {
			return ServiceActive(active, tt.TripService[t]) && !tt.TripSkipped(uint32(t))
		}
		// catchable compares in int64: a shifted departure that lands before
		// today's midnight is in the past, never "infinitely late"
		catchable := func(t int64) bool {
			return int64(tt.TripDep(uint32(t), pos))+int64(dayOff) >= int64(rt)
		}
		boardable := func(t int64) bool { // RT SKIPPED stops cannot be boarded
			return usable(t) && !tt.StopSkipped(uint32(t), pos)
		}
		if trip < 0 {
			// earliest catchable trip at this stop (deps at a fixed position
			// are non-decreasing in trip order, overtaking aside)
			for t := int64(tLo); t < int64(tHi); t++ {
				if boardable(t) && catchable(t) {
					trip = t
					boardPos, boardStop = pos, s
					break
				}
			}
		} else {
			// an earlier trip may now be catchable from this stop
			for t := trip - 1; t >= int64(tLo); t-- {
				if !boardable(t) {
					continue
				}
				if catchable(t) {
					trip = t
					boardPos, boardStop = pos, s
				} else {
					break
				}
			}
		}
	}
}

func addDay(sec uint32, off int32) uint32 {
	v := int64(sec) + int64(off)
	if v < 0 {
		return inf // yesterday's trip before today's midnight: unusable
	}
	return uint32(v)
}

// extract rebuilds the journey ending at target after k rounds.
func (r *Raptor) extract(k int, target int32, arr uint32) (Journey, bool) {
	if target < 0 {
		return Journey{}, false
	}
	j := Journey{Rides: 0, ArrSec: arr, Target: target}
	var rev []RLeg
	s := target
	for kk := k; kk >= 0; {
		switch r.kind[kk][s] {
		case pkAccess:
			for i, l := 0, len(rev); i < l/2; i++ {
				rev[i], rev[l-1-i] = rev[l-1-i], rev[i]
			}
			j.Legs = rev
			for i := range j.Legs {
				if j.Legs[i].Ride {
					j.Rides++
				}
			}
			return j, true
		case pkXfer:
			from := r.pfrom[kk][s]
			rev = append(rev, RLeg{From: from, To: s, Sec: r.tau[kk][s] - r.tau[kk][from]})
			s = from
		case pkRide:
			trip := r.ptrip[kk][s]
			pat := r.patternOfTrip(trip)
			alight := r.posInPattern(pat, s, r.pboard[kk][s])
			rev = append(rev, RLeg{
				Ride: true, Trip: trip, Pattern: pat,
				Board: r.pboard[kk][s], Alight: alight, DayOff: r.pday[kk][s],
			})
			s = r.pfrom[kk][s]
			kk--
		default:
			return Journey{}, false
		}
	}
	return Journey{}, false
}

func (r *Raptor) patternOfTrip(trip uint32) uint32 { return r.tt.PatternOfTrip(trip) }

// posInPattern finds where stop s sits in pattern p at/after boardPos.
func (r *Raptor) posInPattern(p uint32, s int32, after uint16) uint16 {
	stops := r.tt.PatternStops(p)
	for pos := int(after) + 1; pos < len(stops); pos++ {
		if stops[pos] == s {
			return uint16(pos)
		}
	}
	for pos := range stops {
		if stops[pos] == s {
			return uint16(pos)
		}
	}
	return 0
}
