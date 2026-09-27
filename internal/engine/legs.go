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
	if len(j.Legs) == 0 {
		return Itinerary{}, false
	}
	firstRide := -1
	for i := range j.Legs {
		if j.Legs[i].Ride {
			firstRide = i
			break
		}
	}
	if firstRide < 0 {
		return Itinerary{}, false
	}
	var legs []Leg
	sig := ""

	// --- access leg: origin → first boarding stop ---
	firstPat := j.Legs[firstRide].Pattern
	firstStops := tt.PatternStops(firstPat)
	boardStop := firstStops[j.Legs[firstRide].Board]
	sourceStop := boardStop
	if firstRide > 0 {
		sourceStop = j.Legs[0].From
	}
	var initialRailReady time.Time
	if acc.mode == "none" {
		if priorArr, ok := acc.priorRailArr[sourceStop]; ok {
			initialRailReady = base.Add(time.Duration(priorArr)*time.Second + e.Cfg.Routing.RailTransferBuffer)
		} else if readySec, ok := acc.sec[sourceStop]; ok {
			initialRailReady = base.Add(time.Duration(readySec)*time.Second + e.Cfg.Routing.RailEntryBuffer)
		}
	}
	accSec, okA := acc.sec[sourceStop]
	if !okA {
		return Itinerary{}, false
	}
	if acc.mode != "none" { // "none": journey starts at the stop (onboard replans)
		if plan, shared := acc.shared[sourceStop]; shared {
			sharedLegs, ok := e.sharedLegs(plan, fLat, fLon, tt.StopLat[sourceStop], tt.StopLon[sourceStop],
				base.Add(time.Duration(depSec)*time.Second))
			if !ok {
				return Itinerary{}, false
			}
			legs = append(legs, sharedLegs...)
			sig += "s" + rentalSignature(plan.Assignment) + "."
		} else {
			accessArr := base.Add(time.Duration(depSec+accSec) * time.Second)
			accessLeg := e.stopStreetLeg(gb, tb, acc.mode, fLat, fLon, sourceStop, false)
			accessLeg.Depart = base.Add(time.Duration(depSec) * time.Second)
			accessLeg.Arrive = accessArr
			accessLeg.DurationS = int(accSec)
			accessLeg.From = Place{Lat: e7f(fLat), Lon: e7f(fLon)}
			accessLeg.To = stopPlace(tt, sourceStop)
			reconcileLegSteps(&accessLeg)
			legs = append(legs, accessLeg)
		}
	}

	// --- rides and transfers ---
	for _, l := range j.Legs {
		if l.Ride {
			leg := e.transitLeg(tt, l, base)
			if len(legs) > 0 && legs[len(legs)-1].Arrive.After(leg.Depart) {
				return Itinerary{}, false // exact shared access no longer catches this ride
			}
			if len(legs) == firstRide && !initialRailReady.IsZero() && leg.Route != nil &&
				transit.IsRailLikeRouteType(leg.Route.Type) {
				leg.BoardReadyAt = initialRailReady
			}
			legs = append(legs, leg)
			sig += fmt.Sprintf("t%d.", l.Trip)
		} else {
			leg := e.transferLeg(gb, tb, l.From, l.To)
			var prevArr time.Time
			if len(legs) > 0 {
				prevArr = legs[len(legs)-1].Arrive
			} else if acc.mode == "none" {
				// Onboard replans may begin with an in-station footpath from
				// their source stop to the first outgoing platform.
				readySec, ok := acc.sec[l.From]
				if !ok {
					return Itinerary{}, false
				}
				if acc.absolute {
					prevArr = base.Add(time.Duration(readySec) * time.Second)
				} else {
					prevArr = base.Add(time.Duration(depSec+readySec) * time.Second)
				}
			} else {
				return Itinerary{}, false
			}
			leg.Depart = prevArr
			leg.Arrive = prevArr.Add(time.Duration(l.Sec) * time.Second)
			leg.DurationS = int(l.Sec)
			reconcileLegSteps(&leg)
			legs = append(legs, leg)
		}
	}

	// --- egress: last stop → destination ---
	egrSec, okE := egr.sec[j.Target]
	if !okE {
		return Itinerary{}, false
	}
	lastArr := legs[len(legs)-1].Arrive
	if plan, shared := egr.shared[j.Target]; shared {
		sharedLegs, ok := e.sharedLegs(plan, tt.StopLat[j.Target], tt.StopLon[j.Target], tLat, tLon, lastArr)
		if !ok {
			return Itinerary{}, false
		}
		legs = append(legs, sharedLegs...)
		sig += "s" + rentalSignature(plan.Assignment) + "."
	} else {
		egressLeg := e.stopStreetLeg(gb, tb, egr.mode, tLat, tLon, j.Target, true)
		egressLeg.Depart = lastArr
		egressLeg.Arrive = lastArr.Add(time.Duration(egrSec) * time.Second)
		egressLeg.DurationS = int(egrSec)
		egressLeg.From = stopPlace(tt, j.Target)
		egressLeg.To = Place{Lat: e7f(tLat), Lon: e7f(tLon)}
		reconcileLegSteps(&egressLeg)
		legs = append(legs, egressLeg)
	}

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
	anchor, totalDs := int32(-1), uint32(0)
	if dU, okU := ns.Dist(ss.NodeU); okU {
		anchor, totalDs = ss.NodeU, dU+fixedSpeedDs(uint32(ss.MetersU), sf)
	}
	if ss.NodeV >= 0 {
		if dV, okV := ns.Dist(ss.NodeV); okV {
			cand := dV + fixedSpeedDs(uint32(ss.MetersV), sf)
			if anchor < 0 || cand < totalDs {
				anchor, totalDs = ss.NodeV, cand
			}
		}
	}
	if anchor < 0 {
		return straightLeg(leg, pLat, pLon, tt.StopLat[stop], tt.StopLon[stop], reverse)
	}
	edges := ns.PathTo(g, anchor)
	pointAnchor := anchor
	if len(edges) > 0 {
		pointAnchor = g.SourceOf(edges[0])
	}

	if reverse {
		if rev, ok := g.ReversePath(edges); ok {
			edges = rev
		} else {
			return straightLeg(leg, pLat, pLon, tt.StopLat[stop], tt.StopLon[stop], true)
		}
	}

	pointM := snapConnectorMeters(sn, pointAnchor)
	stopM := stopConnectorMeters(ss, anchor)
	startM, endM := pointM, stopM
	startLat, startLon := pLat, pLon
	endLat, endLon := tt.StopLat[stop], tt.StopLon[stop]
	if reverse {
		startM, endM = stopM, pointM
		startLat, startLon, endLat, endLon = endLat, endLon, startLat, startLon
	}
	leg.Steps = routedSteps(graph.Steps(g, edges, m, sf), startM, endM,
		fixedSpeedDs(uint32(startM), sf), fixedSpeedDs(uint32(endM), sf),
		startLat, startLon, endLat, endLon)

	// Stitch the off-network and partial-edge connectors onto the routed path.
	stopLat, stopLon := tt.StopLat[stop], tt.StopLon[stop]
	var enc geo.PolylineEncoder
	if !reverse {
		enc.Add(pLat, pLon)
		appendSnapToNode(&enc, g, sn, pointAnchor)
		appendPath(&enc, g, edges)
		appendStopConnector(&enc, g, ss, anchor, stopLat, stopLon, false)
		enc.Add(stopLat, stopLon)
	} else {
		enc.Add(stopLat, stopLon)
		appendStopConnector(&enc, g, ss, anchor, stopLat, stopLon, true)
		appendPath(&enc, g, edges)
		appendNodeToSnap(&enc, g, sn, pointAnchor)
		enc.Add(pLat, pLon)
	}
	leg.Polyline = enc.String()
	leg.DistanceM = int(graph.PathMeters(g, edges) + startM + endM)
	leg.DurationS = int((totalDs + 5) / 10)
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
	seeds := []graph.Seed{{Node: sa.NodeU, Ds: fixedSpeedDs(uint32(sa.MetersU), sf)}}
	if sa.NodeV >= 0 {
		seeds = append(seeds, graph.Seed{Node: sa.NodeV, Ds: fixedSpeedDs(uint32(sa.MetersV), sf)})
	}
	ns.Run(g, seeds, graph.ModeFoot, sf,
		fixedSpeedDs(uint32(e.Cfg.Routing.TransferRadiusM), sf))

	anchor := int32(-1)
	bestD := ^uint32(0)
	for _, cand := range []struct {
		n int32
		m uint16
	}{{sb.NodeU, sb.MetersU}, {sb.NodeV, sb.MetersV}} {
		if cand.n < 0 {
			continue
		}
		if d, ok := ns.Dist(cand.n); ok && d+fixedSpeedDs(uint32(cand.m), sf) < bestD {
			bestD = d + fixedSpeedDs(uint32(cand.m), sf)
			anchor = cand.n
		}
	}
	if anchor < 0 {
		return straightLeg(leg, tt.StopLat[from], tt.StopLon[from], tt.StopLat[to], tt.StopLon[to], false)
	}
	edges := ns.PathTo(g, anchor)
	fromAnchor := anchor
	if len(edges) > 0 {
		fromAnchor = g.SourceOf(edges[0])
	}
	fromM := stopConnectorMeters(sa, fromAnchor)
	toM := stopConnectorMeters(sb, anchor)
	var enc geo.PolylineEncoder
	enc.Add(tt.StopLat[from], tt.StopLon[from])
	appendStopConnector(&enc, g, sa, fromAnchor, tt.StopLat[from], tt.StopLon[from], true)
	appendPath(&enc, g, edges)
	appendStopConnector(&enc, g, sb, anchor, tt.StopLat[to], tt.StopLon[to], false)
	enc.Add(tt.StopLat[to], tt.StopLon[to])
	leg.Polyline = enc.String()
	leg.DistanceM = int(graph.PathMeters(g, edges) + fromM + toM)
	leg.DurationS = int((bestD + 5) / 10)
	leg.Steps = routedSteps(graph.Steps(g, edges, graph.ModeFoot, sf), fromM, toM,
		fixedSpeedDs(uint32(fromM), sf), fixedSpeedDs(uint32(toM), sf),
		tt.StopLat[from], tt.StopLon[from], tt.StopLat[to], tt.StopLon[to])
	return leg
}

// roadLeg builds the single leg of a car/bike/walk direct route.
func (e *Engine) roadLeg(g *graph.Graph, modeName string, m graph.Mode, sf uint32,
	edges []uint32, endSeed int32, snF, snT graph.Snap, fLat, fLon, tLat, tLon int32) Leg {

	leg := Leg{Mode: modeName,
		From: Place{Lat: e7f(fLat), Lon: e7f(fLon)},
		To:   Place{Lat: e7f(tLat), Lon: e7f(tLon)},
	}
	startNode := endSeed
	if len(edges) > 0 {
		startNode = g.SourceOf(edges[0])
	}
	startM := snapConnectorMeters(snF, startNode)
	endM := snapConnectorMeters(snT, endSeed)
	var enc geo.PolylineEncoder
	enc.Add(fLat, fLon)
	appendSnapToNode(&enc, g, snF, startNode)
	appendPath(&enc, g, edges)
	appendNodeToSnap(&enc, g, snT, endSeed)
	enc.Add(tLat, tLon)
	leg.Polyline = enc.String()
	leg.DistanceM = int(graph.PathMeters(g, edges) + startM + endM)
	leg.Steps = routedSteps(graph.Steps(g, edges, m, sf), startM, endM,
		snapConnectorDs(g, snF, startNode, m, sf, true),
		snapConnectorDs(g, snT, endSeed, m, sf, false),
		fLat, fLon, tLat, tLon)
	return leg
}

func snapConnectorMeters(sn graph.Snap, node int32) float64 {
	switch node {
	case sn.U:
		return sn.PerpM + sn.AlongU
	case sn.V:
		return sn.PerpM + sn.AlongV
	}
	return sn.PerpM
}

func stopConnectorMeters(sn transit.StopSnap, node int32) float64 {
	if node == sn.NodeU {
		return float64(sn.MetersU)
	}
	if node == sn.NodeV {
		return float64(sn.MetersV)
	}
	return 0
}

func snapConnectorDs(g *graph.Graph, sn graph.Snap, node int32, m graph.Mode, sf uint32, source bool) uint32 {
	edge := int32(-1)
	if source {
		if node == sn.V && sn.Fwd >= 0 && g.Allowed(uint32(sn.Fwd), m) {
			edge = sn.Fwd
		} else if node == sn.U && sn.Bwd >= 0 && g.Allowed(uint32(sn.Bwd), m) {
			edge = sn.Bwd
		}
	} else {
		if node == sn.U && sn.Fwd >= 0 && g.Allowed(uint32(sn.Fwd), m) {
			edge = sn.Fwd
		} else if node == sn.V && sn.Bwd >= 0 && g.Allowed(uint32(sn.Bwd), m) {
			edge = sn.Bwd
		}
	}
	if edge < 0 {
		edge = max32(sn.Fwd, 0)
	}
	return partialDs(g, uint32(edge), snapConnectorMeters(sn, node), m, sf)
}

func appendPath(enc *geo.PolylineEncoder, g *graph.Graph, edges []uint32) {
	if len(edges) == 0 {
		return
	}
	lats, lons := graph.PathGeometry(g, edges)
	for i := range lats {
		enc.Add(lats[i], lons[i])
	}
}

// appendSnapToNode emits the partial snapped edge in travel order. Including
// its intermediate geometry keeps the displayed line and the routed metres
// in agreement even when the snapped street bends.
func appendSnapToNode(enc *geo.PolylineEncoder, g *graph.Graph, sn graph.Snap, node int32) {
	lats, lons, seg := snapGeometry(g, sn)
	enc.Add(sn.PLat, sn.PLon)
	if len(lats) == 0 {
		if node >= 0 {
			enc.Add(g.NodeLat[node], g.NodeLon[node])
		}
		return
	}
	if node == sn.U {
		for i := seg; i >= 0; i-- {
			enc.Add(lats[i], lons[i])
		}
		return
	}
	if node == sn.V {
		for i := seg + 1; i < len(lats); i++ {
			enc.Add(lats[i], lons[i])
		}
		return
	}
	enc.Add(g.NodeLat[node], g.NodeLon[node])
}

func appendNodeToSnap(enc *geo.PolylineEncoder, g *graph.Graph, sn graph.Snap, node int32) {
	lats, lons, seg := snapGeometry(g, sn)
	if len(lats) == 0 {
		if node >= 0 {
			enc.Add(g.NodeLat[node], g.NodeLon[node])
		}
		enc.Add(sn.PLat, sn.PLon)
		return
	}
	if node == sn.U {
		for i := 0; i <= seg; i++ {
			enc.Add(lats[i], lons[i])
		}
	} else if node == sn.V {
		for i := len(lats) - 1; i >= seg+1; i-- {
			enc.Add(lats[i], lons[i])
		}
	} else {
		enc.Add(g.NodeLat[node], g.NodeLon[node])
	}
	enc.Add(sn.PLat, sn.PLon)
}

func snapGeometry(g *graph.Graph, sn graph.Snap) (lats, lons []int32, segment int) {
	if sn.Fwd < 0 {
		return nil, nil, 0
	}
	lats, lons = g.AppendGeometry(uint32(sn.Fwd), sn.U, false, nil, nil)
	segment = 0
	remaining := sn.AlongU
	for segment+1 < len(lats)-1 {
		m := geo.Dist(lats[segment], lons[segment], lats[segment+1], lons[segment+1])
		if remaining <= m {
			break
		}
		remaining -= m
		segment++
	}
	return lats, lons, segment
}

func appendStopConnector(enc *geo.PolylineEncoder, g *graph.Graph, sn transit.StopSnap,
	node, stopLat, stopLon int32, stopToNode bool) {
	streetSnap := graph.Snap{
		Fwd: sn.Edge, U: sn.NodeU, V: sn.NodeV,
		PLat: sn.SnapLat, PLon: sn.SnapLon, AlongU: float64(sn.AlongU),
	}
	if stopToNode {
		enc.Add(stopLat, stopLon)
		appendSnapToNode(enc, g, streetSnap, node)
		return
	}
	appendNodeToSnap(enc, g, streetSnap, node)
	enc.Add(stopLat, stopLon)
}

func routedSteps(in []graph.Step, startM, endM float64, startDs, endDs uint32,
	startLat, startLon, endLat, endLon int32) []Step {
	if len(in) == 0 {
		return []Step{
			{Kind: "depart", Modifier: "straight", DistanceM: int(startM + endM),
				DurationS: int((startDs + endDs) / 10), Lat: e7f(startLat), Lon: e7f(startLon)},
			{Kind: "arrive", Modifier: "straight", Lat: e7f(endLat), Lon: e7f(endLon)},
		}
	}
	in[0].DistM += startM
	in[0].Ds += startDs
	in[0].Lat, in[0].Lon = startLat, startLon
	lastMove := len(in) - 2 // graph.Steps always terminates with an arrive marker
	in[lastMove].DistM += endM
	in[lastMove].Ds += endDs
	in[len(in)-1].Lat, in[len(in)-1].Lon = endLat, endLon
	return stepsDTO(in)
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

// reconcileLegSteps absorbs integer/decisecond rounding into the final
// movement instruction so API consumers can trust both counters exactly.
func reconcileLegSteps(leg *Leg) {
	if leg.Mode == "transit" || leg.DistanceM == 0 && leg.DurationS == 0 {
		return
	}
	if len(leg.Steps) == 0 {
		leg.Steps = []Step{
			{Kind: "depart", Modifier: "straight", DistanceM: leg.DistanceM,
				DurationS: leg.DurationS, Lat: leg.From.Lat, Lon: leg.From.Lon},
			{Kind: "arrive", Modifier: "straight", Lat: leg.To.Lat, Lon: leg.To.Lon},
		}
		return
	}
	leg.Steps[0].Lat, leg.Steps[0].Lon = leg.From.Lat, leg.From.Lon
	last := len(leg.Steps) - 1
	leg.Steps[last].Lat, leg.Steps[last].Lon = leg.To.Lat, leg.To.Lon
	reconcileStepDistance(leg.Steps, leg.DistanceM)
	reconcileStepDuration(leg.Steps, leg.DurationS)
}

func reconcileStepDistance(steps []Step, target int) {
	sum := 0
	for i := range steps {
		sum += steps[i].DistanceM
	}
	adjustStepTotal(steps, target-sum, func(s *Step) *int { return &s.DistanceM })
}

func reconcileStepDuration(steps []Step, target int) {
	sum := 0
	for i := range steps {
		sum += steps[i].DurationS
	}
	adjustStepTotal(steps, target-sum, func(s *Step) *int { return &s.DurationS })
}

func adjustStepTotal(steps []Step, delta int, field func(*Step) *int) {
	lastMove := len(steps) - 1
	if lastMove > 0 && steps[lastMove].Kind == "arrive" {
		lastMove--
	}
	if delta >= 0 {
		*field(&steps[lastMove]) += delta
		return
	}
	for i := lastMove; i >= 0 && delta < 0; i-- {
		v := field(&steps[i])
		take := min(*v, -delta)
		*v -= take
		delta += take
	}
}
