// Package lifecycle wires every subsystem into the orchestrator's main loop.
//
// It owns the run-level invariants:
//   - exclusive state-dir lock,
//   - chain identity computation,
//   - fresh-vs-resume decision,
//   - journal/payloads/manifest persistence,
//   - shutdown / termination accounting.
//
// The hot loop lives in runLoop and delegates per-iteration work to runOneBatch.
package lifecycle

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/builderpool"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/controller"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/facade"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/journal"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/lock"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/manifest"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/payloads"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/rpc"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/sensor"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/signer"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/target"
)

// Config is the top-level configuration for the lifecycle run.
type Config struct {
	RPCURL              string
	JWTPath             string
	StateDir            string
	TargetYAMLPath      string
	GenesisSHA256       string
	ReferenceFPath      string
	ManifestPath        string
	MaxBatches          int
	EnableProbe         bool
	DeployPrivateKey    string
	BuilderWorkerCmd    []string
	BuilderWorkers      int
	Epsilon             float64
	TotalBatchBytes     int
	SensorPollInterval  time.Duration
	SensorDeadline      time.Duration
	Verbs               []string
	PluginGitSHA        string
	NethermindCommitSHA string
	DotnetRuntimeMajor  string
}

const (
	defaultEpsilon         = 0.05
	defaultTotalBatchBytes = 8 * 1024 * 1024 // Engine API hard cap
	defaultAddressStride   = uint64(1) << 40
	terminationOvershoot   = "overshoot"
	terminationTargetMet   = "target_reached"
	terminationBatchLimit  = "batch_limit"
	terminationSignal      = "signal"
	terminationProbeFailed = "probe_failed"
	overshootWindowSize    = 5
	overshootMaxTrips      = 3
	overshootGrace         = 5
	overshootThreshold     = 0.20
)

// Run executes the orchestrator's main loop. It blocks until the context is
// cancelled, the target is reached, or an unrecoverable error occurs.
func Run(ctx context.Context, cfg Config) error {
	cfg = withDefaults(cfg)

	// 1. Acquire lock.
	lk, err := lock.Acquire(cfg.StateDir)
	if err != nil {
		return err
	}
	defer func() {
		if err := lk.Release(); err != nil {
			slog.Warn("lifecycle: lock release", "err", err)
		}
	}()

	// 2. Load target + reference-F.
	rawTarget, ctrlTarget, err := loadTarget(cfg.TargetYAMLPath)
	if err != nil {
		return err
	}
	refF, err := loadReferenceF(cfg.ReferenceFPath)
	if err != nil {
		return err
	}

	// 3. Chain identity.
	chainIdentity, err := computeChainIdentity(cfg.GenesisSHA256, rawTarget.SHA256)
	if err != nil {
		return err
	}

	// 4. RPC client + chain id + head.
	rpcOpts := []rpc.Option{}
	if cfg.JWTPath != "" {
		rpcOpts = append(rpcOpts, rpc.WithJWTFile(cfg.JWTPath))
	}
	rpcCli, err := rpc.NewClient(cfg.RPCURL, rpcOpts...)
	if err != nil {
		return fmt.Errorf("lifecycle: rpc client: %w", err)
	}
	chainID, err := rpcCli.ChainID(ctx)
	if err != nil {
		return fmt.Errorf("lifecycle: chain id: %w", err)
	}
	head, err := rpcCli.BlockByNumber(ctx, -1)
	if err != nil {
		return fmt.Errorf("lifecycle: head: %w", err)
	}
	slog.Info("lifecycle: connected to el client",
		"chain_id", chainID,
		"head_block", head.Number,
		"gas_limit", head.GasLimit,
	)

	// 5. Decide fresh vs resume.
	decision, err := resolveStartupMode(ctx, cfg.StateDir, rpcCli)
	if err != nil {
		return err
	}
	slog.Info("lifecycle: startup mode resolved", "mode", decision.Mode.String(), "resumed_from", decision.ResumedFromBatch)

	if decision.Mode == modeResume {
		if err := reconcilePending(ctx, rpcCli, decision); err != nil {
			return err
		}
	}

	// 6. Build signer.
	deployKey := cfg.DeployPrivateKey
	if deployKey == "" {
		deployKey = os.Getenv("ORCH_DEPLOY_PRIVATE_KEY")
	}
	if deployKey == "" {
		return errors.New("lifecycle: deploy private key not set (--deploy-private-key or $ORCH_DEPLOY_PRIVATE_KEY)")
	}
	signr, err := signer.New(deployKey)
	if err != nil {
		return fmt.Errorf("lifecycle: signer: %w", err)
	}

	// 7. Open builder pool, sensor, journal writer, payloads writer.
	pool, err := builderpool.Open(ctx, builderpool.Config{
		Cmd:     cfg.BuilderWorkerCmd,
		Workers: cfg.BuilderWorkers,
	})
	if err != nil {
		return fmt.Errorf("lifecycle: builder pool: %w", err)
	}
	defer pool.Close()

	sensorOpts := []sensor.SensorOption{}
	if cfg.SensorPollInterval > 0 {
		sensorOpts = append(sensorOpts, sensor.WithPollInterval(cfg.SensorPollInterval))
	}
	if cfg.SensorDeadline > 0 {
		sensorOpts = append(sensorOpts, sensor.WithDeadline(cfg.SensorDeadline))
	}
	sens := sensor.New(rpcCli, sensorOpts...)

	jw, err := journal.OpenWriter(decision.JournalPath)
	if err != nil {
		return fmt.Errorf("lifecycle: journal writer: %w", err)
	}
	defer jw.Close()

	pw, err := payloads.OpenWriter(decision.PayloadsPath)
	if err != nil {
		return fmt.Errorf("lifecycle: payloads writer: %w", err)
	}
	defer pw.Close()

	// 8. Build controller state, hydrating from tail on resume.
	state := controller.NewState(cfg.Verbs, refF, chainIdentity, cfg.Epsilon)
	if decision.Mode == modeResume && decision.TailRecord != nil {
		hydrateStateFromTail(state, decision.TailRecord)
	}

	// 9. Facade context. Base address from $ORCH_BASE_ADDRESS (hex), else zeros.
	// Initial nonce from $ORCH_INITIAL_ADDRESS_CURSOR override, else queried from RPC.
	facadeCtx := buildFacadeContext(rawTarget, chainID, head.GasLimit)
	if hex := strings.TrimPrefix(os.Getenv("ORCH_BASE_ADDRESS"), "0x"); hex != "" {
		b, err := decodeBaseAddress(hex)
		if err != nil {
			return fmt.Errorf("lifecycle: ORCH_BASE_ADDRESS: %w", err)
		}
		facadeCtx.BaseAddress = b
	}
	if raw := os.Getenv("ORCH_INITIAL_ADDRESS_CURSOR"); raw != "" {
		v, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			return fmt.Errorf("lifecycle: ORCH_INITIAL_ADDRESS_CURSOR: %w", err)
		}
		facadeCtx.AddressCursor = v
	} else if decision.Mode == modeFresh {
		n, err := rpcCli.TransactionCount(ctx, signr.Address())
		if err != nil {
			return fmt.Errorf("lifecycle: query master nonce: %w", err)
		}
		facadeCtx.AddressCursor = n
		slog.Info("lifecycle: master nonce primed from RPC", "address", signr.Address().Hex(), "nonce", n)
	}

	// 10. Initial observation from sensor. Use expected=0: we don't care which
	// block we start from — the sensor often lags chain head by 1 block, and
	// the controller will see post-commit deltas anyway.
	initialSnap, err := sens.Read(ctx, 0)
	if err != nil {
		return fmt.Errorf("lifecycle: initial sensor read: %w", err)
	}
	currentObs := &controller.Observation{
		AccountTrieBytes: initialSnap.AccountTrieBytes,
		StorageTrieBytes: initialSnap.StorageTrieBytes,
		CodeBytesTotal:   initialSnap.CodeBytesTotal,
		BlockNumber:      initialSnap.BlockNumber,
	}

	// 11. Manifest.
	sessionID := newSessionID()
	manifestPath := cfg.ManifestPath
	if manifestPath == "" {
		manifestPath = cfg.StateDir + "/run-manifest.json"
	}
	mf := freshManifest(cfg, rawTarget, chainIdentity, sessionID)
	if err := mf.Save(manifestPath); err != nil {
		return fmt.Errorf("lifecycle: initial manifest save: %w", err)
	}

	// 12. Target watcher.
	targetCh := make(chan *target.Target, 1)
	watcher, err := target.NewWatcher(ctx, cfg.TargetYAMLPath, func(t *target.Target) {
		select {
		case targetCh <- t:
		default:
		}
	})
	if err != nil {
		slog.Warn("lifecycle: target watcher disabled", "err", err)
	} else {
		defer watcher.Close()
	}

	// 13. Hot loop.
	dispatcher := facade.New(pool, signr)
	deps := &batchDeps{
		rpc:          rpcCli,
		sensor:       sens,
		dispatcher:   dispatcher,
		jw:           jw,
		pw:           pw,
		state:        state,
		facadeCtx:    facadeCtx,
		pendingPath:  decision.PendingPath,
		sessionID:    sessionID,
		resumedFrom:  decision.ResumedFromBatch,
		target:       ctrlTarget,
		targetDigest: targetSha256Bytes(rawTarget.SHA256),
	}

	termination, finalBlock := runLoop(ctx, cfg, deps, &ctrlTarget, &currentObs, targetCh, rawTarget, mf)

	// 14. Finalise manifest.
	finishISO := time.Now().UTC().Format(time.RFC3339Nano)
	mf.FinishedAtISO = finishISO
	mf.Terminated = termination
	mf.BatchCount = int(state.BatchID)
	tailHash := jw.CurrentChainHash()
	mf.LastChainHashCheckpoint = hexBytes(tailHash[:])
	if finalBlock != nil {
		mf.FinalStateRoot = finalBlock.StateRoot.Hex()
	}
	if err := mf.Save(manifestPath); err != nil {
		slog.Error("lifecycle: final manifest save", "err", err)
		return err
	}

	slog.Info("lifecycle: run complete",
		"termination", termination,
		"batches", state.BatchID,
	)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return nil
}

func withDefaults(cfg Config) Config {
	if cfg.Epsilon <= 0 {
		cfg.Epsilon = defaultEpsilon
	}
	if cfg.TotalBatchBytes <= 0 {
		cfg.TotalBatchBytes = defaultTotalBatchBytes
	}
	if len(cfg.BuilderWorkerCmd) == 0 {
		cfg.BuilderWorkerCmd = []string{"python", "-m", "builder_worker"}
	}
	if len(cfg.Verbs) == 0 {
		cfg.Verbs = defaultVerbs()
	}
	return cfg
}

func defaultVerbs() []string {
	return []string{
		"eoatx", "calltx", "deploytx", "factorydeploytx",
		"storagespam", "erc20_bloater", "erc20tx", "uniswap_swaps",
		"storagerefundtx", "gasburnertx", "evm_fuzz", "noop",
	}
}

// decodeBaseAddress parses a 20-byte hex string (0x optional, padded left with zeros).
func decodeBaseAddress(s string) ([]byte, error) {
	s = strings.TrimPrefix(s, "0x")
	if len(s)%2 == 1 {
		s = "0" + s
	}
	raw, err := hex.DecodeString(s)
	if err != nil {
		return nil, err
	}
	if len(raw) > 20 {
		return nil, fmt.Errorf("base address > 20 bytes (got %d)", len(raw))
	}
	out := make([]byte, 20)
	copy(out[20-len(raw):], raw)
	return out, nil
}

// buildFacadeContext creates a fresh facade.Context for this run.
func buildFacadeContext(t *target.Target, chainID, gasLimit uint64) *facade.Context {
	return &facade.Context{
		BaseAddress:    make([]byte, 20),
		Revision:       0,
		AddressStride:  defaultAddressStride,
		ChainID:        chainID,
		GasLimit:       gasLimit,
		BlockGasLimit:  gasLimit,
		AddressCursor:  0,
		SaltCursor:     0,
		VerbGasFactors: map[string]float64{},
	}
}

// runLoop drives runOneBatch repeatedly until a termination condition fires.
// Returns the termination reason and the last committed block (or nil).
func runLoop(
	ctx context.Context,
	cfg Config,
	deps *batchDeps,
	ctrlTarget **controller.Target,
	currentObs **controller.Observation,
	targetCh chan *target.Target,
	rawTarget *target.Target,
	mf *manifest.Manifest,
) (string, *rpc.BlockHeader) {
	var lastBlock *rpc.BlockHeader
	batchID := deps.resumedFrom
	if batchID > 0 {
		batchID++ // continue after the resume point
	}

	for {
		select {
		case <-ctx.Done():
			return terminationSignal, lastBlock
		case newT := <-targetCh:
			_, newCtrl, _ := refreshTarget(newT)
			*ctrlTarget = newCtrl
			deps.target = newCtrl
			deps.targetDigest = targetSha256Bytes(newT.SHA256)
			mf.TargetHistory = append(mf.TargetHistory, manifest.TargetEntry{
				AppliedAtBatch: int(batchID),
				AppliedAtISO:   time.Now().UTC().Format(time.RFC3339Nano),
				TargetSHA256:   newT.SHA256,
				Shares:         cloneShares(newT.Shares),
				TotalBytes:     newT.TotalBytes,
			})
			slog.Info("lifecycle: target reloaded", "sha", newT.SHA256[:8], "total_bytes", newT.TotalBytes)
		default:
		}

		if deps.state.HasInstability(overshootThreshold, overshootWindowSize, overshootMaxTrips, overshootGrace) {
			return terminationOvershoot, lastBlock
		}
		if reachedTarget(*currentObs, *ctrlTarget) {
			return terminationTargetMet, lastBlock
		}
		if cfg.MaxBatches > 0 && batchID >= uint64(cfg.MaxBatches) {
			return terminationBatchLimit, lastBlock
		}

		res, err := runOneBatch(ctx, deps, batchID, *currentObs)
		if err != nil {
			if ctx.Err() != nil {
				return terminationSignal, lastBlock
			}
			slog.Error("lifecycle: batch failed", "batch_id", batchID, "err", err)
			return "error: " + err.Error(), lastBlock
		}
		if !res.committed {
			// Forward-progress safety: skip without advancing batchID, but
			// avoid a tight loop by sleeping briefly.
			select {
			case <-ctx.Done():
				return terminationSignal, lastBlock
			case <-time.After(50 * time.Millisecond):
			}
			continue
		}

		lastBlock = res.blockHeader
		*currentObs = res.postObs
		batchID++
	}
}

// refreshTarget reloads the target and returns the controller projection.
func refreshTarget(t *target.Target) (*target.Target, *controller.Target, error) {
	if t == nil {
		return nil, nil, errors.New("lifecycle: nil target")
	}
	return t, shareTargetFromTarget(t.Shares, t.TotalBytes, t.SHA256), nil
}

func hexBytes(b []byte) string {
	const hexChars = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, v := range b {
		out[i*2] = hexChars[v>>4]
		out[i*2+1] = hexChars[v&0x0f]
	}
	return string(out)
}
