package memrelease

import (
	"runtime"
	"runtime/debug"
	"testing"
	"time"
)

func TestAutomaticSettingsRespectProgrammaticOverrides(t *testing.T) {
	t.Setenv("GOGC", "")
	oldGC := debug.SetGCPercent(-1)
	defer debug.SetGCPercent(oldGC)
	if got := readGOGC(); got != -1 {
		t.Fatalf("GOGC=off reported as %d", got)
	}
	if automaticGC() {
		t.Fatal("programmatic GOGC override was ignored")
	}
	debug.SetGCPercent(73)

	t.Setenv("GOMEMLIMIT", "")
	const limit = int64(123 << 20)
	oldLimit := debug.SetMemoryLimit(limit)
	defer debug.SetMemoryLimit(oldLimit)
	configureMemoryLimit()
	if got := debug.SetMemoryLimit(-1); got != limit {
		t.Fatalf("programmatic GOMEMLIMIT changed to %d", got)
	}
}

func TestReleasePolicyScalesWithHeapAndChurn(t *testing.T) {
	now := time.Now()
	base := observation{at: now.Add(-interval), alloc: 100 << 20, gc: 10}
	stats := runtime.MemStats{HeapAlloc: 2 << 30, HeapIdle: 2 << 30, HeapReleased: 256 << 20, TotalAlloc: 200 << 20, NumGC: 11}
	last := now.Add(-minCooldown)
	if d := decide(base, now, stats, last, 0, true); !d.release {
		t.Fatalf("large stable idle heap was not eligible: %+v", d)
	}

	t.Run("small network", func(t *testing.T) {
		s := stats
		s.HeapAlloc = 16 << 20
		s.TotalAlloc = base.alloc
		s.HeapIdle, s.HeapReleased = 3<<20, 1<<20
		if d := decide(base, now, s, last, 0, true); !d.release || d.minimum != minUsefulRelease {
			t.Fatalf("small heaps must scale below fixed hundreds of MiB: %+v", d)
		}
	})

	t.Run("very large network caps relative threshold", func(t *testing.T) {
		s := stats
		s.HeapAlloc = 100 << 30
		s.HeapIdle, s.HeapReleased = 700<<20, 100<<20
		if d := decide(base, now, s, last, 0, true); !d.release || d.minimum != maxRelativeThreshold {
			t.Fatalf("large heap threshold did not cap: %+v", d)
		}
	})

	t.Run("rapid reuse defers scavenging", func(t *testing.T) {
		p := base
		s := stats
		s.TotalAlloc = p.alloc + 30*(1<<30)
		if d := decide(p, now, s, last, 0, true); d.release || d.minimum < 2<<30 {
			t.Fatalf("pages about to be reused should stay resident: %+v", d)
		}
	})

	for _, tc := range []struct {
		name    string
		pending bool
		last    time.Time
		busy    bool
	}{
		{"no collection", false, last, false},
		{"cooldown", true, now, false},
		{"busy", true, last, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.busy {
				busy.Add(1)
				defer busy.Add(-1)
			}
			if d := decide(base, now, stats, tc.last, 0, tc.pending); d.release {
				t.Fatalf("release should be deferred: %+v", d)
			}
		})
	}
}

func TestAdaptiveCooldownBoundsReleaseOverhead(t *testing.T) {
	if got := adaptiveCooldown(2*time.Millisecond, false); got != minCooldown {
		t.Fatalf("short release cooldown=%s", got)
	}
	if got := adaptiveCooldown(300*time.Millisecond, false); got != 5*time.Minute {
		t.Fatalf("measured release cooldown=%s", got)
	}
	if got := adaptiveCooldown(3*time.Second, false); got != maxCooldown {
		t.Fatalf("long release cooldown=%s", got)
	}
	if got := adaptiveCooldown(time.Second, true); got != pressureCooldown {
		t.Fatalf("pressure cooldown=%s", got)
	}
}

func TestAutomaticGOGCHasHysteresisAndBounds(t *testing.T) {
	if got := tuneGOGC(100, 0.001, false); got != 85 {
		t.Fatalf("cheap GC should reduce heap headroom, got %d", got)
	}
	if got := tuneGOGC(100, 0.01, false); got != 100 {
		t.Fatalf("GC inside CPU band changed target to %d", got)
	}
	if got := tuneGOGC(100, 0.03, false); got != 125 {
		t.Fatalf("expensive GC should increase headroom, got %d", got)
	}
	if got := tuneGOGC(minAutoGOGC, 0.001, true); got != minAutoGOGC {
		t.Fatalf("minimum GOGC violated: %d", got)
	}
}

func TestBudgetsScaleAndRespectPressure(t *testing.T) {
	if got := cacheBudgetFor(32<<30, 10<<30, 12<<30, false); got != 256<<20 {
		t.Fatalf("cache budget should be capped by headroom: %d", got)
	}
	if got := cacheBudgetFor(2<<30, 0, 0, true); got != 0 {
		t.Fatalf("pressure cache budget=%d", got)
	}
}
