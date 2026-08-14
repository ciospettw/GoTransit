package tests

import (
	"testing"
	"time"

	"gotransit/internal/engine"
	"gotransit/internal/gtfs"
	"gotransit/internal/transit"
)

// buildTestTT: 6 stops, 2 lines and a footpath.
//
//	line A (pattern 0): S0 →(10m)→ S1 →(10m)→ S2   every 15m from 07:00
//	line B (pattern 1): S3 →(10m)→ S4              every 20m from 07:05
//	footpath S1 ↔ S3 (3 minutes)
//	night line N (pattern 2): S0 →(20m)→ S5 at 24:30 (yesterday's service day)
//
// service 0: weekdays. service 1 (line N): only weekdays.
func buildTestTT() *transit.Timetable {
	tt := &transit.Timetable{}
	for i := 0; i < 6; i++ {
		tt.StopID = append(tt.StopID, "t:"+string(rune('0'+i)))
		tt.StopName = append(tt.StopName, "Stop "+string(rune('0'+i)))
		tt.StopCode = append(tt.StopCode, "")
		tt.StopLat = append(tt.StopLat, 419000000+int32(i)*10000)
		tt.StopLon = append(tt.StopLon, 125000000)
		tt.StopFeed = append(tt.StopFeed, 0)
	}
	tt.TZ = time.UTC
	tt.Feeds = []string{"t"}
	tt.Routes = []transit.RouteMeta{{Short: "A"}, {Short: "B"}, {Short: "N"}}
	tt.Headsigns = []string{""}
	// weekday-only service
	tt.Services = []gtfs.Service{
		{Mask: 0b0011111, Start: 20260101, End: 20261231},
		{Mask: 0b0011111, Start: 20260101, End: 20261231},
	}

	addPattern := func(route int32, stops []int32) {
		tt.PatFirstStop = append(tt.PatFirstStop, uint32(len(tt.PatStops)))
		tt.PatStops = append(tt.PatStops, stops...)
		tt.PatFirstTrip = append(tt.PatFirstTrip, uint32(len(tt.TripService)))
		tt.PatRoute = append(tt.PatRoute, route)
		tt.PatShape = append(tt.PatShape, -1)
	}
	addTrip := func(svc int32, times ...uint32) {
		tt.TripService = append(tt.TripService, svc)
		tt.TripHeadsign = append(tt.TripHeadsign, 0)
		tt.TripID = append(tt.TripID, "trip")
		tt.TripTimeOff = append(tt.TripTimeOff, uint32(len(tt.Arr)))
		for _, t := range times {
			tt.Arr = append(tt.Arr, t)
			tt.Dep = append(tt.Dep, t)
		}
	}
	h := func(hh, mm int) uint32 { return uint32(hh*3600 + mm*60) }

	addPattern(0, []int32{0, 1, 2}) // line A
	for i := 0; i < 20; i++ {
		dep := h(7, 0) + uint32(i)*900
		addTrip(0, dep, dep+600, dep+1200)
	}
	addPattern(1, []int32{3, 4}) // line B
	for i := 0; i < 15; i++ {
		dep := h(7, 5) + uint32(i)*1200
		addTrip(0, dep, dep+600)
	}
	addPattern(2, []int32{0, 5}) // night line, dep 24:30
	addTrip(1, h(24, 30), h(24, 50))

	tt.PatFirstStop = append(tt.PatFirstStop, uint32(len(tt.PatStops)))
	tt.PatFirstTrip = append(tt.PatFirstTrip, uint32(len(tt.TripService)))

	// stop→pattern CSR
	cnt := make([]uint32, len(tt.StopID)+1)
	for p := 0; p < len(tt.PatRoute); p++ {
		for _, s := range tt.PatternStops(uint32(p)) {
			cnt[s+1]++
		}
	}
	for i := 1; i < len(cnt); i++ {
		cnt[i] += cnt[i-1]
	}
	tt.StopFirstPat = cnt
	tt.StopPat = make([]uint32, cnt[len(cnt)-1])
	tt.StopPatPos = make([]uint16, cnt[len(cnt)-1])
	fill := make([]uint32, len(tt.StopID))
	for p := 0; p < len(tt.PatRoute); p++ {
		for pos, s := range tt.PatternStops(uint32(p)) {
			at := tt.StopFirstPat[s] + fill[s]
			fill[s]++
			tt.StopPat[at] = uint32(p)
			tt.StopPatPos[at] = uint16(pos)
		}
	}
	// transfers: S1 ↔ S3, 180s = 1800ds
	tt.XferFirst = make([]uint32, len(tt.StopID)+1)
	for s := 0; s <= 1; s++ {
		tt.XferFirst[s+1] = tt.XferFirst[s]
	}
	tt.XferFirst[2] = 1 // after stop 1
	tt.XferTo = append(tt.XferTo, 3)
	tt.XferDs = append(tt.XferDs, 1800)
	for s := 2; s <= 3; s++ {
		tt.XferFirst[s+1] = tt.XferFirst[s]
	}
	tt.XferFirst[4] = 2 // after stop 3
	tt.XferTo = append(tt.XferTo, 1)
	tt.XferDs = append(tt.XferDs, 1800)
	tt.XferFirst[5] = 2
	tt.XferFirst[6] = 2
	return tt
}

// fixed test date: Friday 2026-07-10
var testQ = transit.Query{
	Date: 20260710, Weekday: time.Friday,
	PrevDate: 20260709, PrevWeekday: time.Thursday,
	MaxTransfers: 4, SlackSec: 60,
}

func TestRaptorDirect(t *testing.T) {
	r := transit.NewRaptor(buildTestTT())
	q := testQ
	q.Sources = []transit.StopSeed{{Stop: 0, Sec: 7*3600 + 120}} // at S0 07:02
	q.Targets = []transit.StopSeed{{Stop: 2, Sec: 60}}           // 1 min egress
	js := r.Plan(q)
	if len(js) != 1 {
		t.Fatalf("journeys = %d, want 1", len(js))
	}
	// board 07:15, arrive S2 07:35, +60s egress = 07:36
	if js[0].ArrSec != 7*3600+36*60 {
		t.Errorf("arr = %d, want %d", js[0].ArrSec, 7*3600+36*60)
	}
	if js[0].Rides != 1 || len(js[0].Legs) != 1 || !js[0].Legs[0].Ride {
		t.Errorf("legs = %+v", js[0].Legs)
	}
	if js[0].Legs[0].Board != 0 || js[0].Legs[0].Alight != 2 {
		t.Errorf("board/alight = %d/%d", js[0].Legs[0].Board, js[0].Legs[0].Alight)
	}
}

func TestRaptorTransfer(t *testing.T) {
	r := transit.NewRaptor(buildTestTT())
	q := testQ
	q.Sources = []transit.StopSeed{{Stop: 0, Sec: 7 * 3600}}
	q.Targets = []transit.StopSeed{{Stop: 4, Sec: 0}}
	js := r.Plan(q)
	if len(js) != 1 {
		t.Fatalf("journeys = %d, want 1", len(js))
	}
	// A dep 07:00 arr S1 07:10, walk 3m → S3 07:13, B dep 07:25 arr S4 07:35
	want := uint32(7*3600 + 35*60)
	if js[0].ArrSec != want {
		t.Errorf("arr = %v, want %v", hhmmT(js[0].ArrSec), hhmmT(want))
	}
	if js[0].Rides != 2 || len(js[0].Legs) != 3 {
		t.Fatalf("rides=%d legs=%d", js[0].Rides, len(js[0].Legs))
	}
	if js[0].Legs[1].Ride || js[0].Legs[1].From != 1 || js[0].Legs[1].To != 3 {
		t.Errorf("middle leg = %+v", js[0].Legs[1])
	}
}

func TestRaptorRailEntryBuffer(t *testing.T) {
	tt := buildTestTT()
	tt.Routes[0].Type = 1 // subway/metro
	r := transit.NewRaptor(tt)
	q := testQ
	q.RailEntrySec = 60
	q.RailTransferSec = 90
	// Reaching the platform only 45 seconds before the 07:15 train is not
	// enough: the one-minute station-entry buffer moves us to the 07:30 run.
	q.Sources = []transit.StopSeed{{Stop: 0, Sec: 7*3600 + 14*60 + 15}}
	q.Targets = []transit.StopSeed{{Stop: 2, Sec: 0}}
	js := r.Plan(q)
	if len(js) != 1 {
		t.Fatalf("journeys = %d, want 1", len(js))
	}
	want := uint32(7*3600 + 50*60)
	if js[0].ArrSec != want {
		t.Errorf("arr = %v, want %v", hhmmT(js[0].ArrSec), hhmmT(want))
	}
}

func TestRaptorRailTransferBufferAfterFootpath(t *testing.T) {
	tt := buildTestTT()
	tt.Routes[0].Type = 1 // incoming metro
	tt.Routes[1].Type = 2 // outgoing rail
	// Put the second B run at 07:14. A arrives S1 at 07:10 and the footpath
	// reaches S3 at 07:13: those three minutes already include the required
	// 90-second change, so the run must remain catchable.
	trip := uint32(21)
	off := tt.TripTimeOff[trip]
	tt.Arr[off], tt.Dep[off] = 7*3600+14*60, 7*3600+14*60
	tt.Arr[off+1], tt.Dep[off+1] = 7*3600+24*60, 7*3600+24*60

	r := transit.NewRaptor(tt)
	q := testQ
	q.RailEntrySec = 60
	q.RailTransferSec = 90
	q.Sources = []transit.StopSeed{{Stop: 0, Sec: 6*3600 + 58*60}}
	q.Targets = []transit.StopSeed{{Stop: 4, Sec: 0}}
	js := r.Plan(q)
	if len(js) != 1 {
		t.Fatalf("journeys = %d, want 1", len(js))
	}
	want := uint32(7*3600 + 24*60) // B dep 07:14, arr 07:24
	if js[0].ArrSec != want {
		t.Errorf("arr = %v, want %v", hhmmT(js[0].ArrSec), hhmmT(want))
	}
}

func TestRaptorRailTransferRejectsLessThanNinetySeconds(t *testing.T) {
	tt := buildTestTT()
	tt.Routes[0].Type = 1
	tt.Routes[1].Type = 2
	// A reaches S1 at 07:10 and a shortened footpath reaches S3 at 07:11.
	// A 07:11:15 departure provides only 75 seconds total, so it is missed.
	tt.XferDs[0], tt.XferDs[1] = 600, 600
	trip := uint32(21)
	off := tt.TripTimeOff[trip]
	tt.Arr[off], tt.Dep[off] = 7*3600+11*60+15, 7*3600+11*60+15
	tt.Arr[off+1], tt.Dep[off+1] = 7*3600+21*60+15, 7*3600+21*60+15

	r := transit.NewRaptor(tt)
	q := testQ
	q.RailEntrySec = 60
	q.RailTransferSec = 90
	q.Sources = []transit.StopSeed{{Stop: 0, Sec: 6*3600 + 58*60}}
	q.Targets = []transit.StopSeed{{Stop: 4, Sec: 0}}
	js := r.Plan(q)
	if len(js) != 1 {
		t.Fatalf("journeys = %d, want 1", len(js))
	}
	want := uint32(7*3600 + 55*60) // next B run dep 07:45, arr 07:55
	if js[0].ArrSec != want {
		t.Errorf("arr = %v, want %v", hhmmT(js[0].ArrSec), hhmmT(want))
	}
}

func TestRaptorOnboardRailSourceUsesTransferBufferOnlyForRail(t *testing.T) {
	const (
		trainDep = 7*3600 + 15*60
		firstArr = 7*3600 + 35*60
		nextArr  = 7*3600 + 50*60
	)
	tests := []struct {
		name       string
		outgoing   int
		priorArr   uint32
		ready      uint32
		wantArrSec uint32
	}{
		{
			name:     "rail rejects 80 seconds total even with ready seed at plus 15",
			outgoing: 1, priorArr: trainDep - 80, ready: trainDep - 65,
			wantArrSec: nextArr,
		},
		{
			name:     "rail accepts exactly 90 seconds total without adding another 15",
			outgoing: 1, priorArr: trainDep - 90, ready: trainDep - 75,
			wantArrSec: firstArr,
		},
		{
			name:     "bus keeps only ready time after incoming rail",
			outgoing: 3, priorArr: trainDep - 80, ready: trainDep - 65,
			wantArrSec: firstArr,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tt := buildTestTT()
			tt.Routes[0].Type = tc.outgoing
			r := transit.NewRaptor(tt)
			q := testQ
			q.RailEntrySec = 60
			q.RailTransferSec = 90
			q.Sources = []transit.StopSeed{{
				Stop: 0, Sec: tc.ready,
				PriorRail: true, PriorRailArrSec: tc.priorArr,
			}}
			q.Targets = []transit.StopSeed{{Stop: 2, Sec: 0}}
			js := r.Plan(q)
			if len(js) != 1 {
				t.Fatalf("journeys = %d, want 1", len(js))
			}
			if got := js[0].ArrSec; got != tc.wantArrSec {
				t.Fatalf("arrival = %v, want %v", hhmmT(got), hhmmT(tc.wantArrSec))
			}
		})
	}
}

func TestRaptorOnboardRailSourceKeepsContextAcrossFootpath(t *testing.T) {
	const priorArr = uint32(7*3600 + 10*60)
	tests := []struct {
		name       string
		walkSec    uint16
		depart     uint32
		wantArrSec uint32
	}{
		{
			// ready at 07:10:15, walk to 07:11:15, but only 75 seconds
			// elapsed since the incoming train: the 07:11:15 rail is missed.
			name:    "short walk still requires remainder of 90 seconds",
			walkSec: 60, depart: 7*3600 + 11*60 + 15,
			wantArrSec: 7*3600 + 55*60,
		},
		{
			// The two-minute walk ends 135 seconds after rail arrival. It
			// already exceeds the transfer buffer, so no extra wait is added.
			name:    "long walk already satisfies total transfer buffer",
			walkSec: 120, depart: 7*3600 + 12*60 + 15,
			wantArrSec: 7*3600 + 22*60 + 15,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tt := buildTestTT()
			tt.Routes[1].Type = 2 // outgoing rail at S3
			tt.XferDs[0], tt.XferDs[1] = tc.walkSec*10, tc.walkSec*10
			trip := uint32(21)
			off := tt.TripTimeOff[trip]
			tt.Arr[off], tt.Dep[off] = tc.depart, tc.depart
			tt.Arr[off+1], tt.Dep[off+1] = tc.depart+10*60, tc.depart+10*60

			r := transit.NewRaptor(tt)
			q := testQ
			q.RailEntrySec = 60
			q.RailTransferSec = 90
			q.Sources = []transit.StopSeed{{
				Stop: 1, Sec: priorArr + 15,
				PriorRail: true, PriorRailArrSec: priorArr,
			}}
			q.Targets = []transit.StopSeed{{Stop: 4, Sec: 0}}
			js := r.Plan(q)
			if len(js) != 1 {
				t.Fatalf("journeys = %d, want 1", len(js))
			}
			if got := js[0].ArrSec; got != tc.wantArrSec {
				t.Fatalf("arrival = %v, want %v", hhmmT(got), hhmmT(tc.wantArrSec))
			}
			if len(js[0].Legs) != 2 || js[0].Legs[0].Ride || !js[0].Legs[1].Ride {
				t.Fatalf("journey must be source footpath then rail, got %+v", js[0].Legs)
			}
		})
	}
}

func TestPlanFromStopsCarriesPriorRailContext(t *testing.T) {
	w := buildWorld(t, worldOpts{metroInsteadOfA: true})
	w.e.Cfg.Routing.RailEntryBuffer = 60 * time.Second
	w.e.Cfg.Routing.RailTransferBuffer = 90 * time.Second

	trip, ok := w.tt.TripIdx["test:A1"]
	if !ok {
		t.Fatal("fixture trip A1 missing")
	}
	sa := int32(-1)
	for i, id := range w.tt.StopID {
		if id == "test:SA" {
			sa = int32(i)
			break
		}
	}
	if sa < 0 {
		t.Fatal("fixture stop SA missing")
	}
	dep := w.base.Add(time.Duration(w.tt.ScheduledDep(trip, 0)) * time.Second)
	_, _, toLat, toLon := w.od()
	source := engine.StopPlanSource{PriorRail: true, ReadySlack: 15 * time.Second}

	tests := []struct {
		name       string
		gap        time.Duration
		wantTripID string
	}{
		{name: "80 second rail change misses A1", gap: 80 * time.Second, wantTripID: "test:B1"},
		{name: "exact 90 second rail change catches A1", gap: 90 * time.Second, wantTripID: "test:A1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			its, err := w.e.PlanFromStops(
				map[int32]time.Time{sa: dep.Add(-tc.gap)},
				toLat, toLon, w.now, 3, source,
			)
			if err != nil {
				t.Fatal(err)
			}
			if len(its) == 0 {
				t.Fatal("expected onboard replan itinerary")
			}
			if got := rideOf(t, its[0]); got != tc.wantTripID {
				t.Fatalf("first ride = %s, want %s", got, tc.wantTripID)
			}
			if tc.wantTripID == "test:A1" {
				for i := range its[0].Legs {
					if its[0].Legs[i].Mode != "transit" {
						continue
					}
					if got := its[0].Legs[i].BoardReadyAt; !got.Equal(dep) {
						t.Fatalf("tracked rail-ready anchor = %s, want incoming arrival + 90s = %s", got, dep)
					}
					break
				}
			}
		})
	}
}

func TestRaptorNightTrip(t *testing.T) {
	r := transit.NewRaptor(buildTestTT())
	q := testQ
	// 00:10 on the 10th: yesterday's 24:30 night trip should pick us up at 00:30
	q.Sources = []transit.StopSeed{{Stop: 0, Sec: 10 * 60}}
	q.Targets = []transit.StopSeed{{Stop: 5, Sec: 0}}
	js := r.Plan(q)
	if len(js) != 1 {
		t.Fatalf("journeys = %d, want 1", len(js))
	}
	want := uint32(50 * 60) // 24:50 - 24:00
	if js[0].ArrSec != want {
		t.Errorf("arr = %v, want %v", hhmmT(js[0].ArrSec), hhmmT(want))
	}
	if js[0].Legs[0].DayOff != -86400 {
		t.Errorf("dayoff = %d", js[0].Legs[0].DayOff)
	}
}

func TestRaptorWeekendNoService(t *testing.T) {
	r := transit.NewRaptor(buildTestTT())
	q := testQ
	q.Date, q.Weekday = 20260711, time.Saturday // Saturday: no service
	q.PrevDate, q.PrevWeekday = 20260710, time.Friday
	q.Sources = []transit.StopSeed{{Stop: 0, Sec: 12 * 3600}}
	q.Targets = []transit.StopSeed{{Stop: 2, Sec: 0}}
	if js := r.Plan(q); len(js) != 0 {
		t.Errorf("Saturday should have no journeys, got %d", len(js))
	}
}

func hhmmT(s uint32) string {
	return time.Unix(int64(s), 0).UTC().Format("15:04:05")
}
