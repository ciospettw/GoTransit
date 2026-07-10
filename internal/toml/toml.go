// Package toml implements the subset of TOML that a human configuration
// file actually needs: tables, arrays of tables, strings, integers, floats,
// booleans, arrays and comments. Zero dependencies, clear line-numbered
// errors. Unsupported TOML constructs fail loudly instead of misparsing.
package toml

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Table is a parsed TOML table: keys map to string, int64, float64, bool,
// []any, *Table or []*Table (array of tables).
type Table struct {
	m map[string]any
}

func newTable() *Table { return &Table{m: make(map[string]any)} }

// Parse parses TOML source.
func Parse(src []byte) (*Table, error) {
	p := &parser{root: newTable()}
	p.cur = p.root
	lines := strings.Split(string(src), "\n")
	for i, raw := range lines {
		p.line = i + 1
		if err := p.parseLine(raw); err != nil {
			return nil, err
		}
	}
	return p.root, nil
}

type parser struct {
	root *Table
	cur  *Table
	line int
}

func (p *parser) errf(format string, a ...any) error {
	return fmt.Errorf("toml: line %d: %s", p.line, fmt.Sprintf(format, a...))
}

func (p *parser) parseLine(raw string) error {
	s := strings.TrimSpace(stripComment(raw))
	if s == "" {
		return nil
	}
	switch {
	case strings.HasPrefix(s, "[["): // array of tables
		name := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(s, "[["), "]]"))
		if name == "" || !strings.HasSuffix(s, "]]") {
			return p.errf("malformed table array header %q", s)
		}
		t, err := p.descend(p.root, name, true)
		if err != nil {
			return err
		}
		p.cur = t
	case strings.HasPrefix(s, "["):
		name := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(s, "["), "]"))
		if name == "" || !strings.HasSuffix(s, "]") {
			return p.errf("malformed table header %q", s)
		}
		t, err := p.descend(p.root, name, false)
		if err != nil {
			return err
		}
		p.cur = t
	default:
		eq := indexTopLevel(s, '=')
		if eq < 0 {
			return p.errf("expected key = value, got %q", s)
		}
		key := strings.TrimSpace(s[:eq])
		key = strings.Trim(key, `"'`)
		if key == "" {
			return p.errf("empty key")
		}
		if strings.Contains(key, ".") {
			return p.errf("dotted keys are not supported, use a [table] header")
		}
		val, err := p.parseValue(strings.TrimSpace(s[eq+1:]))
		if err != nil {
			return err
		}
		if _, dup := p.cur.m[key]; dup {
			return p.errf("duplicate key %q", key)
		}
		p.cur.m[key] = val
	}
	return nil
}

// descend resolves a possibly dotted table name, creating tables on the way.
func (p *parser) descend(t *Table, name string, arr bool) (*Table, error) {
	parts := strings.Split(name, ".")
	for i, part := range parts {
		part = strings.TrimSpace(strings.Trim(part, `"'`))
		if part == "" {
			return nil, p.errf("empty table name component in %q", name)
		}
		last := i == len(parts)-1
		cur, ok := t.m[part]
		if last && arr {
			if !ok {
				nt := newTable()
				t.m[part] = []*Table{nt}
				return nt, nil
			}
			ts, isArr := cur.([]*Table)
			if !isArr {
				return nil, p.errf("%q is already a value, cannot extend as table array", part)
			}
			nt := newTable()
			t.m[part] = append(ts, nt)
			return nt, nil
		}
		if !ok {
			nt := newTable()
			t.m[part] = nt
			t = nt
			continue
		}
		switch v := cur.(type) {
		case *Table:
			t = v
		case []*Table:
			t = v[len(v)-1] // sub-tables of the most recent array element
		default:
			return nil, p.errf("%q is already a value, cannot use as table", part)
		}
	}
	return t, nil
}

func (p *parser) parseValue(s string) (any, error) {
	if s == "" {
		return nil, p.errf("missing value")
	}
	switch {
	case strings.HasPrefix(s, `"""`) || strings.HasPrefix(s, "'''"):
		return nil, p.errf("multi-line strings are not supported")
	case s[0] == '"':
		return p.parseBasicString(s)
	case s[0] == '\'':
		if len(s) < 2 || s[len(s)-1] != '\'' {
			return nil, p.errf("unterminated literal string")
		}
		return s[1 : len(s)-1], nil
	case s[0] == '[':
		return p.parseArray(s)
	case s[0] == '{':
		return nil, p.errf("inline tables are not supported, use a [table] header")
	case s == "true":
		return true, nil
	case s == "false":
		return false, nil
	}
	// number
	num := strings.ReplaceAll(s, "_", "")
	if i, err := strconv.ParseInt(num, 0, 64); err == nil {
		return i, nil
	}
	if f, err := strconv.ParseFloat(num, 64); err == nil {
		return f, nil
	}
	return nil, p.errf("cannot parse value %q (strings must be quoted)", s)
}

func (p *parser) parseBasicString(s string) (string, error) {
	var b strings.Builder
	i := 1
	for i < len(s) {
		c := s[i]
		switch c {
		case '"':
			if i != len(s)-1 {
				return "", p.errf("trailing characters after string")
			}
			return b.String(), nil
		case '\\':
			i++
			if i >= len(s) {
				return "", p.errf("dangling escape")
			}
			switch s[i] {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case '"':
				b.WriteByte('"')
			case '\\':
				b.WriteByte('\\')
			case 'u', 'U':
				n := 4
				if s[i] == 'U' {
					n = 8
				}
				if i+n >= len(s) {
					return "", p.errf("truncated unicode escape")
				}
				v, err := strconv.ParseUint(s[i+1:i+1+n], 16, 32)
				if err != nil {
					return "", p.errf("bad unicode escape")
				}
				b.WriteRune(rune(v))
				i += n
			default:
				return "", p.errf("unsupported escape \\%c", s[i])
			}
		default:
			b.WriteByte(c)
		}
		i++
	}
	return "", p.errf("unterminated string")
}

func (p *parser) parseArray(s string) ([]any, error) {
	if s[len(s)-1] != ']' {
		return nil, p.errf("arrays must open and close on one line")
	}
	inner := strings.TrimSpace(s[1 : len(s)-1])
	if inner == "" {
		return []any{}, nil
	}
	var out []any
	for _, part := range splitTopLevel(inner, ',') {
		part = strings.TrimSpace(part)
		if part == "" {
			continue // trailing comma
		}
		v, err := p.parseValue(part)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// stripComment removes a # comment, respecting quoted strings.
func stripComment(s string) string {
	inB, inL := false, false
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			if !inL && (i == 0 || s[i-1] != '\\') {
				inB = !inB
			}
		case '\'':
			if !inB {
				inL = !inL
			}
		case '#':
			if !inB && !inL {
				return s[:i]
			}
		}
	}
	return s
}

func indexTopLevel(s string, c byte) int {
	inB, inL := false, false
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			if !inL {
				inB = !inB
			}
		case '\'':
			if !inB {
				inL = !inL
			}
		default:
			if s[i] == c && !inB && !inL {
				return i
			}
		}
	}
	return -1
}

func splitTopLevel(s string, sep byte) []string {
	var parts []string
	depth, inB, inL, start := 0, false, false, 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			if !inL {
				inB = !inB
			}
		case '\'':
			if !inB {
				inL = !inL
			}
		case '[':
			if !inB && !inL {
				depth++
			}
		case ']':
			if !inB && !inL {
				depth--
			}
		default:
			if s[i] == sep && depth == 0 && !inB && !inL {
				parts = append(parts, s[start:i])
				start = i + 1
			}
		}
	}
	return append(parts, s[start:])
}

// ---- typed accessors -------------------------------------------------------

// Has reports whether a dotted path exists.
func (t *Table) Has(path string) bool {
	_, ok := t.lookup(path)
	return ok
}

func (t *Table) lookup(path string) (any, bool) {
	parts := strings.Split(path, ".")
	cur := any(t)
	for _, p := range parts {
		tab, ok := cur.(*Table)
		if !ok {
			return nil, false
		}
		cur, ok = tab.m[p]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// Str returns the string at path, or def.
func (t *Table) Str(path, def string) string {
	if v, ok := t.lookup(path); ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return def
}

// Int returns the integer at path, or def.
func (t *Table) Int(path string, def int64) int64 {
	if v, ok := t.lookup(path); ok {
		switch n := v.(type) {
		case int64:
			return n
		case float64:
			return int64(n)
		}
	}
	return def
}

// Float returns the float at path, or def. Integer values are widened.
func (t *Table) Float(path string, def float64) float64 {
	if v, ok := t.lookup(path); ok {
		switch n := v.(type) {
		case float64:
			return n
		case int64:
			return float64(n)
		}
	}
	return def
}

// Bool returns the boolean at path, or def.
func (t *Table) Bool(path string, def bool) bool {
	if v, ok := t.lookup(path); ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return def
}

// Dur returns a duration at path ("90s", "15m", "6h"), or def.
// Bare numbers are rejected: units are required for clarity.
func (t *Table) Dur(path string, def time.Duration) (time.Duration, error) {
	v, ok := t.lookup(path)
	if !ok {
		return def, nil
	}
	s, ok := v.(string)
	if !ok {
		return 0, fmt.Errorf("toml: %s: durations are strings with units, e.g. \"15m\"", path)
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("toml: %s: %v", path, err)
	}
	return d, nil
}

// Tables returns the array of tables at path ([[path]]), or nil.
func (t *Table) Tables(path string) []*Table {
	if v, ok := t.lookup(path); ok {
		if ts, ok := v.([]*Table); ok {
			return ts
		}
		if tt, ok := v.(*Table); ok {
			return []*Table{tt}
		}
	}
	return nil
}

// Table returns the sub-table at path, or an empty table.
func (t *Table) Table(path string) *Table {
	if v, ok := t.lookup(path); ok {
		if tt, ok := v.(*Table); ok {
			return tt
		}
	}
	return newTable()
}

// Keys returns the top-level keys of the table.
func (t *Table) Keys() []string {
	out := make([]string, 0, len(t.m))
	for k := range t.m {
		out = append(out, k)
	}
	return out
}

// Valid reports whether s is valid UTF-8 (helper for validation messages).
func Valid(s string) bool { return utf8.ValidString(s) }
