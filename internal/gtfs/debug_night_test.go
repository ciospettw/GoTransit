npackage gtfs

import (
	"os"
	"testing"
)

// Hunt for non-monotonic or absurd times surviving the parse fix-ups.
func TestNightTripTimes(t *testing.T) {
	if _, err := os.Stat("../../data/gtfs-roma.zip"); err != nil {
		t.Skip("no data")
	}
	f, err := Load("../../data/gtfs-roma.zip", "roma")
	if err != nil {
		t.Fatal(err)
	}
	violations, huge := 0, 0
	var firstBad int = -1
	for tr := range f.Trips {
		lo, hi := f.TripSTOff[tr], f.TripSTOff[tr+1]
		for i := lo; i < hi; i++ {
			if f.STArr[i] > 200000 || f.STDep[i] > 200000 { // > ~55h: absurd
				huge++
				if firstBad < 0 {
					firstBad = tr
				}
			}
			if i > lo && f.STArr[i] < f.STDep[i-1] {
				violations++
				if firstBad < 0 {
					firstBad = tr
				}
			}
		}
	}
	t.Logf("violations=%d huge=%d", violations, huge)
	if firstBad >= 0 {
		lo, hi := f.TripSTOff[firstBad], f.TripSTOff[firstBad+1]
		t.Logf("first bad trip %s (route %s):", f.Trips[firstBad].ID, f.Routes[f.Trips[firstBad].RouteIdx].Short)
		for i := lo; i < hi && i < lo+40; i++ {
			t.Logf("  stop %s arr %d dep %d", f.Stops[f.STStop[i]].ID, f.STArr[i], f.STDep[i])
		}
	}
}
