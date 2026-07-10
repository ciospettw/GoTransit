package tests

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gotransit/internal/graph"
	"gotransit/internal/gtfs"
	"gotransit/internal/osm"
	"gotransit/internal/rt"
	"gotransit/internal/transit"
)

// Real-data suite. Point GOTRANSIT_TEST_DATA at a directory containing:
//
//	osm.pbf          (e.g. Geofabrik centro-latest.osm.pbf)
//	gtfs-roma.zip    gtfs-cotral.zip    [gtfs-trenitalia.zip]
//	rt/*.pb          (optional captured GTFS-RT frames)
//
// Everything here skips cleanly when the directory is absent.

var real struct {
	once  sync.Once
	g     *graph.Graph
	src   *graph.SrcData
	feeds []*gtfs.Feed
	tt    *transit.Timetable
	err   error
}

func loadReal(t *testing.T) {
	t.Helper()
	dir := testDataDir()
	if dir == "" {
		t.Skip("set GOTRANSIT_TEST_DATA to run the real-data suite")
	}
	real.once.Do(func() {
		pbf := filepath.Join(dir, "osm.pbf")
		if _, err := os.Stat(pbf); err != nil {
			real.err = fmt.Errorf("missing %s", pbf)
			return
		}
		g, src, st, err := graph.BuildFromPBF(pbf, 0)
		if err != nil {
			real.err = err
			return
		}
		fmt.Println("  " + st.String())
		real.g, real.src = g, src
		for _, name := range []string{"roma", "cotral", "trenitalia"} {
			zp := filepath.Join(dir, "gtfs-"+name+".zip")
			if _, err := os.Stat(zp); err != nil {
				continue
			}
			f, err := gtfs.Load(zp, name)
			if err != nil {
				real.err = err
				return
			}
			fmt.Println("  " + f.LoadStats)
			real.feeds = append(real.feeds, f)
		}
		if len(real.feeds) == 0 {
			real.err = fmt.Errorf("no gtfs-*.zip in %s", dir)
			return
		}
		real.tt, _, real.err = transit.Compile(real.feeds, g, 4.8, 400, 300)
	})
	if real.err != nil {
		t.Fatal(real.err)
	}
}

func TestRealPBFScan(t *testing.T) {
	dir := testDataDir()
	if dir == "" {
		t.Skip("set GOTRANSIT_TEST_DATA")
	}
	var nodes, hw atomic.Int64
	start := time.Now()
	hdr, err := osm.ScanPBF(filepath.Join(dir, "osm.pbf"), osm.ScanOpts{
		Nodes: func(nb osm.NodeBatch) { nodes.Add(int64(len(nb.IDs))) },
		Ways: func(ws []osm.Way) {
			for _, w := range ws {
				if w.Tags.Get("highway") != nil {
					hw.Add(1)
				}
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("scan: %d nodes, %d highway ways in %v (replication seq %d)",
		nodes.Load(), hw.Load(), time.Since(start), hdr.ReplicationSeq)
	if nodes.Load() < 1_000_000 || hw.Load() < 100_000 {
		t.Error("implausibly small extract")
	}
}

func TestRealRoadRouting(t *testing.T) {
	loadReal(t)
	g := real.g
	snF, ok1 := g.SnapPoint(418957000, 124825000, graph.ModeCar, 500) // Piazza Venezia
	snT, ok2 := g.SnapPoint(437731000, 112558000, graph.ModeCar, 500) // Firenze Duomo
	if !ok1 || !ok2 {
		t.Skip("route endpoints outside this extract")
	}
	rs := graph.NewRoadSearch(g.NumNodes())
	t0 := time.Now()
	res := rs.Route(g, []graph.Seed{{Node: snF.V}, {Node: snF.U}},
		[]graph.Seed{{Node: snT.U}, {Node: snT.V}}, 437731000, 112558000, graph.ModeCar, 0, 1.2, 8<<20)
	if !res.Found {
		t.Fatal("no car route Roma→Firenze")
	}
	km := graph.PathMeters(g, res.Edges) / 1000
	t.Logf("car Roma→Firenze: %.0f km, %.0f min, settled %d, %v",
		km, float64(res.Ds)/600, res.Settled, time.Since(t0))
	if km < 230 || km > 320 {
		t.Errorf("implausible distance %.0f km", km)
	}
	steps := graph.Steps(g, res.Edges, graph.ModeCar, 0)
	if len(steps) < 10 {
		t.Errorf("only %d turn-by-turn steps", len(steps))
	}
}

func TestRealTimetableAndRaptor(t *testing.T) {
	loadReal(t)
	tt := real.tt
	t.Logf("timetable: %d stops, %d patterns, %d trips, %d excluded by coverage",
		tt.NumStops(), tt.NumPatterns(), tt.NumTrips(), tt.Excluded.Trips)

	// stop_times must be monotonic after parsing fix-ups
	bad := 0
	for _, f := range real.feeds {
		for tr := 0; tr < len(f.Trips) && bad < 5; tr++ {
			lo, hi := f.TripSTOff[tr], f.TripSTOff[tr+1]
			for i := lo + 1; i < hi; i++ {
				if f.STArr[i] < f.STDep[i-1] {
					bad++
				}
			}
		}
	}
	if bad > 0 {
		t.Errorf("%d non-monotonic stop_times survived parsing", bad)
	}

	// Termini → EUR on a weekday morning
	seed := func(lat, lon int32, dep bool) []transit.StopSeed {
		var out []transit.StopSeed
		for s := 0; s < tt.NumStops() && len(out) < 40; s++ {
			dLat := float64(lat-tt.StopLat[s]) / 90
			dLon := float64(lon-tt.StopLon[s]) / 90
			if dLat*dLat+dLon*dLon < 250000 { // ~500m box-ish
				sec := uint32(0)
				if dep {
					sec = 8*3600 + 30*60
				}
				out = append(out, transit.StopSeed{Stop: int32(s), Sec: sec + 60})
			}
		}
		return out
	}
	q := transit.Query{
		Sources: seed(419009000, 125013000, true), Targets: seed(418385000, 124675000, false),
		Date: 20260710, Weekday: time.Friday, PrevDate: 20260709, PrevWeekday: time.Thursday,
		MaxTransfers: 4, SlackSec: 90,
	}
	if len(q.Sources) == 0 || len(q.Targets) == 0 {
		t.Skip("query stops outside this dataset")
	}
	r := transit.NewRaptor(tt)
	js := r.Plan(q)
	t0 := time.Now()
	for i := 0; i < 20; i++ {
		js = r.Plan(q)
	}
	t.Logf("RAPTOR Termini→EUR: %d journeys, %v/query", len(js), time.Since(t0)/20)
	if len(js) == 0 {
		t.Error("no journeys")
	}
	for _, j := range js {
		if j.ArrSec < 8*3600+30*60 {
			t.Errorf("journey arrives before it departs: %s", hhmm(j.ArrSec))
		}
	}
}

func TestRealStoreAndOSCApply(t *testing.T) {
	loadReal(t)
	blob, err := real.src.EncodeStore()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("graph source blob: %.0f MB in RAM", float64(len(blob))/1e6)
	src2, err := graph.DecodeStore(blob)
	if err != nil {
		t.Fatal(err)
	}
	seq, url := src2.Replication()
	if seq <= 0 || url == "" {
		t.Skip("extract carries no replication info")
	}
	// apply the diff matching our sequence (idempotent on our extraction):
	// the exact network path the live updater runs
	data, err := fetchBytes(fmt.Sprintf("%s/%s.osc.gz", url, osm.SeqPath(seq)))
	if err != nil {
		t.Skipf("network unavailable: %v", err)
	}
	ch, err := osm.ParseOSCGz(bytesReader(data))
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	src3, ast := src2.ApplyChange(ch)
	g2 := graph.Assemble(src3, &graph.BuildStats{})
	t.Logf("%s; reassembled in %v total", ast.String(), time.Since(t0))
	if g2.NumNodes() < real.g.NumNodes()/2 {
		t.Error("graph shrank implausibly after diff")
	}
}

func TestRealRTDecode(t *testing.T) {
	dir := testDataDir()
	if dir == "" {
		t.Skip("set GOTRANSIT_TEST_DATA")
	}
	files, _ := filepath.Glob(filepath.Join(dir, "rt", "*.pb"))
	if len(files) == 0 {
		t.Skip("no rt/*.pb captures")
	}
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		f, err := rt.Decode(data)
		if err != nil {
			t.Fatalf("%s: %v", filepath.Base(path), err)
		}
		t.Logf("%s: %d trip updates, %d vehicles, ts age %v",
			filepath.Base(path), len(f.Trips), len(f.Vehicles),
			time.Since(time.Unix(int64(f.Timestamp), 0)).Round(time.Second))
		if len(f.Trips)+len(f.Vehicles) == 0 {
			t.Errorf("%s decoded empty", filepath.Base(path))
		}
	}
}

func fetchBytes(url string) ([]byte, error) {
	c := &http.Client{Timeout: 2 * time.Minute}
	resp, err := c.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 64<<20))
}

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }
