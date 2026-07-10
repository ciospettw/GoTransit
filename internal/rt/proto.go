// Package rt ingests GTFS-Realtime feeds: a hand-written protobuf decoder
// (zero deps, same philosophy as the PBF reader), pollers with conditional
// GET, and the overlay builder that projects trip updates onto the compiled
// timetable.
package rt

import (
	"fmt"
	"math"
)

// Feed is one decoded GTFS-RT FeedMessage (trip updates and/or vehicles).
type Feed struct {
	Timestamp uint64
	Trips     []TripRT
	Vehicles  []VehicleRT
}

// TripRT is one TripUpdate.
type TripRT struct {
	TripID    string
	RouteID   string
	StartDate string
	Cancelled bool
	Added     bool
	DelaySec  int32 // trip-level fallback delay
	HasDelay  bool
	Timestamp uint64
	STUs      []STU
}

// Absent marks missing numeric fields.
const Absent = int32(math.MinInt32)

// STU is one StopTimeUpdate.
type STU struct {
	Seq      int32 // stop_sequence or Absent
	StopID   string
	ArrDelay int32 // seconds or Absent
	DepDelay int32
	ArrTime  int64 // POSIX or 0
	DepTime  int64
	Skipped  bool
	NoData   bool
}

// VehicleRT is one VehiclePosition.
type VehicleRT struct {
	TripID     string
	RouteID    string
	Lat, Lon   float32
	CurrentSeq int32 // or Absent
	StopID     string
	Status     int32 // 0 INCOMING_AT, 1 STOPPED_AT, 2 IN_TRANSIT_TO, Absent
	Timestamp  uint64
}

// Decode parses a FeedMessage.
func Decode(data []byte) (*Feed, error) {
	f := &Feed{}
	p := wire{b: data}
	for p.more() {
		field, wt, err := p.field()
		if err != nil {
			return nil, err
		}
		switch field {
		case 1: // header
			hb, err := p.bytes()
			if err != nil {
				return nil, err
			}
			h := wire{b: hb}
			for h.more() {
				hf, hw, err := h.field()
				if err != nil {
					return nil, err
				}
				if hf == 3 && hw == 0 {
					v, err := h.varint()
					if err != nil {
						return nil, err
					}
					f.Timestamp = v
				} else if err := h.skip(hw); err != nil {
					return nil, err
				}
			}
		case 2: // entity
			eb, err := p.bytes()
			if err != nil {
				return nil, err
			}
			if err := decodeEntity(eb, f); err != nil {
				return nil, err
			}
		default:
			if err := p.skip(wt); err != nil {
				return nil, err
			}
		}
	}
	return f, nil
}

func decodeEntity(b []byte, f *Feed) error {
	p := wire{b: b}
	for p.more() {
		field, wt, err := p.field()
		if err != nil {
			return err
		}
		switch field {
		case 3: // trip_update
			tb, err := p.bytes()
			if err != nil {
				return err
			}
			tu, err := decodeTripUpdate(tb)
			if err != nil {
				return err
			}
			f.Trips = append(f.Trips, tu)
		case 4: // vehicle
			vb, err := p.bytes()
			if err != nil {
				return err
			}
			v, err := decodeVehicle(vb)
			if err != nil {
				return err
			}
			f.Vehicles = append(f.Vehicles, v)
		default:
			if err := p.skip(wt); err != nil {
				return err
			}
		}
	}
	return nil
}

func decodeTripDescriptor(b []byte) (tripID, routeID, startDate string, rel int64, err error) {
	p := wire{b: b}
	rel = -1
	for p.more() {
		field, wt, ferr := p.field()
		if ferr != nil {
			return "", "", "", 0, ferr
		}
		switch field {
		case 1:
			s, err := p.bytes()
			if err != nil {
				return "", "", "", 0, err
			}
			tripID = string(s)
		case 5:
			s, err := p.bytes()
			if err != nil {
				return "", "", "", 0, err
			}
			routeID = string(s)
		case 3:
			s, err := p.bytes()
			if err != nil {
				return "", "", "", 0, err
			}
			startDate = string(s)
		case 4:
			v, err := p.varint()
			if err != nil {
				return "", "", "", 0, err
			}
			rel = int64(v)
		default:
			if err := p.skip(wt); err != nil {
				return "", "", "", 0, err
			}
		}
	}
	return tripID, routeID, startDate, rel, nil
}

func decodeTripUpdate(b []byte) (TripRT, error) {
	tu := TripRT{DelaySec: Absent}
	p := wire{b: b}
	for p.more() {
		field, wt, err := p.field()
		if err != nil {
			return tu, err
		}
		switch field {
		case 1: // trip descriptor
			tb, err := p.bytes()
			if err != nil {
				return tu, err
			}
			tid, rid, sd, rel, err := decodeTripDescriptor(tb)
			if err != nil {
				return tu, err
			}
			tu.TripID, tu.RouteID, tu.StartDate = tid, rid, sd
			tu.Cancelled = rel == 3
			tu.Added = rel == 1
		case 2: // stop_time_update
			sb, err := p.bytes()
			if err != nil {
				return tu, err
			}
			stu, err := decodeSTU(sb)
			if err != nil {
				return tu, err
			}
			tu.STUs = append(tu.STUs, stu)
		case 4:
			v, err := p.varint()
			if err != nil {
				return tu, err
			}
			tu.Timestamp = v
		case 5:
			v, err := p.varint()
			if err != nil {
				return tu, err
			}
			tu.DelaySec = signedVarint32(v)
			tu.HasDelay = true
		default:
			if err := p.skip(wt); err != nil {
				return tu, err
			}
		}
	}
	return tu, nil
}

// signedVarint32: proto int32 is encoded as a (possibly 10-byte) two's
// complement varint; truncate to 32 bits preserving sign.
func signedVarint32(v uint64) int32 { return int32(uint32(v)) }

func decodeSTU(b []byte) (STU, error) {
	s := STU{Seq: Absent, ArrDelay: Absent, DepDelay: Absent}
	p := wire{b: b}
	for p.more() {
		field, wt, err := p.field()
		if err != nil {
			return s, err
		}
		switch field {
		case 1:
			v, err := p.varint()
			if err != nil {
				return s, err
			}
			s.Seq = int32(v)
		case 4:
			sb, err := p.bytes()
			if err != nil {
				return s, err
			}
			s.StopID = string(sb)
		case 2: // arrival StopTimeEvent
			eb, err := p.bytes()
			if err != nil {
				return s, err
			}
			if s.ArrDelay, s.ArrTime, err = decodeSTE(eb); err != nil {
				return s, err
			}
		case 3: // departure
			eb, err := p.bytes()
			if err != nil {
				return s, err
			}
			if s.DepDelay, s.DepTime, err = decodeSTE(eb); err != nil {
				return s, err
			}
		case 5:
			v, err := p.varint()
			if err != nil {
				return s, err
			}
			s.Skipped = v == 1
			s.NoData = v == 2
		default:
			if err := p.skip(wt); err != nil {
				return s, err
			}
		}
	}
	return s, nil
}

func decodeSTE(b []byte) (delay int32, t int64, err error) {
	delay = Absent
	p := wire{b: b}
	for p.more() {
		field, wt, ferr := p.field()
		if ferr != nil {
			return delay, t, ferr
		}
		switch field {
		case 1:
			v, err := p.varint()
			if err != nil {
				return delay, t, err
			}
			delay = signedVarint32(v)
		case 2:
			v, err := p.varint()
			if err != nil {
				return delay, t, err
			}
			t = int64(v)
		default:
			if err := p.skip(wt); err != nil {
				return delay, t, err
			}
		}
	}
	return delay, t, nil
}

func decodeVehicle(b []byte) (VehicleRT, error) {
	v := VehicleRT{CurrentSeq: Absent, Status: Absent}
	p := wire{b: b}
	for p.more() {
		field, wt, err := p.field()
		if err != nil {
			return v, err
		}
		switch field {
		case 1: // trip descriptor
			tb, err := p.bytes()
			if err != nil {
				return v, err
			}
			tid, rid, _, _, err := decodeTripDescriptor(tb)
			if err != nil {
				return v, err
			}
			v.TripID, v.RouteID = tid, rid
		case 2: // position
			pb, err := p.bytes()
			if err != nil {
				return v, err
			}
			q := wire{b: pb}
			for q.more() {
				qf, qw, err := q.field()
				if err != nil {
					return v, err
				}
				switch {
				case qf == 1 && qw == 5:
					bits, err := q.fixed32()
					if err != nil {
						return v, err
					}
					v.Lat = math.Float32frombits(bits)
				case qf == 2 && qw == 5:
					bits, err := q.fixed32()
					if err != nil {
						return v, err
					}
					v.Lon = math.Float32frombits(bits)
				default:
					if err := q.skip(qw); err != nil {
						return v, err
					}
				}
			}
		case 3:
			n, err := p.varint()
			if err != nil {
				return v, err
			}
			v.CurrentSeq = int32(n)
		case 7:
			sb, err := p.bytes()
			if err != nil {
				return v, err
			}
			v.StopID = string(sb)
		case 4:
			n, err := p.varint()
			if err != nil {
				return v, err
			}
			v.Status = int32(n)
		case 5:
			n, err := p.varint()
			if err != nil {
				return v, err
			}
			v.Timestamp = n
		default:
			if err := p.skip(wt); err != nil {
				return v, err
			}
		}
	}
	return v, nil
}

// ---- protobuf wire primitives -------------------------------------------------

type wire struct {
	b []byte
	i int
}

func (p *wire) more() bool { return p.i < len(p.b) }

func (p *wire) varint() (uint64, error) {
	var v uint64
	var shift uint
	for p.i < len(p.b) {
		c := p.b[p.i]
		p.i++
		v |= uint64(c&0x7f) << shift
		if c < 0x80 {
			return v, nil
		}
		shift += 7
		if shift > 63 {
			break
		}
	}
	return 0, fmt.Errorf("gtfs-rt: truncated varint")
}

func (p *wire) field() (int, int, error) {
	k, err := p.varint()
	if err != nil {
		return 0, 0, err
	}
	return int(k >> 3), int(k & 7), nil
}

func (p *wire) bytes() ([]byte, error) {
	n, err := p.varint()
	if err != nil {
		return nil, err
	}
	if p.i+int(n) > len(p.b) {
		return nil, fmt.Errorf("gtfs-rt: truncated bytes field")
	}
	s := p.b[p.i : p.i+int(n)]
	p.i += int(n)
	return s, nil
}

func (p *wire) fixed32() (uint32, error) {
	if p.i+4 > len(p.b) {
		return 0, fmt.Errorf("gtfs-rt: truncated fixed32")
	}
	v := uint32(p.b[p.i]) | uint32(p.b[p.i+1])<<8 | uint32(p.b[p.i+2])<<16 | uint32(p.b[p.i+3])<<24
	p.i += 4
	return v, nil
}

func (p *wire) skip(wt int) error {
	switch wt {
	case 0:
		_, err := p.varint()
		return err
	case 1:
		p.i += 8
	case 2:
		_, err := p.bytes()
		return err
	case 5:
		p.i += 4
	default:
		return fmt.Errorf("gtfs-rt: unsupported wire type %d", wt)
	}
	if p.i > len(p.b) {
		return fmt.Errorf("gtfs-rt: truncated field")
	}
	return nil
}
