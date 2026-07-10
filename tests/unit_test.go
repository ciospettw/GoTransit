package tests

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gotransit/internal/api"
	"gotransit/internal/config"
	"gotransit/internal/geo"
	"gotransit/internal/gtfs"
	"gotransit/internal/osm"
	"gotransit/internal/toml"
)

// ---- TOML ------------------------------------------------------------------------

func TestTOML(t *testing.T) {
	src := `
# comment
data_dir = "./data"   # inline
threads = 8
factor = 1.5
verbose = true

[osm]
url = "https://example.com/x.pbf#frag"
poll = "6h"

[[gtfs]]
name = "roma"
[[gtfs]]
name = "cotral"
allow_insecure = true
`
	tab, err := toml.Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if tab.Str("data_dir", "") != "./data" || tab.Int("threads", 0) != 8 ||
		tab.Float("factor", 0) != 1.5 || !tab.Bool("verbose", false) {
		t.Error("scalar parsing broken")
	}
	if tab.Str("osm.url", "") != "https://example.com/x.pbf#frag" {
		t.Error("table lookup broken")
	}
	if d, err := tab.Dur("osm.poll", 0); err != nil || d != 6*time.Hour {
		t.Errorf("duration: %v %v", d, err)
	}
	feeds := tab.Tables("gtfs")
	if len(feeds) != 2 || feeds[1].Str("name", "") != "cotral" || !feeds[1].Bool("allow_insecure", false) {
		t.Error("array of tables broken")
	}
	for _, bad := range []string{`key = `, `key = {a=1}`, `a.b = 1`, "dup=1\ndup=2", `key = "unterminated`} {
		if _, err := toml.Parse([]byte(bad)); err == nil {
			t.Errorf("should reject %q", bad)
		}
	}
}

func TestConfigDefaultsAndValidation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gotransit.toml")
	os.WriteFile(path, []byte(`
[osm]
url = "https://download.geofabrik.de/europe/italy/centro-latest.osm.pbf"
[[gtfs]]
name = "roma"
url = "https://example.com/gtfs.zip"
`), 0o644)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.IsGeofabrik() || cfg.GeofabrikUpdatesURL() != "https://download.geofabrik.de/europe/italy/centro-updates/" {
		t.Error("geofabrik detection broken")
	}
	if cfg.Feeds[0].Poll != time.Minute || cfg.Routing.MaxTransfers != 4 || !cfg.DebugUI {
		t.Error("defaults broken")
	}
	// plain http requires the explicit flag
	os.WriteFile(path, []byte(`
[osm]
url = "https://x/x.osm.pbf"
[[gtfs]]
name = "a"
url = "http://insecure/gtfs.zip"
`), 0o644)
	if _, err := config.Load(path); err == nil {
		t.Error("plain http without allow_insecure must be rejected")
	}
}

// ---- geo -------------------------------------------------------------------------

func TestGeo(t *testing.T) {
	// Termini ↔ Colosseo ~1.4 km
	h := geo.Haversine(419009000, 125013000, 418902000, 124922000)
	if h < 1350 || h > 1500 {
		t.Errorf("haversine = %.0f", h)
	}
	if d := geo.Dist(419009000, 125013000, 418902000, 124922000); math.Abs(d-h) > 2 {
		t.Errorf("equirectangular off by %.1f m", math.Abs(d-h))
	}
	// polyline: Google's documented vector
	got := geo.EncodePolyline([]int32{385000000, 407000000, 432520000},
		[]int32{-1202000000, -1209500000, -1264530000})
	if got != "_p~iF~ps|U_ulLnnqC_mqNvxq`@" {
		t.Errorf("polyline = %q", got)
	}
	lats, lons := geo.DecodePolyline(got)
	if len(lats) != 3 || math.Abs(float64(lons[2]+1264530000)) > 100 {
		t.Error("polyline roundtrip broken")
	}
}

// ---- GTFS csv + parsers -------------------------------------------------------------

func TestCSVAndParsers(t *testing.T) {
	tab, err := gtfs.NewTable([]byte("a,b,c\r\n1,\"x,\"\"y\",3\n\"q\",w,\n"))
	if err != nil {
		t.Fatal(err)
	}
	var rows [][]string
	for tab.Next() {
		var r []string
		for i := 0; i < 3; i++ {
			r = append(r, string(tab.Field(i)))
		}
		rows = append(rows, r)
	}
	if len(rows) != 2 || rows[0][1] != `x,"y` || rows[1][0] != "q" {
		t.Errorf("csv rows = %v", rows)
	}
	if v := gtfs.ParseGTFSTime([]byte("25:01:00")); v != 25*3600+60 {
		t.Errorf("late time = %d", v)
	}
	if v, ok := gtfs.ParseCoordE7([]byte("41.12345678")); !ok || v != 411234568 {
		t.Errorf("coord rounding = %d", v)
	}
	if gtfs.ParseDate([]byte("20260710")) != 20260710 || gtfs.ParseDate([]byte("bad")) != 0 {
		t.Error("date parsing broken")
	}
}

// ---- OSM PBF: synthetic file through the real decoder -------------------------------

type pbenc struct{ b []byte }

func (e *pbenc) varint(v uint64) {
	for v >= 0x80 {
		e.b = append(e.b, byte(v)|0x80)
		v >>= 7
	}
	e.b = append(e.b, byte(v))
}
func (e *pbenc) tag(f, w int) { e.varint(uint64(f<<3 | w)) }
func (e *pbenc) bytesField(f int, d []byte) {
	e.tag(f, 2)
	e.varint(uint64(len(d)))
	e.b = append(e.b, d...)
}
func (e *pbenc) varintField(f int, v uint64) { e.tag(f, 0); e.varint(v) }
func zz(v int64) uint64                      { return uint64(v<<1) ^ uint64(v>>63) }
func packedZZ(vals []int64) []byte {
	var e pbenc
	var prev int64
	for _, v := range vals {
		e.varint(zz(v - prev))
		prev = v
	}
	return e.b
}
func packedU32(vals []uint32) []byte {
	var e pbenc
	for _, v := range vals {
		e.varint(uint64(v))
	}
	return e.b
}
func writeBlob(w *bytes.Buffer, btype string, payload []byte, compress bool) {
	var blob pbenc
	if compress {
		var zbuf bytes.Buffer
		zw := zlib.NewWriter(&zbuf)
		zw.Write(payload)
		zw.Close()
		blob.varintField(2, uint64(len(payload)))
		blob.bytesField(3, zbuf.Bytes())
	} else {
		blob.bytesField(1, payload)
	}
	var bh pbenc
	bh.bytesField(1, []byte(btype))
	bh.varintField(3, uint64(len(blob.b)))
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(bh.b)))
	w.Write(lenBuf[:])
	w.Write(bh.b)
	w.Write(blob.b)
}

func TestPBFDecoder(t *testing.T) {
	var out bytes.Buffer
	var bbox pbenc
	bbox.varintField(1, zz(12400000000))
	bbox.varintField(2, zz(12600000000))
	bbox.varintField(3, zz(42000000000))
	bbox.varintField(4, zz(41000000000))
	var hdr pbenc
	hdr.bytesField(1, bbox.b)
	hdr.varintField(33, 3901)
	writeBlob(&out, "OSMHeader", hdr.b, false)

	var st pbenc
	for _, s := range []string{"", "highway", "residential", "name", "Via Roma"} {
		st.bytesField(1, []byte(s))
	}
	var dense pbenc
	dense.bytesField(1, packedZZ([]int64{10, 11, 12}))
	dense.bytesField(8, packedZZ([]int64{415000000, 416000000, 417000000}))
	dense.bytesField(9, packedZZ([]int64{125000000, 125000000, 125000000}))
	var way pbenc
	way.varintField(1, 99)
	way.bytesField(2, packedU32([]uint32{1, 3}))
	way.bytesField(3, packedU32([]uint32{2, 4}))
	way.bytesField(8, packedZZ([]int64{10, 11, 12}))
	var grp pbenc
	grp.bytesField(2, dense.b)
	grp.bytesField(3, way.b)
	var blk pbenc
	blk.bytesField(1, st.b)
	blk.bytesField(2, grp.b)
	writeBlob(&out, "OSMData", blk.b, true)

	path := filepath.Join(t.TempDir(), "tiny.osm.pbf")
	os.WriteFile(path, out.Bytes(), 0o644)

	var mu sync.Mutex
	nodes := map[int64][2]int32{}
	type wayRec struct{ hw, name string }
	var ways []wayRec
	hdr2, err := osm.ScanPBF(path, osm.ScanOpts{
		Nodes: func(nb osm.NodeBatch) {
			mu.Lock()
			for i, id := range nb.IDs {
				nodes[id] = [2]int32{nb.Lats[i], nb.Lons[i]}
			}
			mu.Unlock()
		},
		Ways: func(ws []osm.Way) {
			mu.Lock()
			for _, w := range ws {
				ways = append(ways, wayRec{string(w.Tags.Get("highway")), string(w.Tags.Get("name"))})
			}
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if hdr2.ReplicationSeq != 3901 || !hdr2.HasBBox || hdr2.BBox.MinLat != 410000000 {
		t.Errorf("header = %+v", hdr2)
	}
	if c := nodes[11]; c[0] != 416000000 || c[1] != 125000000 {
		t.Errorf("node 11 = %v", c)
	}
	if len(ways) != 1 || ways[0].hw != "residential" || ways[0].name != "Via Roma" {
		t.Errorf("ways = %v", ways)
	}
}

// ---- osc parsing helpers -------------------------------------------------------------

func TestOSCStateAndPaths(t *testing.T) {
	st, err := osm.ParseStateTxt([]byte("# comment\ntimestamp=2026-07-08T20\\:21\\:31Z\nsequenceNumber=3901\n"))
	if err != nil || st.Sequence != 3901 {
		t.Fatalf("state = %+v err=%v", st, err)
	}
	if osm.SeqPath(3901) != "000/003/901" || osm.SeqPath(123456789) != "123/456/789" {
		t.Error("SeqPath broken")
	}
	ch, err := osm.ParseOSC(strings.NewReader(`<osmChange>
	  <modify><node id="5" lat="41.9" lon="12.5"/></modify>
	  <delete><way id="7"/></delete>
	  <create><way id="8"><nd ref="5"/><nd ref="6"/><tag k="highway" v="residential"/></way></create>
	</osmChange>`))
	if err != nil {
		t.Fatal(err)
	}
	if len(ch.NodeUpsert) != 1 || ch.NodeUpsert[0].Lat != 419000000 ||
		len(ch.WayDelete) != 1 || len(ch.WayUpsert) != 1 || ch.WayUpsert[0].Tags["highway"] != "residential" {
		t.Errorf("osc = %+v", ch)
	}
}

// ---- WebSocket layer ------------------------------------------------------------------

func TestWebSocketRoundtrip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := api.UpgradeWS(w, r)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer ws.Close()
		ws.SendText([]byte(`{"type":"hello"}`))
		msg, err := ws.ReadMessage(5 * time.Second)
		if err != nil {
			return
		}
		ws.SendText([]byte(`{"echo":"` + string(msg) + `"}`))
	}))
	defer srv.Close()

	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	key := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef"))
	conn.Write([]byte("GET /ws HTTP/1.1\r\nHost: x\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: " + key + "\r\nSec-WebSocket-Version: 13\r\n\r\n"))
	br := bufio.NewReader(conn)
	status, _ := br.ReadString('\n')
	if !strings.Contains(status, "101") {
		t.Fatalf("no 101: %q", status)
	}
	sum := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	want := base64.StdEncoding.EncodeToString(sum[:])
	acceptOK := false
	for {
		line, _ := br.ReadString('\n')
		if strings.TrimSpace(line) == "" {
			break
		}
		if strings.Contains(line, want) {
			acceptOK = true
		}
	}
	if !acceptOK {
		t.Fatal("bad Sec-WebSocket-Accept")
	}
	readFrame := func() string {
		var h [2]byte
		io.ReadFull(br, h[:])
		ln := int(h[1] & 0x7f)
		buf := make([]byte, ln)
		io.ReadFull(br, buf)
		return string(buf)
	}
	if got := readFrame(); got != `{"type":"hello"}` {
		t.Fatalf("hello = %q", got)
	}
	payload := []byte(`hi`)
	mask := [4]byte{1, 2, 3, 4}
	frame := append([]byte{0x81, byte(0x80 | len(payload))}, mask[:]...)
	for i, b := range payload {
		frame = append(frame, b^mask[i%4])
	}
	conn.Write(frame)
	if got := readFrame(); got != `{"echo":"hi"}` {
		t.Fatalf("echo = %q", got)
	}
}
