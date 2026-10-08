package engine

import (
	"runtime/debug"
	"testing"

	"gotransit/internal/graph"
)

func TestScratchSurvivesIdleMemoryReleaseAndRemainsBounded(t *testing.T) {
	b := NewGraphBundle(&graph.Graph{NodeLat: make([]int32, 100)})
	b.near.max = func(uint64) int { return 2 }
	a, c, d := b.Near(), b.Near(), b.Near()
	b.PutNear(a)
	b.PutNear(c)
	b.PutNear(d)
	debug.FreeOSMemory()
	if b.Near() != c || b.Near() != a {
		t.Fatal("memory collection evicted reusable query state")
	}
	if len(b.near.idle) != 0 {
		t.Fatal("unexpected retained states")
	}
}

func TestScratchTrimsByByteBudgetAndPressure(t *testing.T) {
	b := NewGraphBundle(&graph.Graph{NodeLat: make([]int32, 100)})
	b.near.max = func(uint64) int { return 1 }
	a, c := b.Near(), b.Near()
	b.PutNear(a)
	b.PutNear(c)
	if len(b.near.idle) != 1 {
		t.Fatalf("idle states=%d, want 1", len(b.near.idle))
	}
	if dropped := b.near.trim(true); dropped != a.MemoryBytes() {
		t.Fatalf("dropped=%d, want %d", dropped, a.MemoryBytes())
	}
	if len(b.near.idle) != 0 {
		t.Fatal("pressure did not empty the idle cache")
	}
}
