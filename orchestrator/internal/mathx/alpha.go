package mathx

import (
	"fmt"
	"math"
)

// Tuning carries the adaptive-α algorithm parameters. Before RunConfig these
// were package-level constants; they are now passed in so the orchestrator can
// resolve them once from config and thread them here. DefaultTuning() returns
// the historical constant values, so calling with it is behaviour-neutral.
type Tuning struct {
	AMin           float64 // lower bound on the learning rate
	AMax           float64 // upper bound on the learning rate
	C              float64 // sigmoid centre of the adaptive-α response
	K              float64 // sigmoid steepness
	SigmaFloor     float64 // lower clamp on the σ innovation tracker
	Eps            float64 // denominator floor in the absRatio computation
	SigmaEWMADecay float64 // EWMA decay for σ
	AlphaEWMADecay float64 // EWMA decay for α
	CoeffBound     float64 // physical magnitude ceiling for F and σ
}

// DefaultTuning returns the historical adaptive-α constants. See the field
// docs and the long-form rationale that previously lived on the package
// constants: the largest legitimate reference-F seed is 3500, so CoeffBound
// (1e6) sits ~285x above any genuine verb footprint yet far below the observed
// garbage value of 6.48e13 — it never clamps a real measurement but always
// catches divergence.
func DefaultTuning() Tuning {
	return Tuning{
		AMin:           0.02,
		AMax:           0.30,
		C:              0.08,
		K:              25.0,
		SigmaFloor:     1.0,
		Eps:            1000.0,
		SigmaEWMADecay: 0.9,
		AlphaEWMADecay: 0.7,
		CoeffBound:     1e6,
	}
}

// IsFiniteInRange reports whether v is finite and within [-CoeffBound,
// CoeffBound]. It is the gate for an observed per-tx effect before it feeds
// UpdateCoeff and for a coefficient reconstructed from the journal: a NaN/Inf
// or absurd value must never be folded into F.
func IsFiniteInRange(v, coeffBound float64) bool {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return false
	}
	return v >= -coeffBound && v <= coeffBound
}

// ClampCoeff bounds v to [-coeffBound, coeffBound]. A non-finite v collapses to
// 0 — a divergent coefficient is more dangerous left near-infinite than reset.
// The sign is preserved (storagerefundtx's storage F is legitimately negative).
func ClampCoeff(v, coeffBound float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	if v > coeffBound {
		return coeffBound
	}
	if v < -coeffBound {
		return -coeffBound
	}
	return v
}

// ClampSigma bounds the σ innovation-RMS tracker to [sigmaFloor, coeffBound].
// σ is non-negative by construction; a non-finite σ collapses to the floor.
func ClampSigma(v, sigmaFloor, coeffBound float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return sigmaFloor
	}
	if v < sigmaFloor {
		return sigmaFloor
	}
	if v > coeffBound {
		return coeffBound
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
// t carries the algorithm parameters (DefaultTuning() reproduces the historical
// behaviour).
func UpdateCoeff(fHat, observed, sigma, alpha float64, t Tuning) AlphaUpdate {
	for label, v := range map[string]float64{"fHat": fHat, "observed": observed, "sigma": sigma, "alpha": alpha} {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			panic(fmt.Sprintf("UpdateCoeff: %s must be finite, got %v", label, v))
		}
	}

	raw := observed - fHat
	denom := math.Max(sigma, t.SigmaFloor)
	// tanh saturates large innovations to ±sigma, preventing runaway updates.
	sat := sigma * math.Tanh(raw/denom)

	sigmaNew := t.SigmaEWMADecay*sigma + (1.0-t.SigmaEWMADecay)*math.Abs(raw)
	absRatio := math.Abs(sat) / math.Max(math.Abs(fHat), t.Eps)
	alphaTarget := t.AMin + (t.AMax-t.AMin)/2.0*(1.0+math.Tanh(t.K*(absRatio-t.C)))
	alphaNew := t.AlphaEWMADecay*alpha + (1.0-t.AlphaEWMADecay)*alphaTarget
	fNew := fHat + alphaNew*sat

	return AlphaUpdate{
		F:                   fNew,
		Sigma:               sigmaNew,
		Alpha:               alphaNew,
		SaturatedInnovation: sat,
		AbsRatio:            absRatio,
	}
}
