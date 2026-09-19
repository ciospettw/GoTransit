package track

import (
	"log/slog"
	"testing"
	"time"

	"gotransit/internal/config"
	"gotransit/internal/engine"
	"gotransit/internal/transit"
)

type eventSink struct{ events []any }

func (s *eventSink) Send(ev any) error { s.events = append(s.events, ev); return nil }

func continuitySession() (*session, *transit.Timetable, time.Time) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	tt := &transit.Timetable{
		StopID: []string{"a", "b", "c", "d"}, StopName: []string{"A", "B", "C", "D"}, StopCode: make([]string, 4),
		StopLat: []int32{419000000, 419000000, 419000000, 419000000}, StopLon: []int32{125000000, 125040000, 125080000, 125120000},
		PatFirstStop: []uint32{0, 4}, PatStops: []int32{0, 1, 2, 3}, PatFirstTrip: []uint32{0, 1}, PatRoute: []int32{0},
		PatShape: []int32{0}, PatShapeIdx: []uint32{0, 1, 2, 3}, ShpFirst: []uint32{0, 4},
		ShpLat: []int32{419000000, 419000000, 419000000, 419000000}, ShpLon: []int32{125000000, 125040000, 125080000, 125120000}, ShpCumDm: []uint32{0, 3310, 6620, 9930},
		TripService: []int32{0}, TripHeadsign: []int32{0}, TripID: []string{"bus"}, TripTimeOff: []uint32{0, 4}, TripIdx: map[string]uint32{"test:bus": 0},
		Arr: []uint32{43320, 43500, 43680, 43860}, Dep: []uint32{43320, 43500, 43680, 43860},
		Routes: []transit.RouteMeta{{Feed: "test", Type: 3}}, Headsigns: []string{"D"}, TZ: time.UTC,
	}
	cfg := config.Default()
	e := engine.New(cfg)
	e.SetTimetable(tt)
	s := &session{t: &Tracker{Cfg: cfg, E: e, Log: slog.Default()}, sink: &eventSink{}, gps: newGPSState(), warned: map[string]bool{}, lastEmit: map[int]legTime{}}
	s.it = engine.Itinerary{Legs: []engine.Leg{
		{Mode: "walk", Depart: now, Arrive: now.Add(time.Minute), DurationS: 60, To: engine.Place{Lat: 41.9, Lon: 12.5}},
		{Mode: "transit", TripID: "test:bus", Realtime: true, From: engine.Place{StopID: "a", Lat: 41.9, Lon: 12.5}, To: engine.Place{StopID: "d", Lat: 41.9, Lon: 12.512}, Depart: now.Add(2 * time.Minute), Arrive: now.Add(11 * time.Minute)},
	}}
	return s, tt, now
}

func TestStopArrivalImmediateAndLocked(t *testing.T) {
	for _, meters := range []float64{29, 31} {
		t.Run(time.Duration(meters).String(), func(t *testing.T) {
			s, tt, now := continuitySession()
			s.gps.update(Fix{Lat: 41.9 + meters/111195, Lon: 12.5, AccuracyM: 20, At: now}, now)
			s.arriveAtBoardingStop(tt, tt.RT(), now)
			if meters > 30 {
				if s.legIdx != 0 || s.atStop != nil {
					t.Fatal("GPS accuracy inflated the 30m arrival radius")
				}
				return
			}
			if s.legIdx != 1 || s.reachedPlace() == nil {
				t.Fatal("arrival did not immediately complete access")
			}
			s.gps.update(Fix{Lat: 41.9008, Lon: 12.5, At: now.Add(time.Second)}, now.Add(time.Second))
			lat, lon := s.replanPoint(now.Add(time.Second))
			if lat != 41.9 || lon != 12.5 {
				t.Fatalf("stop lock drifted: %v,%v", lat, lon)
			}
			if len(s.gps.reachedStops) != 1 {
				t.Fatal("boarding evidence lost at walk handoff")
			}
		})
	}
}

func TestShapeBoardingAtHundredMetersWithoutRealtime(t *testing.T) {
	s, tt, now := continuitySession()
	for i, lon := range []float64{12.5, 12.50065, 12.5013} {
		at := now.Add(time.Duration(i) * 3 * time.Second)
		s.gps.update(Fix{Lat: 41.9, Lon: lon, AccuracyM: 10, At: at}, at)
		s.arriveAtBoardingStop(tt, tt.RT(), at)
		s.advance(tt, tt.RT(), at)
		if i < 2 && s.boarded {
			t.Fatal("boarded before 100m")
		}
	}
	if !s.boarded || !s.boardedByShape || s.legIdx != 1 {
		t.Fatalf("not onboard at 107m: leg=%d boarded=%v", s.legIdx, s.boarded)
	}
}

func TestShapeBoardingRejectsUnreachedStopAndReverseMotion(t *testing.T) {
	for _, reached := range []bool{false, true} {
		s, tt, now := continuitySession()
		s.legIdx = 1
		if reached {
			s.gps.cur.At = now
			s.gps.latchReachedStop(s.boardingKey(1))
		}
		for i, lon := range []float64{12.503, 12.502, 12.5013} {
			at := now.Add(time.Duration(i) * 3 * time.Second)
			s.gps.update(Fix{Lat: 41.9, Lon: lon, At: at}, at)
			_, boarded := s.gpsShapeBoarding(tt, ride{trip: 0, board: 0, alight: 3}, 1, at)
			if boarded {
				t.Fatal("reverse motion or missing stop arrival confirmed boarding")
			}
		}
	}
}

func TestOnboardSwitchNeverBecomesWalk(t *testing.T) {
	s, _, _ := continuitySession()
	s.legIdx = 1
	s.boarded = true
	s.boardedByShape = true
	original := s.it.Legs[1]
	s.switchTo(engine.Itinerary{Legs: []engine.Leg{{Mode: "walk"}}}, "better_arrival", "", 300)
	if s.legIdx != 1 || !s.boarded {
		t.Fatal("invalid walk replacement displaced occupied bus")
	}
	original.Boarded = true
	s.switchTo(engine.Itinerary{ID: "replacement", Legs: []engine.Leg{original, {Mode: "walk"}}}, "better_arrival", "", 300)
	if s.legIdx != 0 || !s.boarded || !s.boardedByShape {
		t.Fatal("valid replacement lost onboard state")
	}
	evs := s.sink.(*eventSink).events
	if ev, ok := evs[len(evs)-1].(evProgress); !ok || ev.Status != "riding" {
		t.Fatalf("missing immediate riding event: %#v", evs)
	}
}

func TestOnboardAlightingNeedsTwoStopsNotice(t *testing.T) {
	s, tt, now := continuitySession()
	s.legIdx = 1
	s.boarded = true
	r := ride{trip: 0, board: 0, alight: 3}
	if got := s.firstAlightPosition(tt, tt.RT(), r, now); got != 2 {
		t.Fatalf("first possible alight=%d, want 2", got)
	}
	s.gps.update(Fix{Lat: 41.9, Lon: 12.505, At: now}, now)
	if got := s.firstAlightPosition(tt, tt.RT(), r, now); got != 3 {
		t.Fatalf("stale feed ignored GPS progress: %d, want 3", got)
	}
}

func TestStaleArrivalCannotRemoveRiderFromShape(t *testing.T) {
	s, tt, now := continuitySession()
	s.legIdx = 1
	s.boarded = true
	now = now.Add(20 * time.Minute)
	s.gps.update(Fix{Lat: 41.9, Lon: 12.505, At: now}, now)
	s.advance(tt, tt.RT(), now)
	if !s.boarded || s.legIdx != 1 {
		t.Fatal("stale ETA removed rider from bus")
	}
}

func TestRerouteRejectsFragileChangesEvenWithRankingDisabled(t *testing.T) {
	s, _, now := continuitySession()
	s.t.Cfg.Routing.RiskRanking = false
	incoming := s.it.Legs[1]
	incoming.Boarded = true
	incoming.Arrive = now.Add(5 * time.Minute)
	for _, gap := range []time.Duration{time.Minute, 3 * time.Minute} {
		outgoing := incoming
		outgoing.Boarded = false
		outgoing.TripID = "other"
		outgoing.Depart = incoming.Arrive.Add(gap)
		outgoing.Arrive = outgoing.Depart.Add(5 * time.Minute)
		it := engine.Itinerary{Depart: now, Arrive: outgoing.Arrive, Legs: []engine.Leg{incoming, outgoing}}
		_, ok := s.pickFeasible([]engine.Itinerary{it}, now)
		if ok != (gap == 3*time.Minute) {
			t.Fatalf("gap %s feasibility=%v", gap, ok)
		}
	}
}

func TestStaleArrivalWithoutShapeKeepsOnboardRider(t *testing.T) {
	s, tt, now := continuitySession()
	s.legIdx = 1
	s.boarded = true
	tt.PatShape[0] = -1
	now = now.Add(20 * time.Minute)
	s.gps.update(Fix{Lat: 41.9, Lon: 12.505, At: now}, now)
	s.advance(tt, tt.RT(), now)
	if !s.boarded || s.legIdx != 1 {
		t.Fatal("clock alighted a rider still far from the stop, without any feed confirmation")
	}
}
