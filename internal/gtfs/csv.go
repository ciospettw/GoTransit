// Package gtfs parses GTFS static feeds. The CSV reader is hand-rolled:
// zero-copy field slices over the raw buffer, full RFC-4180 quoting, and a
// parallel fast path for huge unquoted tables (stop_times can be >200 MB).
package gtfs

import (
	"bytes"
	"fmt"
	"runtime"
	"sync"
)

// Table iterates a CSV buffer. Field slices alias the buffer.
type Table struct {
	data   []byte
	pos    int
	cols   map[string]int
	Fields [][]byte
	line   int
}

// NewTable parses the header line.
func NewTable(data []byte) (*Table, error) {
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF}) // BOM
	t := &Table{data: data, cols: map[string]int{}}
	if !t.Next() {
		return nil, fmt.Errorf("csv: empty file")
	}
	for i, f := range t.Fields {
		t.cols[string(bytes.TrimSpace(f))] = i
	}
	return t, nil
}

// Col returns the column index for a header name, or -1.
func (t *Table) Col(name string) int {
	if i, ok := t.cols[name]; ok {
		return i
	}
	return -1
}

// Field returns column i of the current record (nil when absent).
func (t *Table) Field(i int) []byte {
	if i < 0 || i >= len(t.Fields) {
		return nil
	}
	return t.Fields[i]
}

// Next advances to the next record. Handles quotes, CRLF, embedded newlines.
func (t *Table) Next() bool {
	for t.pos < len(t.data) {
		t.Fields = t.Fields[:0]
		t.pos = parseRecord(t.data, t.pos, &t.Fields)
		t.line++
		if len(t.Fields) == 1 && len(t.Fields[0]) == 0 {
			continue // blank line
		}
		return true
	}
	return false
}

// parseRecord parses one record starting at pos, appending fields.
// Returns the position after the record's newline.
func parseRecord(data []byte, pos int, fields *[][]byte) int {
	for {
		if pos < len(data) && data[pos] == '"' {
			// quoted field; unescape "" lazily only when present
			start := pos + 1
			i := start
			hasEsc := false
			for i < len(data) {
				if data[i] == '"' {
					if i+1 < len(data) && data[i+1] == '"' {
						hasEsc = true
						i += 2
						continue
					}
					break
				}
				i++
			}
			f := data[start:i]
			if hasEsc {
				f = bytes.ReplaceAll(f, []byte(`""`), []byte(`"`))
			}
			*fields = append(*fields, f)
			pos = i + 1 // past closing quote
			if pos < len(data) && data[pos] == ',' {
				pos++
				continue
			}
		} else {
			i := pos
			for i < len(data) && data[i] != ',' && data[i] != '\n' && data[i] != '\r' {
				i++
			}
			*fields = append(*fields, data[pos:i])
			pos = i
			if pos < len(data) && data[pos] == ',' {
				pos++
				continue
			}
		}
		// end of record
		if pos < len(data) && data[pos] == '\r' {
			pos++
		}
		if pos < len(data) && data[pos] == '\n' {
			pos++
		}
		return pos
	}
}

// ForEachParallel iterates data records (header line skipped) across all
// cores when the buffer contains no quotes (typical for stop_times);
// otherwise it falls back to sequential. The handler receives a worker id in
// [0, workers) and the record's fields; fields alias data. Records are NOT
// delivered in file order.
func ForEachParallel(data []byte, workers int, fn func(w int, fields [][]byte)) {
	if workers <= 0 {
		workers = runtime.NumCPU()
	}
	body := data[headerEnd(data):]
	if workers == 1 || len(body) < 1<<20 || bytes.IndexByte(body, '"') >= 0 {
		fields := make([][]byte, 0, 16)
		pos := 0
		for pos < len(body) {
			fields = fields[:0]
			pos = parseRecord(body, pos, &fields)
			if len(fields) == 1 && len(fields[0]) == 0 {
				continue
			}
			fn(0, fields)
		}
		return
	}
	// chunk at line boundaries
	bounds := make([]int, 0, workers+1)
	bounds = append(bounds, 0)
	for w := 1; w < workers; w++ {
		p := len(body) * w / workers
		if nl := bytes.IndexByte(body[p:], '\n'); nl >= 0 {
			p += nl + 1
		} else {
			p = len(body)
		}
		if p > bounds[len(bounds)-1] {
			bounds = append(bounds, p)
		}
	}
	bounds = append(bounds, len(body))

	var wg sync.WaitGroup
	for w := 0; w < len(bounds)-1; w++ {
		wg.Add(1)
		go func(w int, chunk []byte) {
			defer wg.Done()
			fields := make([][]byte, 0, 16)
			pos := 0
			for pos < len(chunk) {
				fields = fields[:0]
				pos = parseRecord(chunk, pos, &fields)
				if len(fields) == 1 && len(fields[0]) == 0 {
					continue
				}
				fn(w, fields)
			}
		}(w, body[bounds[w]:bounds[w+1]])
	}
	wg.Wait()
}

func headerEnd(data []byte) int {
	if nl := bytes.IndexByte(data, '\n'); nl >= 0 {
		return nl + 1
	}
	return len(data)
}

// ---- tiny numeric parsers (no allocations) -----------------------------------

// ParseUint parses a decimal, -1 on failure.
func ParseUint(b []byte) int {
	if len(b) == 0 {
		return -1
	}
	n := 0
	for _, c := range b {
		if c < '0' || c > '9' {
			return -1
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// ParseGTFSTime parses "H:MM:SS" / "HH:MM:SS" (hours may exceed 24).
// Returns seconds since service-day midnight, or -1.
func ParseGTFSTime(b []byte) int {
	// find the two colons
	c1 := bytes.IndexByte(b, ':')
	if c1 < 0 {
		return -1
	}
	c2 := bytes.IndexByte(b[c1+1:], ':')
	if c2 < 0 {
		return -1
	}
	c2 += c1 + 1
	h := ParseUint(bytes.TrimSpace(b[:c1]))
	m := ParseUint(b[c1+1 : c2])
	s := ParseUint(b[c2+1:])
	if h < 0 || m < 0 || s < 0 || m > 59 || s > 59 {
		return -1
	}
	return h*3600 + m*60 + s
}

// ParseCoordE7 parses a decimal degree ("41.895466") into E7 fixed point.
func ParseCoordE7(b []byte) (int32, bool) {
	if len(b) == 0 {
		return 0, false
	}
	neg := false
	i := 0
	if b[0] == '-' {
		neg = true
		i++
	} else if b[0] == '+' {
		i++
	}
	var ip int64
	for i < len(b) && b[i] != '.' {
		c := b[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		ip = ip*10 + int64(c-'0')
		i++
	}
	var fp int64
	digits := 0
	if i < len(b) && b[i] == '.' {
		i++
		for i < len(b) && digits < 7 {
			c := b[i]
			if c < '0' || c > '9' {
				return 0, false
			}
			fp = fp*10 + int64(c-'0')
			digits++
			i++
		}
		// round on the 8th digit
		if i < len(b) && b[i] >= '5' && b[i] <= '9' {
			fp++
		}
	}
	for ; digits < 7; digits++ {
		fp *= 10
	}
	v := ip*1e7 + fp
	if neg {
		v = -v
	}
	if v > 1800000000 || v < -1800000000 {
		return 0, false
	}
	return int32(v), true
}

// ParseDate parses YYYYMMDD into an int (0 on failure).
func ParseDate(b []byte) uint32 {
	if len(b) != 8 {
		return 0
	}
	n := ParseUint(b)
	if n < 19000101 || n > 21001231 {
		return 0
	}
	return uint32(n)
}
