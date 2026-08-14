package rt

import (
	"testing"
	"time"

	"gotransit/internal/transit"
)

func TestTimestamplessVehicleUsesPersistedReceiptNotFreshTripUpdate(t *testing.T) {
	now := time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC)
	received := uint64(now.Add(-10 * time.Minute).Unix())
	tt := &transit.Timetable{
		TripService: []int32{0},
		TripIdx:     map[string]uint32{"test:T1": 0},
		Feeds:       []string{"test"},
	}
	sources := []Source{{FeedIdx: 0, Name: "test"}}
	tus := map[int]*Feed{0: {Timestamp: uint64(now.Unix())}}
	vps := map[int]*Feed{0: {
		ReceivedAt: received,
		Vehicles: []VehicleRT{{
			TripID: "T1", Lat: 41.9, Lon: 12.5, CurrentSeq: Absent, Status: Absent,
		}},
	}}
	stats := map[int]*SourceStats{0: {Name: "test"}}

	o := buildOverlay(tt, sources, tus, vps, stats, now, 1)
	if got := o.VehicleTime(0); got != received {
		t.Fatalf("timestamp-less VP time = %d, want persisted receipt %d", got, received)
	}
	if got := o.FeedTime[0]; got != uint64(now.Unix()) {
		t.Fatalf("aggregate feed time = %d, want fresh TU time %d", got, now.Unix())
	}
}

func TestDecodeStampsFeedReceiptTime(t *testing.T) {
	before := uint64(time.Now().Unix())
	f, err := Decode(Encode(&Feed{}))
	if err != nil {
		t.Fatal(err)
	}
	after := uint64(time.Now().Unix())
	if f.ReceivedAt < before || f.ReceivedAt > after {
		t.Fatalf("decode receipt = %d, want within [%d,%d]", f.ReceivedAt, before, after)
	}
}
