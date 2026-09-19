package tests

import (
	"testing"
	"time"

	"gotransit/internal/engine"
)

func TestPlanFromOnboardStopsRetainsRideBeforeWalking(t *testing.T) {
	w := buildWorld(t, worldOpts{midStopShape: true})
	fLat, fLon, tLat, tLon := w.od()
	resp, err := w.e.Plan(engine.Request{FromLat: fLat, FromLon: fLon, ToLat: tLat, ToLon: tLon, Mode: "transit", When: w.now, Num: 4})
	if err != nil {
		t.Fatal(err)
	}
	var ride *engine.Leg
	for _, it := range resp.Itineraries {
		for i := range it.Legs {
			if it.Legs[i].TripID == "test:A1" {
				l := it.Legs[i]
				ride = &l
				break
			}
		}
		if ride != nil {
			break
		}
	}
	if ride == nil {
		t.Fatal("missing fixture ride")
	}
	trip := w.tt.TripIdx[ride.TripID]
	stops := w.tt.PatternStops(w.tt.PatternOfTrip(trip))
	stop := stops[len(stops)-1]
	at := w.base.Add(time.Duration(w.tt.TripArr(trip, uint16(len(stops)-1))) * time.Second)
	when := ride.Depart.Add(time.Minute)
	its, err := w.e.PlanFromStops(map[int32]time.Time{stop: at}, tLat, tLon, when, 4, engine.StopPlanSource{Onboard: ride, ReadySlack: 90 * time.Second})
	if err != nil || len(its) == 0 {
		t.Fatalf("missing reachable downstream walk: %v", err)
	}
	for _, it := range its {
		first := it.Legs[0]
		if first.Mode != "transit" || !first.Boarded || first.TripID != ride.TripID || first.To.StopID != w.tt.StopID[stop] {
			t.Fatalf("onboard prefix lost: %+v", first)
		}
		if !first.Arrive.Equal(at) || first.Polyline == "" || len(first.Stops) < 2 {
			t.Fatalf("invalid retained ride: %+v", first)
		}
		if it.Legs[1].Mode != "walk" || it.Legs[1].Depart.Before(at) {
			t.Fatalf("walking begins before alighting: %+v", it.Legs)
		}
		cached, ok := w.e.LookupItinerary(it.ID)
		if !ok || !cached.It.Legs[0].Boarded {
			t.Fatal("reconnecting would lose the occupied bus")
		}
	}
}
