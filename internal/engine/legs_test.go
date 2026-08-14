package engine

import (
	"math"
	"testing"

	"gotransit/internal/geo"
	"gotransit/internal/graph"
	"gotransit/internal/gtfs"
	"gotransit/internal/transit"
)

func TestStopConnectorPreservesCurvedPartialEdge(t *testing.T) {
	const (
		originLat = int32(419000000)
		originLon = int32(125000000)
		stopLat   = originLat - 28000
		stopLon   = originLon + 20000
	)
	g := graph.SyntheticNet(originLat, originLon)
	f := &gtfs.Feed{
		Name: "curve", TZ: "Europe/Rome",
		Stops: []gtfs.Stop{{ID: "S", Name: "Curved stop", Lat: stopLat, Lon: stopLon, OK: true}},
	}
	tt, _, err := transit.Compile([]*gtfs.Feed{f}, g, 4.8, 1000, 600)
	if err != nil {
		t.Fatal(err)
	}
	sn := tt.StopSnap[0]
	if sn.NodeU < 0 || sn.Edge < 0 || sn.AlongU <= 0 {
		t.Fatalf("incomplete compiled stop snap: %+v", sn)
	}
	fullLat, fullLon := g.AppendGeometry(uint32(sn.Edge), sn.NodeU, false, nil, nil)
	if len(fullLat) < 4 {
		t.Fatalf("fixture edge is not curved: %d geometry points", len(fullLat))
	}

	var enc geo.PolylineEncoder
	appendStopConnector(&enc, g, sn, sn.NodeU, stopLat, stopLon, false)
	lats, lons := geo.DecodePolyline(enc.String())
	if len(lats) < 4 {
		t.Fatalf("stop connector collapsed curved partial edge to a chord: %d points", len(lats))
	}
	if !containsE7(lats, lons, fullLat[1], fullLon[1], 100) {
		t.Fatalf("stop connector omitted the edge bend (%d,%d): %v %v",
			fullLat[1], fullLon[1], lats, lons)
	}

	polylineM := 0.0
	for i := 1; i < len(lats); i++ {
		polylineM += geo.Dist(lats[i-1], lons[i-1], lats[i], lons[i])
	}
	if math.Abs(polylineM-float64(sn.MetersU)) > 4 {
		t.Fatalf("curved connector polyline %.1fm disagrees with stored distance %dm", polylineM, sn.MetersU)
	}
}

func containsE7(lats, lons []int32, lat, lon, tolerance int32) bool {
	for i := range lats {
		if absE7(lats[i]-lat) <= tolerance && absE7(lons[i]-lon) <= tolerance {
			return true
		}
	}
	return false
}

func absE7(v int32) int32 {
	if v < 0 {
		return -v
	}
	return v
}
