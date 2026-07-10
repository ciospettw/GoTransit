// Package graph is the street network: built once from OSM, then kept in
// compressed flat arrays (CSR) that answer walk/bike/car searches with zero
// pointer chasing. The whole of central Italy fits in a few hundred MB —
// queries never allocate per-node objects.
package graph

import (
	"math"

	"gotransit/internal/geo"
	"gotransit/internal/osm"
)

// Edge flags (per directed edge).
const (
	FCar        uint8 = 1 << 0
	FBike       uint8 = 1 << 1
	FFoot       uint8 = 1 << 2
	FGeomRev    uint8 = 1 << 3 // geometry blob is stored in the opposite direction
	FRoundabout uint8 = 1 << 4
	FLink       uint8 = 1 << 5 // ramp / _link road
	FSteps      uint8 = 1 << 6 // stairs
)

// Mode selects which flag gates an edge during a search.
type Mode uint8

const (
	ModeFoot Mode = Mode(FFoot)
	ModeBike Mode = Mode(FBike)
	ModeCar  Mode = Mode(FCar)
)

// NoGeom marks an edge whose shape is the straight line between endpoints.
const NoGeom = ^uint32(0)

// NoNode is the missing-node sentinel.
const NoNode = int32(-1)

// Graph is the compiled street network. All slices are parallel flat arrays;
// the struct is immutable after build and shared by every query.
type Graph struct {
	// nodes (routing nodes: intersections + way endpoints)
	NodeLat, NodeLon []int32  // E7
	FirstEdge        []uint32 // CSR offsets, len = NumNodes()+1

	// directed edges
	EdgeTarget []int32
	EdgeMeters []uint16 // length; long ways are split at build so this never clamps
	EdgeFlags  []uint8
	EdgeSpeed  []uint8  // car speed, km/h
	EdgeName   []uint32 // index into names, 0 = unnamed
	EdgeGeom   []uint32 // offset into Geom or NoGeom

	// intermediate geometry: per entry, varint count then zigzag-varint
	// E7 deltas from the source node, pairwise (dLat, dLon).
	Geom []byte

	// street names: NameOff[i] .. NameOff[i+1] into NameBlob. Entry 0 is "".
	NameBlob []byte
	NameOff  []uint32

	// SnapOK[n] has a mode bit set when n belongs to a mode component big
	// enough to route on: keeps snapping away from courtyard islands and
	// pedestrian-zone slivers that lead nowhere.
	SnapOK []uint8

	Grid Grid

	BBox           osm.BBox
	ReplicationSeq int64
	ReplicationURL string
}

// NumNodes returns the routing node count.
func (g *Graph) NumNodes() int { return len(g.NodeLat) }

// NumEdges returns the directed edge count.
func (g *Graph) NumEdges() int { return len(g.EdgeTarget) }

// Name returns street name i.
func (g *Graph) Name(i uint32) string {
	if int(i)+1 >= len(g.NameOff) {
		return ""
	}
	return string(g.NameBlob[g.NameOff[i]:g.NameOff[i+1]])
}

// EdgesOf iterates the out-edges of node n as [first, last).
func (g *Graph) EdgesOf(n int32) (uint32, uint32) {
	return g.FirstEdge[n], g.FirstEdge[n+1]
}

// Allowed reports whether directed edge e is usable in mode m.
func (g *Graph) Allowed(e uint32, m Mode) bool { return g.EdgeFlags[e]&uint8(m) != 0 }

// EdgeDs returns the traversal cost of edge e in deciseconds for a mode.
// speedFactor is the fixed-point (16.16) value of 36/speed_kmh for foot/bike;
// for car the per-edge speed is used.
func (g *Graph) EdgeDs(e uint32, m Mode, speedFactor uint32) uint32 {
	meters := uint32(g.EdgeMeters[e])
	if m == ModeCar {
		v := uint32(g.EdgeSpeed[e])
		if v == 0 {
			v = 30
		}
		return (meters*36 + v - 1) / v
	}
	ds := (meters * speedFactor) >> 16
	if ds == 0 {
		ds = 1
	}
	return ds
}

// SpeedFactor converts a km/h speed into the 16.16 fixed-point multiplier
// used by EdgeDs: ds = meters * (36/v).
func SpeedFactor(kmh float64) uint32 {
	if kmh <= 0.1 {
		kmh = 0.1
	}
	return uint32(math.Round(36.0 / kmh * 65536))
}

// AppendGeometry appends the full shape of directed edge e (from its source
// node u to its target) to lats/lons, excluding the source point itself when
// skipFirst is true. Returns the extended slices.
func (g *Graph) AppendGeometry(e uint32, u int32, skipFirst bool, lats, lons []int32) ([]int32, []int32) {
	v := g.EdgeTarget[e]
	if !skipFirst {
		lats = append(lats, g.NodeLat[u])
		lons = append(lons, g.NodeLon[u])
	}
	if off := g.EdgeGeom[e]; off != NoGeom {
		start := len(lats)
		lats, lons = decodeGeom(g.Geom, off, g.NodeLat[u], g.NodeLon[u], g.NodeLat[v], g.NodeLon[v], g.EdgeFlags[e]&FGeomRev != 0, lats, lons)
		_ = start
	}
	lats = append(lats, g.NodeLat[v])
	lons = append(lons, g.NodeLon[v])
	return lats, lons
}

// decodeGeom expands intermediate points. Deltas are encoded from the
// geometry's stored orientation: when rev, we decode fully then reverse.
func decodeGeom(blob []byte, off uint32, uLat, uLon, vLat, vLon int32, rev bool, lats, lons []int32) ([]int32, []int32) {
	i := int(off)
	n, i := uvarint(blob, i)
	baseLat, baseLon := uLat, uLon
	if rev {
		baseLat, baseLon = vLat, vLon
	}
	start := len(lats)
	lat, lon := baseLat, baseLon
	for k := 0; k < int(n); k++ {
		var dLat, dLon int64
		dLat, i = svarint(blob, i)
		dLon, i = svarint(blob, i)
		lat += int32(dLat)
		lon += int32(dLon)
		lats = append(lats, lat)
		lons = append(lons, lon)
	}
	if rev { // reverse the freshly appended run
		for a, b := start, len(lats)-1; a < b; a, b = a+1, b-1 {
			lats[a], lats[b] = lats[b], lats[a]
			lons[a], lons[b] = lons[b], lons[a]
		}
	}
	return lats, lons
}

func uvarint(b []byte, i int) (uint64, int) {
	var v uint64
	var s uint
	for {
		c := b[i]
		i++
		v |= uint64(c&0x7f) << s
		if c < 0x80 {
			return v, i
		}
		s += 7
	}
}

func svarint(b []byte, i int) (int64, int) {
	u, i := uvarint(b, i)
	return int64(u>>1) ^ -int64(u&1), i
}

// EdgeDistM returns straight distance between node n and a point, in meters.
func (g *Graph) NodeDistM(n int32, latE7, lonE7 int32) float64 {
	return geo.Dist(g.NodeLat[n], g.NodeLon[n], latE7, lonE7)
}
