package transit

import (
	"runtime"
	"slices"
	"sort"
	"sync"

	"gotransit/internal/graph"
)

// computeSnaps anchors every stop onto the walking graph.
func computeSnaps(tt *Timetable, g *graph.Graph, snapRadiusM int) {
	tt.StopSnap = make([]StopSnap, len(tt.StopID))
	sf := graph.SpeedFactor(4.8) // snapping cost uses a nominal walk speed
	workers := runtime.NumCPU()
	var wg sync.WaitGroup
	chunk := (len(tt.StopID) + workers - 1) / workers
	var unsnapped []int32
	var mu sync.Mutex
	for w := 0; w < workers; w++ {
		lo, hi := w*chunk, min((w+1)*chunk, len(tt.StopID))
		if lo >= hi {
			break
		}
		wg.Add(1)
		go func(lo, hi int) {
			defer wg.Done()
			local := 0
			for i := lo; i < hi; i++ {
				tt.StopSnap[i] = StopSnap{NodeU: -1, NodeV: -1}
				sn, ok := g.SnapPoint(tt.StopLat[i], tt.StopLon[i], graph.ModeFoot, float64(snapRadiusM))
				if !ok {
					local++
					continue
				}
				dsU := metersToDs(sn.PerpM+sn.AlongU, sf)
				dsV := metersToDs(sn.PerpM+sn.AlongV, sf)
				tt.StopSnap[i] = StopSnap{
					NodeU: sn.U, NodeV: sn.V,
					DsU: clampU16(dsU), DsV: clampU16(dsV),
					PerpM: clampU16(uint32(sn.PerpM)),
				}
			}
			mu.Lock()
			tt.Excluded.Unsnapped += local
			mu.Unlock()
		}(lo, hi)
	}
	wg.Wait()
	_ = unsnapped
}

func metersToDs(m float64, sf uint32) uint32 {
	return (uint32(m) * sf) >> 16
}

func clampU16(v uint32) uint16 {
	if v > 65535 {
		return 65535
	}
	return uint16(v)
}

type xferEntry struct {
	to int32
	ds uint16
}

// computeTransfers runs a bounded walking Dijkstra out of every stop and
// records which other stops it reaches within the radius.
func computeTransfers(tt *Timetable, g *graph.Graph, walkKmh float64, radiusM int) {
	sf := graph.SpeedFactor(walkKmh)
	maxDs := metersToDs(float64(radiusM), sf)

	// reverse index: graph node → (stop, access ds)
	type nodeStop struct {
		node  int32
		stop  int32
		extra uint16
	}
	var ns []nodeStop
	for s := range tt.StopSnap {
		sn := &tt.StopSnap[s]
		if sn.NodeU >= 0 {
			ns = append(ns, nodeStop{sn.NodeU, int32(s), sn.DsU})
		}
		if sn.NodeV >= 0 {
			ns = append(ns, nodeStop{sn.NodeV, int32(s), sn.DsV})
		}
	}
	slices.SortFunc(ns, func(a, b nodeStop) int { return int(a.node) - int(b.node) })
	nsNode := make([]int32, len(ns))
	for i := range ns {
		nsNode[i] = ns[i].node
	}
	// publish the reverse index for query-time seed harvesting
	tt.NSNode = nsNode
	tt.NSStop = make([]int32, len(ns))
	tt.NSExtra = make([]uint16, len(ns))
	for i := range ns {
		tt.NSStop[i] = ns[i].stop
		tt.NSExtra[i] = ns[i].extra
	}

	results := make([][]xferEntry, len(tt.StopID))
	workers := runtime.NumCPU()
	var wg sync.WaitGroup
	next := make(chan int, workers)
	go func() {
		for s := 0; s < len(tt.StopID); s++ {
			next <- s
		}
		close(next)
	}()
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			search := graph.NewNearSearch(g.NumNodes())
			best := map[int32]uint32{}
			for s := range next {
				sn := &tt.StopSnap[s]
				if sn.NodeU < 0 {
					continue
				}
				seeds := []graph.Seed{{Node: sn.NodeU, Ds: uint32(sn.DsU)}}
				if sn.NodeV >= 0 {
					seeds = append(seeds, graph.Seed{Node: sn.NodeV, Ds: uint32(sn.DsV)})
				}
				search.Run(g, seeds, graph.ModeFoot, sf, maxDs)
				clear(best)
				for _, n := range search.Touched() {
					d, _ := search.Dist(n)
					// all stops anchored at this node
					i, _ := slices.BinarySearch(nsNode, n)
					for ; i < len(ns) && ns[i].node == n; i++ {
						s2 := ns[i].stop
						if s2 == int32(s) {
							continue
						}
						total := d + uint32(ns[i].extra)
						if total > maxDs {
							continue
						}
						if cur, ok := best[s2]; !ok || total < cur {
							best[s2] = total
						}
					}
				}
				if len(best) == 0 {
					continue
				}
				out := make([]xferEntry, 0, len(best))
				for s2, ds := range best {
					out = append(out, xferEntry{to: s2, ds: clampU16(ds)})
				}
				sort.Slice(out, func(a, b int) bool { return out[a].to < out[b].to })
				results[s] = out
			}
		}()
	}
	wg.Wait()

	tt.XferFirst = make([]uint32, len(tt.StopID)+1)
	total := 0
	for s := range results {
		total += len(results[s])
	}
	tt.XferTo = make([]int32, 0, total)
	tt.XferDs = make([]uint16, 0, total)
	for s := range results {
		tt.XferFirst[s] = uint32(len(tt.XferTo))
		for _, x := range results[s] {
			tt.XferTo = append(tt.XferTo, x.to)
			tt.XferDs = append(tt.XferDs, x.ds)
		}
	}
	tt.XferFirst[len(tt.StopID)] = uint32(len(tt.XferTo))
}
