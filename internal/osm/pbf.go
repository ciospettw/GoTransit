// Package osm reads OpenStreetMap data: .osm.pbf extracts and .osc.gz
// (osmChange) diffs. The protobuf wire decoding is hand-written for the ~20
// PBF fields we need — no generated code, no dependencies — and blocks are
// inflated and decoded in parallel across all cores.
package osm

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"runtime"
	"sync"
)

// Header is the OSMHeader block of a PBF file.
type Header struct {
	BBox    BBox
	HasBBox bool
	// Osmosis replication markers (Geofabrik writes these): the position in
	// the region's diff stream that this extract corresponds to.
	ReplicationSeq int64
	ReplicationTS  int64
	ReplicationURL string
}

// BBox in E7 degrees.
type BBox struct{ MinLat, MinLon, MaxLat, MaxLon int32 }

// Contains reports whether the point is inside the box extended by marginM
// meters (approximated in degrees; good enough for coverage checks).
func (b BBox) Contains(latE7, lonE7 int32, marginM float64) bool {
	mLat := int32(marginM / 111.195 * 10000) // meters → E7 degrees
	mLon := int32(marginM / 78 * 10000)      // ~cos(45°), generous at IT latitudes
	return latE7 >= b.MinLat-mLat && latE7 <= b.MaxLat+mLat &&
		lonE7 >= b.MinLon-mLon && lonE7 <= b.MaxLon+mLon
}

// StringTable of one primitive block. Entries alias the block buffer.
type StringTable struct{ tab [][]byte }

// Tagged is anything exposing OSM tags: PBF views and osc map tags alike.
type Tagged interface {
	Get(key string) []byte
}

// MapTags adapts a plain map (osmChange XML) to the Tagged interface.
type MapTags map[string]string

// Get returns the value for key, or nil.
func (m MapTags) Get(key string) []byte {
	if v, ok := m[key]; ok {
		return []byte(v)
	}
	return nil
}

// Tags is a key/value view over a block's string table.
type Tags struct {
	keys, vals []uint32
	st         *StringTable
}

// Get returns the value for key, or nil.
func (t Tags) Get(key string) []byte {
	for i, k := range t.keys {
		if string(t.st.tab[k]) == key { // no alloc: compiler optimizes this compare
			return t.st.tab[t.vals[i]]
		}
	}
	return nil
}

// Len returns the number of tags.
func (t Tags) Len() int { return len(t.keys) }

// At returns the i-th key/value pair.
func (t Tags) At(i int) (k, v []byte) { return t.st.tab[t.keys[i]], t.st.tab[t.vals[i]] }

// Way is one OSM way. Refs and Tags alias per-block buffers: handlers must
// copy anything they keep.
type Way struct {
	ID   int64
	Refs []int64
	Tags Tags
}

// NodeBatch carries decoded node coordinates (E7) of one block.
// Slices alias per-worker buffers: handlers must copy what they keep.
type NodeBatch struct {
	IDs  []int64
	Lats []int32
	Lons []int32
}

// ScanOpts configures a PBF scan. Handlers are invoked concurrently from
// multiple workers and must be safe for parallel calls.
type ScanOpts struct {
	Workers int
	Ways    func(ws []Way)     // nil → way decoding skipped
	Nodes   func(nb NodeBatch) // nil → node decoding skipped
}

// ScanPBF streams a .osm.pbf file. It always returns the parsed header.
func ScanPBF(path string, o ScanOpts) (*Header, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return scan(bufio.NewReaderSize(f, 1<<20), o)
}

func scan(r io.Reader, o ScanOpts) (*Header, error) {
	if o.Workers <= 0 {
		o.Workers = runtime.NumCPU()
	}
	var hdr *Header

	type job struct{ blob []byte }
	jobs := make(chan job, o.Workers)
	errCh := make(chan error, o.Workers+1)
	var wg sync.WaitGroup
	for w := 0; w < o.Workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d := newBlockDecoder()
			for j := range jobs {
				if err := d.decodeDataBlob(j.blob, o); err != nil {
					select {
					case errCh <- err:
					default:
					}
					return
				}
			}
		}()
	}

	var scanErr error
	first := true
readLoop:
	for {
		// frame: 4-byte big-endian BlobHeader size, BlobHeader, Blob
		var lenBuf [4]byte
		if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
			if err == io.EOF {
				break
			}
			scanErr = err
			break
		}
		bhLen := binary.BigEndian.Uint32(lenBuf[:])
		if bhLen > 64<<10 {
			scanErr = fmt.Errorf("pbf: implausible BlobHeader size %d", bhLen)
			break
		}
		bh := make([]byte, bhLen)
		if _, err := io.ReadFull(r, bh); err != nil {
			scanErr = err
			break
		}
		btype, dataSize, err := parseBlobHeader(bh)
		if err != nil {
			scanErr = err
			break
		}
		if dataSize > 64<<20 {
			scanErr = fmt.Errorf("pbf: implausible blob size %d", dataSize)
			break
		}
		blob := make([]byte, dataSize)
		if _, err := io.ReadFull(r, blob); err != nil {
			scanErr = err
			break
		}
		switch btype {
		case "OSMHeader":
			raw, err := inflateBlob(blob, nil)
			if err != nil {
				scanErr = err
				break readLoop
			}
			if hdr, err = parseHeaderBlock(raw); err != nil {
				scanErr = err
				break readLoop
			}
		case "OSMData":
			if first && hdr == nil {
				hdr = &Header{} // header block is optional in theory
			}
			select {
			case err := <-errCh:
				scanErr = err
				break readLoop
			case jobs <- job{blob}:
			}
		}
		first = false
	}
	close(jobs)
	wg.Wait()
	if scanErr == nil {
		select {
		case scanErr = <-errCh:
		default:
		}
	}
	if hdr == nil && scanErr == nil {
		scanErr = fmt.Errorf("pbf: no header block found")
	}
	return hdr, scanErr
}

// ---- protobuf primitives ---------------------------------------------------

type pb struct {
	b []byte
	i int
}

func (p *pb) more() bool { return p.i < len(p.b) }

func (p *pb) varint() (uint64, error) {
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
	return 0, fmt.Errorf("pbf: truncated varint")
}

// field reads the next field tag; returns field number and wire type.
func (p *pb) field() (int, int, error) {
	k, err := p.varint()
	if err != nil {
		return 0, 0, err
	}
	return int(k >> 3), int(k & 7), nil
}

func (p *pb) bytes() ([]byte, error) {
	n, err := p.varint()
	if err != nil {
		return nil, err
	}
	if p.i+int(n) > len(p.b) {
		return nil, fmt.Errorf("pbf: truncated bytes field")
	}
	s := p.b[p.i : p.i+int(n)]
	p.i += int(n)
	return s, nil
}

func (p *pb) skip(wire int) error {
	switch wire {
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
		return fmt.Errorf("pbf: unsupported wire type %d", wire)
	}
	if p.i > len(p.b) {
		return fmt.Errorf("pbf: truncated field")
	}
	return nil
}

func zigzag(u uint64) int64 { return int64(u>>1) ^ -int64(u&1) }

// packedSint64Delta appends zigzag-decoded values with delta accumulation.
// acc carries the running value across chunks: proto allows a packed field
// to be split into multiple wire entries.
func packedSint64Delta(data []byte, dst []int64, acc int64) ([]int64, int64, error) {
	p := pb{b: data}
	for p.more() {
		u, err := p.varint()
		if err != nil {
			return nil, 0, err
		}
		acc += zigzag(u)
		dst = append(dst, acc)
	}
	return dst, acc, nil
}

func packedUint32(data []byte, dst []uint32) ([]uint32, error) {
	p := pb{b: data}
	for p.more() {
		u, err := p.varint()
		if err != nil {
			return nil, err
		}
		dst = append(dst, uint32(u))
	}
	return dst, nil
}

// ---- blob / header decoding -------------------------------------------------

func parseBlobHeader(b []byte) (btype string, dataSize int, err error) {
	p := pb{b: b}
	for p.more() {
		f, w, err := p.field()
		if err != nil {
			return "", 0, err
		}
		switch f {
		case 1:
			s, err := p.bytes()
			if err != nil {
				return "", 0, err
			}
			btype = string(s)
		case 3:
			n, err := p.varint()
			if err != nil {
				return "", 0, err
			}
			dataSize = int(n)
		default:
			if err := p.skip(w); err != nil {
				return "", 0, err
			}
		}
	}
	return btype, dataSize, nil
}

// inflateBlob decodes a Blob message and returns the raw payload,
// reusing buf when possible.
func inflateBlob(blob []byte, d *blockDecoder) ([]byte, error) {
	p := pb{b: blob}
	var raw, zdata []byte
	rawSize := 0
	for p.more() {
		f, w, err := p.field()
		if err != nil {
			return nil, err
		}
		switch f {
		case 1:
			if raw, err = p.bytes(); err != nil {
				return nil, err
			}
		case 2:
			n, err := p.varint()
			if err != nil {
				return nil, err
			}
			rawSize = int(n)
		case 3:
			if zdata, err = p.bytes(); err != nil {
				return nil, err
			}
		case 4, 5, 6, 7:
			return nil, fmt.Errorf("pbf: unsupported blob compression (only raw and zlib)")
		default:
			if err := p.skip(w); err != nil {
				return nil, err
			}
		}
	}
	if raw != nil {
		return raw, nil
	}
	if zdata == nil {
		return nil, fmt.Errorf("pbf: empty blob")
	}
	var out []byte
	if d != nil {
		if cap(d.inflBuf) < rawSize {
			d.inflBuf = make([]byte, rawSize+rawSize/4)
		}
		out = d.inflBuf[:rawSize]
	} else {
		out = make([]byte, rawSize)
	}
	var zr io.ReadCloser
	var err error
	if d != nil && d.zr != nil {
		if err = d.zr.(zlib.Resetter).Reset(bytes.NewReader(zdata), nil); err != nil {
			return nil, err
		}
		zr = d.zr
	} else {
		if zr, err = zlib.NewReader(bytes.NewReader(zdata)); err != nil {
			return nil, err
		}
		if d != nil {
			d.zr = zr
		}
	}
	if _, err := io.ReadFull(zr, out); err != nil {
		return nil, fmt.Errorf("pbf: inflate: %w", err)
	}
	return out, nil
}

func parseHeaderBlock(b []byte) (*Header, error) {
	h := &Header{}
	p := pb{b: b}
	for p.more() {
		f, w, err := p.field()
		if err != nil {
			return nil, err
		}
		switch f {
		case 1: // HeaderBBox, nanodegrees
			bb, err := p.bytes()
			if err != nil {
				return nil, err
			}
			q := pb{b: bb}
			var vals [5]int64
			for q.more() {
				qf, _, err := q.field()
				if err != nil {
					return nil, err
				}
				u, err := q.varint()
				if err != nil {
					return nil, err
				}
				if qf >= 1 && qf <= 4 {
					vals[qf] = zigzag(u)
				}
			}
			// proto order: 1=left(minLon) 2=right(maxLon) 3=top(maxLat) 4=bottom(minLat)
			h.BBox = BBox{
				MinLat: int32(vals[4] / 100), MinLon: int32(vals[1] / 100),
				MaxLat: int32(vals[3] / 100), MaxLon: int32(vals[2] / 100),
			}
			h.HasBBox = true
		case 32:
			u, err := p.varint()
			if err != nil {
				return nil, err
			}
			h.ReplicationTS = int64(u)
		case 33:
			u, err := p.varint()
			if err != nil {
				return nil, err
			}
			h.ReplicationSeq = int64(u)
		case 34:
			s, err := p.bytes()
			if err != nil {
				return nil, err
			}
			h.ReplicationURL = string(s)
		default:
			if err := p.skip(w); err != nil {
				return nil, err
			}
		}
	}
	return h, nil
}

// ---- primitive block decoding ------------------------------------------------

type blockDecoder struct {
	inflBuf []byte
	zr      io.ReadCloser
	st      StringTable
	ways    []Way
	refs    []int64
	keys    []uint32
	vals    []uint32
	ids     []int64
	rawLat  []int64
	rawLon  []int64
	lats    []int32
	lons    []int32
}

func newBlockDecoder() *blockDecoder { return &blockDecoder{} }

func (d *blockDecoder) decodeDataBlob(blob []byte, o ScanOpts) error {
	raw, err := inflateBlob(blob, d)
	if err != nil {
		return err
	}
	return d.decodePrimitiveBlock(raw, o)
}

func (d *blockDecoder) decodePrimitiveBlock(b []byte, o ScanOpts) error {
	p := pb{b: b}
	granularity := int64(100)
	latOff, lonOff := int64(0), int64(0)
	d.st.tab = d.st.tab[:0]
	var groups [][]byte
	for p.more() {
		f, w, err := p.field()
		if err != nil {
			return err
		}
		switch f {
		case 1: // stringtable
			stb, err := p.bytes()
			if err != nil {
				return err
			}
			q := pb{b: stb}
			for q.more() {
				qf, qw, err := q.field()
				if err != nil {
					return err
				}
				if qf == 1 && qw == 2 {
					s, err := q.bytes()
					if err != nil {
						return err
					}
					d.st.tab = append(d.st.tab, s)
				} else if err := q.skip(qw); err != nil {
					return err
				}
			}
		case 2:
			g, err := p.bytes()
			if err != nil {
				return err
			}
			groups = append(groups, g)
		case 17:
			u, err := p.varint()
			if err != nil {
				return err
			}
			granularity = int64(u)
		case 19:
			u, err := p.varint()
			if err != nil {
				return err
			}
			latOff = int64(u)
		case 20:
			u, err := p.varint()
			if err != nil {
				return err
			}
			lonOff = int64(u)
		default:
			if err := p.skip(w); err != nil {
				return err
			}
		}
	}
	for _, g := range groups {
		if err := d.decodeGroup(g, granularity, latOff, lonOff, o); err != nil {
			return err
		}
	}
	return nil
}

func (d *blockDecoder) decodeGroup(g []byte, gran, latOff, lonOff int64, o ScanOpts) error {
	p := pb{b: g}
	d.ways = d.ways[:0]
	d.refs = d.refs[:0]
	d.keys = d.keys[:0]
	d.vals = d.vals[:0]
	d.ids = d.ids[:0]
	d.rawLat = d.rawLat[:0]
	d.rawLon = d.rawLon[:0]
	for p.more() {
		f, w, err := p.field()
		if err != nil {
			return err
		}
		switch f {
		case 1: // plain Node (rare)
			nb, err := p.bytes()
			if err != nil {
				return err
			}
			if o.Nodes != nil {
				if err := d.decodePlainNode(nb, gran, latOff, lonOff); err != nil {
					return err
				}
			}
		case 2: // DenseNodes
			db, err := p.bytes()
			if err != nil {
				return err
			}
			if o.Nodes != nil {
				if err := d.decodeDense(db); err != nil {
					return err
				}
			}
		case 3: // Way
			wb, err := p.bytes()
			if err != nil {
				return err
			}
			if o.Ways != nil {
				if err := d.decodeWay(wb); err != nil {
					return err
				}
			}
		default:
			if err := p.skip(w); err != nil {
				return err
			}
		}
	}
	if o.Ways != nil && len(d.ways) > 0 {
		o.Ways(d.ways)
	}
	if o.Nodes != nil && len(d.ids) > 0 {
		d.lats = d.lats[:0]
		d.lons = d.lons[:0]
		for i := range d.ids {
			d.lats = append(d.lats, int32((latOff+gran*d.rawLat[i])/100))
			d.lons = append(d.lons, int32((lonOff+gran*d.rawLon[i])/100))
		}
		o.Nodes(NodeBatch{IDs: d.ids, Lats: d.lats, Lons: d.lons})
	}
	return nil
}

func (d *blockDecoder) decodeDense(b []byte) error {
	p := pb{b: b}
	var accID, accLat, accLon int64
	for p.more() {
		f, w, err := p.field()
		if err != nil {
			return err
		}
		switch f {
		case 1:
			data, err := p.bytes()
			if err != nil {
				return err
			}
			if d.ids, accID, err = packedSint64Delta(data, d.ids, accID); err != nil {
				return err
			}
		case 8:
			data, err := p.bytes()
			if err != nil {
				return err
			}
			if d.rawLat, accLat, err = packedSint64Delta(data, d.rawLat, accLat); err != nil {
				return err
			}
		case 9:
			data, err := p.bytes()
			if err != nil {
				return err
			}
			if d.rawLon, accLon, err = packedSint64Delta(data, d.rawLon, accLon); err != nil {
				return err
			}
		default:
			if err := p.skip(w); err != nil {
				return err
			}
		}
	}
	if len(d.rawLat) != len(d.ids) || len(d.rawLon) != len(d.ids) {
		return fmt.Errorf("pbf: dense nodes arrays disagree (%d ids, %d lats, %d lons)",
			len(d.ids), len(d.rawLat), len(d.rawLon))
	}
	return nil
}

func (d *blockDecoder) decodePlainNode(b []byte, gran, latOff, lonOff int64) error {
	p := pb{b: b}
	var id, rawLat, rawLon int64
	for p.more() {
		f, w, err := p.field()
		if err != nil {
			return err
		}
		switch f {
		case 1, 8, 9:
			u, err := p.varint()
			if err != nil {
				return err
			}
			switch f {
			case 1:
				id = zigzag(u)
			case 8:
				rawLat = zigzag(u)
			case 9:
				rawLon = zigzag(u)
			}
		default:
			if err := p.skip(w); err != nil {
				return err
			}
		}
	}
	d.ids = append(d.ids, id)
	d.rawLat = append(d.rawLat, rawLat)
	d.rawLon = append(d.rawLon, rawLon)
	return nil
}

func (d *blockDecoder) decodeWay(b []byte) error {
	p := pb{b: b}
	w := Way{Tags: Tags{st: &d.st}}
	refStart := len(d.refs)
	keyStart, valStart := len(d.keys), len(d.vals)
	var accRef int64
	for p.more() {
		f, wire, err := p.field()
		if err != nil {
			return err
		}
		switch f {
		case 1:
			u, err := p.varint()
			if err != nil {
				return err
			}
			w.ID = int64(u)
		case 2:
			data, err := p.bytes()
			if err != nil {
				return err
			}
			if d.keys, err = packedUint32(data, d.keys); err != nil {
				return err
			}
		case 3:
			data, err := p.bytes()
			if err != nil {
				return err
			}
			if d.vals, err = packedUint32(data, d.vals); err != nil {
				return err
			}
		case 8:
			data, err := p.bytes()
			if err != nil {
				return err
			}
			if d.refs, accRef, err = packedSint64Delta(data, d.refs, accRef); err != nil {
				return err
			}
		default:
			if err := p.skip(wire); err != nil {
				return err
			}
		}
	}
	w.Refs = d.refs[refStart:]
	w.Tags.keys = d.keys[keyStart:]
	w.Tags.vals = d.vals[valStart:]
	d.ways = append(d.ways, w)
	return nil
}
