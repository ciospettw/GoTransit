package track

import (
	"testing"
	"time"

	"gotransit/internal/engine"
)

func TestStreetTimeRemainingExcludesWaitBeforeDeparture(t *testing.T) {
	now := time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC)
	leg := &engine.Leg{Mode: "walk", DurationS: 600}
	lt := legTime{
		Depart: now.Add(5 * time.Minute),
		Arrive: now.Add(15 * time.Minute),
	}

	if got := streetTimeRemaining(leg, lt, true, true, now); got != 10*time.Minute {
		t.Fatalf("pre-departure remaining walk = %s, want 10m (waiting must be excluded)", got)
	}
	if got := streetTimeRemaining(leg, lt, true, true, lt.Depart.Add(4*time.Minute)); got != 6*time.Minute {
		t.Fatalf("in-progress remaining walk = %s, want 6m", got)
	}
	if got := streetTimeRemaining(leg, lt, true, true, lt.Arrive.Add(time.Second)); got != 0 {
		t.Fatalf("completed remaining walk = %s, want 0", got)
	}
}
