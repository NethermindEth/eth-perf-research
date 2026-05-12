package lifecycle

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
)

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

// loadReferenceF loads the reference-F YAML (or returns an empty one if path is "").
func loadReferenceF(path string) (*referencef.ReferenceF, error) {
	if path == "" {
		return &referencef.ReferenceF{
			Verbs:    map[string]map[string]float64{},
			AvgTxRLP: map[string]float64{},
		}, nil
	}
	return referencef.Load(path)
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
		if head.Number == 0 {
			return &startupDecision{
				Mode:         modeFresh,
				JournalPath:  jp,
				PayloadsPath: pp,
				PendingPath:  pendp,
			}, nil
		}
		return nil, fmt.Errorf(
			"lifecycle: empty journal but chain head=%d; expected genesis (refuse to bloat over an unknown chain)",
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
		return nil, fmt.Errorf(
			"lifecycle: chain head=%d does not match journal tail=%d (pending=%v); cannot resume",
			head.Number, tailBlock, pendingExists,
		)
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

// hydrateStateFromTail rebuilds the controller State by re-seeding from the
// reference-F and then folding any observability information stored in the
// tail record. This is a pragmatic resume — exact F is not reconstructible
// from the tail alone, but the tail's Coeffs/Alpha/Sigma maps let us bring
// the controller close to where it was.
func hydrateStateFromTail(state *controller.State, tail *orchpb.Record) {
	if state == nil || tail == nil || tail.Observability == nil {
		return
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
		}
	}
	for k, v := range obs.AlphaCurrent {
		verb, ax, ok := splitFlatKey(k)
		if !ok {
			continue
		}
		if _, has := state.Alpha[verb]; has {
			state.Alpha[verb][ax] = v
		}
	}
	for k, v := range obs.SigmaInnov {
		verb, ax, ok := splitFlatKey(k)
		if !ok {
			continue
		}
		if _, has := state.Sigma[verb]; has {
			state.Sigma[verb][ax] = v
		}
	}
	state.BatchID = tail.BatchId + 1
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
