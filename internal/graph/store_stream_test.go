package graph

import (
	"bytes"
	"path/filepath"
	"testing"

	"gotransit/internal/osm"
)

func TestStoreStreamingMatchesMemoryFormat(t *testing.T) {
	src := &SrcData{
		bbox:    osm.BBox{MinLat: 1, MinLon: 2, MaxLat: 3, MaxLon: 4},
		replSeq: 42,
		replURL: "https://example.test/updates",
		names:   []string{"", "Main Street"},
	}
	blob, err := src.EncodeStore()
	if err != nil {
		t.Fatal(err)
	}
	if cap(blob) >= 1<<20 {
		t.Fatalf("small source received an oversized backing buffer: %d", cap(blob))
	}
	var streamed bytes.Buffer
	if err := src.WriteStore(&streamed); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(blob, streamed.Bytes()) {
		t.Fatal("streaming and in-memory stores differ")
	}
	path := filepath.Join(t.TempDir(), "source.gts")
	if err := src.SaveStore(path); err != nil {
		t.Fatal(err)
	}
	got, err := LoadStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.replSeq != src.replSeq || got.replURL != src.replURL || got.bbox != src.bbox || len(got.names) != len(src.names) {
		t.Fatalf("round trip mismatch: %+v", got)
	}
}
