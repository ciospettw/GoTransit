package geo

// Google encoded polyline, precision 5 — the interchange format every mobile
// map SDK understands. Coordinates come in as E7 and are rounded to E5.

// PolylineEncoder accumulates points into an encoded polyline string.
type PolylineEncoder struct {
	buf      []byte
	lat, lon int32 // last emitted, in E5
}

// Add appends a point given in E7.
func (p *PolylineEncoder) Add(latE7, lonE7 int32) {
	lat := roundE7toE5(latE7)
	lon := roundE7toE5(lonE7)
	if len(p.buf) > 0 && lat == p.lat && lon == p.lon {
		return // collapse consecutive duplicates
	}
	p.buf = encodeSigned(p.buf, lat-p.lat)
	p.buf = encodeSigned(p.buf, lon-p.lon)
	p.lat, p.lon = lat, lon
}

// String returns the encoded polyline.
func (p *PolylineEncoder) String() string { return string(p.buf) }

// Len returns the number of bytes encoded so far.
func (p *PolylineEncoder) Len() int { return len(p.buf) }

func roundE7toE5(v int32) int32 {
	if v >= 0 {
		return (v + 50) / 100
	}
	return (v - 50) / 100
}

func encodeSigned(dst []byte, v int32) []byte {
	u := uint32(v) << 1
	if v < 0 {
		u = ^u
	}
	for u >= 0x20 {
		dst = append(dst, byte(0x20|(u&0x1f))+63)
		u >>= 5
	}
	return append(dst, byte(u)+63)
}

// EncodePolyline encodes a whole path of E7 points.
func EncodePolyline(lats, lons []int32) string {
	var e PolylineEncoder
	for i := range lats {
		e.Add(lats[i], lons[i])
	}
	return e.String()
}

// DecodePolyline decodes to E7 points (test helper and future API input).
func DecodePolyline(s string) (lats, lons []int32) {
	var lat, lon int32
	i := 0
	next := func() int32 {
		var res uint32
		var shift uint
		for {
			b := uint32(s[i]) - 63
			i++
			res |= (b & 0x1f) << shift
			if b < 0x20 {
				break
			}
			shift += 5
		}
		if res&1 != 0 {
			return int32(^(res >> 1))
		}
		return int32(res >> 1)
	}
	for i < len(s) {
		lat += next()
		lon += next()
		lats = append(lats, lat*100)
		lons = append(lons, lon*100)
	}
	return lats, lons
}
