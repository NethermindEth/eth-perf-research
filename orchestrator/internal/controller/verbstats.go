package controller

import (
	"math"
	"os"
	"strconv"
)

// verbStatsColdStartN is the sample count below which Pick uses the static
// baseline tables instead of EWMA values. Ten samples is enough for the EWMA
// mean to settle past its arithmetic warm-up phase.
const verbStatsColdStartN uint64 = 10

const defaultEWMAAlpha = 0.10

// EWMA is a single exponentially-weighted moving average with an arithmetic
// warm-up: the first verbStatsColdStartN samples are averaged plainly so the
// initial estimate doesn't anchor on the first observation. After that the
// classic alpha-blended recurrence takes over.
type EWMA struct {
	alpha float64
	mean  float64
	n     uint64
}

// NewEWMA builds an EWMA with the given decay parameter. Caller is responsible
// for choosing alpha; sensible range is (0, 1]. Out-of-range values fall back
// to defaultEWMAAlpha.
func NewEWMA(alpha float64) *EWMA {
	if alpha <= 0 || alpha > 1 || math.IsNaN(alpha) {
		alpha = defaultEWMAAlpha
	}
	return &EWMA{alpha: alpha}
}

// Update folds x into the running mean.
func (e *EWMA) Update(x float64) {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return
	}
	e.n++
	if e.n <= verbStatsColdStartN {
		// Arithmetic warm-up: mean_n = mean_{n-1} + (x - mean_{n-1})/n.
		e.mean += (x - e.mean) / float64(e.n)
		return
	}
	e.mean = e.alpha*x + (1-e.alpha)*e.mean
}

// Value returns the current mean. Zero when no samples have been seen.
func (e *EWMA) Value() float64 { return e.mean }

// Samples returns the count of Update calls accepted.
func (e *EWMA) Samples() uint64 { return e.n }

// VerbStats tracks per-verb runtime measurements used by Pick for gas-aware
// sizing once the cold-start window has passed.
type VerbStats struct {
	GasPerTx    *EWMA
	BytesPerTx  *EWMA
	BytesPerGas *EWMA
	Samples     uint64
}

func newVerbStats(alpha float64) *VerbStats {
	return &VerbStats{
		GasPerTx:    NewEWMA(alpha),
		BytesPerTx:  NewEWMA(alpha),
		BytesPerGas: NewEWMA(alpha),
	}
}

// ewmaAlpha reads the EWMA alpha from ORCH_EWMA_ALPHA or returns the default.
func ewmaAlpha() float64 {
	raw := os.Getenv("ORCH_EWMA_ALPHA")
	if raw == "" {
		return defaultEWMAAlpha
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || v <= 0 || v > 1 || math.IsNaN(v) {
		return defaultEWMAAlpha
	}
	return v
}

// UpdateVerbStats folds one batch's measurements into the running EWMAs.
// gasUsed is the block's total gas consumption attributed to this batch;
// txCount is the number of txs actually included; bytesPerTx is the per-tx
// dispatched RLP byte average (sumF). Calls with txCount == 0 or gasUsed == 0
// are ignored so a no-op batch can't pollute the EWMAs.
func (s *State) UpdateVerbStats(verb string, gasUsed, txCount uint64, bytesPerTx float64) {
	if txCount == 0 || gasUsed == 0 {
		return
	}
	gasPerTx := float64(gasUsed) / float64(txCount)
	if gasPerTx <= 0 || math.IsNaN(gasPerTx) || math.IsInf(gasPerTx, 0) {
		return
	}
	if bytesPerTx < 0 || math.IsNaN(bytesPerTx) || math.IsInf(bytesPerTx, 0) {
		bytesPerTx = 0
	}
	bytesPerGas := bytesPerTx / gasPerTx
	if math.IsNaN(bytesPerGas) || math.IsInf(bytesPerGas, 0) {
		bytesPerGas = 0
	}

	s.verbStatsMu.Lock()
	defer s.verbStatsMu.Unlock()
	if s.VerbStats == nil {
		s.VerbStats = make(map[string]*VerbStats)
	}
	vs, ok := s.VerbStats[verb]
	if !ok {
		vs = newVerbStats(ewmaAlpha())
		s.VerbStats[verb] = vs
	}
	vs.GasPerTx.Update(gasPerTx)
	vs.BytesPerTx.Update(bytesPerTx)
	vs.BytesPerGas.Update(bytesPerGas)
	vs.Samples = vs.GasPerTx.Samples()
}

// GetVerbStats returns a read-only snapshot of the per-verb stats. Returns nil
// when no samples have been observed for verb. The returned pointer references
// the live struct; callers must treat the returned values as immutable.
func (s *State) GetVerbStats(verb string) *VerbStats {
	s.verbStatsMu.RLock()
	defer s.verbStatsMu.RUnlock()
	if s.VerbStats == nil {
		return nil
	}
	return s.VerbStats[verb]
}

// bytesPerGasEstimate returns the EWMA bytes-per-gas estimate after cold-start
// or the baseline-derived value (sum_F / baseGas) before. Used by Pick's
// gas-aware bias.
func (s *State) bytesPerGasEstimate(verb string) float64 {
	vs := s.GetVerbStats(verb)
	if vs != nil && vs.Samples >= verbStatsColdStartN {
		if v := vs.BytesPerGas.Value(); v > 0 {
			return v
		}
	}
	bytesPerTx := 0.0
	if row, ok := s.F[verb]; ok {
		for _, ax := range Axes {
			bytesPerTx += row[ax]
		}
	}
	if bytesPerTx < 0 {
		bytesPerTx = 0
	}
	return bytesPerTx / float64(baselineGasPerVerb(verb))
}
