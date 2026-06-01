// Package lifecycle wires every subsystem into the orchestrator's main loop.
//
// It owns the run-level invariants:
//   - exclusive state-dir lock,
//   - chain identity computation,
//   - fresh-vs-resume decision,
//   - journal/payloads/manifest persistence,
//   - shutdown / termination accounting.
//
// The hot loop lives in runLoop: a strictly sequential, sensor-paced cycle.
// Each iteration commits exactly one block, then blocks until the
// StateComposition trie-diff has folded that block into the sensor, and only
// then Picks the next batch off that fresh observation.
package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/config"
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

// Config is the top-level configuration for the lifecycle run. It carries the
// CLI-supplied paths/SHAs plus the resolved RunConfig holding every tunable
// value (controller / cost / run-loop / sensor). Run resolves RunConfig from
// target.yaml + env once at startup and threads it everywhere.
type Config struct {
	RPCURL              string
	SensorRPCURL        string // optional: separate RPC endpoint for statecomp_get (sidecar)
	JWTPath             string
	StateDir            string
	TargetYAMLPath      string
	GenesisSHA256       string
	ReferenceFPath      string
	ManifestPath        string
	MaxBatches          int
	DeployPrivateKey    string
	Verbs               []string
	PluginGitSHA        string
	NethermindCommitSHA string
	DotnetRuntimeMajor  string

	// AllowNonZeroFreshHead allows an empty-journal startup against a chain
	// whose head block != 0. Used by re-baseline runs where the snapshot was
	// imported from another node. Threaded into resolveStartupMode, where it
	// ORs with the ORCH_ALLOW_NON_ZERO_FRESH_HEAD env var.
	AllowNonZeroFreshHead bool

	// Run is the resolved RunConfig. If left zero-valued, Run() resolves it
	// from TargetYAMLPath + environment via config.Load.
	Run config.RunConfig
}

const (
	terminationOvershoot  = "overshoot"
	terminationTargetMet  = "target_reached"
	terminationBatchLimit = "batch_limit"
	terminationSignal     = "signal"
)

// Run executes the orchestrator's main loop. It blocks until the context is
// cancelled, the target is reached, or an unrecoverable error occurs.
func Run(ctx context.Context, cfg Config) error {
	cfg, err := withDefaults(cfg)
	if err != nil {
		return err
	}
	rc := cfg.Run

	lk, err := lock.Acquire(cfg.StateDir)
	if err != nil {
		return err
	}
	defer func() {
		if err := lk.Release(); err != nil {
			slog.Warn("lifecycle: lock release", "err", err)
		}
	}()

	rawTarget, ctrlTarget, err := loadTarget(cfg.TargetYAMLPath)
	if err != nil {
		return err
	}
	refF, err := loadReferenceF(cfg.ReferenceFPath)
	if err != nil {
		return err
	}

	chainIdentity, err := computeChainIdentity(cfg.GenesisSHA256, rawTarget.SHA256)
	if err != nil {
		return err
	}

	rpcOpts := []rpc.Option{}
	if cfg.JWTPath != "" {
		rpcOpts = append(rpcOpts, rpc.WithJWTFile(cfg.JWTPath))
	}
	rpcOpts = append(rpcOpts, rpc.WithTimeout(time.Duration(rc.Run.RPCTimeoutS)*time.Second))
	rpcCli, err := rpc.NewClient(cfg.RPCURL, rpcOpts...)
	if err != nil {
		return fmt.Errorf("lifecycle: rpc client: %w", err)
	}
	// In the v19 architecture statecomp_get is served by the external sidecar,
	// not by Nethermind. When a separate URL is provided, the sensor talks to
	// that endpoint while everything else (commits, queries, codeAt) keeps
	// using the main rpc client.
	sensorRPC := rpcCli
	if cfg.SensorRPCURL != "" && cfg.SensorRPCURL != cfg.RPCURL {
		sensorRPC, err = rpc.NewClient(cfg.SensorRPCURL,
			rpc.WithTimeout(time.Duration(rc.Run.RPCTimeoutS)*time.Second))
		if err != nil {
			return fmt.Errorf("lifecycle: sensor rpc client: %w", err)
		}
		slog.Info("lifecycle: sensor using dedicated rpc endpoint", "sensor_rpc_url", cfg.SensorRPCURL)
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

	decision, err := resolveStartupMode(ctx, cfg.StateDir, rpcCli, rc.Run.ResumeReorgTolerance, cfg.AllowNonZeroFreshHead)
	if err != nil {
		return err
	}
	slog.Info("lifecycle: startup mode resolved", "mode", decision.Mode.String(), "resumed_from", decision.ResumedFromBatch)

	if decision.Mode == modeResume {
		if err := reconcilePending(ctx, rpcCli, decision); err != nil {
			return err
		}
	}

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

	metricsReg := metrics.New()
	mux := http.NewServeMux()
	mux.Handle("/metrics", metricsReg.Handler())
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	metricsSrv := &http.Server{
		Addr:    rc.Run.MetricsAddr,
		Handler: mux,
	}
	go func() {
		slog.Info("lifecycle: metrics listening", "addr", rc.Run.MetricsAddr)
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

	// Transaction building is in-process (internal/verbs) — no builder subprocess pool.
	sens := sensor.New(sensorRPC,
		sensor.WithPollInterval(time.Duration(rc.Run.SensorPollIntervalMS)*time.Millisecond),
		sensor.WithDeadline(time.Duration(rc.Run.SensorDeadlineMS)*time.Millisecond),
	)

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

	// On resume the controller's F / σ / α are reconstructed from the journal
	// tail so the controller does not re-explore the verb mix from scratch every
	// restart; if the tail carries no coefficients the State keeps its cold-start seed.
	state := controller.NewState(rc, cfg.Verbs, refF, chainIdentity)
	if decision.Mode == modeResume && decision.TailRecord != nil {
		hr := hydrateStateFromTail(state, decision.TailRecord)
		if hr.Reconstructed {
			slog.Info("lifecycle: controller reconstructed from journal tail",
				"f_cells", hr.CoeffCells,
				"alpha_cells", hr.AlphaCells,
				"sigma_cells", hr.SigmaCells,
				"resumed_from_batch", decision.ResumedFromBatch)
		} else {
			slog.Warn("lifecycle: resume with no controller coefficients in journal tail — controller cold-started",
				"resumed_from_batch", decision.ResumedFromBatch)
		}
	} else {
		slog.Info("lifecycle: controller cold-started from reference-F seed")
	}

	facadeCtx := buildFacadeContext(chainID, head.GasLimit, rc.Run.AddressStride)
	// The chain's master-signer nonce is authoritative for the address cursor
	// and survives EL-client crashes / reorgs. Derive it from the chain for both
	// fresh and resume — a resumed run after an EL restart must pick up the
	// rolled-back nonce, not the (stale) journal cursor.
	masterNonce, err := rpcCli.TransactionCount(ctx, signr.Address())
	if err != nil {
		return fmt.Errorf("lifecycle: query master nonce: %w", err)
	}
	facadeCtx.AddressCursor.Store(masterNonce)
	slog.Info("lifecycle: master nonce primed from RPC",
		"address", signr.Address().Hex(), "nonce", masterNonce, "mode", decision.Mode.String())

	// Loop until the plugin returns valid trie stats — during a bootstrap scan,
	// state-comp returns all zeros. A blind start (zero stats) would feed garbage
	// into the controller.
	initialSnap, err := waitForValidSensor(ctx, sens,
		time.Duration(rc.Run.SensorPollGapMS)*time.Millisecond,
		time.Duration(rc.Run.SensorLogIntervalS)*time.Second)
	if err != nil {
		return fmt.Errorf("lifecycle: initial sensor read: %w", err)
	}
	currentObs := &controller.Observation{
		AccountTrieBytes: initialSnap.AccountTrieBytes,
		StorageTrieBytes: initialSnap.StorageTrieBytes,
		CodeBytesTotal:   initialSnap.CodeBytesTotal,
	}

	metricsReg.ObservedFlatBytes.WithLabelValues("accounts").Set(float64(currentObs.AccountTrieBytes))
	metricsReg.ObservedFlatBytes.WithLabelValues("storage").Set(float64(currentObs.StorageTrieBytes))
	metricsReg.ObservedFlatBytes.WithLabelValues("code").Set(float64(currentObs.CodeBytesTotal))

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

	targetCh := make(chan *target.Target, 1)
	watcher, err := target.NewWatcher(ctx, cfg.TargetYAMLPath,
		time.Duration(rc.Run.TargetWatchDebounceMS)*time.Millisecond,
		func(t *target.Target) {
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

	// Prime fee policy once synchronously so the first batch has a valid
	// max-fee/tip pair before the planner loop runs.
	if err := refreshFeePolicy(ctx, rpcCli, facadeCtx, rc.Cost.EthPerGasTarget, rc.Cost.PriorityTipWei); err != nil {
		return fmt.Errorf("lifecycle: prime fee policy: %w", err)
	}
	feeCtx, feeCancel := context.WithCancel(ctx)
	var feeWG sync.WaitGroup
	feeWG.Go(func() {
		feeInterval := time.Duration(rc.Cost.FeePolicyIntervalMS) * time.Millisecond
		if err := runFeePolicyLoop(feeCtx, rpcCli, facadeCtx, feeInterval, rc.Cost.EthPerGasTarget, rc.Cost.PriorityTipWei); err != nil && !errors.Is(err, context.Canceled) {
			slog.Warn("lifecycle: fee policy loop exited", "err", err)
		}
	})
	defer func() {
		feeCancel()
		feeWG.Wait()
	}()

	// Shared monotonic block-timestamp counter. Seeded with the current chain
	// head's timestamp so the first orchestrator block's ts > head.ts (Geth and
	// the normal Engine API require strictly-increasing block timestamps; the
	// NoValidation testing_commitBlockV1 path does not, which is the bug this
	// guards against). Bootstrap and the main loop share this one counter.
	lastBlockTS := new(atomic.Uint64)
	lastBlockTS.Store(head.Timestamp)

	contractRegistry, err := bootstrapContracts(ctx, &bootstrapDeps{
		rpc:         rpcCli,
		signer:      signr,
		facadeCtx:   facadeCtx,
		stateDir:    cfg.StateDir,
		chainID:     chainID,
		deployGas:   rc.Run.ContractDeployGas,
		lastBlockTS: lastBlockTS,
	}, cfg.Verbs)
	if err != nil {
		return fmt.Errorf("lifecycle: bootstrap: %w", err)
	}
	facadeCtx.Contracts = contractRegistry
	if err := assertVerbContractsHaveCode(ctx, rpcCli, contractRegistry, cfg.Verbs); err != nil {
		return fmt.Errorf("lifecycle: pre-loop contract guard: %w", err)
	}

	// 13b. Contract-dependency-aware verb eligibility. A verb whose contract
	// dependency is not deployed-and-verified on this chain is structurally
	// dead — every batch of it is 100%-rejected on commit. Compute the eligible
	// set from the bootstrap registry and install it on the controller so Pick
	// never selects an undeployed-contract verb. Fail loud if no verb can run.
	eligibility := computeEligibleVerbs(cfg.Verbs, contractRegistry)
	logEligibility(eligibility)
	if len(eligibility.eligible) == 0 {
		return fmt.Errorf(
			"lifecycle: no eligible verbs — every configured verb's contract dependency is undeployed; refusing to start")
	}
	state.SetEligibleVerbs(eligibility.eligible)

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
		cfg:          rc,
		lastBlockTS:  lastBlockTS,
	}

	termination, finalBlock := runLoop(ctx, cfg, deps, &ctrlTarget, currentObs, targetCh, mf)

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

// withDefaults fills the parts of Config that callers may leave zero-valued. If
// cfg.Run is zero-valued it is resolved from target.yaml + environment via
// config.Load (Defaults → control:/cost:/run: blocks → env overrides). A
// validation failure from config.Load is returned so a bad value fails fast.
func withDefaults(cfg Config) (Config, error) {
	if len(cfg.Verbs) == 0 {
		cfg.Verbs = defaultVerbs()
	}
	// A zero RunConfig is detected via MetricsAddr, which Defaults() always
	// sets non-empty; RunConfig contains a map so it is not directly
	// comparable.
	if cfg.Run.Run.MetricsAddr == "" {
		rc, err := config.Load(cfg.TargetYAMLPath)
		if err != nil {
			return Config{}, err
		}
		cfg.Run = rc
	}
	return cfg, nil
}

func defaultVerbs() []string {
	return []string{
		"eoatx", "calltx", "deploytx", "factorydeploytx",
		"storagespam", "erc20_bloater", "erc20tx", "uniswap_swaps",
		"gasburnertx",
	}
}

// buildFacadeContext creates a fresh facade.Context for this run.
// AddressCursor / SaltCursor / BlockGasLimit are zero-valued atomics; callers
// must use the typed Set / Reserve methods to seed/advance them. addressStride
// is the resolved RunConfig value.
func buildFacadeContext(chainID, gasLimit, addressStride uint64) *facade.Context {
	c := &facade.Context{
		BaseAddress:   make([]byte, 20),
		Revision:      0,
		AddressStride: addressStride,
		ChainID:       chainID,
	}
	c.SetBlockGasLimit(gasLimit)
	return c
}

// runLoop drives the Pick → build → sign → Commit → sensor → Apply → journal
// cycle until a termination condition fires. Returns the termination reason and
// the last committed block (or nil).
//
// The loop is strictly sequential and sensor-paced. Each iteration commits one
// block and then blocks in commitBatch until the StateComposition trie-diff has
// folded that block into the sensor; only then does the next iteration Pick off
// that fresh observation. The orchestrator never runs ahead of the sensor — so
// the per-block trie-diff can never fall behind, and a baseline rescan (which
// freezes the sensor) automatically pauses bloating until it completes.
// Throughput is paced by the sensor, by design. There is one goroutine, so the
// nonce/salt cursor and the controller State need no cross-goroutine guarding.
func runLoop(
	ctx context.Context,
	cfg Config,
	deps *batchDeps,
	ctrlTarget **controller.Target,
	currentObs *controller.Observation,
	targetCh chan *target.Target,
	mf *manifest.Manifest,
) (string, *rpc.BlockHeader) {
	// Depth-1 build-ahead pipeline: planner Picks/builds/signs batch N+1 while
	// committer commits+sensor-waits+applies batch N. The ready channel buffers
	// one prepared batch — the planner blocks once a batch is queued, preventing
	// runaway dispatch. Observation lag of one batch is by design.
	startBatchID := deps.resumedFrom
	if startBatchID > 0 {
		startBatchID++
	}

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

	// Drain residual pending sidecar in case commit was interrupted mid-flight.
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
	return t, shareTargetFromTarget(t.Shares, t.TotalBytes), nil
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
	before, after, ok := strings.Cut(msg, sep)
	if !ok {
		return 0, 0
	}
	// Walk left to extract the integer before " transactions but only".
	left := strings.TrimRight(before, " ")
	if li := strings.LastIndex(left, " "); li >= 0 {
		left = left[li+1:]
	}
	if v, perr := strconv.ParseUint(left, 10, 64); perr == nil {
		expected = v
	}
	// Walk right to extract the integer after "transactions but only ".
	right := strings.TrimLeft(after, " ")
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
