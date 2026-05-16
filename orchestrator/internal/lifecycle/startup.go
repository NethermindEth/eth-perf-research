package lifecycle

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/controller"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/journal"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/manifest"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/orchpb"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/referencef"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/rpc"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/target"
)

const (
	journalFilename  = "journal.bin"
	payloadsFilename = "payloads.rlp"
	pendingFilename  = "pending-batch.bin"

	// defaultResumeReorgTolerance bounds the head/journal-tail gap that a resume
	// will tolerate. After an EL-client crash + restart the chain rolls back to
	// its FlatDb reorg boundary; the journal then sits a few blocks ahead of (or,
	// if its last write didn't flush, behind) the chain head. Anything inside
	// this window is a recoverable resume — the master-nonce cursor is re-derived
	// from the chain so a bounded mismatch self-heals. NM's FlatDb.MaxReorgDepth
	// is 192; 256 leaves headroom.
	defaultResumeReorgTolerance = 256
)

// resumeReorgTolerance reads ORCH_RESUME_REORG_TOLERANCE or returns the default.
func resumeReorgTolerance() int64 {
	if raw := os.Getenv("ORCH_RESUME_REORG_TOLERANCE"); raw != "" {
		if v, err := strconv.ParseInt(raw, 10, 64); err == nil && v >= 0 {
			return v
		}
	}
	return defaultResumeReorgTolerance
}

// startupMode enumerates the run's startup decision.
type startupMode int

const (
	modeFresh startupMode = iota
	modeResume
)

func (m startupMode) String() string {
	switch m {
	case modeFresh:
		return "fresh"
	case modeResume:
		return "resume"
	default:
		return "unknown"
	}
}

// startupDecision is the outcome of resolveStartupMode.
type startupDecision struct {
	Mode             startupMode
	JournalPath      string
	PayloadsPath     string
	PendingPath      string
	ResumedFromBatch uint64
	TailRecord       *orchpb.Record
}

// loadTarget loads + parses the target YAML and wraps it in a controller.Target.
func loadTarget(path string) (*target.Target, *controller.Target, error) {
	t, err := target.Load(path)
	if err != nil {
		return nil, nil, fmt.Errorf("lifecycle: load target: %w", err)
	}
	ct := shareTargetFromTarget(t.Shares, t.TotalBytes, t.SHA256)
	return t, ct, nil
}

// loadReferenceF loads the reference-F YAML. When no path is supplied it
// returns the built-in design-v3 §B.1 default table; when a file IS supplied,
// any verb (or axis) absent from it is backfilled from that same default. This
// guarantees NewState never seeds an all-zero F-row — an all-zero row gives a
// verb a zero gradient in the controller's ‖Fx − r‖² objective, structurally
// trapping it as unselectable cold-start dead state.
func loadReferenceF(path string) (*referencef.ReferenceF, error) {
	if path == "" {
		return referencef.DefaultReferenceF(), nil
	}
	rf, err := referencef.Load(path)
	if err != nil {
		return nil, err
	}
	return referencef.WithDefaults(rf), nil
}

// resolveStartupMode inspects the state-dir and the RPC head to decide
// between fresh start, resume, or error.
func resolveStartupMode(ctx context.Context, stateDir string, rpcCli *rpc.Client) (*startupDecision, error) {
	jp := filepath.Join(stateDir, journalFilename)
	pp := filepath.Join(stateDir, payloadsFilename)
	pendp := filepath.Join(stateDir, pendingFilename)

	head, err := rpcCli.BlockByNumber(ctx, -1)
	if err != nil {
		return nil, fmt.Errorf("lifecycle: fetch latest head: %w", err)
	}

	jStat, jErr := os.Stat(jp)
	journalExists := jErr == nil && jStat.Size() > 0

	if !journalExists {
		if head.Number == 0 || os.Getenv("ORCH_ALLOW_NON_ZERO_FRESH_HEAD") == "1" {
			return &startupDecision{
				Mode:         modeFresh,
				JournalPath:  jp,
				PayloadsPath: pp,
				PendingPath:  pendp,
			}, nil
		}
		return nil, fmt.Errorf(
			"lifecycle: empty journal but chain head=%d; expected genesis (set ORCH_ALLOW_NON_ZERO_FRESH_HEAD=1 to bypass)",
			head.Number,
		)
	}

	// Journal exists — verify chain.
	count, _, err := journal.VerifyAll(jp)
	if err != nil {
		return nil, fmt.Errorf("lifecycle: verify journal: %w", err)
	}
	if count == 0 {
		return nil, fmt.Errorf("lifecycle: journal %q is non-empty but no records decode", jp)
	}

	tailRec, _, err := journal.Tail(jp)
	if err != nil {
		return nil, fmt.Errorf("lifecycle: read tail: %w", err)
	}
	if tailRec == nil || tailRec.ReplayCore == nil {
		return nil, errors.New("lifecycle: journal tail has no replay_core")
	}

	tailBlock := tailRec.ReplayCore.BlockNumber
	pendingExists := false
	if _, err := os.Stat(pendp); err == nil {
		pendingExists = true
	}

	switch {
	case head.Number == tailBlock:
		// clean resume
	case pendingExists && head.Number == tailBlock+1:
		// pending sidecar to reconcile — we accept either tail or tail+1.
	default:
		// Crash-resilient resume. After an EL-client crash + restart the chain
		// rolls back to its FlatDb reorg boundary, so head lands below (or, if
		// the journal's last write didn't flush, above) the journal tail. A gap
		// inside the reorg window is recoverable: the master-nonce cursor is
		// re-derived from the chain at startup, so the orchestrator picks up
		// from wherever the chain actually is. Only a gap larger than the
		// window indicates a genuinely different chain.
		tol := resumeReorgTolerance()
		delta := int64(head.Number) - int64(tailBlock)
		if delta < -tol || delta > tol {
			return nil, fmt.Errorf(
				"lifecycle: chain head=%d diverges from journal tail=%d by %d blocks (> tolerance %d); cannot resume",
				head.Number, tailBlock, delta, tol,
			)
		}
		slog.Warn("lifecycle: resuming across a reorg-window gap",
			"chain_head", head.Number, "journal_tail", tailBlock, "delta", delta, "tolerance", tol)
	}

	return &startupDecision{
		Mode:             modeResume,
		JournalPath:      jp,
		PayloadsPath:     pp,
		PendingPath:      pendp,
		ResumedFromBatch: tailRec.BatchId,
		TailRecord:       tailRec,
	}, nil
}

// hydrationResult summarises what hydrateStateFromTail reconstructed from the
// journal tail. A zero-valued result (all counts 0, Reconstructed false) means
// the controller cold-started — the caller logs the distinction.
type hydrationResult struct {
	Reconstructed bool // true if any F/σ/α cell was seeded from the tail
	CoeffCells    int  // F-matrix cells reconstructed
	AlphaCells    int  // α cells reconstructed
	SigmaCells    int  // σ cells reconstructed
}

// hydrateStateFromTail rebuilds the controller State by re-seeding from the
// reference-F and then folding the controller coefficients (F / α / σ) stored
// in the tail record's Observability block. The orchestrator persists the
// post-Apply F/α/σ on every batch (buildRecord -> Observability.CoeffsAfter /
// AlphaCurrent / SigmaInnov), so the tail carries the controller's full
// learned state as of the last committed batch.
//
// This is the resume-time reconstruction: without it every restart cold-starts
// the controller from reference-F / zero and re-explores the verb mix from
// scratch. Reconstruction is best-effort and never fails — if the tail has no
// Observability block, or the coefficient maps are empty/unparsable, the State
// keeps its cold-start seed values and the returned result reports
// Reconstructed=false so the caller can log a cold start.
func hydrateStateFromTail(state *controller.State, tail *orchpb.Record) hydrationResult {
	var res hydrationResult
	if state == nil || tail == nil || tail.Observability == nil {
		return res
	}
	obs := tail.Observability

	// Coeffs ("verb.axis" -> F).
	for k, v := range obs.CoeffsAfter {
		verb, ax, ok := splitFlatKey(k)
		if !ok {
			continue
		}
		if _, has := state.F[verb]; has {
			state.F[verb][ax] = v
			res.CoeffCells++
		}
	}
	for k, v := range obs.AlphaCurrent {
		verb, ax, ok := splitFlatKey(k)
		if !ok {
			continue
		}
		if _, has := state.Alpha[verb]; has {
			state.Alpha[verb][ax] = v
			res.AlphaCells++
		}
	}
	for k, v := range obs.SigmaInnov {
		verb, ax, ok := splitFlatKey(k)
		if !ok {
			continue
		}
		if _, has := state.Sigma[verb]; has {
			state.Sigma[verb][ax] = v
			res.SigmaCells++
		}
	}
	state.BatchID = tail.BatchId + 1
	res.Reconstructed = res.CoeffCells > 0 || res.AlphaCells > 0 || res.SigmaCells > 0
	return res
}

func splitFlatKey(k string) (string, controller.Axis, bool) {
	// last "." splits verb from axis.
	for i := len(k) - 1; i >= 0; i-- {
		if k[i] == '.' {
			verb := k[:i]
			ax := controller.Axis(k[i+1:])
			switch ax {
			case controller.AxisAccounts, controller.AxisStorage, controller.AxisCode:
				return verb, ax, true
			}
			return "", "", false
		}
	}
	return "", "", false
}

// reconcilePending inspects a pending-batch sidecar. If the chain advanced
// past the sidecar's recorded txs we conservatively delete it (best effort);
// otherwise we leave it and surface an error so an operator can investigate.
func reconcilePending(ctx context.Context, rpcCli *rpc.Client, decision *startupDecision) error {
	pending, err := journal.ReadPending(decision.PendingPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("lifecycle: read pending sidecar: %w", err)
	}
	// We only know the chain head — if it has advanced by exactly one block
	// past the tail, assume the pending batch landed and clear.
	if decision.TailRecord != nil {
		head, err := rpcCli.BlockByNumber(ctx, -1)
		if err == nil && head.Number > decision.TailRecord.ReplayCore.BlockNumber {
			if err := journal.ClearPending(decision.PendingPath); err != nil {
				return fmt.Errorf("lifecycle: clear stale pending: %w", err)
			}
			return nil
		}
	}
	return fmt.Errorf(
		"lifecycle: pending sidecar batch=%d verb=%s still active; remove or resume manually",
		pending.BatchId, pending.Verb,
	)
}

// newSessionID returns a 16-byte hex token used in journal records.
func newSessionID() string {
	var b [16]byte
	if _, err := io.ReadFull(rand.Reader, b[:]); err != nil {
		return fmt.Sprintf("session-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// freshManifest constructs a minimal manifest at startup. It's saved again
// at shutdown with the final fields populated.
func freshManifest(cfg Config, t *target.Target, chainIdentity [32]byte, sessionID string) *manifest.Manifest {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	return &manifest.Manifest{
		SchemaVersion:       manifest.SchemaVersion,
		SessionID:           sessionID,
		StartedAtISO:        now,
		GenesisSHA256:       cfg.GenesisSHA256,
		PluginGitSHA:        cfg.PluginGitSHA,
		NethermindCommitSHA: cfg.NethermindCommitSHA,
		DotnetRuntimeMajor:  cfg.DotnetRuntimeMajor,
		ChainIdentityHash:   hex.EncodeToString(chainIdentity[:]),
		TargetHistory: []manifest.TargetEntry{
			{
				AppliedAtBatch: 0,
				AppliedAtISO:   now,
				TargetSHA256:   t.SHA256,
				Shares:         cloneShares(t.Shares),
				TotalBytes:     t.TotalBytes,
			},
		},
	}
}

func cloneShares(in map[string]float64) map[string]float64 {
	out := make(map[string]float64, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
