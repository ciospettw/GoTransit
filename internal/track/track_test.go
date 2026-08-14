package track

import (
	"testing"
	"time"

	"gotransit/internal/config"
	"gotransit/internal/engine"
	"gotransit/internal/transit"
)

type discardSink struct{}

func (discardSink) Send(any) error { return nil }

func shiftedRailTimetable(base, scheduledDepart, realtimeDepart time.Time) *transit.Timetable {
	sec := func(at time.Time) uint32 { return uint32(at.Sub(base).Seconds()) }
	scheduledArrive := scheduledDepart.Add(10 * time.Minute)
	realtimeArrive := realtimeDepart.Add(10 * time.Minute)
	tt := &transit.Timetable{
		StopID:       []string{"station", "destination"},
		PatFirstStop: []uint32{0, 2}, PatStops: []int32{0, 1},
		PatFirstTrip: []uint32{0, 1}, PatRoute: []int32{0},
		TripService: []int32{0}, TripID: []string{"rail"},
		TripTimeOff: []uint32{0}, TripIdx: map[string]uint32{"test:rail": 0},
		Arr:    []uint32{sec(scheduledDepart), sec(scheduledArrive)},
		Dep:    []uint32{sec(scheduledDepart), sec(scheduledArrive)},
		Routes: []transit.RouteMeta{{Type: 1}}, TZ: time.UTC,
	}
	tt.SetRT(&transit.RTOverlay{
		TripOff: []int32{0}, Skip: []bool{false}, HasRT: []bool{true}, Passed: []int16{-1},
		ArrDelta: []int32{
			int32(sec(realtimeDepart)) - int32(sec(scheduledDepart)),
			int32(sec(realtimeArrive)) - int32(sec(scheduledArrive)),
		},
		DepDelta: []int32{
			int32(sec(realtimeDepart)) - int32(sec(scheduledDepart)),
			int32(sec(realtimeArrive)) - int32(sec(scheduledArrive)),
		},
		StopSkip: []bool{false, false},
		VehLat:   []int32{0}, VehLon: []int32{0}, VehPos: []int16{-1}, VehStatus: []int8{-1}, VehTime: []uint64{0},
	})
	return tt
}

func TestRefreshTimesCurrentWalkKeepsCountdownAnchor(t *testing.T) {
	start := time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC)
	s := session{
		it: engine.Itinerary{Legs: []engine.Leg{{
			Mode:      "walk",
			Depart:    start,
			Arrive:    start.Add(10 * time.Minute),
			DurationS: 600,
		}}},
	}

	first, feas := s.refreshTimes(nil, nil, start.Add(3*time.Minute))
	if !feas.ok || len(first) != 1 {
		t.Fatalf("first refresh = %#v, feasibility = %#v", first, feas)
	}
	second, feas := s.refreshTimes(nil, nil, start.Add(4*time.Minute))
	if !feas.ok || len(second) != 1 {
		t.Fatalf("second refresh = %#v, feasibility = %#v", second, feas)
	}
	if !first[0].Arrive.Equal(start.Add(10*time.Minute)) || !second[0].Arrive.Equal(first[0].Arrive) {
		t.Fatalf("walking ETA slid between ticks: first=%s second=%s", first[0].Arrive, second[0].Arrive)
	}
}

func TestBoardingLatchKeySurvivesCompatibleReroute(t *testing.T) {
	start := time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC)
	s := session{it: engine.Itinerary{Legs: []engine.Leg{{
		Mode: "transit", TripID: "feed:trip", Depart: start,
		From: engine.Place{StopID: "feed:stop"},
	}}}}
	want := s.boardingKey(0)

	s.it = engine.Itinerary{Legs: []engine.Leg{
		{Mode: "walk"},
		{
			Mode: "transit", TripID: "feed:trip", Depart: start.Add(4 * time.Minute),
			From: engine.Place{StopID: "feed:stop"},
		},
	}}
	if got := s.boardingKey(1); got != want {
		t.Fatalf("compatible reroute changed boarding latch key: before=%q after=%q", want, got)
	}
}

func TestScheduleRailEntryUsesDetectedArrivalAnchor(t *testing.T) {
	now := time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC)
	base := time.Date(2026, 8, 14, 0, 0, 0, 0, time.UTC)
	tt := shiftedRailTimetable(base, now.Add(2*time.Minute), now.Add(20*time.Second))
	cfg := config.Default()
	s := session{
		t: &Tracker{Cfg: cfg}, sink: discardSink{}, legIdx: 0,
		gps: newGPSState(), railReadyAt: map[int]time.Time{},
		it: engine.Itinerary{Legs: []engine.Leg{
			{
				Mode: "walk", Depart: now.Add(-time.Minute), Arrive: now.Add(time.Minute),
				To: engine.Place{Lat: 41.9, Lon: 12.5},
			},
			{
				Mode: "transit", TripID: "test:rail", Depart: now.Add(2 * time.Minute),
				Arrive: now.Add(10 * time.Minute), Route: &engine.RouteRef{Type: 1},
				From: engine.Place{StopID: "station"}, To: engine.Place{StopID: "destination"},
			},
		}},
	}
	s.gps.update(Fix{Lat: 41.9, Lon: 12.5, AccuracyM: 5, At: now}, now)
	s.gps.nearStopSince = now.Add(-atStopHold)

	s.advance(tt, tt.RT(), now)
	if s.legIdx != 1 || s.boarded {
		t.Fatalf("late station arrival advanced to boarded rail: leg=%d boarded=%v", s.legIdx, s.boarded)
	}
	if got, want := s.railReadyAt[1], now.Add(time.Minute); !got.Equal(want) {
		t.Fatalf("rail entry anchor = %s, want %s", got, want)
	}
	_, feas := s.refreshTimes(tt, tt.RT(), now)
	if feas.ok || feas.reason != "missed_connection" {
		t.Fatalf("20-second rail entry remained feasible: %#v", feas)
	}
}

func TestScheduleRailTransferKeepsNinetySecondArrivalAnchor(t *testing.T) {
	arrived := time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC)
	base := time.Date(2026, 8, 14, 0, 0, 0, 0, time.UTC)
	tt := shiftedRailTimetable(base, arrived.Add(2*time.Minute), arrived.Add(30*time.Second))
	cfg := config.Default()
	s := session{
		t: &Tracker{Cfg: cfg}, sink: discardSink{}, legIdx: 0, boarded: true,
		gps: newGPSState(), railReadyAt: map[int]time.Time{},
		it: engine.Itinerary{Legs: []engine.Leg{
			{
				Mode: "transit", TripID: "missing:incoming", Depart: arrived.Add(-10 * time.Minute),
				Arrive: arrived, Route: &engine.RouteRef{Type: 1},
				From: engine.Place{StopID: "origin"}, To: engine.Place{StopID: "platform-a"},
			},
			{
				Mode: "walk", Depart: arrived, Arrive: arrived.Add(10 * time.Second), DurationS: 10,
				From: engine.Place{StopID: "platform-a"}, To: engine.Place{StopID: "platform-b"},
			},
			{
				Mode: "transit", TripID: "test:rail", Depart: arrived.Add(2 * time.Minute),
				Arrive: arrived.Add(10 * time.Minute), Route: &engine.RouteRef{Type: 2},
				From: engine.Place{StopID: "station"}, To: engine.Place{StopID: "destination"},
			},
		}},
	}
	s.completeAlight(tt, nil, arrived, arrived, "schedule_assumed")
	if got, want := s.railReadyAt[2], arrived.Add(90*time.Second); !got.Equal(want) {
		t.Fatalf("rail transfer anchor = %s, want %s", got, want)
	}

	now := arrived.Add(11 * time.Second)
	s.advance(tt, tt.RT(), now)
	if s.legIdx != 2 || s.boarded {
		t.Fatalf("30-second rail change advanced to boarded: leg=%d boarded=%v", s.legIdx, s.boarded)
	}
	_, feas := s.refreshTimes(tt, tt.RT(), now)
	if feas.ok || feas.reason != "missed_connection" {
		t.Fatalf("30-second rail change remained feasible: %#v", feas)
	}
}
