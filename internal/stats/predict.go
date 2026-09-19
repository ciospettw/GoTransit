// Il modello di probabilità delle coincidenze.
//
// Una coincidenza è presa sse   D_dep + S ≥ D_arr + W   dove
//
//	S     = slack pianificato (partenza − arrivo alla fermata, già RT-adjusted),
//	D_arr = ritardo residuo del mezzo/camminata in arrivo,
//	D_dep = ritardo residuo del mezzo in partenza (un bus in ritardo AIUTA),
//	W     = scostamento della camminata, ~N(0, σw).
//
// Con gli istogrammi appresi: doppia convoluzione discreta (≤32×32 termini)
// col termine gaussiano della camminata via erf. Senza dati sufficienti:
// modello analitico gaussiano con σ derivata dall'headway della linea —
// le probabilità ci sono SEMPRE, i dati le raffinano soltanto.
package stats

import "math"

// Conn describes one connection to score.
type Conn struct {
	// SlackSec: scheduled/RT-adjusted seconds between reaching the stop and
	// the departure. May be negative during live re-evaluation.
	SlackSec float64
	// WalkSec: planned walking seconds inside the connection (0 = same-stop).
	WalkSec float64
	// WalkSigmaFrac scales walk uncertainty: σw = max(20, frac·WalkSec).
	WalkSigmaFrac float64

	// ArrDist / DepDist: learned residual-delay distributions (nil → analytic).
	// When the corresponding trip already has realtime data the distribution
	// should be CENTERED (see Centered): its mean is already inside SlackSec.
	ArrDist *Hist
	DepDist *Hist

	// Analytic fallbacks, used when the matching Dist is nil.
	// Sigmas ≤0 collapse that term to a deterministic 0.
	ArrSigma float64
	DepSigma float64
	// ForecastSigma is the small independent horizon/distance component. It
	// applies to learned distributions as well as analytic fallbacks.
	ForecastSigma float64
}

// Catch returns P(connection made) ∈ [0,1].
func Catch(c Conn) float64 {
	sw := math.Max(20, c.WalkSigmaFrac*c.WalkSec)
	if c.WalkSec <= 0 {
		sw = 15 // same-stop transfer: residual jitter only
	}

	// Fully analytic: Φ((S − (μa−μd)) / sqrt(σa²+σd²+σw²)); residual means
	// are zero (current delays are already folded into SlackSec).
	if c.ArrDist == nil && c.DepDist == nil {
		sigma := math.Sqrt(c.ArrSigma*c.ArrSigma + c.DepSigma*c.DepSigma + sw*sw + c.ForecastSigma*c.ForecastSigma)
		if sigma <= 0 {
			if c.SlackSec >= 0 {
				return 1
			}
			return 0
		}
		return phi(c.SlackSec / sigma)
	}

	// Histogram path: P = Σi Σj pA(i)·pD(j)·Φ((S + d_j − a_i)/σw'), where the
	// missing histogram side degrades to its analytic gaussian folded into σ.
	sigmaExtra := sw*sw + c.ForecastSigma*c.ForecastSigma
	arr := c.ArrDist
	dep := c.DepDist
	if arr == nil {
		sigmaExtra += c.ArrSigma * c.ArrSigma
	}
	if dep == nil {
		sigmaExtra += c.DepSigma * c.DepSigma
	}
	sigma := math.Sqrt(sigmaExtra)

	p := 0.0
	for i := 0; i < NumBins; i++ {
		pa := 1.0
		ai := 0.0
		if arr != nil {
			pa = arr.P[i]
			if pa == 0 {
				continue
			}
			ai = binMid(i)
		}
		for j := 0; j < NumBins; j++ {
			pd := 1.0
			dj := 0.0
			if dep != nil {
				pd = dep.P[j]
				if pd == 0 {
					continue
				}
				dj = binMid(j)
			}
			margin := c.SlackSec + dj - ai
			var term float64
			if sigma > 0 {
				term = phi(margin / sigma)
			} else if margin >= 0 {
				term = 1
			}
			p += pa * pd * term
			if dep == nil {
				break
			}
		}
		if arr == nil {
			break
		}
	}
	return clamp01(p)
}

// ForecastSigma modestly widens a prediction as its target gets farther away.
// Time contributes at most 30s and distance at most 15s: walking and actual
// transfer margins remain the dominant factors. Overdue ETAs never invert it.
func ForecastSigma(remainingSec, distanceM float64) float64 {
	return math.Min(30, 0.025*math.Max(0, remainingSec)) + math.Min(15, 0.003*math.Max(0, distanceM))
}

// Centered returns a copy of h shifted so its mean is zero: the residual
// distribution to use when the trip's current delay is already known (and
// therefore already included in the connection slack).
func Centered(h Hist) Hist {
	m := h.Mean()
	if m == 0 {
		return h
	}
	var out Hist
	out.Weight = h.Weight
	for i, p := range h.P {
		if p == 0 {
			continue
		}
		out.P[binOf(binMid(i)-m)] += p
	}
	return out
}

// HeadwaySigma derives the analytic delay σ from a service headway:
// frequent lines are regular, rare lines drift more. Clamped to [60s, 300s].
func HeadwaySigma(headwaySec float64) float64 {
	if headwaySec <= 0 {
		headwaySec = 900 // unknown: assume a 15-minute service
	}
	return math.Min(300, math.Max(60, 0.3*headwaySec))
}

// phi is the standard normal CDF.
func phi(z float64) float64 {
	return 0.5 * (1 + math.Erf(z/math.Sqrt2))
}

func clamp01(x float64) float64 {
	if x < 0 {
		return 0
	}
	if x > 1 {
		return 1
	}
	return x
}
