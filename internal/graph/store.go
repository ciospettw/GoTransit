package graph

import (
	"bufio"
	"bytes"
	"compress/flate"
	"encoding/binary"
	"fmt"
	"io"
	"os"
)

// SrcData persistence: the compact graph source survives as one compressed
// blob (in RAM by default — the engine keeps no files) so live osmChange
// updates never re-read the PBF. Flate(1), varint and delta encoded — a
// fraction of the PBF's size, decodes in ~a second.

const storeMagic = "GTSS0002"

// EncodeStore serializes the source data into one compressed in-memory blob.
// In ephemeral mode this blob is all that survives the build: it lives in
// RAM (a fraction of the PBF's size) and feeds osmChange diff application.
func (src *SrcData) EncodeStore() ([]byte, error) {
	var buf bytes.Buffer
	buf.Grow(64 << 20)
	buf.WriteString(storeMagic)
	fw, _ := flate.NewWriter(&buf, flate.BestSpeed)
	w := &countWriter{w: fw}
	src.encode(w)
	if w.err != nil {
		return nil, w.err
	}
	if err := fw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// DecodeStore parses a blob produced by EncodeStore.
func DecodeStore(data []byte) (*SrcData, error) {
	if len(data) < len(storeMagic) || string(data[:len(storeMagic)]) != storeMagic {
		return nil, fmt.Errorf("store: bad magic (old or foreign blob)")
	}
	r := &countReader{r: bufio.NewReaderSize(flate.NewReader(bytes.NewReader(data[len(storeMagic):])), 1<<20)}
	src := &SrcData{}
	src.decode(r)
	if r.err != nil {
		return nil, fmt.Errorf("store: %w", r.err)
	}
	return src, nil
}

// SaveStore writes the blob to a file (tests and tooling; the server itself
// keeps no files).
func (src *SrcData) SaveStore(path string) error {
	data, err := src.EncodeStore()
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// LoadStore reads a file written by SaveStore.
func LoadStore(path string) (*SrcData, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return DecodeStore(data)
}

func (src *SrcData) encode(w *countWriter) {
	w.i64(int64(src.bbox.MinLat))
	w.i64(int64(src.bbox.MinLon))
	w.i64(int64(src.bbox.MaxLat))
	w.i64(int64(src.bbox.MaxLon))
	w.i64(src.replSeq)
	w.str(src.replURL)

	w.u64(uint64(len(src.names)))
	for _, n := range src.names {
		w.str(n)
	}

	w.u64(uint64(len(src.ways)))
	for i := range src.ways {
		wr := &src.ways[i]
		w.u64(uint64(wr.refCnt))
		w.b(wr.fwd)
		w.b(wr.bwd)
		w.b(wr.speed)
		w.u64(uint64(wr.nameIdx))
	}
	var prevID int64
	for _, id := range src.wayIDs {
		w.i64(id - prevID)
		prevID = id
	}
	w.u64(uint64(len(src.refs)))
	var prev int64
	for _, r := range src.refs {
		w.i64(r - prev)
		prev = r
	}

	w.u64(uint64(len(src.ids)))
	prev = 0
	for _, id := range src.ids {
		w.u64(uint64(id - prev)) // sorted: deltas are positive
		prev = id
	}
	var pla, plo int64
	for i := range src.ids {
		w.i64(int64(src.lats[i]) - pla)
		w.i64(int64(src.lons[i]) - plo)
		pla, plo = int64(src.lats[i]), int64(src.lons[i])
	}
	// route bitset
	for i := 0; i < len(src.route); i += 8 {
		var b byte
		for j := 0; j < 8 && i+j < len(src.route); j++ {
			if src.route[i+j] {
				b |= 1 << j
			}
		}
		w.b(b)
	}
}

func (src *SrcData) decode(r *countReader) {
	src.bbox.MinLat = int32(r.i64())
	src.bbox.MinLon = int32(r.i64())
	src.bbox.MaxLat = int32(r.i64())
	src.bbox.MaxLon = int32(r.i64())
	src.replSeq = r.i64()
	src.replURL = r.str()

	src.names = make([]string, r.u64())
	for i := range src.names {
		src.names[i] = r.str()
	}

	src.ways = make([]wayRec, r.u64())
	var refOff uint32
	for i := range src.ways {
		wr := &src.ways[i]
		wr.refOff = refOff
		wr.refCnt = uint32(r.u64())
		refOff += wr.refCnt
		wr.fwd = r.b()
		wr.bwd = r.b()
		wr.speed = r.b()
		wr.nameIdx = uint32(r.u64())
	}
	src.wayIDs = make([]int64, len(src.ways))
	var prevID int64
	for i := range src.wayIDs {
		prevID += r.i64()
		src.wayIDs[i] = prevID
	}
	src.refs = make([]int64, r.u64())
	var prev int64
	for i := range src.refs {
		prev += r.i64()
		src.refs[i] = prev
	}

	src.ids = make([]int64, r.u64())
	prev = 0
	for i := range src.ids {
		prev += int64(r.u64())
		src.ids[i] = prev
	}
	src.lats = make([]int32, len(src.ids))
	src.lons = make([]int32, len(src.ids))
	var pla, plo int64
	for i := range src.ids {
		pla += r.i64()
		plo += r.i64()
		src.lats[i] = int32(pla)
		src.lons[i] = int32(plo)
	}
	src.route = make([]bool, len(src.ids))
	for i := 0; i < len(src.route); i += 8 {
		b := r.b()
		for j := 0; j < 8 && i+j < len(src.route); j++ {
			src.route[i+j] = b&(1<<j) != 0
		}
	}
}

// ---- little varint stream helpers -------------------------------------------

type countWriter struct {
	w   io.Writer
	buf [10]byte
	err error
}

func (w *countWriter) b(v byte) {
	if w.err != nil {
		return
	}
	w.buf[0] = v
	_, w.err = w.w.Write(w.buf[:1])
}

func (w *countWriter) u64(v uint64) {
	if w.err != nil {
		return
	}
	n := binary.PutUvarint(w.buf[:], v)
	_, w.err = w.w.Write(w.buf[:n])
}

func (w *countWriter) i64(v int64) {
	if w.err != nil {
		return
	}
	n := binary.PutVarint(w.buf[:], v)
	_, w.err = w.w.Write(w.buf[:n])
}

func (w *countWriter) str(s string) {
	w.u64(uint64(len(s)))
	if w.err != nil {
		return
	}
	_, w.err = io.WriteString(w.w, s)
}

type countReader struct {
	r   io.ByteReader
	err error
}

func (r *countReader) b() byte {
	if r.err != nil {
		return 0
	}
	var v byte
	v, r.err = r.r.ReadByte()
	return v
}

func (r *countReader) u64() uint64 {
	if r.err != nil {
		return 0
	}
	var v uint64
	v, r.err = binary.ReadUvarint(r.r)
	return v
}

func (r *countReader) i64() int64 {
	if r.err != nil {
		return 0
	}
	var v int64
	v, r.err = binary.ReadVarint(r.r)
	return v
}

func (r *countReader) str() string {
	n := r.u64()
	if r.err != nil || n == 0 {
		return ""
	}
	if n > 1<<20 {
		r.err = fmt.Errorf("string too long (%d)", n)
		return ""
	}
	buf := make([]byte, n)
	br, ok := r.r.(io.Reader)
	if !ok {
		r.err = fmt.Errorf("reader cannot read strings")
		return ""
	}
	if _, err := io.ReadFull(br, buf); err != nil {
		r.err = err
		return ""
	}
	return string(buf)
}

// Bounds exposes the source coverage for validation and status reporting.
func (src *SrcData) Bounds() (minLat, minLon, maxLat, maxLon int32) {
	return src.bbox.MinLat, src.bbox.MinLon, src.bbox.MaxLat, src.bbox.MaxLon
}

// Replication exposes the osmosis position this source corresponds to.
func (src *SrcData) Replication() (seq int64, url string) { return src.replSeq, src.replURL }

// SetReplication records a new osmosis position after diffs are applied.
func (src *SrcData) SetReplication(seq int64) { src.replSeq = seq }
