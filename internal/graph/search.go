package graph

import (
	"gotransit/internal/geo"
)

// Seed is a search start: a node plus the cost already paid to reach it
// (e.g. walking from the query point to the snapped edge endpoint).
type Seed struct {
	Node int32
	Ds   uint32
}

// ---- bounded multi-source Dijkstra ------------------------------------------
//
// Powers stop access/egress and stop-to-stop transfer precomputation.
// Distances are uint16 deciseconds (max ~1.8 h — far beyond any access leg),
// state arrays are epoch-stamped so reuse costs nothing.

type NearSearch struct {
	dist    []uint16
	stamp   []uint16
	parent  []int32
	epoch   uint16
	heap    []uint64 // ds<<32 | node
	touched []int32
}

// NewNearSearch sizes the search state for a graph.
func NewNearSearch(numNodes int) *NearSearch {
	return &NearSearch{
		dist:   make([]uint16, numNodes),
		stamp:  make([]uint16, numNodes),
		parent: make([]int32, numNodes),
	}
}

const maxNearDs = 65000

// Run explores from seeds until maxDs (deciseconds), in mode m.
// speedFactor: see SpeedFactor.
func (s *NearSearch) Run(g *Graph, seeds []Seed, m Mode, speedFactor uint32, maxDs uint32) {
	if maxDs > maxNearDs {
		maxDs = maxNearDs
	}
	s.epoch++
	if s.epoch == 0 {
		clear(s.stamp)
		s.epoch = 1
	}
	s.heap = s.heap[:0]
	s.touched = s.touched[:0]
	for _, sd := range seeds {
		if sd.Ds > maxDs {
			continue
		}
		if s.improve(sd.Node, uint16(sd.Ds), -1) {
			s.heap = heapPush(s.heap, uint64(sd.Ds)<<32|uint64(uint32(sd.Node)))
		}
	}
	for len(s.heap) > 0 {
		var it uint64
		it, s.heap = heapPop(s.heap)
		ds := uint32(it >> 32)
		n := int32(uint32(it))
		if uint16(ds) != s.dist[n] || s.stamp[n] != s.epoch {
			continue // stale
		}
		lo, hi := g.EdgesOf(n)
		for e := lo; e < hi; e++ {
			if g.EdgeFlags[e]&uint8(m) == 0 {
				continue
			}
			nd := ds + g.EdgeDs(e, m, speedFactor)
			if nd > maxDs {
				continue
			}
			t := g.EdgeTarget[e]
			if s.improve(t, uint16(nd), int32(e)) {
				s.heap = heapPush(s.heap, uint64(nd)<<32|uint64(uint32(t)))
			}
		}
	}
}

func (s *NearSearch) improve(n int32, ds uint16, parent int32) bool {
	if s.stamp[n] == s.epoch && s.dist[n] <= ds {
		return false
	}
	if s.stamp[n] != s.epoch {
		s.touched = append(s.touched, n)
	}
	s.stamp[n] = s.epoch
	s.dist[n] = ds
	s.parent[n] = parent
	return true
}

// Dist returns the cost to n in deciseconds, if reached.
func (s *NearSearch) Dist(n int32) (uint32, bool) {
	if s.stamp[n] != s.epoch {
		return 0, false
	}
	return uint32(s.dist[n]), true
}

// Touched lists every node reached by the last Run.
func (s *NearSearch) Touched() []int32 { return s.touched }

// PathTo returns the directed edges from the seed to n, in travel order.
func (s *NearSearch) PathTo(g *Graph, n int32) []uint32 {
	if s.stamp[n] != s.epoch {
		return nil
	}
	var rev []uint32
	for {
		pe := s.parent[n]
		if pe < 0 {
			break
		}
		rev = append(rev, uint32(pe))
		n = g.sourceOf(uint32(pe))
	}
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	return rev
}

// ---- point-to-point weighted A* ---------------------------------------------
//
// Car and direct-bike routing. Unidirectional A* over the live CSR arrays:
// no preprocessing to invalidate when runtime OSM diffs land. eps > 1 trades
// a bounded optimality gap for a much smaller search cone ("fast" mode).

type RoadSearch struct {
	dist   []uint32
	stamp  []uint32
	parent []int32
	epoch  uint32
	heap   []uint64
}

func NewRoadSearch(numNodes int) *RoadSearch {
	return &RoadSearch{
		dist:   make([]uint32, numNodes),
		stamp:  make([]uint32, numNodes),
		parent: make([]int32, numNodes),
	}
}

// RoadResult is a point-to-point route as directed edges.
type RoadResult struct {
	Edges   []uint32
	Ds      uint32 // total cost including seed and target extras
	EndSeed int32  // which dst seed the route reached
	Settled int
	Found   bool
}

// Route runs A* from srcSeeds to dstSeeds. dstLat/dstLon is the geometric
// target for the heuristic; extraDs are per-dst-seed completion costs.
func (s *RoadSearch) Route(g *Graph, srcSeeds, dstSeeds []Seed, dstLat, dstLon int32,
	m Mode, speedFactor uint32, eps float64, maxSettled int) RoadResult {

	s.epoch++
	if s.epoch == 0 {
		clear(s.stamp)
		s.epoch = 1
	}
	s.heap = s.heap[:0]

	// admissible cost-per-meter lower bound
	var dsPerM float64
	if m == ModeCar {
		dsPerM = 36.0 / 130.0
	} else {
		dsPerM = float64(speedFactor) / 65536.0
	}
	h := func(n int32) uint32 {
		return uint32(eps * dsPerM * geo.Dist(g.NodeLat[n], g.NodeLon[n], dstLat, dstLon))
	}

	type dstInfo struct{ extra uint32 }
	dst := make(map[int32]dstInfo, len(dstSeeds))
	for _, d := range dstSeeds {
		if cur, ok := dst[d.Node]; !ok || d.Ds < cur.extra {
			dst[d.Node] = dstInfo{extra: d.Ds}
		}
	}

	res := RoadResult{EndSeed: -1}
	best := ^uint32(0)
	var bestNode int32 = -1

	push := func(n int32, ds uint32, parent int32) {
		if s.stamp[n] == s.epoch && s.dist[n] <= ds {
			return
		}
		s.stamp[n] = s.epoch
		s.dist[n] = ds
		s.parent[n] = parent
		s.heap = heapPush(s.heap, uint64(ds+h(n))<<32|uint64(uint32(n)))
	}
	for _, sd := range srcSeeds {
		push(sd.Node, sd.Ds, -1)
	}

	settled := 0
	for len(s.heap) > 0 {
		var it uint64
		it, s.heap = heapPop(s.heap)
		key := uint32(it >> 32)
		n := int32(uint32(it))
		ds := s.dist[n]
		if s.stamp[n] != s.epoch || key != ds+h(n) {
			continue // stale entry
		}
		if key >= best {
			break // no path through n can improve the best complete route
		}
		if d, ok := dst[n]; ok {
			if total := ds + d.extra; total < best {
				best, bestNode = total, n
			}
		}
		settled++
		if settled > maxSettled {
			break
		}
		lo, hi := g.EdgesOf(n)
		for e := lo; e < hi; e++ {
			if g.EdgeFlags[e]&uint8(m) == 0 {
				continue
			}
			push(g.EdgeTarget[e], ds+g.EdgeDs(e, m, speedFactor), int32(e))
		}
	}
	if bestNode < 0 {
		return res
	}
	res.Found = true
	res.Ds = best
	res.EndSeed = bestNode
	res.Settled = settled
	var rev []uint32
	n := bestNode
	for {
		pe := s.parent[n]
		if pe < 0 {
			break
		}
		rev = append(rev, uint32(pe))
		n = g.sourceOf(uint32(pe))
	}
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	res.Edges = rev
	return res
}

// ---- binary heap on packed uint64 (key<<32 | node) ---------------------------

func heapPush(h []uint64, it uint64) []uint64 {
	h = append(h, it)
	i := len(h) - 1
	for i > 0 {
		p := (i - 1) / 2
		if h[p] <= h[i] {
			break
		}
		h[p], h[i] = h[i], h[p]
		i = p
	}
	return h
}

func heapPop(h []uint64) (uint64, []uint64) {
	top := h[0]
	last := len(h) - 1
	h[0] = h[last]
	h = h[:last]
	i := 0
	for {
		l, r := 2*i+1, 2*i+2
		small := i
		if l < len(h) && h[l] < h[small] {
			small = l
		}
		if r < len(h) && h[r] < h[small] {
			small = r
		}
		if small == i {
			break
		}
		h[i], h[small] = h[small], h[i]
		i = small
	}
	return top, h
}
