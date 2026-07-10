package graph

import "gotransit/internal/osm"

// SyntheticGrid builds a rows×cols two-way street grid — the test and demo
// backbone (E2E harnesses plug synthetic GTFS stops onto it).
func SyntheticGrid(rows, cols int, originLatE7, originLonE7, stepE7 int32) *Graph {
	src := &SrcData{names: []string{"", "Via Test"}}
	id := func(r, c int) int64 { return int64(1000 + r*cols + c) }
	addWay := func(ids ...int64) {
		src.ways = append(src.ways, wayRec{
			refOff: uint32(len(src.refs)), refCnt: uint32(len(ids)),
			fwd: FCar | FBike | FFoot, bwd: FCar | FBike | FFoot, speed: 30, nameIdx: 1,
		})
		src.wayIDs = append(src.wayIDs, int64(len(src.ways)))
		src.refs = append(src.refs, ids...)
	}
	for r := 0; r < rows; r++ { // horizontal streets
		ids := make([]int64, cols)
		for c := 0; c < cols; c++ {
			ids[c] = id(r, c)
		}
		addWay(ids...)
	}
	for c := 0; c < cols; c++ { // vertical streets
		ids := make([]int64, rows)
		for r := 0; r < rows; r++ {
			ids[r] = id(r, c)
		}
		addWay(ids...)
	}
	src.indexNodes()
	for i, nid := range src.ids {
		k := int(nid - 1000)
		r, c := k/cols, k%cols
		src.lats[i] = originLatE7 + int32(r)*stepE7
		src.lons[i] = originLonE7 + int32(c)*stepE7
	}
	src.bbox = osm.BBox{
		MinLat: originLatE7, MinLon: originLonE7,
		MaxLat: originLatE7 + int32(rows-1)*stepE7,
		MaxLon: originLonE7 + int32(cols-1)*stepE7,
	}
	return Assemble(src, &BuildStats{})
}

// SyntheticNet is the richer test fixture: a 3×3 two-way grid (~500 m
// spacing), a car+bike ONEWAY diagonal from corner 0 to center 4 (foot goes
// both ways), and one curvy street with intermediate geometry nodes.
// Node numbering: 0..8 row-major from the SW corner.
func SyntheticNet(originLatE7, originLonE7 int32) *Graph {
	const step = int32(45000)
	src := &SrcData{names: []string{"", "Via A", "Via B", "Via Curva"}}
	base := int64(100)
	all := FCar | FBike | FFoot
	addWay := func(name uint32, fwd, bwd uint8, ids ...int64) {
		src.ways = append(src.ways, wayRec{
			refOff: uint32(len(src.refs)), refCnt: uint32(len(ids)),
			fwd: fwd, bwd: bwd, speed: 30, nameIdx: name,
		})
		src.wayIDs = append(src.wayIDs, int64(len(src.ways)))
		src.refs = append(src.refs, ids...)
	}
	for r := 0; r < 3; r++ { // horizontal
		addWay(1, all, all, base+int64(r*3), base+int64(r*3+1), base+int64(r*3+2))
	}
	for c := 0; c < 3; c++ { // vertical
		addWay(2, all, all, base+int64(c), base+int64(c+3), base+int64(c+6))
	}
	addWay(0, all, FFoot, base+0, base+4) // oneway diagonal (car+bike fwd only)
	// curvy street south of the grid: endpoints + 2 geometry nodes
	addWay(3, FFoot, FFoot, 900, 901, 902, 903)

	src.indexNodes()
	for i, id := range src.ids {
		if id >= 900 && id < 1000 {
			k := int32(id - 900)
			src.lats[i] = originLatE7 - 30000
			src.lons[i] = originLonE7 + k*15000
			if k == 1 {
				src.lats[i] -= 8000 // bend the street
			}
			continue
		}
		k := int(id - base)
		src.lats[i] = originLatE7 + int32(k/3)*step
		src.lons[i] = originLonE7 + int32(k%3)*step
	}
	src.bbox = osm.BBox{
		MinLat: originLatE7 - 60000, MinLon: originLonE7 - 30000,
		MaxLat: originLatE7 + 2*step + 30000, MaxLon: originLonE7 + 2*step + 30000,
	}
	return Assemble(src, &BuildStats{})
}
