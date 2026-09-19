package stats

import "testing"

func TestForecastDistanceIsModestAndMonotonic(t *testing.T) {
	near := ForecastSigma(300, 100)
	far := ForecastSigma(300, 5000)
	if far <= near || far-near > 15 {
		t.Fatalf("distance coefficient near=%v far=%v", near, far)
	}
	if ForecastSigma(-600, 0) != 0 || ForecastSigma(100000, 100000) > 45 {
		t.Fatal("unbounded or negative forecast uncertainty")
	}
	c := Conn{SlackSec: 90, ArrSigma: 25, DepSigma: 25}
	base := Catch(c)
	c.ForecastSigma = far
	if got := Catch(c); got >= base || got < 0.8 {
		t.Fatalf("coefficient dominates transfer feasibility: before=%v after=%v", base, got)
	}
}
