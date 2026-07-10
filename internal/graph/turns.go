package graph

import (
	"gotransit/internal/geo"
)

// Step is one turn-by-turn instruction for a walk/bike/car leg.
type Step struct {
	Kind     string  // depart | turn | continue | roundabout | exit_roundabout | arrive
	Modifier string  // straight | slight_left | left | sharp_left | uturn | slight_right | right | sharp_right
	Name     string  // street name (may be empty)
	DistM    float64 // length of this step
	Ds       uint32  // duration, deciseconds
	Lat, Lon int32   // maneuver point (E7)
}

// Steps turns a directed edge path into instructions. The path must be
// non-empty and connected (as produced by NearSearch.PathTo / RoadSearch).
func Steps(g *Graph, edges []uint32, m Mode, speedFactor uint32) []Step {
	if len(edges) == 0 {
		return nil
	}
	var steps []Step
	var lats, lons []int32

	u := g.sourceOf(edges[0])
	cur := Step{Kind: "depart", Modifier: "straight", Name: g.Name(g.EdgeName[edges[0]]),
		Lat: g.NodeLat[u], Lon: g.NodeLon[u]}
	prevOut := 0.0 // bearing at the end of the previous edge
	first := true

	for _, e := range edges {
		name := g.Name(g.EdgeName[e])
		flags := g.EdgeFlags[e]

		lats, lons = lats[:0], lons[:0]
		lats, lons = g.AppendGeometry(e, u, false, lats, lons)
		bIn := geo.Bearing(lats[0], lons[0], lats[1], lons[1])
		n := len(lats)
		bOut := geo.Bearing(lats[n-2], lons[n-2], lats[n-1], lons[n-1])

		if !first {
			turn := angleDelta(prevOut, bIn)
			mod := modifierOf(turn)
			newRoundabout := flags&FRoundabout != 0 && stepsLastNotRoundabout(steps, cur)
			leftRoundabout := flags&FRoundabout == 0 && cur.Kind == "roundabout"
			if name != cur.Name || newRoundabout || leftRoundabout || mod != "straight" && absF(turn) >= 40 {
				steps = append(steps, cur)
				kind := "turn"
				switch {
				case newRoundabout:
					kind = "roundabout"
				case leftRoundabout:
					kind = "exit_roundabout"
				case mod == "straight":
					kind = "continue"
				}
				cur = Step{Kind: kind, Modifier: mod, Name: name,
					Lat: g.NodeLat[u], Lon: g.NodeLon[u]}
			}
		}
		first = false
		cur.DistM += float64(g.EdgeMeters[e])
		cur.Ds += g.EdgeDs(e, m, speedFactor)
		prevOut = bOut
		u = g.EdgeTarget[e]
	}
	steps = append(steps, cur)
	steps = append(steps, Step{Kind: "arrive", Modifier: "straight",
		Lat: g.NodeLat[u], Lon: g.NodeLon[u]})
	return steps
}

func stepsLastNotRoundabout(steps []Step, cur Step) bool {
	return cur.Kind != "roundabout"
}

// angleDelta returns the signed turn angle in (-180, 180]: negative = left.
func angleDelta(from, to float64) float64 {
	d := to - from
	for d > 180 {
		d -= 360
	}
	for d <= -180 {
		d += 360
	}
	return d
}

func modifierOf(turn float64) string {
	a := absF(turn)
	switch {
	case a < 25:
		return "straight"
	case a >= 165:
		return "uturn"
	}
	side := "right"
	if turn < 0 {
		side = "left"
	}
	switch {
	case a < 60:
		return "slight_" + side
	case a < 130:
		return side
	default:
		return "sharp_" + side
	}
}

func absF(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

// PathGeometry decodes the full shape of an edge path, prepending the source
// node. Returns E7 arrays ready for polyline encoding.
func PathGeometry(g *Graph, edges []uint32) (lats, lons []int32) {
	if len(edges) == 0 {
		return nil, nil
	}
	u := g.sourceOf(edges[0])
	lats, lons = g.AppendGeometry(edges[0], u, false, lats, lons)
	for _, e := range edges[1:] {
		u = g.sourceOf(e)
		lats, lons = g.AppendGeometry(e, u, true, lats, lons)
	}
	return lats, lons
}

// PathMeters sums the edge lengths of a path.
func PathMeters(g *Graph, edges []uint32) float64 {
	var m float64
	for _, e := range edges {
		m += float64(g.EdgeMeters[e])
	}
	return m
}
