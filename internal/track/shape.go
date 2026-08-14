package track

// Corridoio sulla shape del pattern: serve sia al boarding senza una
// VehiclePosition affidabile, sia al check "sei rimasto sulla linea"
// (gps.go). La shape vive negli array globali ShpLat/ShpLon del timetable;
// PatShapeIdx mappa ogni fermata del pattern al suo punto di shape.

import (
	"math"

	"gotransit/internal/transit"
)

// shapeSeg is an inclusive range of global shape points [lo, hi].
type shapeSeg struct{ lo, hi uint32 }

// shapeBetween returns the portion of the pattern shape covered by a ride,
// from its boarding stop through its alighting stop. Besides keeping the
// rider inside the right line, limiting the corridor to this range prevents
// a nearby parallel/opposite branch from looking like a boarding.
func shapeBetween(tt *transit.Timetable, r ride) (shapeSeg, bool) {
	pat := tt.PatternOfTrip(r.trip)
	sh := tt.PatShape[pat]
	if sh < 0 {
		return shapeSeg{}, false
	}
	first := tt.PatFirstStop[pat]
	lo := tt.PatShapeIdx[first+uint32(r.board)]
	hi := tt.PatShapeIdx[first+uint32(r.alight)]
	if hi <= lo || lo < tt.ShpFirst[sh] || hi >= tt.ShpFirst[sh+1] {
		return shapeSeg{}, false
	}
	return shapeSeg{lo, hi}, true
}

// shapeBeyond returns the pattern shape from the ride's alight stop to the
// end of the shape (ok=false when the pattern has no usable shape there).
func shapeBeyond(tt *transit.Timetable, r ride) (shapeSeg, bool) {
	pat := tt.PatternOfTrip(r.trip)
	sh := tt.PatShape[pat]
	if sh < 0 {
		return shapeSeg{}, false
	}
	lo := tt.PatShapeIdx[tt.PatFirstStop[pat]+uint32(r.alight)]
	hi := tt.ShpFirst[sh+1]
	if hi <= lo+1 {
		return shapeSeg{}, false
	}
	return shapeSeg{lo, hi - 1}, true
}

// within reports whether (lat, lon) lies inside tol meters of the segment's
// polyline. Distances are computed point-to-segment on a local equirectangular
// projection — shapes are dense enough that this is exact for our purposes.
func (sg shapeSeg) within(tt *transit.Timetable, lat, lon, tolM float64) bool {
	_, distance, ok := sg.project(tt, lat, lon)
	return ok && distance <= tolM
}

// project snaps a coordinate to the polyline and returns both its distance
// along the shape (from sg.lo) and its perpendicular distance from it. The
// along value gives the GPS fusion code a direction: successive fixes must
// progress board -> alight, not merely touch the same corridor in any order.
func (sg shapeSeg) project(tt *transit.Timetable, lat, lon float64) (alongM, distanceM float64, ok bool) {
	if sg.hi <= sg.lo || int(sg.hi) >= len(tt.ShpLat) || int(sg.hi) >= len(tt.ShpLon) {
		return 0, 0, false
	}
	cosLat := math.Cos(lat * math.Pi / 180)
	const mPerDeg = 111320.0
	px, py := 0.0, 0.0 // the fix is the local origin

	toXY := func(i uint32) (float64, float64) {
		return (float64(tt.ShpLon[i])/1e7 - lon) * mPerDeg * cosLat,
			(float64(tt.ShpLat[i])/1e7 - lat) * mPerDeg
	}

	distanceM = math.Inf(1)
	cumulative := 0.0
	ax, ay := toXY(sg.lo)
	for i := sg.lo + 1; i <= sg.hi; i++ {
		bx, by := toXY(i)
		dx, dy := bx-ax, by-ay
		segLen := math.Hypot(dx, dy)
		t := segFraction(px, py, ax, ay, bx, by)
		d := math.Hypot(px-(ax+t*dx), py-(ay+t*dy))
		if d < distanceM {
			distanceM = d
			alongM = cumulative + t*segLen
		}
		cumulative += segLen
		ax, ay = bx, by
	}
	return alongM, distanceM, true
}

// segDist is the distance from point p to segment a-b (planar).
func segDist(px, py, ax, ay, bx, by float64) float64 {
	t := segFraction(px, py, ax, ay, bx, by)
	return math.Hypot(px-(ax+t*(bx-ax)), py-(ay+t*(by-ay)))
}

func segFraction(px, py, ax, ay, bx, by float64) float64 {
	dx, dy := bx-ax, by-ay
	l2 := dx*dx + dy*dy
	t := 0.0
	if l2 > 0 {
		t = ((px-ax)*dx + (py-ay)*dy) / l2
		t = math.Max(0, math.Min(1, t))
	}
	return t
}
