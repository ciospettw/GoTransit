package engine

import (
	"testing"
	"time"

	"gotransit/internal/config"
	"gotransit/internal/graph"
	"gotransit/internal/transit"
)

func TestAssembleOnboardReplanStartingWithFootpath(t *testing.T) {
	const (
		baseLat = int32(419000000)
		baseLon = int32(125000000)
	)
	tt := &transit.Timetable{
		StopID:   []string{"t:A", "t:B", "t:C"},
		StopCode: []string{"", "", ""},
		StopName: []string{"A", "B", "C"},
		StopLat:  []int32{baseLat, baseLat, baseLat},
		StopLon:  []int32{baseLon, baseLon + 10000, baseLon + 20000},
		StopSnap: []transit.StopSnap{
			{NodeU: -1, NodeV: -1, Edge: -1},
			{NodeU: -1, NodeV: -1, Edge: -1},
			{NodeU: -1, NodeV: -1, Edge: -1},
		},
		PatFirstStop: []uint32{0, 2},
		PatStops:     []int32{1, 2},
		PatFirstTrip: []uint32{0, 1},
		PatRoute:     []int32{0},
		PatShape:     []int32{-1},
		TripService:  []int32{0},
		TripHeadsign: []int32{0},
		TripID:       []string{"R1"},
		TripFeed:     []uint8{0},
		TripTimeOff:  []uint32{0, 2},
		Arr:          []uint32{1100, 1600},
		Dep:          []uint32{1100, 1600},
		Seq:          []uint16{1, 2},
		Routes:       []transit.RouteMeta{{Feed: "t", GTFSID: "R", Short: "R", Type: 2}},
		Headsigns:    []string{""},
		TZ:           time.UTC,
	}
	g := graph.SyntheticGrid(2, 2, baseLat, baseLon, 10000)
	e := New(config.Default())
	gb, tb := NewGraphBundle(g), NewTTBundle(tt)
	base := time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)
	j := transit.Journey{
		Rides: 1, ArrSec: 1600, Target: 2,
		Legs: []transit.RLeg{
			{From: 0, To: 1, Sec: 120},
			{Ride: true, Trip: 0, Pattern: 0, Board: 0, Alight: 1},
		},
	}
	acc := accessSet{sec: map[int32]uint32{0: 900}, mode: "none", absolute: true}
	egr := accessSet{sec: map[int32]uint32{2: 0}, mode: "walk"}

	it, ok := e.assemble(gb, tb, j, base, 800, 0, 0, baseLat, baseLon+20000, &acc, &egr)
	if !ok {
		t.Fatal("assemble rejected onboard journey with leading footpath")
	}
	if len(it.Legs) != 3 || it.Legs[0].Mode != "walk" || it.Legs[1].Mode != "transit" {
		t.Fatalf("legs = %+v", it.Legs)
	}
	if want := base.Add(900 * time.Second); !it.Legs[0].Depart.Equal(want) {
		t.Fatalf("footpath depart = %v, want %v", it.Legs[0].Depart, want)
	}
	if want := base.Add(1020 * time.Second); !it.Legs[0].Arrive.Equal(want) {
		t.Fatalf("footpath arrive = %v, want %v", it.Legs[0].Arrive, want)
	}
}
