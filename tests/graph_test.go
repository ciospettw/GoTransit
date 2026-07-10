package tests

import (
	"testing"

	"gotransit/internal/graph"
)

// SyntheticNet: 3×3 two-way grid + oneway diagonal (0→4, foot both ways)
// + a detached curvy foot street with intermediate geometry.
func TestGraphSearchesAndOneways(t *testing.T) {
	g := graph.SyntheticNet(419000000, 125000000)
	if g.NumNodes() < 11 { // 9 grid + 2 curvy endpoints
		t.Fatalf("nodes = %d", g.NumNodes())
	}

	// snapping onto the middle of a street: both along-distances populated
	sn, ok := g.SnapPoint(418995000, 125022000, graph.ModeFoot, 300)
	if !ok || sn.PerpM > 100 || sn.AlongU < 50 || sn.AlongV < 50 {
		t.Fatalf("snap = %+v ok=%v", sn, ok)
	}

	// foot from the SW corner reaches the whole 9-node grid (the curvy
	// street is deliberately detached)
	corner, ok := g.SnapPoint(419000000, 125010000, graph.ModeFoot, 200)
	if !ok {
		t.Fatal("corner snap failed")
	}
	sf := graph.SpeedFactor(4.8)
	ns := graph.NewNearSearch(g.NumNodes())
	ns.Run(g, []graph.Seed{{Node: corner.U}, {Node: corner.V}}, graph.ModeFoot, sf, 60000)
	if len(ns.Touched()) != 9 {
		t.Errorf("foot reached %d nodes, want the 9-node grid", len(ns.Touched()))
	}

	// the oneway diagonal: snap its midpoint and read the directed flags
	diag, ok := g.SnapPoint(419022500, 125022500, graph.ModeCar, 400)
	if !ok || diag.Fwd < 0 || diag.Bwd < 0 {
		t.Fatalf("diagonal snap = %+v", diag)
	}
	fwdCar := g.Allowed(uint32(diag.Fwd), graph.ModeCar)
	bwdCar := g.Allowed(uint32(diag.Bwd), graph.ModeCar)
	if fwdCar == bwdCar {
		t.Errorf("diagonal must be car-oneway: fwd=%v bwd=%v", fwdCar, bwdCar)
	}
	if !g.Allowed(uint32(diag.Fwd), graph.ModeFoot) || !g.Allowed(uint32(diag.Bwd), graph.ModeFoot) {
		t.Error("foot must ignore the oneway")
	}

	// point-to-point car across the grid produces a route with named steps
	a, _ := g.SnapPoint(419000000, 125010000, graph.ModeCar, 200)
	b, _ := g.SnapPoint(419090000, 125080000, graph.ModeCar, 200)
	rs := graph.NewRoadSearch(g.NumNodes())
	res := rs.Route(g, []graph.Seed{{Node: a.U}, {Node: a.V}},
		[]graph.Seed{{Node: b.U}, {Node: b.V}}, 419090000, 125080000, graph.ModeCar, 0, 1.0, 1<<20)
	if !res.Found || len(res.Edges) == 0 {
		t.Fatal("car route across the grid not found")
	}
	steps := graph.Steps(g, res.Edges, graph.ModeCar, 0)
	if len(steps) < 2 || steps[0].Kind != "depart" || steps[len(steps)-1].Kind != "arrive" {
		t.Errorf("steps = %+v", steps)
	}

	// curvy street: geometry decodes with intermediate points, mirrored in reverse
	cs, ok := g.SnapPoint(418972000, 125020000, graph.ModeFoot, 600)
	if !ok || cs.Fwd < 0 {
		t.Fatalf("curvy snap failed: %+v", cs)
	}
	lats, _ := g.AppendGeometry(uint32(cs.Fwd), cs.U, false, nil, nil)
	if len(lats) < 4 {
		t.Fatalf("curvy street should carry intermediate geometry, got %d points", len(lats))
	}
	if rl, ok := g.ReversePath([]uint32{uint32(cs.Fwd)}); ok {
		lats2, _ := g.AppendGeometry(rl[0], g.EdgeTarget[cs.Fwd], false, nil, nil)
		if len(lats2) != len(lats) || lats2[0] != lats[len(lats)-1] {
			t.Error("reverse geometry is not the mirror of forward")
		}
	}
}
