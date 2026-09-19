package engine

import (
	"testing"
	"time"

	"gotransit/internal/config"
	"gotransit/internal/transit"
)

func TestConnectionMarginsAndForecastUncertainty(t *testing.T) {
	e := New(config.Default())
	tt := &transit.Timetable{TZ: time.UTC}
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name      string
		gap, walk float64
		wantSafe  bool
	}{
		{"one minute same stop", 60, 0, false},
		{"three minutes same stop", 180, 0, true},
		{"three minutes across road", 180, 30, true},
		{"three minutes long walk", 180, 150, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			incoming := &Leg{Realtime: true, Arrive: now.Add(5 * time.Minute)}
			outgoing := &Leg{Realtime: true, Depart: incoming.Arrive.Add(time.Duration(tc.gap) * time.Second)}
			p := e.ConnectionProbability(tt, outgoing, incoming, incoming.Arrive.Add(time.Duration(tc.walk)*time.Second), tc.walk, now)
			if (p >= 0.8) != tc.wantSafe {
				t.Fatalf("P=%f wantSafe=%v", p, tc.wantSafe)
			}
		})
	}
	in := &Leg{Realtime: true, Arrive: now.Add(5 * time.Minute)}
	out := &Leg{Realtime: true, Depart: in.Arrive.Add(3 * time.Minute)}
	near := e.ConnectionProbability(tt, out, in, in.Arrive, 0, now)
	in.Arrive = in.Arrive.Add(30 * time.Minute)
	out.Depart = out.Depart.Add(30 * time.Minute)
	far := e.ConnectionProbability(tt, out, in, in.Arrive, 0, now)
	if far >= near || far < 0.8 {
		t.Fatalf("bounded horizon effect: near=%f far=%f", near, far)
	}
	if got := e.ConnectionProbability(tt, &Leg{Depart: now.Add(time.Second)}, nil, now, 0, now); got != 1 {
		t.Fatalf("waiting at stop needs no margin: %f", got)
	}
}

func TestVehicleDistanceAffectsBothSidesOfConnection(t *testing.T) {
	e := New(config.Default())
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	tt := &transit.Timetable{TZ: time.UTC, TripIdx: map[string]uint32{"incoming": 0, "outgoing": 1}}
	overlay := &transit.RTOverlay{VehPos: []int16{-1, -1}, VehLat: []int32{419000000, 419000000}, VehLon: []int32{125000000, 125000000}, VehStatus: []int8{2, 2}}
	tt.SetRT(overlay)
	incoming := &Leg{TripID: "incoming", Realtime: true, Arrive: now.Add(5 * time.Minute), To: Place{Lat: 41.9, Lon: 12.5}}
	outgoing := &Leg{TripID: "outgoing", Realtime: true, Depart: incoming.Arrive.Add(3 * time.Minute), From: incoming.To}
	near := e.ConnectionProbability(tt, outgoing, incoming, incoming.Arrive, 0, now)
	for _, trip := range []int{0, 1} {
		overlay.VehLon[trip] = 125600000
		far := e.ConnectionProbability(tt, outgoing, incoming, incoming.Arrive, 0, now)
		if far >= near || near-far > 0.1 {
			t.Fatalf("distance effect for trip %d: near=%f far=%f", trip, near, far)
		}
		overlay.VehLon[trip] = 125000000
	}
}
