package mathx

import (
	"fmt"
	"math"
)

const (
	aMin            = 0.02
	aMax            = 0.30
	c               = 0.08
	k               = 25.0
	sigmaFloor      = 1.0
	eps             = 1000.0
	sigmaEWMADecay  = 0.9
	alphaEWMADecay  = 0.7
)

// AlphaUpdate holds the outputs of a single UpdateCoeff call.
type AlphaUpdate struct {
	F                   float64
	Sigma               float64
	Alpha               float64
	SaturatedInnovation float64
	AbsRatio            float64
}

// UpdateCoeff applies the tanh-saturated adaptive-α innovation update for one
// (axis, verb) coefficient. fHat is the current estimate, observed is the new
// measurement, sigma is the running scale, alpha is the current learning rate.
func UpdateCoeff(fHat, observed, sigma, alpha float64) AlphaUpdate {
	for label, v := range map[string]float64{"fHat": fHat, "observed": observed, "sigma": sigma, "alpha": alpha} {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			panic(fmt.Sprintf("UpdateCoeff: %s must be finite, got %v", label, v))
		}
	}

	raw := observed - fHat
	denom := math.Max(sigma, sigmaFloor)
	// tanh saturates large innovations to ±sigma, preventing runaway updates.
	sat := sigma * math.Tanh(raw/denom)

	sigmaNew := sigmaEWMADecay*sigma + (1.0-sigmaEWMADecay)*math.Abs(raw)
	absRatio := math.Abs(sat) / math.Max(math.Abs(fHat), eps)
	alphaTarget := aMin + (aMax-aMin)/2.0*(1.0+math.Tanh(k*(absRatio-c)))
	alphaNew := alphaEWMADecay*alpha + (1.0-alphaEWMADecay)*alphaTarget
	fNew := fHat + alphaNew*sat

	return AlphaUpdate{
		F:                   fNew,
		Sigma:               sigmaNew,
		Alpha:               alphaNew,
		SaturatedInnovation: sat,
		AbsRatio:            absRatio,
	}
}
