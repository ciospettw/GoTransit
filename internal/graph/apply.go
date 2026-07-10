package graph

import (
	"fmt"
	"slices"
	"time"

	"gotransit/internal/osm"
)

// ApplyStats reports what one osmChange application did.
type ApplyStats struct {
	WaysAdded, WaysReplaced, WaysDeleted int
	NodesMoved, NodesDeleted             int
	Elapsed                              time.Duration
}

func (s ApplyStats) String() string {
	return fmt.Sprintf("osc applied: %d ways added, %d replaced, %d deleted; %d nodes moved, %d deleted (%v)",
		s.WaysAdded, s.WaysReplaced, s.WaysDeleted, s.NodesMoved, s.NodesDeleted, s.Elapsed.Round(time.Millisecond))
}

// ApplyChange folds an osmChange into the graph source and returns a fresh
// SrcData ready for Assemble. The old SrcData is not mutated (in-flight use
// stays safe). No PBF, no downloads: this is the whole live-update path.
func (src *SrcData) ApplyChange(ch *osm.Change) (*SrcData, ApplyStats) {
	t0 := time.Now()
	st := ApplyStats{}

	delWay := make(map[int64]bool, len(ch.WayDelete))
	for _, id := range ch.WayDelete {
		delWay[id] = true
	}
	upWay := make(map[int64]bool, len(ch.WayUpsert))
	for _, wc := range ch.WayUpsert {
		upWay[wc.ID] = true
	}
	delNode := make(map[int64]bool, len(ch.NodeDelete))
	for _, id := range ch.NodeDelete {
		delNode[id] = true
	}

	out := &SrcData{
		names:   append([]string(nil), src.names...),
		bbox:    src.bbox,
		replSeq: src.replSeq,
		replURL: src.replURL,
	}
	nameIdx := make(map[string]uint32, 256) // lazy: filled only when needed

	// carry over surviving ways
	out.ways = make([]wayRec, 0, len(src.ways)+len(ch.WayUpsert))
	out.wayIDs = make([]int64, 0, cap(out.ways))
	out.refs = make([]int64, 0, len(src.refs)+len(ch.WayUpsert)*16)
	for i := range src.ways {
		id := src.wayIDs[i]
		if delWay[id] {
			st.WaysDeleted++
			continue
		}
		if upWay[id] {
			st.WaysReplaced++
			continue // the new version is appended below
		}
		w := src.ways[i]
		newOff := uint32(len(out.refs))
		out.refs = append(out.refs, src.refs[w.refOff:w.refOff+w.refCnt]...)
		w.refOff = newOff
		out.ways = append(out.ways, w)
		out.wayIDs = append(out.wayIDs, id)
	}
	// append upserted ways that still classify as routable
	lookupName := func(name string) uint32 {
		if len(nameIdx) == 0 {
			for i, n := range out.names {
				nameIdx[n] = uint32(i)
			}
		}
		if ni, ok := nameIdx[name]; ok {
			return ni
		}
		ni := uint32(len(out.names))
		out.names = append(out.names, name)
		nameIdx[name] = ni
		return ni
	}
	for _, wc := range ch.WayUpsert {
		p := classifyWay(osm.MapTags(wc.Tags))
		if !p.keep || len(wc.Refs) < 2 {
			continue // not routable (anymore): stays out
		}
		name := ""
		if n := osm.MapTags(wc.Tags).Get("name"); n != nil {
			name = string(n)
		} else if r := osm.MapTags(wc.Tags).Get("ref"); r != nil {
			name = string(r)
		}
		out.ways = append(out.ways, wayRec{
			refOff: uint32(len(out.refs)), refCnt: uint32(len(wc.Refs)),
			fwd: p.fwd, bwd: p.bwd, speed: p.speed, nameIdx: lookupName(name),
		})
		out.wayIDs = append(out.wayIDs, wc.ID)
		out.refs = append(out.refs, wc.Refs...)
		st.WaysAdded++
	}
	if st.WaysAdded -= st.WaysReplaced; st.WaysAdded < 0 {
		st.WaysAdded = 0 // replaced ways that no longer classify as routable
	}

	// node index rebuilt from the live refs
	out.indexNodes()

	// coordinates: carry over, then overlay the osc node changes
	for i, id := range out.ids {
		if j, ok := slices.BinarySearch(src.ids, id); ok {
			out.lats[i] = src.lats[j]
			out.lons[i] = src.lons[j]
		}
	}
	for _, nc := range ch.NodeUpsert {
		if j, ok := slices.BinarySearch(out.ids, nc.ID); ok {
			if out.lats[j] != nc.Lat || out.lons[j] != nc.Lon {
				st.NodesMoved++
			}
			out.lats[j] = nc.Lat
			out.lons[j] = nc.Lon
		}
	}
	for id := range delNode {
		if j, ok := slices.BinarySearch(out.ids, id); ok {
			out.lats[j] = missingCoord // assembly cuts chains there
			st.NodesDeleted++
		}
	}
	st.Elapsed = time.Since(t0)
	return out, st
}
