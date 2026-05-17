// Package controller picks the next batch scenario and updates the F-coefficient
// matrix after each committed block.
package controller

import (
	"sync"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/config"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/mathx"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/referencef"
)

// Axis names the three state-growth dimensions tracked by the controller.
type Axis string

const (
	AxisAccounts Axis = "accounts"
	AxisStorage  Axis = "storage"
	AxisCode     Axis = "code"
)

// Axes is the canonical ordered triple used wherever axis iteration is needed.
var Axes = [3]Axis{AxisAccounts, AxisStorage, AxisCode}

// Observation is a snapshot of the three state-growth counters.
type Observation struct {
	AccountTrieBytes uint64
	StorageTrieBytes uint64
	CodeBytesTotal   uint64
	BlockNumber      uint64
}

// Target describes the desired axis proportions and the absolute byte budget.
type Target struct {
	Shares     map[Axis]float64 // axis -> proportion (must sum to 1.0)
	TotalBytes int64
	SHA256Hex  string
}

// ByteTarget returns the desired cumulative bytes for axis a at full progress.
func (t *Target) ByteTarget(a Axis) float64 {
	return t.Shares[a] * float64(t.TotalBytes)
}

// State holds the per-(verb, axis) controller matrices and supporting bookkeeping.
type State struct {
	Verbs         []string                    // ordered list of verb names
	F             map[string]map[Axis]float64 // verb -> axis -> F coefficient
	Sigma         map[string]map[Axis]float64 // verb -> axis -> innovation scale
	Alpha         map[string]map[Axis]float64 // verb -> axis -> learning rate
	AvgTxRLP      map[string]float64          // verb -> EWMA of bytes-per-tx
	BatchID       uint64
	ChainIdentity [32]byte // sha256(genesis || target) seed material
	Epsilon       float64  // ε-greedy exploration rate in [0, 1]

	// overshootMu guards the rolling overshoot window. Apply (commit goroutine)
	// writes via pushOvershoot; planner goroutines read via HasInstability.
	// F/Sigma/Alpha races are tolerated (benign — readers see either pre-Apply
	// or post-Apply state, both valid plan inputs); the ring buffer is not, so
	// it gets explicit synchronisation.
	overshootMu     sync.Mutex
	overshootWindow []bool
	overshootHead   int
	overshootFilled int

	// verbStatsMu guards the per-verb EWMA stats. UpdateVerbStats (commit
	// goroutine) takes the write lock; Pick (planner goroutines) takes the
	// read lock via GetVerbStats. The EWMA structs themselves are mutated
	// only under the write lock, so a concurrent read sees a consistent
	// snapshot.
	verbStatsMu sync.RWMutex
	VerbStats   map[string]*VerbStats

	// pickApplyMu serialises the depth-1 pipeline's cross-goroutine access to
	// F/Sigma/Alpha/AvgTxRLP/BatchID. The committer goroutine writes
	// these in Apply; the planner goroutine reads them in Pick. Held only for
	// the brief Pick-read and Apply-write — never during build/sign/commit.
	pickApplyMu sync.Mutex

	lastResidualL2 float64

	// debugPick gates the per-batch, per-verb structured debug log emitted by
	// Pick. Read once from $ORCH_DEBUG_PICK at construction so it can be
	// enabled on a live system without a rebuild. Off by default; when off,
	// Pick performs no extra formatting and its behaviour is unaffected.
	debugPick bool

	// cfg holds every controller tuning value. It is resolved once at startup
	// (config.Load) and threaded in via NewState so no controller constant is
	// package-level any more.
	cfg config.RunConfig
}

// CoeffBound returns the physical magnitude ceiling for an F-coefficient / σ
// from the resolved RunConfig. Used by the journal-tail hydration to validate
// reconstructed coefficients against the same bound the controller enforces.
func (s *State) CoeffBound() float64 { return s.cfg.Control.CoeffBound }

// LockState acquires the mutex guarding the controller matrices for the
// pipeline's Pick/Apply hand-off. Callers must pair it with UnlockState.
func (s *State) LockState() { s.pickApplyMu.Lock() }

// UnlockState releases the LockState mutex.
func (s *State) UnlockState() { s.pickApplyMu.Unlock() }

// NewState initialises a State from the reference-F seed values and the
// resolved RunConfig. verbs must be the complete ordered list. Every tuning
// value (ε, the σ/α seeds, the gas tables) is read from cfg — no controller
// constant is package-level.
func NewState(cfg config.RunConfig, verbs []string, ref *referencef.ReferenceF, chainIdentity [32]byte) *State {
	verbsCopy := make([]string, len(verbs))
	copy(verbsCopy, verbs)

	f := make(map[string]map[Axis]float64, len(verbs))
	sigma := make(map[string]map[Axis]float64, len(verbs))
	alpha := make(map[string]map[Axis]float64, len(verbs))
	avgTxRLP := make(map[string]float64, len(verbs))

	for _, verb := range verbsCopy {
		refPerVerb := ref.Verbs[verb] // map[string]float64 from YAML

		fRow := make(map[Axis]float64, 3)
		sigmaRow := make(map[Axis]float64, 3)
		alphaRow := make(map[Axis]float64, 3)

		for _, ax := range Axes {
			fRow[ax] = refPerVerb[string(ax)] // zero if not present
			sigmaRow[ax] = cfg.Control.DefaultSigma
			alphaRow[ax] = cfg.Control.AlphaMin
		}
		f[verb] = fRow
		sigma[verb] = sigmaRow
		alpha[verb] = alphaRow

		if rlp, ok := ref.AvgTxRLP[verb]; ok && rlp > 0 {
			avgTxRLP[verb] = rlp
		} else {
			avgTxRLP[verb] = cfg.Control.DefaultAvgTxRLP
		}
	}

	return &State{
		Verbs:          verbsCopy,
		F:              f,
		Sigma:          sigma,
		Alpha:          alpha,
		AvgTxRLP:       avgTxRLP,
		BatchID:        0,
		ChainIdentity:  chainIdentity,
		Epsilon:        cfg.Control.Epsilon,
		VerbStats:      make(map[string]*VerbStats),
		lastResidualL2: 0,
		debugPick:      config.EnvTruthy("ORCH_DEBUG_PICK"),
		cfg:            cfg,
	}
}

// alphaTuning derives the mathx adaptive-α parameters from the controller's
// resolved RunConfig.
func (s *State) alphaTuning() mathx.Tuning {
	ck := s.cfg.Control
	return mathx.Tuning{
		AMin:           ck.AlphaMin,
		AMax:           ck.AlphaMax,
		C:              ck.SigmoidCenter,
		K:              ck.SigmoidK,
		SigmaFloor:     ck.SigmaFloor,
		Eps:            ck.AlphaEpsilon,
		SigmaEWMADecay: ck.SigmaEWMADecay,
		AlphaEWMADecay: ck.AlphaEWMADecay,
		CoeffBound:     ck.CoeffBound,
	}
}

// BatchPlan is the output of Pick.
type BatchPlan struct {
	Verb          string
	DeadlineBytes int
	Mix           map[string]float64 // post-projection simplex weights (per verb)
	NMaxTxs       int                // hard cap derived from axis headroom; 0 = uncapped
}

// ResidualSnapshot summarises the per-axis residuals after a committed batch.
type ResidualSnapshot struct {
	PerAxis            map[Axis]float64
	L2Norm             float64
	DispatchedRLPBytes uint64
}

// AlphaSnapshot returns a copy of the current Alpha matrix.
// The returned map is verb -> axis -> value and is safe to read after the call returns.
func (s *State) AlphaSnapshot() map[string]map[Axis]float64 {
	out := make(map[string]map[Axis]float64, len(s.Alpha))
	for verb, row := range s.Alpha {
		rowCopy := make(map[Axis]float64, len(row))
		for ax, v := range row {
			rowCopy[ax] = v
		}
		out[verb] = rowCopy
	}
	return out
}

// SigmaSnapshot returns a copy of the current Sigma (innovation scale) matrix.
// The returned map is verb -> axis -> value and is safe to read after the call returns.
func (s *State) SigmaSnapshot() map[string]map[Axis]float64 {
	out := make(map[string]map[Axis]float64, len(s.Sigma))
	for verb, row := range s.Sigma {
		rowCopy := make(map[Axis]float64, len(row))
		for ax, v := range row {
			rowCopy[ax] = v
		}
		out[verb] = rowCopy
	}
	return out
}
