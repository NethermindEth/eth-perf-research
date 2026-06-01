// Package controller picks the next batch scenario and updates the F-coefficient
// matrix after each committed block.
package controller

import (
	"encoding/binary"
	"math"
	"math/rand"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/config"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/mathx"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/referencef"
)

// Axis names the state-growth dimensions tracked by the controller. It is a
// uint8 enum whose value IS the slot index into an AxisVec — AxisAccounts==0,
// AxisStorage==1, AxisCode==2 — so an Axis can index a vector directly with no
// lookup table. The wire form (journal keys `verb.accounts`, Prometheus axis
// labels) is produced by String and parsed by ParseAxis, so on-disk and metric
// formats are unchanged. numAxes is the iota sentinel: it tracks the constant
// count automatically and is the single source for every fixed-size axis array.
type Axis uint8

const (
	AxisAccounts Axis = iota
	AxisStorage
	AxisCode
	numAxes
)

// Axes is the canonical ordered set used wherever axis iteration needs the Axis
// values themselves (e.g. building a map[Axis]…). Length tracks numAxes.
var Axes = [numAxes]Axis{AxisAccounts, AxisStorage, AxisCode}

// String renders the wire form used by journal keys and Prometheus labels.
func (a Axis) String() string {
	switch a {
	case AxisAccounts:
		return "accounts"
	case AxisStorage:
		return "storage"
	case AxisCode:
		return "code"
	}
	return "unknown"
}

// ParseAxis maps a wire-form axis name to its enum value. ok is false for any
// unrecognised string, so callers reject malformed journal/target keys instead
// of silently defaulting to AxisAccounts.
func ParseAxis(s string) (Axis, bool) {
	switch s {
	case "accounts":
		return AxisAccounts, true
	case "storage":
		return AxisStorage, true
	case "code":
		return AxisCode, true
	}
	return 0, false
}

// AxisVec is a fixed-length per-axis float64 vector. Its length (numAxes) and
// the slot↔Axis mapping live in exactly one place — the Axis enum — so callers
// index it with an Axis value and never restate the axis count or order. It has
// the same memory layout as the previous bare [numAxes]float64, so it stays
// alloc-free on the Pick/Apply hot path and the methods inline away.
type AxisVec [numAxes]float64

// Sum returns the sum of every axis slot.
func (v AxisVec) Sum() float64 {
	var s float64
	for _, x := range v {
		s += x
	}
	return s
}

// Sub returns the element-wise difference v - o.
func (v AxisVec) Sub(o AxisVec) AxisVec {
	var r AxisVec
	for a := range v {
		r[a] = v[a] - o[a]
	}
	return r
}

// L2 returns the Euclidean norm of the vector.
func (v AxisVec) L2() float64 {
	var s float64
	for _, x := range v {
		s += x * x
	}
	return math.Sqrt(s)
}

// VerbRow is the per-verb flat coefficient row replacing the nested
// map[string]map[Axis]float64 trio. Field order (F, Sigma, Alpha) matches the
// previous struct-level grouping; each AxisVec slot is indexed by Axis.
//
// Slots are read under pickApplyMu by Pick (planner goroutine) and written
// under the same mutex by Apply (committer goroutine). The lock-free snapshot
// path (FSnapshot / SigmaSnapshot / AlphaSnapshot, used by buildRecord and
// the Prometheus emitter) reads slots WITHOUT holding the mutex; to keep that
// reader race-detector clean against Apply's concurrent writes we go through
// atomicLoadFloat / atomicStoreFloat on every snapshot read and every Apply
// write. Pick stays plain-read because the Pick/Apply pair already serialise
// via pickApplyMu.
type VerbRow struct {
	Verb  string
	F     AxisVec
	Sigma AxisVec
	Alpha AxisVec
}

// atomicLoadFloat reads a float64 slot atomically by re-interpreting it as a
// uint64. The aliasing is safe because AxisVec (a [numAxes]float64) has the same
// layout as [numAxes]uint64 on every supported architecture and float64 /
// uint64 are both 8-byte word-aligned in array context.
func atomicLoadFloat(p *float64) float64 {
	bits := atomic.LoadUint64((*uint64)(unsafe.Pointer(p)))
	return math.Float64frombits(bits)
}

// atomicStoreFloat is the write-side counterpart of atomicLoadFloat.
func atomicStoreFloat(p *float64, v float64) {
	atomic.StoreUint64((*uint64)(unsafe.Pointer(p)), math.Float64bits(v))
}

// Observation is a snapshot of the three state-growth counters.
type Observation struct {
	AccountTrieBytes uint64
	StorageTrieBytes uint64
	CodeBytesTotal   uint64
}

// Target describes the desired axis proportions and the absolute byte budget.
type Target struct {
	Shares     map[Axis]float64 // axis -> proportion (must sum to 1.0)
	TotalBytes int64
}

// ByteTarget returns the desired cumulative bytes for axis a at full progress.
func (t *Target) ByteTarget(a Axis) float64 {
	return t.Shares[a] * float64(t.TotalBytes)
}

// pickScratch is the bag of working buffers Pick reuses across calls. It is
// allocated once on the State at NewState and reset (not re-allocated) each
// Pick. Sized to len(Verbs); never grown, so concurrent readers of `Rows` see
// a fixed slice header.
type pickScratch struct {
	fMat       [numAxes][]float64 // [axis][verb] F-matrix view
	score      []float64          // per-verb argmax score
	maxNTxs    []float64          // per-verb trajectory cap
	candidates []int              // eligible+feasible verb indices
}

func newPickScratch(n int) *pickScratch {
	p := &pickScratch{
		score:      make([]float64, n),
		maxNTxs:    make([]float64, n),
		candidates: make([]int, 0, n),
	}
	for a := range p.fMat {
		p.fMat[a] = make([]float64, n)
	}
	return p
}

// reset zeroes every scratch buffer in-place, then re-fills the F-matrix from
// the Rows table. Allocates nothing on a steady-state call.
func (p *pickScratch) reset(rows []VerbRow) {
	n := len(rows)
	for ax := range numAxes {
		row := p.fMat[ax]
		for j := range n {
			row[j] = rows[j].F[ax]
		}
	}
	for j := 0; j < n; j++ {
		p.score[j] = 0
		p.maxNTxs[j] = 0
	}
	p.candidates = p.candidates[:0]
}

// State holds the per-verb controller rows and supporting bookkeeping. F/σ/α
// live in a single flat `Rows []VerbRow` indexed by verb position (the same
// position as `Verbs[i]`), eliminating the nested-map allocations and the
// concurrent-map-iteration risk the previous shape carried.
type State struct {
	Verbs         []string
	Rows          []VerbRow
	verbIdx       map[string]int     // verb name -> index into Verbs/Rows
	AvgTxRLP      map[string]float64 // verb -> EWMA of bytes-per-tx
	BatchID       uint64
	ChainIdentity [32]byte // sha256(genesis || target) seed material

	// pick scratch buffers, pre-allocated at NewState and reset on every Pick.
	// They live on State (not the goroutine stack) so Pick allocates nothing on
	// the hot path; Pick runs single-threaded under pickApplyMu so the buffers
	// have no sharing hazard.
	pick *pickScratch

	// overshootMu guards the rolling overshoot window. Apply (commit goroutine)
	// writes via pushOvershoot; planner goroutines read via HasInstability.
	// F/Sigma/Alpha races are not the concern they were under the nested-map
	// representation — Rows is fixed-length post-NewState, and slot reads on an
	// AxisVec are word-atomic, so a snapshot reader sees either pre-Apply or
	// post-Apply values per slot, both valid.
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
	// the controller's mutable state. The committer goroutine writes Rows in
	// Apply; the planner goroutine reads them in Pick. Held only for the brief
	// Pick-read and Apply-write — never during build/sign/commit. Snapshot
	// readers (FSnapshot/SigmaSnapshot/AlphaSnapshot) do NOT take this lock:
	// Rows is fixed-length, and each VerbRow slot is read independently — a
	// torn-update reader sees either the pre-Apply or post-Apply value per
	// slot, both valid plan inputs.
	pickApplyMu sync.Mutex

	// debugPick gates per-batch per-verb debug logs (set by --debug-pick).
	debugPick bool

	// UseRatioScoring selects the ratio-on-trajectory verb-scoring formula in
	// Pick. Mirrored from cfg.Control.UseRatioScoring at construction so
	// callers can flip it post-NewState in tests without touching the
	// resolved RunConfig. Default false preserves the legacy residual-to-
	// end-target scoring.
	UseRatioScoring bool

	// cfg holds every controller tuning value. It is resolved once at startup
	// (config.Load) and threaded in via NewState so no controller constant is
	// package-level any more.
	cfg config.RunConfig

	// contractEligible is the set of verbs whose contract dependencies are
	// deployed-and-verified on this chain. It is computed once after bootstrap
	// and installed via SetEligibleVerbs; Pick restricts its candidate set to
	// these verbs (ineligible verbs get zero gradient weight, are skipped by
	// the simplex, and are NOT floored by R1). A nil map means "eligibility
	// not configured" — every verb is treated as eligible, which preserves the
	// pre-eligibility behaviour for tests and any caller that never calls
	// SetEligibleVerbs.
	contractEligible map[string]bool

	// refF retains the reference-F seed (per-verb expected per-axis effect
	// coefficients) NewState was constructed from. NewState folds the seed into
	// the live F matrix, where online learning then overwrites it — so once a
	// verb has learned, F no longer reflects the seed. The exploration floor
	// needs the *original* expected effect of a never-learned verb to decide
	// which axes it would grow, so the seed is kept here unmodified.
	refF *referencef.ReferenceF

	// rng drives ε-greedy verb exploration in Pick. Seeded deterministically
	// from ChainIdentity so a run's exploration sequence is reproducible. Pick
	// runs under pickApplyMu (LockState), so the RNG needs no extra guarding.
	rng *rand.Rand
}

// SetEligibleVerbs installs the contract-eligible verb set the lifecycle
// computes after bootstrap. It is called once, before the hot loop starts and
// before any Pick — Pick itself stays read-only on State. A verb absent from
// the set is structurally dead on this chain (its contract is not deployed)
// and is excluded from the gradient, the simplex, and the R1 entropy floor.
func (s *State) SetEligibleVerbs(eligible []string) {
	set := make(map[string]bool, len(eligible))
	for _, v := range eligible {
		set[v] = true
	}
	s.contractEligible = set
}

// isContractEligible reports whether verb may be selected this run. A nil
// eligibility set (SetEligibleVerbs never called) means every verb is eligible.
func (s *State) isContractEligible(verb string) bool {
	if s.contractEligible == nil {
		return true
	}
	return s.contractEligible[verb]
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

// GetF returns the current F coefficient for (verb, ax). Returns 0 when the
// verb is unknown or ax is out of range. Read-only; safe under pickApplyMu but
// does not take it.
func (s *State) GetF(verb string, ax Axis) float64 {
	i, ok := s.verbIdx[verb]
	if !ok || ax >= numAxes {
		return 0
	}
	return atomicLoadFloat(&s.Rows[i].F[ax])
}

// GetSigma mirrors GetF for the σ row.
func (s *State) GetSigma(verb string, ax Axis) float64 {
	i, ok := s.verbIdx[verb]
	if !ok || ax >= numAxes {
		return 0
	}
	return atomicLoadFloat(&s.Rows[i].Sigma[ax])
}

// GetAlpha mirrors GetF for the α row.
func (s *State) GetAlpha(verb string, ax Axis) float64 {
	i, ok := s.verbIdx[verb]
	if !ok || ax >= numAxes {
		return 0
	}
	return atomicLoadFloat(&s.Rows[i].Alpha[ax])
}

// SetF assigns a new F coefficient for (verb, ax). No-op when the verb is
// unknown — the journal-tail hydration relies on this to silently skip cells
// for verbs no longer in the registry. Callers writing under the Pick/Apply
// mutex (the hot path) should hold pickApplyMu; the hydration path runs
// before any pipeline goroutine starts so it needs no lock.
func (s *State) SetF(verb string, ax Axis, val float64) {
	i, ok := s.verbIdx[verb]
	if !ok || ax >= numAxes {
		return
	}
	atomicStoreFloat(&s.Rows[i].F[ax], val)
}

// SetSigma mirrors SetF for σ.
func (s *State) SetSigma(verb string, ax Axis, val float64) {
	i, ok := s.verbIdx[verb]
	if !ok || ax >= numAxes {
		return
	}
	atomicStoreFloat(&s.Rows[i].Sigma[ax], val)
}

// SetAlpha mirrors SetF for α.
func (s *State) SetAlpha(verb string, ax Axis, val float64) {
	i, ok := s.verbIdx[verb]
	if !ok || ax >= numAxes {
		return
	}
	atomicStoreFloat(&s.Rows[i].Alpha[ax], val)
}

// NewState initialises a State from the reference-F seed values and the
// resolved RunConfig. verbs must be the complete ordered list. Every tuning
// value (ε, the σ/α seeds, the gas tables) is read from cfg — no controller
// constant is package-level.
func NewState(cfg config.RunConfig, verbs []string, ref *referencef.ReferenceF, chainIdentity [32]byte) *State {
	verbsCopy := make([]string, len(verbs))
	copy(verbsCopy, verbs)

	rows := make([]VerbRow, len(verbsCopy))
	verbIdx := make(map[string]int, len(verbsCopy))
	avgTxRLP := make(map[string]float64, len(verbsCopy))

	for i, verb := range verbsCopy {
		refPerVerb := ref.Verbs[verb]
		row := VerbRow{Verb: verb}
		for axIdx, ax := range Axes {
			row.F[axIdx] = refPerVerb[ax.String()]
			row.Sigma[axIdx] = cfg.Control.DefaultSigma
			row.Alpha[axIdx] = cfg.Control.AlphaMin
		}
		rows[i] = row
		verbIdx[verb] = i

		if rlp, ok := ref.AvgTxRLP[verb]; ok && rlp > 0 {
			avgTxRLP[verb] = rlp
		} else {
			avgTxRLP[verb] = cfg.Control.DefaultAvgTxRLP
		}
	}

	return &State{
		Verbs:           verbsCopy,
		Rows:            rows,
		verbIdx:         verbIdx,
		AvgTxRLP:        avgTxRLP,
		BatchID:         0,
		ChainIdentity:   chainIdentity,
		pick:            newPickScratch(len(verbsCopy)),
		VerbStats:       make(map[string]*VerbStats),
		debugPick:       cfg.Control.DebugPick,
		UseRatioScoring: cfg.Control.UseRatioScoring,
		cfg:             cfg,
		refF:            ref,
		rng:             rand.New(rand.NewSource(int64(binary.BigEndian.Uint64(chainIdentity[:8])))),
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

// FSnapshot returns a verb -> axis -> F-coefficient map built from the current
// Rows. Slow path: O(len(Verbs)*3) per call. Used by the journal-record writer
// to serialise CoeffsAfter; safe to call without holding pickApplyMu because
// every slot read goes through atomicLoadFloat — a concurrent Apply (which
// uses atomicStoreFloat) cannot tear the load, and the result is either the
// pre-Apply or the post-Apply value per slot, both valid for the journal
// record's "F after the last batch I confirmed" semantics.
func (s *State) FSnapshot() map[string]map[Axis]float64 {
	out := make(map[string]map[Axis]float64, len(s.Rows))
	for i := range s.Rows {
		row := make(map[Axis]float64, 3)
		for axIdx, ax := range Axes {
			row[ax] = atomicLoadFloat(&s.Rows[i].F[axIdx])
		}
		out[s.Rows[i].Verb] = row
	}
	return out
}

// AlphaSnapshot mirrors FSnapshot for α.
func (s *State) AlphaSnapshot() map[string]map[Axis]float64 {
	out := make(map[string]map[Axis]float64, len(s.Rows))
	for i := range s.Rows {
		row := make(map[Axis]float64, 3)
		for axIdx, ax := range Axes {
			row[ax] = atomicLoadFloat(&s.Rows[i].Alpha[axIdx])
		}
		out[s.Rows[i].Verb] = row
	}
	return out
}

// SigmaSnapshot mirrors FSnapshot for σ.
func (s *State) SigmaSnapshot() map[string]map[Axis]float64 {
	out := make(map[string]map[Axis]float64, len(s.Rows))
	for i := range s.Rows {
		row := make(map[Axis]float64, 3)
		for axIdx, ax := range Axes {
			row[ax] = atomicLoadFloat(&s.Rows[i].Sigma[axIdx])
		}
		out[s.Rows[i].Verb] = row
	}
	return out
}
