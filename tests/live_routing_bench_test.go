package tests

import (
	"gotransit/internal/engine"
	"testing"
)

func BenchmarkLiveRoutingPlan(b *testing.B) {
	w := buildWorld(b, worldOpts{midStopShape: true})
	fLat, fLon, tLat, tLon := w.od()
	req := engine.Request{FromLat: fLat, FromLon: fLon, ToLat: tLat, ToLon: tLon, Mode: "transit", When: w.now, Num: 4}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := w.e.Plan(req); err != nil {
			b.Fatal(err)
		}
	}
}
