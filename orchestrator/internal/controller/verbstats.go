package controller

import (
	"math"
)

// EWMA is a single exponentially-weighted moving average with an arithmetic
// warm-up: the first coldStartN samples are averaged plainly so the initial
// estimate doesn't anchor on the first observation. After that the classic
// alpha-blended recurrence takes over. Both alpha and coldStartN are config
// values threaded in by NewEWMA so no EWMA tuning constant is package-level.
type EWMA struct {
	alpha     float64
	coldStart uint64
	mean      float64
	n         uint64
}

// NewEWMA builds an EWMA with the given decay parameter and cold-start sample
// count. Caller is responsible for choosing alpha; sensible range is (0, 1].
// Both are sourced from config.RunConfig (Control.EWMAAlpha,
// Control.VerbStatsColdStartN).
func NewEWMA(alpha float64, coldStartN uint64) *EWMA {
	if alpha <= 0 || alpha > 1 || math.IsNaN(alpha) {
		alpha = 0.10
	}
	return &EWMA{alpha: alpha, coldStart: coldStartN}
}

// Update folds x into the running mean.
func (e *EWMA) Update(x float64) {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return
	}
	e.n++
	if e.n <= e.coldStart {
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
// sizing once the cold-start window has passed. Average tx size is tracked by
// the separate State.AvgTxRLP EWMA (design-v3 §2.6, batch-byte sizing) — this
// struct does not duplicate it.
type VerbStats struct {
	GasPerTx    *EWMA
	BytesPerGas *EWMA
	Samples     uint64
}

func newVerbStats(alpha float64, coldStartN uint64) *VerbStats {
	return &VerbStats{
		GasPerTx:    NewEWMA(alpha, coldStartN),
		BytesPerGas: NewEWMA(alpha, coldStartN),
	}
}

// UpdateVerbStats folds one batch's measurements into the running EWMAs.
// gasUsed is the block's total gas consumption attributed to this batch;
// txCount is the number of txs actually included; bytesPerTx is the per-tx
// dispatched RLP byte average (sumF), used only to derive BytesPerGas. Calls
// with txCount == 0 or gasUsed == 0 are ignored so a no-op batch can't pollute
// the EWMAs.
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
		vs = newVerbStats(s.cfg.Control.EWMAAlpha, s.cfg.Control.VerbStatsColdStartN)
		s.VerbStats[verb] = vs
	}
	vs.GasPerTx.Update(gasPerTx)
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
