// Rischio coincidenze LIVE: a ogni ciclo di valutazione (ogni update GTFS-RT
// e ogni tick) si ricalcola la probabilità di prendere ciascuna coincidenza
// rimanente con gli orari RT-adjusted correnti. Sotto soglia si agisce PRIMA
// che la coincidenza sia persa: warning al client, poi reroute proattivo —
// con isteresi (N valutazioni consecutive + tempo minimo di breach) per non
// sbatacchiare l'utente a ogni oscillazione dei delta.
//
// Il backstop reattivo resta in refreshTimes: "missed_connection" quando il
// feed conferma che il mezzo è già passato. Questo modulo agisce a monte.
package track

import (
	"fmt"
	"time"

	"gotransit/internal/engine"
	"gotransit/internal/stats"
	"gotransit/internal/transit"
)

type evRisk struct {
	Type           string  `json:"type"` // "risk"
	LegIndex       int     `json:"leg_index"`
	ConnectionStop *place  `json:"connection_stop,omitempty"`
	Probability    float64 `json:"probability"`
	Level          string  `json:"level"` // ok | warn | critical
}

// riskState tracks hysteresis per connection (keyed by leg index).
type riskState struct {
	belowWarn     int       // consecutive evaluations under risk_warn_live
	belowCritical int       // consecutive evaluations under risk_reroute_threshold
	breachAt      time.Time // first time under the reroute threshold
	lastLevel     string    // last emitted level (dedupe)
}

// residualSigma: uncertainty of an RT-tracked vehicle shrinks as it
// approaches — near the stop the probability converges to the deterministic
// verdict of refreshTimes.
func residualSigma(remaining time.Duration) float64 {
	s := 20 + 0.15*remaining.Seconds()
	if s > 120 {
		s = 120
	}
	return s
}

// evaluateRisk scores every remaining connection and acts on the risky ones.
// times comes from refreshTimes (RT-adjusted). Returns true when it fired a
// proactive reroute (the caller's cycle is done).
func (s *session) evaluateRisk(tt *transit.Timetable, o *transit.RTOverlay, now time.Time, times []legTime) bool {
	cfg := s.t.Cfg
	if !cfg.Routing.RiskRanking {
		return false
	}
	if s.risk == nil {
		s.risk = map[int]*riskState{}
	}

	byIdx := map[int]legTime{}
	for _, lt := range times {
		byIdx[lt.Index] = lt
	}

	var prevTransit int = -1
	walkSec := 0.0
	arrivalAtStop := now

	for i := s.legIdx; i < len(s.it.Legs); i++ {
		leg := &s.it.Legs[i]
		lt, hasLT := byIdx[i]
		if leg.Mode != "transit" {
			walkSec += float64(leg.DurationS)
			if hasLT {
				arrivalAtStop = lt.Arrive
			}
			continue
		}
		if i == s.legIdx && s.boarded {
			prevTransit = i
			walkSec = 0
			if hasLT {
				arrivalAtStop = lt.Arrive
			}
			continue // already on it: nothing to catch
		}

		depRT := leg.Depart
		if hasLT && !lt.Depart.IsZero() {
			depRT = lt.Depart
		}
		slack := depRT.Sub(arrivalAtStop).Seconds()

		conn := stats.Conn{
			SlackSec:      slack,
			WalkSec:       walkSec,
			WalkSigmaFrac: cfg.Routing.WalkSigmaFrac,
		}
		// Incoming side: the vehicle (or walk) bringing the rider here.
		if prevTransit >= 0 {
			pl := &s.it.Legs[prevTransit]
			plt, ok := byIdx[prevTransit]
			remaining := time.Duration(0)
			if ok {
				remaining = plt.Arrive.Sub(now)
			}
			if pl.Realtime || (ok && plt.Realtime) {
				conn.ArrSigma = residualSigma(remaining)
			} else if h, hok := s.legDist(tt, stats.Arrival, pl); hok {
				conn.ArrDist = h
			} else {
				conn.ArrSigma = 180
			}
		}
		// Outgoing side: the bus to catch.
		if leg.Realtime || (hasLT && lt.Realtime) {
			conn.DepSigma = residualSigma(depRT.Sub(now))
		} else if h, hok := s.legDist(tt, stats.Departure, leg); hok {
			conn.DepDist = h
		} else {
			conn.DepSigma = 180
		}

		p := stats.Catch(conn)
		st := s.risk[i]
		if st == nil {
			st = &riskState{}
			s.risk[i] = st
		}

		level := "ok"
		switch {
		case p < cfg.Realtime.RiskRerouteThreshold:
			level = "critical"
		case p < cfg.Realtime.RiskWarnLive:
			level = "warn"
		}

		// hysteresis bookkeeping
		if level == "critical" {
			if st.belowCritical == 0 {
				st.breachAt = now
			}
			st.belowCritical++
		} else {
			st.belowCritical = 0
			st.breachAt = time.Time{}
		}
		if level != "ok" {
			st.belowWarn++
		} else {
			st.belowWarn = 0
		}

		confirm := cfg.Realtime.RiskConfirmEvals
		if confirm < 1 {
			confirm = 1
		}

		// emit level transitions (warn/critical sustained; ok on recovery)
		emit := ""
		switch {
		case level == "critical" && st.belowCritical >= confirm && st.lastLevel != "critical":
			emit = "critical"
		case level == "warn" && st.belowWarn >= confirm && st.lastLevel != "warn" && st.lastLevel != "critical":
			emit = "warn"
		case level == "ok" && st.lastLevel != "" && st.lastLevel != "ok" && p >= cfg.Routing.RiskOk:
			emit = "ok"
		}
		if emit != "" {
			st.lastLevel = emit
			s.sink.Send(evRisk{
				Type: "risk", LegIndex: i,
				ConnectionStop: &place{StopID: leg.From.StopID, Name: leg.From.Name},
				Probability:    round3(p), Level: emit,
			})
		}

		// proactive reroute: sustained critical breach, hysteresis satisfied,
		// shared cooldown with the opportunistic path, never mid-boarding.
		if st.belowCritical >= confirm && !st.breachAt.IsZero() &&
			now.Sub(st.breachAt) >= cfg.Realtime.RiskMinBreach &&
			now.Sub(s.lastRR) >= betterArrivalCooldown &&
			s.rerouteAllowed(tt) && !s.boardingImminent(now) {
			msg := fmt.Sprintf("connection at %s is at risk (%.0f%%); rerouting before it is lost",
				leg.From.Name, p*100)
			if err := s.reroute(tt, o, now, "connection_risk", msg, 10*time.Minute); err == nil {
				return true
			}
		}

		prevTransit = i
		walkSec = 0
		if hasLT {
			arrivalAtStop = lt.Arrive
		} else {
			arrivalAtStop = leg.Arrive
		}
		// only the nearest two connections matter live; scoring far-future
		// boardings adds noise, not signal
		if i > s.legIdx+4 {
			break
		}
	}
	return false
}

// legDist queries the learned distribution for a leg's route at its local
// hour, centered when the trip already has realtime data.
func (s *session) legDist(tt *transit.Timetable, kind stats.Kind, leg *engine.Leg) (*stats.Hist, bool) {
	if s.t.E == nil || s.t.E.Stats == nil || leg.Route == nil {
		return nil, false
	}
	local := leg.Depart.In(tt.TZ)
	h, ok := s.t.E.Stats.Dist(kind, leg.Route.ID, stats.DayTypeOf(local), local.Hour())
	if !ok {
		return nil, false
	}
	if leg.Realtime {
		h = stats.Centered(h)
	}
	return &h, true
}

func round3(x float64) float64 { return float64(int(x*1000+0.5)) / 1000 }
