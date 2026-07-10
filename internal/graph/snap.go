package graph

import (
	"math"
	"slices"
	"sort"

	"gotransit/internal/geo"
)

// Grid is a uniform spatial index over canonical edges, stored as CSR.
type Grid struct {
	MinLat, MinLon int32 // E7 origin
	CellSize       int32 // E7 units per cell (~333 m)
	W, H           int32
	CellStart      []uint32
	Items          []uint32 // canonical directed edge ids
}

const gridCellE7 = 30000 // 0.003° ≈ 333 m N-S

func (gr *Grid) cellOf(lat, lon int32) (int32, int32) {
	return (lon - gr.MinLon) / gr.CellSize, (lat - gr.MinLat) / gr.CellSize
}

// sourceOf recovers the source node of a directed edge (CSR binary search).
func (g *Graph) sourceOf(e uint32) int32 {
	return int32(sort.Search(len(g.FirstEdge)-1, func(i int) bool { return g.FirstEdge[i+1] > e }))
}

// SourceOf is the exported source-node lookup.
func (g *Graph) SourceOf(e uint32) int32 { return g.sourceOf(e) }

// Twin returns the opposite directed edge of e, or -1 (e.g. oneways).
func (g *Graph) Twin(e uint32) int32 {
	u := g.sourceOf(e)
	return g.twin(e, u, g.EdgeTarget[e])
}

// ReversePath maps a directed edge path onto its reverse traversal.
// Returns false if any edge has no twin (shouldn't happen on foot paths).
func (g *Graph) ReversePath(edges []uint32) ([]uint32, bool) {
	out := make([]uint32, len(edges))
	for i, e := range edges {
		tw := g.Twin(e)
		if tw < 0 {
			return nil, false
		}
		out[len(edges)-1-i] = uint32(tw)
	}
	return out, true
}

// twin finds the opposite directed edge of e (u→v), or -1.
func (g *Graph) twin(e uint32, u, v int32) int32 {
	lo, hi := g.EdgesOf(v)
	for f := lo; f < hi; f++ {
		if g.EdgeTarget[f] != u || f == e {
			continue
		}
		if g.EdgeGeom[f] == g.EdgeGeom[e] && g.EdgeMeters[f] == g.EdgeMeters[e] {
			return int32(f)
		}
	}
	return -1
}

// isCanonical: an edge is indexed in the grid once per undirected pair.
func (g *Graph) isCanonical(e uint32, u, v int32) bool {
	if u < v {
		return true
	}
	if u == v {
		return false
	}
	return g.twin(e, u, v) == -1 // reverse-only oneways
}

func buildGrid(g *Graph) Grid {
	if g.NumNodes() == 0 {
		return Grid{CellSize: gridCellE7, W: 1, H: 1, CellStart: make([]uint32, 2)}
	}
	minLat, minLon := int32(math.MaxInt32), int32(math.MaxInt32)
	maxLat, maxLon := int32(math.MinInt32), int32(math.MinInt32)
	for i := range g.NodeLat {
		minLat = min(minLat, g.NodeLat[i])
		maxLat = max(maxLat, g.NodeLat[i])
		minLon = min(minLon, g.NodeLon[i])
		maxLon = max(maxLon, g.NodeLon[i])
	}
	gr := Grid{MinLat: minLat, MinLon: minLon, CellSize: gridCellE7}
	gr.W = (maxLon-minLon)/gr.CellSize + 1
	gr.H = (maxLat-minLat)/gr.CellSize + 1

	// collect (cell, edge) pairs by sampling each edge's shape
	var entries []uint64
	var lats, lons []int32
	for u := int32(0); u < int32(g.NumNodes()); u++ {
		lo, hi := g.EdgesOf(u)
		for e := lo; e < hi; e++ {
			v := g.EdgeTarget[e]
			if !g.isCanonical(e, u, v) {
				continue
			}
			lats, lons = lats[:0], lons[:0]
			lats, lons = g.AppendGeometry(e, u, false, lats, lons)
			prevCell := int64(-1)
			for i := 1; i < len(lats); i++ {
				entries, prevCell = gr.appendSegmentCells(entries, uint32(e), prevCell,
					lats[i-1], lons[i-1], lats[i], lons[i])
			}
		}
	}
	slices.Sort(entries)

	gr.CellStart = make([]uint32, int(gr.W)*int(gr.H)+1)
	var items []uint32
	var last uint64 = math.MaxUint64
	for _, en := range entries {
		if en == last {
			continue
		}
		last = en
		cell := en >> 32
		gr.CellStart[cell+1]++
		items = append(items, uint32(en))
	}
	for i := 1; i < len(gr.CellStart); i++ {
		gr.CellStart[i] += gr.CellStart[i-1]
	}
	gr.Items = items
	return gr
}

// appendSegmentCells samples a segment every half cell so every crossed cell
// gets an entry, without a full supercover rasterizer.
func (gr *Grid) appendSegmentCells(entries []uint64, e uint32, prevCell int64, aLat, aLon, bLat, bLon int32) ([]uint64, int64) {
	steps := max(absI32(bLat-aLat), absI32(bLon-aLon))/(gr.CellSize/2) + 1
	for s := int32(0); s <= steps; s++ {
		lat := aLat + int32(int64(bLat-aLat)*int64(s)/int64(steps))
		lon := aLon + int32(int64(bLon-aLon)*int64(s)/int64(steps))
		cx, cy := gr.cellOf(lat, lon)
		cx = min(max(cx, 0), gr.W-1)
		cy = min(max(cy, 0), gr.H-1)
		cell := int64(cy)*int64(gr.W) + int64(cx)
		if cell != prevCell {
			entries = append(entries, uint64(cell)<<32|uint64(e))
			prevCell = cell
		}
	}
	return entries, prevCell
}

func absI32(v int32) int32 {
	if v < 0 {
		return -v
	}
	return v
}

// Snap is a query point matched onto the street network.
type Snap struct {
	Fwd, Bwd   int32 // directed edge ids u→v / v→u (-1 when absent)
	U, V       int32
	PLat, PLon int32   // projected point on the edge shape
	PerpM      float64 // straight distance query → projected point
	AlongU     float64 // meters from U to the point along the shape
	AlongV     float64 // meters from the point to V along the shape
	Flags      uint8   // union of both directions' flags
}

// SnapPoint finds the nearest edge usable in mode m within maxM meters.
func (g *Graph) SnapPoint(latE7, lonE7 int32, m Mode, maxM float64) (Snap, bool) {
	gr := &g.Grid
	if len(gr.Items) == 0 {
		return Snap{}, false
	}
	cx, cy := gr.cellOf(latE7, lonE7)
	cellM := float64(gr.CellSize) * 1e-7 * 111195 // N-S meters per cell
	maxRing := int32(maxM/cellM) + 1

	best := Snap{Fwd: -1, Bwd: -1, PerpM: math.MaxFloat64}
	var lats, lons []int32
	consider := func(e uint32) {
		u := g.sourceOf(e)
		v := g.EdgeTarget[e]
		if len(g.SnapOK) > 0 && g.SnapOK[u]&uint8(m) == 0 {
			return // edge belongs to a tiny disconnected island in this mode
		}
		tw := g.twin(e, u, v)
		flags := g.EdgeFlags[e]
		if tw >= 0 {
			flags |= g.EdgeFlags[tw]
		}
		if flags&uint8(m) == 0 {
			return
		}
		lats, lons = lats[:0], lons[:0]
		lats, lons = g.AppendGeometry(e, u, false, lats, lons)
		var cum float64
		for i := 1; i < len(lats); i++ {
			segLen := geo.Dist(lats[i-1], lons[i-1], lats[i], lons[i])
			_, t, qLat, qLon := geo.ProjectOnSegment(latE7, lonE7, lats[i-1], lons[i-1], lats[i], lons[i])
			perp := geo.Dist(latE7, lonE7, qLat, qLon)
			if perp < best.PerpM {
				best = Snap{
					Fwd: int32(e), Bwd: tw, U: u, V: v,
					PLat: qLat, PLon: qLon, PerpM: perp,
					AlongU: cum + t*segLen, Flags: flags,
				}
			}
			cum += segLen
		}
		if best.Fwd == int32(e) {
			best.AlongV = math.Max(0, cum-best.AlongU)
		}
	}

	for ring := int32(0); ring <= maxRing; ring++ {
		// once the best hit is closer than the nearest unexplored ring, stop
		if best.PerpM < float64(ring-1)*cellM || (ring > 0 && float64(ring-1)*cellM > maxM) {
			break
		}
		for dy := -ring; dy <= ring; dy++ {
			for dx := -ring; dx <= ring; dx++ {
				if maxI32(absI32(dx), absI32(dy)) != ring {
					continue // ring perimeter only
				}
				x, y := cx+dx, cy+dy
				if x < 0 || y < 0 || x >= gr.W || y >= gr.H {
					continue
				}
				cell := int64(y)*int64(gr.W) + int64(x)
				for _, e := range gr.Items[gr.CellStart[cell]:gr.CellStart[cell+1]] {
					consider(e)
				}
			}
		}
	}
	if best.PerpM > maxM {
		return Snap{}, false
	}
	// orientation bookkeeping: Fwd is the canonical edge u→v; if only the
	// reverse direction allows the mode the caller uses Bwd.
	return best, true
}

func maxI32(a, b int32) int32 {
	if a > b {
		return a
	}
	return b
}
