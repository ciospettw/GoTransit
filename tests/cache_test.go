package tests

import (
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"

	"gotransit/internal/updater"
)

// TestCacheRoundTrip: payload + validators survive a store/load cycle, and a
// URL change invalidates the meta (never serve a cached copy of another source).
func TestCacheRoundTrip(t *testing.T) {
	c := updater.Cache{Dir: t.TempDir()}
	if err := c.StoreBytes("gtfs-x.zip", "http://a/x.zip", []byte("zipdata"), `"etag1"`, "lm1", "sha1"); err != nil {
		t.Fatal(err)
	}
	data, ok := c.LoadBytes("gtfs-x.zip")
	if !ok || string(data) != "zipdata" {
		t.Fatalf("payload perso: %q ok=%v", data, ok)
	}
	meta, ok := c.Meta("gtfs-x.zip", "http://a/x.zip")
	if !ok || meta.ETag != `"etag1"` || meta.LastMod != "lm1" || meta.SHA256 != "sha1" {
		t.Fatalf("meta inattesa: %+v ok=%v", meta, ok)
	}
	if _, ok := c.Meta("gtfs-x.zip", "http://b/other.zip"); ok {
		t.Fatal("meta valida per un URL diverso")
	}
	// disabled cache: everything is a no-op miss
	var off updater.Cache
	if off.Enabled() {
		t.Fatal("cache vuota dichiarata abilitata")
	}
	if _, ok := off.LoadBytes("gtfs-x.zip"); ok {
		t.Fatal("cache disabilitata ha restituito dati")
	}
}

// TestFetchToTempCond304: with matching validators the server answers 304 and
// nothing is downloaded; without them the payload lands in a temp file that
// AdoptFile moves into the cache.
func TestFetchToTempCond304(t *testing.T) {
	var gets atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		gets.Add(1)
		w.Header().Set("ETag", `"v1"`)
		w.Write([]byte("pbf-bytes"))
	}))
	defer srv.Close()

	// cold: full download
	tmp, n, changed, etag, _, err := updater.FetchToTempCond(srv.URL, false, "", "")
	if err != nil || !changed || n != int64(len("pbf-bytes")) || etag != `"v1"` {
		t.Fatalf("cold fetch: n=%d changed=%v etag=%q err=%v", n, changed, etag, err)
	}
	c := updater.Cache{Dir: t.TempDir()}
	cached, err := c.AdoptFile("osm.pbf", srv.URL, tmp, etag, "")
	if err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(cached); string(data) != "pbf-bytes" {
		t.Fatalf("file adottato corrotto: %q", data)
	}

	// warm: revalidation hits 304, no new download
	before := gets.Load()
	_, _, changed, _, _, err = updater.FetchToTempCond(srv.URL, false, `"v1"`, "")
	if err != nil || changed {
		t.Fatalf("atteso 304: changed=%v err=%v", changed, err)
	}
	if gets.Load() != before {
		t.Fatal("il server ha servito un download completo nonostante il 304")
	}
	if p, ok := c.FilePath("osm.pbf"); !ok || p != cached {
		t.Fatalf("file di cache sparito: %q ok=%v", p, ok)
	}
}
