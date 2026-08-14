package track

// Corridoio sulla shape del pattern: serve al check "sei rimasto sulla
// linea" (gps.go). La shape di un pattern vive negli array globali
// ShpLat/ShpLon del timetable; PatShapeIdx mappa ogni fermata del pattern al
// suo punto di shape, quindi "oltre la fermata di discesa" è semplicemente
// l'intervallo [shapeIdx(alight), fine shape].

import (
	"math"

	"gotransit/internal/transit"
)

// shapeSeg is a half-open range of global shape points [lo, hi].
type shapeSeg struct{ lo, hi uint32 }

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
	cosLat := math.Cos(lat * math.Pi / 180)
	const mPerDeg = 111320.0
	px, py := 0.0, 0.0 // the fix is the local origin

	toXY := func(i uint32) (float64, float64) {
		return (float64(tt.ShpLon[i])/1e7 - lon) * mPerDeg * cosLat,
			(float64(tt.ShpLat[i])/1e7 - lat) * mPerDeg
	}

	ax, ay := toXY(sg.lo)
	for i := sg.lo + 1; i <= sg.hi; i++ {
		bx, by := toXY(i)
		if segDist(px, py, ax, ay, bx, by) <= tolM {
			return true
		}
		ax, ay = bx, by
	}
	return false
}

// segDist is the distance from point p to segment a-b (planar).
func segDist(px, py, ax, ay, bx, by float64) float64 {
	dx, dy := bx-ax, by-ay
	l2 := dx*dx + dy*dy
	t := 0.0
	if l2 > 0 {
		t = ((px-ax)*dx + (py-ay)*dy) / l2
		t = math.Max(0, math.Min(1, t))
	}
	return math.Hypot(px-(ax+t*dx), py-(ay+t*dy))
}
