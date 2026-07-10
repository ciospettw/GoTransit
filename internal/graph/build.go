package graph

import (
	"fmt"
	"math"
	"slices"
	"sync"
	"time"

	"gotransit/internal/geo"
	"gotransit/internal/osm"
)

// BuildStats reports what a build did.
type BuildStats struct {
	Ways        int
	Nodes       int
	Edges       int
	NameCount   int
	GeomBytes   int
	PassWays    time.Duration
	PassNodes   time.Duration
	Assemble    time.Duration
	Total       time.Duration
	MissingRefs int // way references outside the extract (cut at boundary)
}

func (s *BuildStats) String() string {
	return fmt.Sprintf("graph: %d nodes, %d edges, %d names, %.1f MB geometry (ways %v + nodes %v + assemble %v = %v)",
		s.Nodes, s.Edges, s.NameCount, float64(s.GeomBytes)/1e6, s.PassWays.Round(time.Millisecond),
		s.PassNodes.Round(time.Millisecond), s.Assemble.Round(time.Millisecond), s.Total.Round(time.Millisecond))
}

type wayRec struct {
	refOff  uint32
	refCnt  uint32
	fwd     uint8
	bwd     uint8
	speed   uint8
	nameIdx uint32
}

// SrcData is the compact intermediate the builder works from. It is kept (see
// store.go) so live OSM diffs can be applied and the graph re-assembled in
// seconds — no PBF re-parse, no re-download.
type SrcData struct {
	ways   []wayRec
	wayIDs []int64 // OSM way ids, parallel to ways (diff application)
	refs   []int64 // way node refs, all ways back to back
	names  []string

	ids   []int64 // sorted unique node ids referenced by kept ways
	lats  []int32 // parallel to ids; MinInt32 = missing from extract
	lons  []int32
	route []bool // parallel to ids: must become a routing node

	bbox    osm.BBox
	replSeq int64
	replURL string
}

const missingCoord = math.MinInt32

// BuildFromPBF parses a .osm.pbf and assembles the routing graph.
func BuildFromPBF(path string, workers int) (*Graph, *SrcData, *BuildStats, error) {
	st := &BuildStats{}
	t0 := time.Now()
	src, err := extractFromPBF(path, workers, st)
	if err != nil {
		return nil, nil, nil, err
	}
	g := Assemble(src, st)
	st.Total = time.Since(t0)
	return g, src, st, nil
}

// extractFromPBF runs the two PBF passes and returns the compact source data.
func extractFromPBF(path string, workers int, st *BuildStats) (*SrcData, error) {
	src := &SrcData{names: []string{""}}
	nameIdx := map[string]uint32{"": 0}

	// pass 1: ways
	t := time.Now()
	var mu sync.Mutex
	hdr, err := osm.ScanPBF(path, osm.ScanOpts{
		Workers: workers,
		Ways: func(ws []osm.Way) {
			type stagedWay struct {
				p    wayProfile
				id   int64
				name string
				refs []int64
			}
			// classify outside the lock, append inside
			staged := make([]stagedWay, 0, len(ws))
			for _, w := range ws {
				p := classifyWay(w.Tags)
				if !p.keep || len(w.Refs) < 2 {
					continue
				}
				staged = append(staged, stagedWay{p: p, id: w.ID, name: nameOf(w.Tags), refs: w.Refs})
			}
			mu.Lock()
			for _, sw := range staged {
				ni, ok := nameIdx[sw.name]
				if !ok {
					ni = uint32(len(src.names))
					src.names = append(src.names, sw.name)
					nameIdx[sw.name] = ni
				}
				src.ways = append(src.ways, wayRec{
					refOff: uint32(len(src.refs)), refCnt: uint32(len(sw.refs)),
					fwd: sw.p.fwd, bwd: sw.p.bwd, speed: sw.p.speed, nameIdx: ni,
				})
				src.wayIDs = append(src.wayIDs, sw.id)
				src.refs = append(src.refs, sw.refs...)
			}
			mu.Unlock()
		},
	})
	if err != nil {
		return nil, err
	}
	if hdr.HasBBox {
		src.bbox = hdr.BBox
	}
	src.replSeq = hdr.ReplicationSeq
	src.replURL = hdr.ReplicationURL
	st.PassWays = time.Since(t)
	st.Ways = len(src.ways)

	src.indexNodes()

	// pass 2: coordinates of referenced nodes
	t = time.Now()
	if _, err := osm.ScanPBF(path, osm.ScanOpts{
		Workers: workers,
		Nodes: func(nb osm.NodeBatch) {
			ids := src.ids
			for i, id := range nb.IDs {
				// distinct indices are written by distinct nodes: safe in parallel
				if j, ok := slices.BinarySearch(ids, id); ok {
					src.lats[j] = nb.Lats[i]
					src.lons[j] = nb.Lons[i]
				}
			}
		},
	}); err != nil {
		return nil, err
	}
	st.PassNodes = time.Since(t)
	return src, nil
}

// indexNodes computes the sorted unique node ids, marks routing nodes
// (used by ≥2 way positions, or way endpoints) and prepares coord storage.
func (src *SrcData) indexNodes() {
	sorted := make([]int64, len(src.refs))
	copy(sorted, src.refs)
	slices.Sort(sorted)
	src.ids = sorted[:0]
	src.route = make([]bool, 0, len(sorted))
	for i := 0; i < len(sorted); {
		j := i + 1
		for j < len(sorted) && sorted[j] == sorted[i] {
			j++
		}
		src.ids = append(src.ids, sorted[i])
		src.route = append(src.route, j-i >= 2)
		i = j
	}
	for _, w := range src.ways {
		for _, r := range []int64{src.refs[w.refOff], src.refs[w.refOff+w.refCnt-1]} {
			if j, ok := slices.BinarySearch(src.ids, r); ok {
				src.route[j] = true
			}
		}
	}
	src.lats = make([]int32, len(src.ids))
	src.lons = make([]int32, len(src.ids))
	for i := range src.lats {
		src.lats[i] = missingCoord
	}
}

type stagedEdge struct {
	u, v    int32
	meters  uint16
	fwd     uint8
	bwd     uint8
	speed   uint8
	nameIdx uint32
	geomOff uint32
}

// assembler carries the mutable state of one graph assembly.
type assembler struct {
	src              *SrcData
	nodeID           []int32
	nodeLat, nodeLon []int32
	edges            []stagedEdge
	geom             []byte
}

// promote turns a source node (by unique index) into a routing node.
func (a *assembler) promote(i int32) int32 {
	if a.nodeID[i] == NoNode {
		a.nodeID[i] = int32(len(a.nodeLat))
		a.nodeLat = append(a.nodeLat, a.src.lats[i])
		a.nodeLon = append(a.nodeLon, a.src.lons[i])
	}
	return a.nodeID[i]
}

// emitEdge stages one undirected edge; chain holds unique indices u..v.
func (a *assembler) emitEdge(w *wayRec, chain []int32, meters float64) {
	u := a.promote(chain[0])
	v := a.promote(chain[len(chain)-1])
	m := uint16(math.Max(1, math.Round(meters)))
	geomOff := NoGeom
	if len(chain) > 2 {
		geomOff = uint32(len(a.geom))
		a.geom = appendUvarint(a.geom, uint64(len(chain)-2))
		prevLat, prevLon := a.src.lats[chain[0]], a.src.lons[chain[0]]
		for _, ci := range chain[1 : len(chain)-1] {
			la, lo := a.src.lats[ci], a.src.lons[ci]
			a.geom = appendSvarint(a.geom, int64(la-prevLat))
			a.geom = appendSvarint(a.geom, int64(lo-prevLon))
			prevLat, prevLon = la, lo
		}
	}
	a.edges = append(a.edges, stagedEdge{u, v, m, w.fwd, w.bwd, w.speed, w.nameIdx, geomOff})
}

// emitChain splits a chain so no single edge overflows uint16 meters.
func (a *assembler) emitChain(w *wayRec, chain []int32) {
	if len(chain) < 2 {
		return
	}
	src := a.src
	segStart := 0
	var meters float64
	for k := 1; k < len(chain); k++ {
		p, q := chain[k-1], chain[k]
		d := geo.Dist(src.lats[p], src.lons[p], src.lats[q], src.lons[q])
		if meters+d > 65000 && k-1 > segStart {
			a.emitEdge(w, chain[segStart:k], meters)
			segStart, meters = k-1, 0
		}
		meters += d
	}
	a.emitEdge(w, chain[segStart:], meters)
}

// Assemble builds the final CSR graph from source data. This is the only step
// that runs again when live OSM diffs arrive — seconds, no PBF, no downloads.
func Assemble(src *SrcData, st *BuildStats) *Graph {
	t := time.Now()
	a := &assembler{src: src, nodeID: make([]int32, len(src.ids))}
	for i := range a.nodeID {
		a.nodeID[i] = NoNode
	}
	// pre-create ids for plain routing nodes so numbering is stable-ish
	for i := range src.ids {
		if src.route[i] && src.lats[i] != missingCoord {
			a.promote(int32(i))
		}
	}

	var chain []int32
	missing := 0
	for wi := range src.ways {
		w := &src.ways[wi]
		refs := src.refs[w.refOff : w.refOff+w.refCnt]
		chain = chain[:0]
		for _, r := range refs {
			j, ok := slices.BinarySearch(src.ids, r)
			if !ok || src.lats[j] == missingCoord {
				missing++ // hole in the extract: cut the chain here
				a.emitChain(w, chain)
				chain = chain[:0]
				continue
			}
			if src.route[j] && len(chain) > 0 {
				chain = append(chain, int32(j))
				a.emitChain(w, chain)
				chain = chain[:0]
			}
			chain = append(chain, int32(j))
		}
		a.emitChain(w, chain)
	}
	if st != nil {
		st.MissingRefs = missing
	}

	g := &Graph{
		NodeLat: a.nodeLat, NodeLon: a.nodeLon,
		Geom: a.geom, BBox: src.bbox,
		ReplicationSeq: src.replSeq, ReplicationURL: src.replURL,
	}

	// names
	g.NameOff = make([]uint32, len(src.names)+1)
	total := 0
	for _, n := range src.names {
		total += len(n)
	}
	g.NameBlob = make([]byte, 0, total)
	for i, n := range src.names {
		g.NameOff[i] = uint32(len(g.NameBlob))
		g.NameBlob = append(g.NameBlob, n...)
	}
	g.NameOff[len(src.names)] = uint32(len(g.NameBlob))

	// CSR
	n := len(a.nodeLat)
	deg := make([]uint32, n+1)
	for i := range a.edges {
		e := &a.edges[i]
		if e.u == e.v {
			continue // self loops are useless for routing
		}
		if e.fwd != 0 {
			deg[e.u+1]++
		}
		if e.bwd != 0 {
			deg[e.v+1]++
		}
	}
	for i := 1; i <= n; i++ {
		deg[i] += deg[i-1]
	}
	g.FirstEdge = deg
	m := int(deg[n])
	g.EdgeTarget = make([]int32, m)
	g.EdgeMeters = make([]uint16, m)
	g.EdgeFlags = make([]uint8, m)
	g.EdgeSpeed = make([]uint8, m)
	g.EdgeName = make([]uint32, m)
	g.EdgeGeom = make([]uint32, m)
	fill := make([]uint32, n)
	place := func(from, to int32, meters uint16, flags, speed uint8, name, geomOff uint32) {
		pos := g.FirstEdge[from] + fill[from]
		fill[from]++
		g.EdgeTarget[pos] = to
		g.EdgeMeters[pos] = meters
		g.EdgeFlags[pos] = flags
		g.EdgeSpeed[pos] = speed
		g.EdgeName[pos] = name
		g.EdgeGeom[pos] = geomOff
	}
	for i := range a.edges {
		e := &a.edges[i]
		if e.u == e.v {
			continue
		}
		if e.fwd != 0 {
			place(e.u, e.v, e.meters, e.fwd, e.speed, e.nameIdx, e.geomOff)
		}
		if e.bwd != 0 {
			rev := e.bwd
			if e.geomOff != NoGeom {
				rev |= FGeomRev
			}
			place(e.v, e.u, e.meters, rev, e.speed, e.nameIdx, e.geomOff)
		}
	}

	g.SnapOK = snapComponents(g)
	g.Grid = buildGrid(g)
	if st != nil {
		st.Nodes = n
		st.Edges = m
		st.GeomBytes = len(a.geom)
		st.NameCount = len(src.names)
		st.Assemble = time.Since(t)
	}
	return g
}

// minComponentNodes: mode components smaller than this are not snap targets.
// Big enough to kill parking-lot islands, small enough to keep real islands
// (Ponza, Giglio) routable. Tiny graphs (tests) scale it down.
const minComponentNodes = 200

func componentThreshold(n int) int32 {
	if t := n / 4; t < minComponentNodes {
		return int32(max(t, 2))
	}
	return minComponentNodes
}

// snapComponents labels undirected connected components per mode (union-find,
// so oneways still glue their endpoints) and marks nodes of adequately sized
// components as snappable.
func snapComponents(g *Graph) []uint8 {
	n := g.NumNodes()
	ok := make([]uint8, n)
	parent := make([]int32, n)
	var find func(x int32) int32
	find = func(x int32) int32 {
		for parent[x] != x {
			parent[x] = parent[parent[x]] // path halving
			x = parent[x]
		}
		return x
	}
	for _, mode := range []Mode{ModeCar, ModeBike, ModeFoot} {
		for i := range parent {
			parent[i] = int32(i)
		}
		for u := int32(0); u < int32(n); u++ {
			lo, hi := g.EdgesOf(u)
			for e := lo; e < hi; e++ {
				if g.EdgeFlags[e]&uint8(mode) == 0 {
					continue
				}
				ru, rv := find(u), find(g.EdgeTarget[e])
				if ru != rv {
					parent[ru] = rv
				}
			}
		}
		sizes := make(map[int32]int32)
		hasMode := make([]bool, n)
		for u := int32(0); u < int32(n); u++ {
			lo, hi := g.EdgesOf(u)
			for e := lo; e < hi; e++ {
				if g.EdgeFlags[e]&uint8(mode) != 0 {
					hasMode[u] = true
					hasMode[g.EdgeTarget[e]] = true
				}
			}
		}
		for i := int32(0); i < int32(n); i++ {
			if hasMode[i] {
				sizes[find(i)]++
			}
		}
		thr := componentThreshold(n)
		for i := int32(0); i < int32(n); i++ {
			if hasMode[i] && sizes[find(i)] >= thr {
				ok[i] |= uint8(mode)
			}
		}
	}
	return ok
}

func appendUvarint(b []byte, v uint64) []byte {
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}

func appendSvarint(b []byte, v int64) []byte {
	return appendUvarint(b, uint64(v<<1)^uint64(v>>63))
}
