package gtfs

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"slices"
	"sort"
	"time"
)

// Feed is one parsed GTFS static dataset, ready for timetable compilation.
type Feed struct {
	Name string
	TZ   string

	Stops   []Stop
	StopIdx map[string]int32

	Routes   []Route
	RouteIdx map[string]int32

	Services   []Service
	ServiceIdx map[string]int32

	Shapes   []Shape
	ShapeIdx map[string]int32

	Trips     []Trip
	Headsigns []string

	// stop_times, regrouped: rows of trip t are TripSTOff[t] .. TripSTOff[t+1]
	STArr     []uint32
	STDep     []uint32
	STStop    []int32
	STSeq     []uint16 // original GTFS stop_sequence (GTFS-RT matching)
	TripSTOff []uint32

	Warnings  []string
	LoadStats string
}

type Stop struct {
	ID, Code, Name string
	Lat, Lon       int32
	OK             bool // parsed coordinates
}

type Route struct {
	ID, Short, Long  string
	Color, TextColor string
	Type             int
	Agency           string
}

type Trip struct {
	RouteIdx    int32
	ServiceIdx  int32
	ShapeIdx    int32 // -1 when absent
	HeadsignIdx int32
	ID          string
	DirID       uint8
}

// Service activity: weekday mask + range, plus explicit add/remove dates.
type Service struct {
	Mask       uint8 // bit 0 = Monday ... bit 6 = Sunday
	Start, End uint32
	Add, Del   []uint32 // YYYYMMDD, sorted
}

// ActiveOn reports service activity on date (YYYYMMDD) with weekday wd
// (time.Weekday).
func (s *Service) ActiveOn(date uint32, wd time.Weekday) bool {
	if _, ok := slices.BinarySearch(s.Del, date); ok {
		return false
	}
	if _, ok := slices.BinarySearch(s.Add, date); ok {
		return true
	}
	if s.Mask == 0 || date < s.Start || date > s.End {
		return false
	}
	bit := (int(wd) + 6) % 7 // Monday = bit 0
	return s.Mask&(1<<bit) != 0
}

type Shape struct {
	ID       string
	Lat, Lon []int32
	CumDm    []uint32 // cumulative decimeters along the shape
}

// Load reads a GTFS zip from disk.
func Load(path, name string) (*Feed, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil, fmt.Errorf("gtfs %s: %w", name, err)
	}
	defer zr.Close()
	return parseZip(&zr.Reader, name)
}

// LoadBytes reads a GTFS zip held in memory (ephemeral mode: feeds are
// downloaded, parsed and never written to disk).
func LoadBytes(data []byte, name string) (*Feed, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("gtfs %s: %w", name, err)
	}
	return parseZip(zr, name)
}

func parseZip(zr *zip.Reader, name string) (*Feed, error) {
	t0 := time.Now()
	files := map[string][]byte{}
	want := map[string]bool{
		"agency.txt": true, "stops.txt": true, "routes.txt": true,
		"trips.txt": true, "stop_times.txt": true, "calendar.txt": true,
		"calendar_dates.txt": true, "shapes.txt": true, "transfers.txt": true,
	}
	for _, f := range zr.File {
		base := f.Name
		if i := lastSlash(base); i >= 0 {
			base = base[i+1:]
		}
		if !want[base] || files[base] != nil {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("gtfs %s: %s: %w", name, f.Name, err)
		}
		buf := make([]byte, 0, f.UncompressedSize64)
		buf, err = readAllInto(buf, rc)
		rc.Close()
		if err != nil {
			return nil, fmt.Errorf("gtfs %s: %s: %w", name, f.Name, err)
		}
		files[base] = buf
	}
	for _, req := range []string{"stops.txt", "routes.txt", "trips.txt", "stop_times.txt"} {
		if files[req] == nil {
			return nil, fmt.Errorf("gtfs %s: missing %s", name, req)
		}
	}

	f := &Feed{Name: name, TZ: "Europe/Rome"}
	if a := files["agency.txt"]; a != nil {
		f.parseAgency(a)
	}
	f.parseStops(files["stops.txt"])
	f.parseRoutes(files["routes.txt"])
	f.parseServices(files["calendar.txt"], files["calendar_dates.txt"])
	f.parseShapes(files["shapes.txt"])
	f.parseTrips(files["trips.txt"])
	f.parseStopTimes(files["stop_times.txt"])

	f.LoadStats = fmt.Sprintf("gtfs %s: %d stops, %d routes, %d trips, %d stop_times, %d shapes, %d services in %v",
		name, len(f.Stops), len(f.Routes), len(f.Trips), len(f.STArr), len(f.Shapes), len(f.Services),
		time.Since(t0).Round(time.Millisecond))
	return f, nil
}

func lastSlash(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '/' {
			return i
		}
	}
	return -1
}

func readAllInto(buf []byte, r io.Reader) ([]byte, error) {
	for {
		if len(buf) == cap(buf) {
			buf = append(buf, 0)[:len(buf)]
		}
		n, err := r.Read(buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+n]
		if err == io.EOF {
			return buf, nil
		}
		if err != nil {
			return buf, err
		}
	}
}

func (f *Feed) warnf(format string, a ...any) {
	if len(f.Warnings) < 50 {
		f.Warnings = append(f.Warnings, fmt.Sprintf(format, a...))
	}
}

func (f *Feed) parseAgency(data []byte) {
	t, err := NewTable(data)
	if err != nil {
		return
	}
	cTZ := t.Col("agency_timezone")
	if t.Next() {
		if tz := t.Field(cTZ); len(tz) > 0 {
			f.TZ = string(tz)
		}
	}
}

func (f *Feed) parseStops(data []byte) {
	t, err := NewTable(data)
	if err != nil {
		return
	}
	cID, cCode, cName := t.Col("stop_id"), t.Col("stop_code"), t.Col("stop_name")
	cLat, cLon, cType := t.Col("stop_lat"), t.Col("stop_lon"), t.Col("location_type")
	f.StopIdx = make(map[string]int32, 16384)
	for t.Next() {
		id := t.Field(cID)
		if len(id) == 0 {
			continue
		}
		if lt := t.Field(cType); len(lt) > 0 && lt[0] != '0' {
			continue // stations/entrances are not boardable stops
		}
		lat, ok1 := ParseCoordE7(t.Field(cLat))
		lon, ok2 := ParseCoordE7(t.Field(cLon))
		s := Stop{
			ID: string(id), Code: string(t.Field(cCode)), Name: string(t.Field(cName)),
			Lat: lat, Lon: lon, OK: ok1 && ok2,
		}
		if !s.OK {
			f.warnf("stop %s: bad coordinates", s.ID)
		}
		if _, dup := f.StopIdx[s.ID]; dup {
			continue
		}
		f.StopIdx[s.ID] = int32(len(f.Stops))
		f.Stops = append(f.Stops, s)
	}
}

func (f *Feed) parseRoutes(data []byte) {
	t, err := NewTable(data)
	if err != nil {
		return
	}
	cID, cShort, cLong := t.Col("route_id"), t.Col("route_short_name"), t.Col("route_long_name")
	cType, cColor, cText := t.Col("route_type"), t.Col("route_color"), t.Col("route_text_color")
	cAg := t.Col("agency_id")
	f.RouteIdx = make(map[string]int32, 1024)
	for t.Next() {
		id := t.Field(cID)
		if len(id) == 0 {
			continue
		}
		r := Route{
			ID: string(id), Short: string(t.Field(cShort)), Long: string(t.Field(cLong)),
			Type: ParseUint(t.Field(cType)), Color: string(t.Field(cColor)),
			TextColor: string(t.Field(cText)), Agency: string(t.Field(cAg)),
		}
		if _, dup := f.RouteIdx[r.ID]; dup {
			continue
		}
		f.RouteIdx[r.ID] = int32(len(f.Routes))
		f.Routes = append(f.Routes, r)
	}
}

func (f *Feed) parseServices(calendar, calendarDates []byte) {
	f.ServiceIdx = make(map[string]int32, 512)
	get := func(id []byte) *Service {
		if i, ok := f.ServiceIdx[string(id)]; ok {
			return &f.Services[i]
		}
		f.ServiceIdx[string(id)] = int32(len(f.Services))
		f.Services = append(f.Services, Service{})
		return &f.Services[len(f.Services)-1]
	}
	if calendar != nil {
		t, err := NewTable(calendar)
		if err == nil {
			cID := t.Col("service_id")
			var cDay [7]int
			for i, d := range []string{"monday", "tuesday", "wednesday", "thursday", "friday", "saturday", "sunday"} {
				cDay[i] = t.Col(d)
			}
			cStart, cEnd := t.Col("start_date"), t.Col("end_date")
			for t.Next() {
				id := t.Field(cID)
				if len(id) == 0 {
					continue
				}
				s := get(id)
				for i := 0; i < 7; i++ {
					if v := t.Field(cDay[i]); len(v) > 0 && v[0] == '1' {
						s.Mask |= 1 << i
					}
				}
				s.Start = ParseDate(t.Field(cStart))
				s.End = ParseDate(t.Field(cEnd))
			}
		}
	}
	if calendarDates != nil {
		t, err := NewTable(calendarDates)
		if err == nil {
			cID, cDate, cType := t.Col("service_id"), t.Col("date"), t.Col("exception_type")
			for t.Next() {
				id := t.Field(cID)
				date := ParseDate(t.Field(cDate))
				if len(id) == 0 || date == 0 {
					continue
				}
				s := get(id)
				if et := t.Field(cType); len(et) > 0 && et[0] == '2' {
					s.Del = append(s.Del, date)
				} else {
					s.Add = append(s.Add, date)
				}
			}
		}
	}
	for i := range f.Services {
		slices.Sort(f.Services[i].Add)
		slices.Sort(f.Services[i].Del)
	}
}

func (f *Feed) parseShapes(data []byte) {
	f.ShapeIdx = make(map[string]int32, 1024)
	if data == nil {
		return
	}
	t, err := NewTable(data)
	if err != nil {
		return
	}
	cID, cLat, cLon, cSeq := t.Col("shape_id"), t.Col("shape_pt_lat"), t.Col("shape_pt_lon"), t.Col("shape_pt_sequence")
	type pt struct {
		seq      int32
		lat, lon int32
	}
	byShape := map[string][]pt{}
	for t.Next() {
		id := t.Field(cID)
		lat, ok1 := ParseCoordE7(t.Field(cLat))
		lon, ok2 := ParseCoordE7(t.Field(cLon))
		seq := ParseUint(t.Field(cSeq))
		if len(id) == 0 || !ok1 || !ok2 || seq < 0 {
			continue
		}
		byShape[string(id)] = append(byShape[string(id)], pt{int32(seq), lat, lon})
	}
	for id, pts := range byShape {
		sort.Slice(pts, func(i, j int) bool { return pts[i].seq < pts[j].seq })
		sh := Shape{ID: id, Lat: make([]int32, len(pts)), Lon: make([]int32, len(pts))}
		for i, p := range pts {
			sh.Lat[i], sh.Lon[i] = p.lat, p.lon
		}
		f.ShapeIdx[id] = int32(len(f.Shapes))
		f.Shapes = append(f.Shapes, sh)
	}
}

func (f *Feed) parseTrips(data []byte) {
	t, err := NewTable(data)
	if err != nil {
		return
	}
	cID, cRoute, cService := t.Col("trip_id"), t.Col("route_id"), t.Col("service_id")
	cHead, cDir, cShape := t.Col("trip_headsign"), t.Col("direction_id"), t.Col("shape_id")
	headIdx := map[string]int32{}
	f.Headsigns = []string{""}
	headIdx[""] = 0
	for t.Next() {
		id := t.Field(cID)
		ri, okR := f.RouteIdx[string(t.Field(cRoute))]
		si, okS := f.ServiceIdx[string(t.Field(cService))]
		if len(id) == 0 || !okR || !okS {
			f.warnf("trip %s: unknown route or service", string(id))
			continue
		}
		hi, ok := headIdx[string(t.Field(cHead))]
		if !ok {
			hi = int32(len(f.Headsigns))
			f.Headsigns = append(f.Headsigns, string(t.Field(cHead)))
			headIdx[string(t.Field(cHead))] = hi
		}
		shape := int32(-1)
		if v, ok := f.ShapeIdx[string(t.Field(cShape))]; ok {
			shape = v
		}
		var dir uint8
		if d := t.Field(cDir); len(d) > 0 && d[0] == '1' {
			dir = 1
		}
		f.Trips = append(f.Trips, Trip{
			RouteIdx: ri, ServiceIdx: si, ShapeIdx: shape,
			HeadsignIdx: hi, ID: string(id), DirID: dir,
		})
	}
}

func (f *Feed) parseStopTimes(data []byte) {
	header, err := NewTable(data)
	if err != nil {
		return
	}
	cTrip, cArr, cDep := header.Col("trip_id"), header.Col("arrival_time"), header.Col("departure_time")
	cStop, cSeq := header.Col("stop_id"), header.Col("stop_sequence")

	tripIdx := make(map[string]int32, len(f.Trips))
	for i := range f.Trips {
		tripIdx[f.Trips[i].ID] = int32(i)
	}

	type row struct {
		trip, stop int32
		seq        int32
		arr, dep   int32 // -1 = missing (interpolated later)
	}
	workers := 8
	local := make([][]row, workers)
	badRows := make([]int, workers)
	ForEachParallel(data, workers, func(w int, fields [][]byte) {
		get := func(i int) []byte {
			if i < 0 || i >= len(fields) {
				return nil
			}
			return fields[i]
		}
		ti, okT := tripIdx[string(get(cTrip))]
		si, okS := f.StopIdx[string(get(cStop))]
		seq := ParseUint(get(cSeq))
		if !okT || !okS || seq < 0 {
			badRows[w]++
			return
		}
		arr := ParseGTFSTime(get(cArr))
		dep := ParseGTFSTime(get(cDep))
		if arr < 0 && dep >= 0 {
			arr = dep
		}
		if dep < 0 && arr >= 0 {
			dep = arr
		}
		local[w] = append(local[w], row{trip: ti, stop: si, seq: int32(seq), arr: int32(arr), dep: int32(dep)})
	})

	// regroup rows by trip
	counts := make([]uint32, len(f.Trips)+1)
	total := 0
	for _, rs := range local {
		total += len(rs)
		for i := range rs {
			counts[rs[i].trip+1]++
		}
	}
	for i := 1; i <= len(f.Trips); i++ {
		counts[i] += counts[i-1]
	}
	f.TripSTOff = counts
	f.STArr = make([]uint32, total)
	f.STDep = make([]uint32, total)
	f.STStop = make([]int32, total)
	f.STSeq = make([]uint16, total)
	seqs := make([]int32, total)
	fill := make([]uint32, len(f.Trips))
	for _, rs := range local {
		for i := range rs {
			r := &rs[i]
			pos := f.TripSTOff[r.trip] + fill[r.trip]
			fill[r.trip]++
			f.STArr[pos] = uint32(r.arr)
			f.STDep[pos] = uint32(r.dep)
			f.STStop[pos] = r.stop
			sq := r.seq
			if sq > 65535 {
				sq = 65535
			}
			f.STSeq[pos] = uint16(sq)
			seqs[pos] = r.seq
		}
	}
	// per-trip: sort by seq, interpolate missing times, force monotonicity
	interpolated, fixed := 0, 0
	for t := 0; t < len(f.Trips); t++ {
		lo, hi := f.TripSTOff[t], f.TripSTOff[t+1]
		if hi <= lo {
			continue
		}
		sortTripRows(seqs, f.STArr, f.STDep, f.STStop, f.STSeq, int(lo), int(hi))
		interpolated += interpolateTimes(f.STArr[lo:hi], f.STDep[lo:hi])
		// monotonic sanity: arr[i] >= dep[i-1], dep[i] >= arr[i]
		for i := lo + 1; i < hi; i++ {
			if f.STArr[i] < f.STDep[i-1] {
				f.STArr[i] = f.STDep[i-1]
				fixed++
			}
			if f.STDep[i] < f.STArr[i] {
				f.STDep[i] = f.STArr[i]
				fixed++
			}
		}
	}
	bad := 0
	for _, b := range badRows {
		bad += b
	}
	if bad > 0 {
		f.warnf("stop_times: %d rows referenced unknown trips/stops", bad)
	}
	if interpolated > 0 {
		f.warnf("stop_times: interpolated %d missing times", interpolated)
	}
	if fixed > 0 {
		f.warnf("stop_times: fixed %d non-monotonic times", fixed)
	}
}

// sortTripRows sorts one trip's slice of parallel arrays by seq (insertion
// sort: rows arrive nearly ordered).
func sortTripRows(seqs []int32, arr, dep []uint32, stop []int32, oseq []uint16, lo, hi int) {
	for i := lo + 1; i < hi; i++ {
		j := i
		for j > lo && seqs[j-1] > seqs[j] {
			seqs[j-1], seqs[j] = seqs[j], seqs[j-1]
			arr[j-1], arr[j] = arr[j], arr[j-1]
			dep[j-1], dep[j] = dep[j], dep[j-1]
			stop[j-1], stop[j] = stop[j], stop[j-1]
			oseq[j-1], oseq[j] = oseq[j], oseq[j-1]
			j--
		}
	}
}

// interpolateTimes fills missing (^uint32(0)) times linearly between known
// ones. Returns how many were filled.
func interpolateTimes(arr, dep []uint32) int {
	const miss = ^uint32(0)
	n := 0
	for i := 0; i < len(arr); i++ {
		if arr[i] != miss {
			continue
		}
		// find previous and next known
		p := i - 1
		for p >= 0 && arr[p] == miss {
			p--
		}
		q := i
		for q < len(arr) && arr[q] == miss {
			q++
		}
		if p < 0 || q >= len(arr) {
			// cannot interpolate at the ends: copy the neighbor
			var v uint32
			if p >= 0 {
				v = dep[p]
			} else if q < len(arr) {
				v = arr[q]
			}
			arr[i], dep[i] = v, v
			n++
			continue
		}
		span := int(q - p)
		v := dep[p] + (arr[q]-dep[p])*uint32(i-p)/uint32(span)
		arr[i], dep[i] = v, v
		n++
	}
	return n
}
