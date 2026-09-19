// Fattibilità probabilistica degli itinerari, leg per leg.
//
// Per ogni salita transit si valuta la coincidenza in ingresso (arrivo alla
// fermata → partenza del mezzo): slack reale, incertezza della camminata,
// distribuzione dei ritardi del mezzo in arrivo e di quello in partenza
// (istogrammi appresi via stats.Provider quando ci sono, modello analitico
// dall'headway altrimenti — le probabilità ci sono SEMPRE).
//
// Il risultato finisce in tre posti: Leg.CatchProb/RiskLevel (badge in app),
// Itinerary.Feasibility/RiskLevel (prodotto/peggiore), e expArr — l'arrivo
// ATTESO (arrivo nominale + Σ (1−P)·costo del buco) usato da dedupeRank:
// itinerari con 4 leg attaccate smettono di vincere solo perché veloci
// sulla carta, ma non vengono MAI nascosti.
package engine

import (
	"math"
	"time"

	"gotransit/internal/geo"
	"gotransit/internal/stats"
	"gotransit/internal/transit"
)

// riskSigma: analytic σ (seconds) for a leg's residual delay when no learned
// distribution is available. RT-covered trips are largely predictable.
const rtResidualSigma = 25

// ExpectedArrive is the risk-adjusted arrival (nominal + expected miss cost)
// used for ranking; falls back to the nominal arrival when risk annotation
// did not run. Reroute target selection compares THIS, not Arrive: a plan
// that looks 3 minutes faster but hinges on a coin-flip connection is not
// actually faster.
func (it *Itinerary) ExpectedArrive() time.Time {
	if it.expArr.IsZero() {
		return it.Arrive
	}
	return it.expArr
}

// annotateRisk stamps probabilities on every itinerary (no-op when disabled).
func (e *Engine) annotateRisk(its []Itinerary, now time.Time) {
	if e.Cfg == nil || !e.Cfg.Routing.RiskRanking {
		return
	}
	tb := e.TTBundle()
	if tb == nil || tb.TT == nil {
		return
	}
	for i := range its {
		e.riskOne(&its[i], tb.TT, now)
	}
}

func (e *Engine) riskOne(it *Itinerary, tt *transit.Timetable, now time.Time) {
	r := e.Cfg.Routing
	product := 1.0
	expShift := 0.0 // expected extra seconds from potentially missed connections
	worst := ""

	var prevTransit *Leg
	walkSec := 0.0 // street time since the previous transit leg (or the start)
	prevArrive := now

	for li := range it.Legs {
		l := &it.Legs[li]
		if l.Mode != "transit" {
			walkSec += float64(l.DurationS)
			prevArrive = l.Arrive
			continue
		}

		headway := e.headwaySec(tt, l)
		p := e.ConnectionProbability(tt, l, prevTransit, prevArrive, walkSec, now)
		if l.Boarded {
			p = 1 // its arrival uncertainty still contributes to the next change
		}
		l.CatchProb = math.Round(p*1000) / 1000
		l.RiskLevel = riskLabel(p, r.RiskOk, r.RiskWarn)
		worst = worstRisk(worst, l.RiskLevel)
		product *= p

		// Miss cost: the headway of the missed line (you wait for the next
		// one), bounded by the configured default when unknown.
		miss := headway
		if miss <= 0 {
			miss = r.MissCostDefault.Seconds()
		}
		miss = math.Min(math.Max(miss, 300), 2*r.MissCostDefault.Seconds())
		expShift += (1 - p) * miss

		prevTransit = l
		walkSec = 0
		prevArrive = l.Arrive
	}

	if prevTransit == nil {
		return // no transit legs: deterministic, nothing to stamp
	}
	it.Feasibility = math.Round(product*1000) / 1000
	it.RiskLevel = worst
	it.expArr = it.Arrive.Add(time.Duration(expShift * float64(time.Second)))
}

// AssessRisk is also used to validate live replacements when ordinary search
// risk ranking is disabled. A reroute must still be physically catchable.
func (e *Engine) AssessRisk(it *Itinerary, now time.Time) {
	if tb := e.TTBundle(); tb != nil {
		e.riskOne(it, tb.TT, now)
	}
}

// ConnectionProbability is shared by planning and live evaluation, so a
// replacement cannot pass one model and immediately fail the other.
func (e *Engine) ConnectionProbability(tt *transit.Timetable, outgoing, incoming *Leg, ready time.Time, walkSec float64, now time.Time) float64 {
	if incoming == nil && walkSec == 0 && !outgoing.Depart.Before(now) {
		return 1 // already at this stop: there is no access walk left to miss
	}
	margin := e.Cfg.Track.BoardBuffer.Seconds()
	if incoming != nil {
		margin = e.Cfg.Routing.TransferSlack.Seconds()
	}
	c := stats.Conn{SlackSec: outgoing.Depart.Sub(ready).Seconds() - margin,
		WalkSec: walkSec, WalkSigmaFrac: e.Cfg.Routing.WalkSigmaFrac}
	fill := func(leg *Leg, kind stats.Kind) (*stats.Hist, float64, float64) {
		at := leg.Depart
		if kind == stats.Arrival {
			at = leg.Arrive
		}
		local := at.In(tt.TZ)
		forecast := predictionUncertainty(tt, leg, kind == stats.Arrival, now)
		if h, ok := e.dist(kind, leg, stats.DayTypeOf(local), local.Hour()); ok {
			return h, 0, forecast
		}
		if leg.Realtime {
			return nil, rtResidualSigma, forecast
		}
		return nil, stats.HeadwaySigma(e.headwaySec(tt, leg)), forecast
	}
	var a, d float64
	if incoming != nil {
		c.ArrDist, c.ArrSigma, a = fill(incoming, stats.Arrival)
	}
	c.DepDist, c.DepSigma, d = fill(outgoing, stats.Departure)
	c.ForecastSigma = math.Hypot(a, d)
	return stats.Catch(c)
}

func predictionUncertainty(tt *transit.Timetable, leg *Leg, arrival bool, now time.Time) float64 {
	target, at := leg.From, leg.Depart
	if arrival {
		target, at = leg.To, leg.Arrive
	}
	distance := 0.0
	if trip, ok := tt.TripIdx[leg.TripID]; ok {
		lat, lon, pos, _, has := tt.RT().Vehicle(trip)
		if has {
			if lat == 0 && lon == 0 {
				stops := tt.PatternStops(tt.PatternOfTrip(trip))
				if pos >= 0 && int(pos) < len(stops) {
					lat, lon = tt.StopLat[stops[pos]], tt.StopLon[stops[pos]]
				}
			}
			if lat != 0 || lon != 0 {
				distance = geo.Dist(lat, lon, int32(target.Lat*1e7), int32(target.Lon*1e7))
			}
		}
	}
	return stats.ForecastSigma(at.Sub(now).Seconds(), distance)
}

// dist queries the learned distribution for a leg's route, centering it when
// the trip already has realtime data (its current delay is inside the times).
func (e *Engine) dist(kind stats.Kind, l *Leg, day stats.DayType, hour int) (*stats.Hist, bool) {
	if e.Stats == nil || l.Route == nil {
		return nil, false
	}
	h, ok := e.Stats.Dist(kind, l.Route.ID, day, hour)
	if !ok {
		return nil, false
	}
	if l.Realtime {
		h = stats.Centered(h)
	}
	return &h, true
}

// headwaySec estimates the service headway of a leg's line at its boarding
// stop from the compiled timetable: the gap to the next trip of the same
// pattern. 0 = unknown. Approximate on purpose (service calendars ignored):
// it feeds σ and miss costs, not the schedule itself.
func (e *Engine) headwaySec(tt *transit.Timetable, l *Leg) float64 {
	trip, ok := tt.TripIdx[l.TripID]
	if !ok {
		return 0
	}
	pat := tt.PatternOfTrip(trip)
	lo, hi := tt.PatternTrips(pat)
	if hi-lo < 2 {
		return 0
	}
	// boarding position = the leg's From stop within the pattern
	pos := -1
	if l.From.StopID != "" {
		for i, s := range tt.PatternStops(pat) {
			if tt.StopID[s] == l.From.StopID {
				pos = i
				break
			}
		}
	}
	if pos < 0 {
		pos = 0
	}
	dep := tt.ScheduledDep(trip, uint16(pos))
	best := uint32(0)
	found := false
	for t := lo; t < hi; t++ {
		if t == trip {
			continue
		}
		d := tt.ScheduledDep(t, uint16(pos))
		if d > dep && (!found || d < best) {
			best, found = d, true
		}
	}
	if !found {
		return 0
	}
	return float64(best - dep)
}

func riskLabel(p, ok, warn float64) string {
	switch {
	case p >= ok:
		return "ok"
	case p >= warn:
		return "warn"
	default:
		return "risk"
	}
}

// worstRisk merges levels: risk > warn > ok.
func worstRisk(cur, next string) string {
	rank := map[string]int{"": 0, "ok": 1, "warn": 2, "risk": 3}
	if rank[next] > rank[cur] {
		return next
	}
	return cur
}
