package engine

import (
	"fmt"
	"time"

	"gotransit/internal/geo"
	"gotransit/internal/graph"
	"gotransit/internal/transit"
)

// assemble turns a RAPTOR journey into a full itinerary with geometry.
func (e *Engine) assemble(gb *GraphBundle, tb *TTBundle, j transit.Journey, base time.Time, depSec uint32,
	fLat, fLon, tLat, tLon int32, acc, egr *accessSet) (Itinerary, bool) {

	tt := tb.TT
	if len(j.Legs) == 0 || !j.Legs[0].Ride {
		return Itinerary{}, false
	}
	var legs []Leg
	sig := ""

	// --- access leg: origin → first boarding stop ---
	firstPat := j.Legs[0].Pattern
	firstStops := tt.PatternStops(firstPat)
	boardStop := firstStops[j.Legs[0].Board]
	accSec, okA := acc.sec[boardStop]
	if !okA {
		return Itinerary{}, false
	}
	if acc.mode != "none" { // "none": journey starts at the stop (onboard replans)
		accessArr := base.Add(time.Duration(depSec+accSec) * time.Second)
		accessLeg := e.stopStreetLeg(gb, tb, acc.mode, fLat, fLon, boardStop, false)
		accessLeg.Depart = base.Add(time.Duration(depSec) * time.Second)
		accessLeg.Arrive = accessArr
		accessLeg.DurationS = int(accSec)
		accessLeg.From = Place{Lat: e7f(fLat), Lon: e7f(fLon)}
		accessLeg.To = stopPlace(tt, boardStop)
		legs = append(legs, accessLeg)
	}

	// --- rides and transfers ---
	for _, l := range j.Legs {
		if l.Ride {
			leg := e.transitLeg(tt, l, base)
			legs = append(legs, leg)
			sig += fmt.Sprintf("t%d.", l.Trip)
		} else {
			leg := e.transferLeg(gb, tb, l.From, l.To)
			prevArr := legs[len(legs)-1].Arrive
			leg.Depart = prevArr
			leg.Arrive = prevArr.Add(time.Duration(l.Sec) * time.Second)
			leg.DurationS = int(l.Sec)
			legs = append(legs, leg)
		}
	}

	// --- egress: last stop → destination ---
	egrSec, okE := egr.sec[j.Target]
	if !okE {
		return Itinerary{}, false
	}
	egressLeg := e.stopStreetLeg(gb, tb, egr.mode, tLat, tLon, j.Target, true)
	lastArr := legs[len(legs)-1].Arrive
	egressLeg.Depart = lastArr
	egressLeg.Arrive = lastArr.Add(time.Duration(egrSec) * time.Second)
	egressLeg.DurationS = int(egrSec)
	egressLeg.From = stopPlace(tt, j.Target)
	egressLeg.To = Place{Lat: e7f(tLat), Lon: e7f(tLon)}
	legs = append(legs, egressLeg)

	it := Itinerary{
		Depart:    legs[0].Depart,
		Arrive:    legs[len(legs)-1].Arrive,
		Transfers: j.Rides - 1,
		Legs:      legs,
		sig:       sig,
	}
	it.DurationS = int(it.Arrive.Sub(it.Depart).Seconds())
	return it, true
}

func e7f(v int32) float64 { return float64(v) / 1e7 }

func stopPlace(tt *transit.Timetable, s int32) Place {
	return Place{
		Name: tt.StopName[s], StopID: tt.StopID[s], Code: tt.StopCode[s],
		Lat: e7f(tt.StopLat[s]), Lon: e7f(tt.StopLon[s]),
	}
}

// transitLeg builds the ride leg with per-stop times and sliced shape.
func (e *Engine) transitLeg(tt *transit.Timetable, l transit.RLeg, base time.Time) Leg {
	stops := tt.PatternStops(l.Pattern)
	rm := tt.Routes[tt.PatRoute[l.Pattern]]
	leg := Leg{
		Mode: "transit",
		Route: &RouteRef{
			ID: rm.Feed + ":" + rm.GTFSID, ShortName: rm.Short, LongName: rm.Long,
			Color: rm.Color, TextColor: rm.TextColor, Agency: rm.Agency, Type: rm.Type,
		},
		TripID:   rm.Feed + ":" + tt.TripID[l.Trip],
		Headsign: tt.Headsigns[tt.TripHeadsign[l.Trip]],
	}
	att := func(sec uint32) time.Time {
		return base.Add(time.Duration(int64(sec)+int64(l.DayOff)) * time.Second)
	}
	for pos := l.Board; pos <= l.Alight; pos++ {
		s := stops[pos]
		leg.Stops = append(leg.Stops, StopTime{
			ID: tt.StopID[s], Code: tt.StopCode[s], Name: tt.StopName[s],
			Lat: e7f(tt.StopLat[s]), Lon: e7f(tt.StopLon[s]),
			Arrive: att(tt.TripArr(l.Trip, pos)),
			Depart: att(tt.TripDep(l.Trip, pos)),
		})
	}
	leg.From = stopPlace(tt, stops[l.Board])
	leg.To = stopPlace(tt, stops[l.Alight])
	leg.Depart = att(tt.TripDep(l.Trip, l.Board))
	leg.Arrive = att(tt.TripArr(l.Trip, l.Alight))
	leg.DurationS = int(leg.Arrive.Sub(leg.Depart).Seconds())

	if o := tt.RT(); o.TripHasRT(l.Trip) {
		leg.Realtime = true
		leg.DelayS = int(tt.TripDep(l.Trip, l.Board)) - int(tt.ScheduledDep(l.Trip, l.Board))
	}

	// geometry: slice the GTFS shape between the two stops when available
	if sh := tt.PatShape[l.Pattern]; sh >= 0 {
		bi := tt.PatShapeIdx[tt.PatFirstStop[l.Pattern]+uint32(l.Board)]
		ai := tt.PatShapeIdx[tt.PatFirstStop[l.Pattern]+uint32(l.Alight)]
		if ai > bi {
			var enc geo.PolylineEncoder
			for k := bi; k <= ai; k++ {
				enc.Add(tt.ShpLat[k], tt.ShpLon[k])
			}
			leg.Polyline = enc.String()
			leg.DistanceM = int((tt.ShpCumDm[ai] - tt.ShpCumDm[bi]) / 10)
		}
	}
	if leg.Polyline == "" { // no shape: connect the stops
		var enc geo.PolylineEncoder
		meters := 0.0
		for pos := l.Board; pos <= l.Alight; pos++ {
			s := stops[pos]
			enc.Add(tt.StopLat[s], tt.StopLon[s])
			if pos > l.Board {
				p := stops[pos-1]
				meters += geo.Dist(tt.StopLat[p], tt.StopLon[p], tt.StopLat[s], tt.StopLon[s])
			}
		}
		leg.Polyline = enc.String()
		leg.DistanceM = int(meters)
	}
	return leg
}

// stopStreetLeg reconstructs the street path between a point and a stop.
// reverse=false: point → stop (access). reverse=true: stop → point (egress).
func (e *Engine) stopStreetLeg(gb *GraphBundle, tb *TTBundle, modeTag string, pLat, pLon int32, stop int32, reverse bool) Leg {
	g, tt := gb.G, tb.TT
	m, sf, modeName := graph.ModeFoot, graph.SpeedFactor(e.Cfg.Routing.WalkSpeedKmh), "walk"
	maxDs := uint32(e.Cfg.Routing.MaxWalkAccess.Seconds() * 10)
	if modeTag == "bike" {
		m, sf, modeName = graph.ModeBike, graph.SpeedFactor(e.Cfg.Routing.BikeSpeedKmh), "bike"
		maxDs = uint32(e.Cfg.Routing.MaxBikeAccess.Seconds() * 10)
	}
	leg := Leg{Mode: modeName}

	sn, ok := g.SnapPoint(pLat, pLon, m, float64(e.Cfg.Routing.SnapRadiusM))
	ss := tt.StopSnap[stop]
	if !ok || ss.NodeU < 0 {
		return straightLeg(leg, pLat, pLon, tt.StopLat[stop], tt.StopLon[stop], reverse)
	}
	ns := gb.Near()
	defer gb.PutNear(ns)
	ns.Run(g, srcSeeds(g, sn, m, sf), m, sf, maxDs+1200)

	// choose the cheaper stop anchor that was actually reached
	anchor, extra := int32(-1), uint32(0)
	if dU, okU := ns.Dist(ss.NodeU); okU {
		anchor, extra = ss.NodeU, dU+uint32(ss.DsU)
	}
	if ss.NodeV >= 0 {
		if dV, okV := ns.Dist(ss.NodeV); okV {
			if anchor < 0 || dV+uint32(ss.DsV) < extra {
				anchor, extra = ss.NodeV, dV+uint32(ss.DsV)
			}
		}
	}
	_ = extra
	if anchor < 0 {
		return straightLeg(leg, pLat, pLon, tt.StopLat[stop], tt.StopLon[stop], reverse)
	}
	edges := ns.PathTo(g, anchor)

	if reverse {
		if rev, ok := g.ReversePath(edges); ok {
			edges = rev
		} else {
			edges = nil
		}
	}
	var lats, lons []int32
	if len(edges) > 0 {
		lats, lons = graph.PathGeometry(g, edges)
		leg.Steps = stepsDTO(graph.Steps(g, edges, m, sf))
	}
	// stitch endpoints: point ↔ snapped entry, anchor ↔ stop
	stopLat, stopLon := tt.StopLat[stop], tt.StopLon[stop]
	var enc geo.PolylineEncoder
	dist := graph.PathMeters(g, edges)
	if !reverse {
		enc.Add(pLat, pLon)
		enc.Add(sn.PLat, sn.PLon)
		for i := range lats {
			enc.Add(lats[i], lons[i])
		}
		enc.Add(stopLat, stopLon)
	} else {
		enc.Add(stopLat, stopLon)
		for i := range lats {
			enc.Add(lats[i], lons[i])
		}
		enc.Add(sn.PLat, sn.PLon)
		enc.Add(pLat, pLon)
	}
	dist += sn.PerpM + geo.Dist(pLat, pLon, sn.PLat, sn.PLon)*0 // perp already covers it
	dist += float64(ss.PerpM)
	leg.Polyline = enc.String()
	leg.DistanceM = int(dist)
	return leg
}

func straightLeg(leg Leg, aLat, aLon, bLat, bLon int32, reverse bool) Leg {
	if reverse {
		aLat, aLon, bLat, bLon = bLat, bLon, aLat, aLon
	}
	leg.Polyline = geo.EncodePolyline([]int32{aLat, bLat}, []int32{aLon, bLon})
	leg.DistanceM = int(geo.Dist(aLat, aLon, bLat, bLon))
	return leg
}

// transferLeg reconstructs the walk between two stops.
func (e *Engine) transferLeg(gb *GraphBundle, tb *TTBundle, from, to int32) Leg {
	g, tt := gb.G, tb.TT
	sf := graph.SpeedFactor(e.Cfg.Routing.WalkSpeedKmh)
	leg := Leg{Mode: "walk", From: stopPlace(tt, from), To: stopPlace(tt, to)}

	sa, sb := tt.StopSnap[from], tt.StopSnap[to]
	if sa.NodeU < 0 || sb.NodeU < 0 {
		return straightLeg(leg, tt.StopLat[from], tt.StopLon[from], tt.StopLat[to], tt.StopLon[to], false)
	}
	ns := gb.Near()
	defer gb.PutNear(ns)
	seeds := []graph.Seed{{Node: sa.NodeU, Ds: uint32(sa.DsU)}}
	if sa.NodeV >= 0 {
		seeds = append(seeds, graph.Seed{Node: sa.NodeV, Ds: uint32(sa.DsV)})
	}
	ns.Run(g, seeds, graph.ModeFoot, sf, uint32(e.Cfg.Routing.TransferRadiusM)*15)

	anchor := int32(-1)
	bestD := ^uint32(0)
	for _, cand := range []struct {
		n  int32
		ds uint16
	}{{sb.NodeU, sb.DsU}, {sb.NodeV, sb.DsV}} {
		if cand.n < 0 {
			continue
		}
		if d, ok := ns.Dist(cand.n); ok && d+uint32(cand.ds) < bestD {
			bestD = d + uint32(cand.ds)
			anchor = cand.n
		}
	}
	if anchor < 0 {
		return straightLeg(leg, tt.StopLat[from], tt.StopLon[from], tt.StopLat[to], tt.StopLon[to], false)
	}
	edges := ns.PathTo(g, anchor)
	var enc geo.PolylineEncoder
	enc.Add(tt.StopLat[from], tt.StopLon[from])
	if len(edges) > 0 {
		lats, lons := graph.PathGeometry(g, edges)
		for i := range lats {
			enc.Add(lats[i], lons[i])
		}
		leg.Steps = stepsDTO(graph.Steps(g, edges, graph.ModeFoot, sf))
	}
	enc.Add(tt.StopLat[to], tt.StopLon[to])
	leg.Polyline = enc.String()
	leg.DistanceM = int(graph.PathMeters(g, edges) + float64(sa.PerpM) + float64(sb.PerpM))
	return leg
}

// roadLeg builds the single leg of a car/bike/walk direct route.
func (e *Engine) roadLeg(g *graph.Graph, modeName string, m graph.Mode, sf uint32,
	edges []uint32, snF, snT graph.Snap, fLat, fLon, tLat, tLon int32) Leg {

	leg := Leg{Mode: modeName,
		From: Place{Lat: e7f(fLat), Lon: e7f(fLon)},
		To:   Place{Lat: e7f(tLat), Lon: e7f(tLon)},
	}
	var enc geo.PolylineEncoder
	enc.Add(fLat, fLon)
	enc.Add(snF.PLat, snF.PLon)
	if len(edges) > 0 {
		lats, lons := graph.PathGeometry(g, edges)
		for i := range lats {
			enc.Add(lats[i], lons[i])
		}
		leg.Steps = stepsDTO(graph.Steps(g, edges, m, sf))
	}
	enc.Add(snT.PLat, snT.PLon)
	enc.Add(tLat, tLon)
	leg.Polyline = enc.String()
	leg.DistanceM = int(graph.PathMeters(g, edges) + snF.PerpM + snT.PerpM +
		minF(snF.AlongU, snF.AlongV) + minF(snT.AlongU, snT.AlongV))
	return leg
}

func minF(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func stepsDTO(in []graph.Step) []Step {
	out := make([]Step, len(in))
	for i, s := range in {
		out[i] = Step{
			Kind: s.Kind, Modifier: s.Modifier, Name: s.Name,
			DistanceM: int(s.DistM), DurationS: int(s.Ds / 10),
			Lat: e7f(s.Lat), Lon: e7f(s.Lon),
		}
	}
	return out
}
