// Package lifecycle wires every subsystem into the orchestrator's main loop.
//
// It owns the run-level invariants:
//   - exclusive state-dir lock,
//   - chain identity computation,
//   - fresh-vs-resume decision,
//   - journal/payloads/manifest persistence,
//   - shutdown / termination accounting.
//
// The hot loop lives in runLoop, which runs a depth-1 build-ahead pipeline: a
// planner goroutine Picks/builds/signs batch N+1 while a committer goroutine
// commits/journals batch N.
package lifecycle

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/controller"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/facade"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/journal"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/lock"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/manifest"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/metrics"
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
	Epsilon             float64
	TotalBatchBytes     int
	SensorPollInterval  time.Duration
	SensorDeadline      time.Duration
	Verbs               []string
	PluginGitSHA        string
	NethermindCommitSHA string
	DotnetRuntimeMajor  string
	MetricsAddr         string // TCP address for the Prometheus /metrics endpoint (default ":9101")
	// TargetEthPerGasWei is the fixed gas price (wei per gas) the orchestrator
	// pins the EIP-1559 fee policy to and uses to seed the controller's
	// per-verb EthPerTx cost model. Default 1 gwei (defaultTargetEthPerGasWei).
	TargetEthPerGasWei uint64
}

const (
	defaultEpsilon = 0.5
	// defaultTotalBatchBytes is the SECONDARY batch-size clamp passed to Pick;
	// the primary sizer is now the gas budget (gasFillFraction × blockGasLimit).
	// 7.5 MiB stays just below NM's BlockProductionMaxTxKilobytes=7.75 MiB once
	// per-tx RLP overhead is counted, so it remains a real cap for ultra-cheap
	// verbs while no longer starving mid/expensive verbs of gas fill.
	defaultTotalBatchBytes = 7680 * 1024
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
	if raw := os.Getenv("ORCH_RPC_TIMEOUT_S"); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 {
			rpcOpts = append(rpcOpts, rpc.WithTimeout(time.Duration(v)*time.Second))
		}
	} else {
		rpcOpts = append(rpcOpts, rpc.WithTimeout(120*time.Second))
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

	// 7a. Metrics registry + HTTP server.
	metricsReg := metrics.New()
	metricsReg.Epsilon.Set(cfg.Epsilon)
	metricsSrv := &http.Server{
		Addr:    cfg.MetricsAddr,
		Handler: metricsReg.Handler(),
	}
	go func() {
		slog.Info("lifecycle: metrics listening", "addr", cfg.MetricsAddr)
		if err := metricsSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Warn("lifecycle: metrics server error", "err", err)
		}
	}()
	defer func() {
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutCancel()
		if err := metricsSrv.Shutdown(shutCtx); err != nil {
			slog.Warn("lifecycle: metrics server shutdown", "err", err)
		}
		slog.Info("lifecycle: metrics server stopped")
	}()

	// 7. Open sensor, journal writer, payloads writer. Transaction building
	// is in-process (internal/verbs) — no builder subprocess pool.
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
	state := controller.NewState(cfg.Verbs, refF, chainIdentity, cfg.Epsilon, cfg.TargetEthPerGasWei)
	if decision.Mode == modeResume && decision.TailRecord != nil {
		hydrateStateFromTail(state, decision.TailRecord)
	}

	// 9. Facade context. Base address from $ORCH_BASE_ADDRESS (hex), else zeros.
	// Initial nonce from $ORCH_INITIAL_ADDRESS_CURSOR override, else queried from RPC.
	facadeCtx := buildFacadeContext(rawTarget, chainID, head.GasLimit)
	facadeCtx.SignerAddr = signr.Address()
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
		facadeCtx.AddressCursor.Store(v)
	} else {
		// The chain's master-signer nonce is authoritative for the address
		// cursor and survives EL-client crashes / reorgs. Derive it from the
		// chain for both fresh and resume — a resumed run after an EL restart
		// must pick up the rolled-back nonce, not the (stale) journal cursor.
		n, err := rpcCli.TransactionCount(ctx, signr.Address())
		if err != nil {
			return fmt.Errorf("lifecycle: query master nonce: %w", err)
		}
		facadeCtx.AddressCursor.Store(n)
		slog.Info("lifecycle: master nonce primed from RPC",
			"address", signr.Address().Hex(), "nonce", n, "mode", decision.Mode.String())
	}

	// 10. Initial observation from sensor. Loop until the plugin returns valid
	// trie stats — during a bootstrap scan, state-comp returns all zeros. The
	// cycle is sensor → scenario → bloat → sensor, so a blind start (zero stats)
	// would feed garbage into the controller. Wait for the plugin instead.
	initialSnap, err := waitForValidSensor(ctx, sens)
	if err != nil {
		return fmt.Errorf("lifecycle: initial sensor read: %w", err)
	}
	currentObs := &controller.Observation{
		AccountTrieBytes: initialSnap.AccountTrieBytes,
		StorageTrieBytes: initialSnap.StorageTrieBytes,
		CodeBytesTotal:   initialSnap.CodeBytesTotal,
		BlockNumber:      initialSnap.BlockNumber,
	}

	// 10a. Seed initial ObservedFlatBytes gauges.
	metricsReg.ObservedFlatBytes.WithLabelValues("accounts").Set(float64(currentObs.AccountTrieBytes))
	metricsReg.ObservedFlatBytes.WithLabelValues("storage").Set(float64(currentObs.StorageTrieBytes))
	metricsReg.ObservedFlatBytes.WithLabelValues("code").Set(float64(currentObs.CodeBytesTotal))

	// 11. Manifest.
	sessionID := newSessionID()
	metricsReg.SessionID.WithLabelValues(sessionID).Set(1)
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

	// 13. Prime fee policy once synchronously so the first batch has a valid
	// max-fee/tip pair before the planner loop runs, then start the background
	// refresher (single RPC per tick).
	if _, err := refreshFeePolicy(ctx, rpcCli, facadeCtx, cfg.TargetEthPerGasWei); err != nil {
		return fmt.Errorf("lifecycle: prime fee policy: %w", err)
	}
	feeCtx, feeCancel := context.WithCancel(ctx)
	var feeWG sync.WaitGroup
	feeWG.Add(1)
	go func() {
		defer feeWG.Done()
		if err := runFeePolicyLoop(feeCtx, rpcCli, facadeCtx, defaultFeePolicyInterval, cfg.TargetEthPerGasWei); err != nil && !errors.Is(err, context.Canceled) {
			slog.Warn("lifecycle: fee policy loop exited", "err", err)
		}
	}()
	defer func() {
		feeCancel()
		feeWG.Wait()
	}()

	// 14. Hot loop.
	dispatcher := facade.New(signr)
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
		metricsReg:   metricsReg,
	}

	termination, finalBlock := runLoop(ctx, cfg, deps, &ctrlTarget, currentObs, targetCh, rawTarget, mf)

	// 16. Finalise manifest.
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
	if len(cfg.Verbs) == 0 {
		cfg.Verbs = defaultVerbs()
	}
	if cfg.MetricsAddr == "" {
		cfg.MetricsAddr = ":9101"
	}
	if cfg.TargetEthPerGasWei == 0 {
		cfg.TargetEthPerGasWei = defaultTargetEthPerGasWei
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
// AddressCursor / SaltCursor / BlockGasLimit are zero-valued atomics; callers
// must use the typed Set / Reserve methods to seed/advance them.
func buildFacadeContext(t *target.Target, chainID, gasLimit uint64) *facade.Context {
	c := &facade.Context{
		BaseAddress:    make([]byte, 20),
		Revision:       0,
		AddressStride:  defaultAddressStride,
		ChainID:        chainID,
		GasLimit:       gasLimit,
		VerbGasFactors: map[string]float64{},
	}
	c.SetBlockGasLimit(gasLimit)
	return c
}

// runLoop drives the Pick → build → sign → Commit → Apply → journal pipeline
// until a termination condition fires. Returns the termination reason and the
// last committed block (or nil).
//
// Topology: a depth-1 build-ahead pipeline. A planner goroutine Picks, builds
// and signs batches and sends each prepared batch on a depth-1 buffered
// channel; a committer goroutine receives them and commits/journals. The buffer
// lets the planner prepare batch N+1 (the CPU-heavy build+sign of tens of
// thousands of secp256k1 txs) while the committer commits batch N — overlapping
// what was strictly serial dead time before.
//
// A single planner guarantees batches are produced in strict order, so there
// is no commit re-ordering, no shared-cursor race, and no min-heap. The only
// cross-goroutine shared state is the controller State (planner Picks, committer
// Applies — serialised by State's pickApplyMu) and the latest observation
// (guarded by pipeline.obsMu). Observation lag of one batch is by design:
// Pick(N+1) may run before Apply(N), so it sees the controller state as of
// batch N-1.
func runLoop(
	ctx context.Context,
	cfg Config,
	deps *batchDeps,
	ctrlTarget **controller.Target,
	currentObs *controller.Observation,
	targetCh chan *target.Target,
	rawTarget *target.Target,
	mf *manifest.Manifest,
) (string, *rpc.BlockHeader) {
	startBatchID := deps.resumedFrom
	if startBatchID > 0 {
		startBatchID++
	}

	// loopCtx is cancelled by either goroutine on a terminal condition; it
	// also fires on the parent ctx being cancelled. The planner stops
	// producing and the committer drains its channel and exits.
	loopCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	p := &pipeline{
		obs:          currentObs,
		ready:        make(chan *dispatched, 1),
		startBatchID: startBatchID,
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		p.planner(loopCtx, cancel, cfg, deps, ctrlTarget, targetCh, mf)
	}()
	go func() {
		defer wg.Done()
		p.committer(loopCtx, cancel, deps)
	}()
	wg.Wait()

	// Drain residual pending sidecar in case commit was interrupted mid-flight
	// (commitBatch normally clears it; on cancel-during-commit we may have left
	// a stale marker). Best-effort: errors are logged but don't fail the run.
	if err := journal.ClearPending(deps.pendingPath); err != nil {
		slog.Warn("lifecycle: clear pending on shutdown", "err", err)
	}

	reason := terminationSignal
	if r := p.termReason(); r != "" {
		reason = r
	}
	return reason, p.lastBlock()
}

// refreshTarget reloads the target and returns the controller projection.
func refreshTarget(t *target.Target) (*target.Target, *controller.Target, error) {
	if t == nil {
		return nil, nil, errors.New("lifecycle: nil target")
	}
	return t, shareTargetFromTarget(t.Shares, t.TotalBytes, t.SHA256), nil
}

// isPartialAcceptanceErr matches NM's testing_commitBlockV1 partial-acceptance
// error pattern, where the block builder included fewer txs than the
// orchestrator submitted. Form: "expected N transactions but only M were included".
func isPartialAcceptanceErr(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "transactions but only")
}

// parsePartialAcceptance pulls the expected/included counts out of NM's
// partial-acceptance error string. Returns zeros if the pattern is unexpected.
// Format: "expected <expected> transactions but only <included> were included".
func parsePartialAcceptance(err error) (expected, included uint64) {
	if err == nil {
		return 0, 0
	}
	msg := err.Error()
	const sep = "transactions but only"
	idx := strings.Index(msg, sep)
	if idx < 0 {
		return 0, 0
	}
	// Walk left to extract the integer before " transactions but only".
	left := strings.TrimRight(msg[:idx], " ")
	if li := strings.LastIndex(left, " "); li >= 0 {
		left = left[li+1:]
	}
	if v, perr := strconv.ParseUint(left, 10, 64); perr == nil {
		expected = v
	}
	// Walk right to extract the integer after "transactions but only ".
	right := strings.TrimLeft(msg[idx+len(sep):], " ")
	end := 0
	for end < len(right) && right[end] >= '0' && right[end] <= '9' {
		end++
	}
	if end > 0 {
		if v, perr := strconv.ParseUint(right[:end], 10, 64); perr == nil {
			included = v
		}
	}
	return expected, included
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
