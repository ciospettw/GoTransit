package rt

import "math"

// Encode builds a valid GTFS-RT FeedMessage from a Feed — the mirror of
// Decode. Used by the E2E test harness to fabricate evolving feeds, and handy
// for mocking operators.

func Encode(f *Feed) []byte {
	var msg wenc
	var hdr wenc
	hdr.bytesField(1, []byte("2.0"))
	hdr.varintField(3, f.Timestamp)
	msg.bytesField(1, hdr.b)

	for i := range f.Trips {
		tu := &f.Trips[i]
		var e wenc
		e.bytesField(1, []byte("e-tu"))
		e.bytesField(3, encodeTripUpdate(tu))
		msg.bytesField(2, e.b)
	}
	for i := range f.Vehicles {
		v := &f.Vehicles[i]
		var e wenc
		e.bytesField(1, []byte("e-vp"))
		e.bytesField(4, encodeVehicle(v))
		msg.bytesField(2, e.b)
	}
	return msg.b
}

func encodeTripDescriptor(tripID, routeID, startDate string, cancelled, added bool) []byte {
	var td wenc
	td.bytesField(1, []byte(tripID))
	if routeID != "" {
		td.bytesField(5, []byte(routeID))
	}
	if startDate != "" {
		td.bytesField(3, []byte(startDate))
	}
	if cancelled {
		td.varintField(4, 3)
	} else if added {
		td.varintField(4, 1)
	}
	return td.b
}

func encodeTripUpdate(tu *TripRT) []byte {
	var b wenc
	b.bytesField(1, encodeTripDescriptor(tu.TripID, tu.RouteID, tu.StartDate, tu.Cancelled, tu.Added))
	for i := range tu.STUs {
		b.bytesField(2, encodeSTU(&tu.STUs[i]))
	}
	if tu.Timestamp != 0 {
		b.varintField(4, tu.Timestamp)
	}
	if tu.HasDelay && tu.DelaySec != Absent {
		b.varintField(5, uint64(uint32(tu.DelaySec)))
	}
	return b.b
}

func encodeSTU(s *STU) []byte {
	var b wenc
	if s.Seq != Absent {
		b.varintField(1, uint64(uint32(s.Seq)))
	}
	if s.ArrDelay != Absent || s.ArrTime != 0 {
		b.bytesField(2, encodeSTE(s.ArrDelay, s.ArrTime))
	}
	if s.DepDelay != Absent || s.DepTime != 0 {
		b.bytesField(3, encodeSTE(s.DepDelay, s.DepTime))
	}
	if s.StopID != "" {
		b.bytesField(4, []byte(s.StopID))
	}
	if s.Skipped {
		b.varintField(5, 1)
	} else if s.NoData {
		b.varintField(5, 2)
	}
	return b.b
}

func encodeSTE(delay int32, t int64) []byte {
	var b wenc
	if delay != Absent {
		b.varintField(1, signedAsVarint(delay))
	}
	if t != 0 {
		b.varintField(2, uint64(t))
	}
	return b.b
}

// signedAsVarint encodes int32 the proto way: two's complement in 64 bits.
func signedAsVarint(v int32) uint64 { return uint64(uint32(v)) | signExt(v) }

func signExt(v int32) uint64 {
	if v < 0 {
		return 0xFFFFFFFF00000000
	}
	return 0
}

func encodeVehicle(v *VehicleRT) []byte {
	var b wenc
	b.bytesField(1, encodeTripDescriptor(v.TripID, v.RouteID, "", false, false))
	if v.Lat != 0 || v.Lon != 0 {
		var pos wenc
		pos.fixed32Field(1, floatBits(v.Lat))
		pos.fixed32Field(2, floatBits(v.Lon))
		b.bytesField(2, pos.b)
	}
	if v.CurrentSeq != Absent {
		b.varintField(3, uint64(uint32(v.CurrentSeq)))
	}
	if v.Status != Absent {
		b.varintField(4, uint64(uint32(v.Status)))
	}
	if v.Timestamp != 0 {
		b.varintField(5, v.Timestamp)
	}
	if v.StopID != "" {
		b.bytesField(7, []byte(v.StopID))
	}
	return b.b
}

// ---- wire writer ----------------------------------------------------------------

type wenc struct{ b []byte }

func (e *wenc) varint(v uint64) {
	for v >= 0x80 {
		e.b = append(e.b, byte(v)|0x80)
		v >>= 7
	}
	e.b = append(e.b, byte(v))
}

func (e *wenc) tag(field, wire int) { e.varint(uint64(field<<3 | wire)) }

func (e *wenc) varintField(field int, v uint64) {
	e.tag(field, 0)
	e.varint(v)
}

func (e *wenc) bytesField(field int, data []byte) {
	e.tag(field, 2)
	e.varint(uint64(len(data)))
	e.b = append(e.b, data...)
}

func (e *wenc) fixed32Field(field int, bits uint32) {
	e.tag(field, 5)
	e.b = append(e.b, byte(bits), byte(bits>>8), byte(bits>>16), byte(bits>>24))
}

func floatBits(f float32) uint32 { return math.Float32bits(f) }
