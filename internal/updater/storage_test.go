package updater

import (
	"io"
	"log/slog"
	"os"
	"testing"

	"gotransit/internal/config"
	"gotransit/internal/engine"
)

func TestCachedFeedDoesNotRetainDuplicateHeapBytes(t *testing.T) {
	cfg := config.Default()
	cfg.Cache.Dir = t.TempDir()
	cfg.Feeds = []config.Feed{{Name: "demo", URL: "https://example.test/demo.zip"}}
	u := New(engine.New(cfg), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	data := []byte("zip payload")
	if err := u.cache.StoreBytes("gtfs-demo.zip", cfg.Feeds[0].URL, data, "etag", "", "sha"); err != nil {
		t.Fatal(err)
	}
	path, ok := u.cache.FilePath("gtfs-demo.zip")
	if !ok {
		t.Fatal("cached payload missing")
	}
	u.InstallFeedFile("demo", path, CondResult{ETag: "etag", SHA256: "sha"})
	u.mu.Lock()
	fd := u.feeds["demo"]
	retained, retainedCap, storedPath := len(fd.zip), cap(fd.zip), fd.path
	u.mu.Unlock()
	if retained != 0 || retainedCap != 0 {
		t.Fatalf("cached ZIP retained in heap: len=%d cap=%d", retained, retainedCap)
	}
	info, err := os.Stat(storedPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != int64(len(data)) {
		t.Fatalf("file bytes=%d, want %d", info.Size(), len(data))
	}
}
