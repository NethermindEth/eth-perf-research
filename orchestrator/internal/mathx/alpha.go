package mathx

import (
	"fmt"
	"math"
)

const (
	aMin           = 0.02
	aMax           = 0.30
	c              = 0.08
	k              = 25.0
	sigmaFloor     = 1.0
	eps            = 1000.0
	sigmaEWMADecay = 0.9
	alphaEWMADecay = 0.7
)

// CoeffBound is the physical magnitude ceiling for a controller F-coefficient
// (bytes-per-tx on one state-growth axis) and for the σ innovation-RMS tracker.
//
// WHY 1e6: a single transaction's per-axis state footprint is physically
// bounded. The largest legitimate reference-F seed (design-v3 §B.1) is 3500
// (deploytx code-bytes). Even an adversarial contract-creation tx is bounded by
// the block gas limit / per-byte gas costs to the low tens of thousands of
// bytes. 1e6 sits ~285x above the largest real seed — comfortably above any
// genuine verb footprint — yet ~6.5e7x below the observed garbage value of
// 6.48e13 bytes/tx. It therefore never clamps a real measurement but always
// catches divergence. σ is an RMS of innovations on the same byte scale, so it
// shares the bound.
const CoeffBound = 1e6

// IsFiniteInRange reports whether v is finite and within [-CoeffBound, CoeffBound].
// It is the gate for an observed per-tx effect before it feeds UpdateCoeff and
// for a coefficient reconstructed from the journal: a NaN/Inf or absurd value
// must never be folded into F.
func IsFiniteInRange(v float64) bool {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return false
	}
	return v >= -CoeffBound && v <= CoeffBound
}

// ClampCoeff bounds v to [-CoeffBound, CoeffBound]. A non-finite v collapses to
// 0 — a divergent coefficient is more dangerous left near-infinite than reset.
// The sign is preserved (storagerefundtx's storage F is legitimately negative).
func ClampCoeff(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	if v > CoeffBound {
		return CoeffBound
	}
	if v < -CoeffBound {
		return -CoeffBound
	}
	return v
}

// ClampSigma bounds the σ innovation-RMS tracker to [sigmaFloor, CoeffBound].
// σ is non-negative by construction; a non-finite σ collapses to the floor.
func ClampSigma(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return sigmaFloor
	}
	if v < sigmaFloor {
		return sigmaFloor
	}
	if v > CoeffBound {
		return CoeffBound
	}
	return v
}

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
