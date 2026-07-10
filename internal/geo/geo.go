// Package geo: coordinate math on fixed-point E7 degrees (int32), the native
// unit of the engine. E7 matches OSM's precision exactly and keeps every
// coordinate in 8 bytes.
package geo

import "math"

const (
	earthR          = 6371008.8 // mean earth radius, meters
	e7              = 1e-7
	DegToRad        = math.Pi / 180
	metersPerDegLat = 111194.9 // earthR * pi / 180
)

// Dist returns the distance in meters between two E7 points.
// Equirectangular approximation: at street-graph scales (< a few hundred km)
// the error is negligible and it is ~3x faster than haversine.
func Dist(aLat, aLon, bLat, bLon int32) float64 {
	dLat := float64(bLat-aLat) * e7
	dLon := float64(bLon-aLon) * e7
	mLat := float64(aLat+bLat) * 0.5 * e7 * DegToRad
	x := dLon * math.Cos(mLat)
	return metersPerDegLat * math.Sqrt(dLat*dLat+x*x)
}

// Haversine is the exact great-circle distance in meters.
func Haversine(aLat, aLon, bLat, bLon int32) float64 {
	la1 := float64(aLat) * e7 * DegToRad
	la2 := float64(bLat) * e7 * DegToRad
	dLa := la2 - la1
	dLo := float64(bLon-aLon) * e7 * DegToRad
	h := sq(math.Sin(dLa/2)) + math.Cos(la1)*math.Cos(la2)*sq(math.Sin(dLo/2))
	return 2 * earthR * math.Asin(math.Sqrt(h))
}

func sq(x float64) float64 { return x * x }

// Bearing returns the initial bearing in degrees [0,360) from a to b.
func Bearing(aLat, aLon, bLat, bLon int32) float64 {
	la1 := float64(aLat) * e7 * DegToRad
	la2 := float64(bLat) * e7 * DegToRad
	dLo := float64(bLon-aLon) * e7 * DegToRad
	y := math.Sin(dLo) * math.Cos(la2)
	x := math.Cos(la1)*math.Sin(la2) - math.Sin(la1)*math.Cos(la2)*math.Cos(dLo)
	deg := math.Atan2(y, x) / DegToRad
	if deg < 0 {
		deg += 360
	}
	return deg
}

// ProjectOnSegment projects point p onto segment a-b. Returns the squared
// "flat" distance in E7-ish units (for comparisons only), the fraction t in
// [0,1] along the segment, and the projected point.
func ProjectOnSegment(pLat, pLon, aLat, aLon, bLat, bLon int32) (d2 float64, t float64, qLat, qLon int32) {
	// scale lon by cos(lat) so distances are isotropic
	cos := math.Cos(float64(pLat) * e7 * DegToRad)
	ax, ay := float64(aLon)*cos, float64(aLat)
	bx, by := float64(bLon)*cos, float64(bLat)
	px, py := float64(pLon)*cos, float64(pLat)
	dx, dy := bx-ax, by-ay
	l2 := dx*dx + dy*dy
	if l2 == 0 {
		t = 0
	} else {
		t = ((px-ax)*dx + (py-ay)*dy) / l2
		t = math.Max(0, math.Min(1, t))
	}
	qx, qy := ax+t*dx, ay+t*dy
	d2 = (px-qx)*(px-qx) + (py-qy)*(py-qy)
	qLat = int32(math.Round(qy))
	qLon = int32(math.Round(qx / cos))
	return d2, t, qLat, qLon
}
