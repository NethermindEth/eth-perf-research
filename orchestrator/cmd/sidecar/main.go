// sidecar — v19 external state-composition tracker.
//
// One binary, four modes:
//
//	--mode=bootstrap   one-shot full scan of the FlatDb → snapshot.bin, exit.
//	--mode=tail        long-running, applies BlockDiffs CF to in-memory tracker,
//	                   periodic snapshots, no RPC.
//	--mode=serve       JSON-RPC server only; read-only over a snapshot.
//	--mode=all         bootstrap-if-needed + tail + serve. Production default.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/NethermindEth/eth-perf-research/orchestrator/pkg/sidecar/config"
	"github.com/NethermindEth/eth-perf-research/orchestrator/pkg/sidecar/db"
	"github.com/NethermindEth/eth-perf-research/orchestrator/pkg/sidecar/promexport"
	"github.com/NethermindEth/eth-perf-research/orchestrator/pkg/sidecar/rpc"
	"github.com/NethermindEth/eth-perf-research/orchestrator/pkg/sidecar/scanner"
	"github.com/NethermindEth/eth-perf-research/orchestrator/pkg/sidecar/snapshot"
	"github.com/NethermindEth/eth-perf-research/orchestrator/pkg/sidecar/state"
	"github.com/NethermindEth/eth-perf-research/orchestrator/pkg/sidecar/tailer"
	"github.com/NethermindEth/eth-perf-research/orchestrator/pkg/sidecar/tracker"
)

func main() {
	cfg, err := config.FromFlags(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if err := cfg.Validate(); err != nil {
		fmt.Fprintln(os.Stderr, "config error:", err)
		os.Exit(2)
	}

	logger := newLogger(cfg.LogLevel)
	logger.Info().Str("mode", string(cfg.Mode)).Msg("sidecar starting")

	ctx, cancel := signalCtx()
	defer cancel()

	if err := run(ctx, cfg, logger); err != nil && !errors.Is(err, context.Canceled) {
		logger.Error().Err(err).Msg("sidecar exited with error")
		os.Exit(1)
	}
	logger.Info().Msg("sidecar exit clean")
}

func newLogger(level string) zerolog.Logger {
	zerolog.TimeFieldFormat = time.RFC3339Nano
	lvl, err := zerolog.ParseLevel(strings.ToLower(level))
	if err != nil {
		lvl = zerolog.InfoLevel
	}
	zerolog.SetGlobalLevel(lvl)
	log.Logger = zerolog.New(os.Stderr).With().Timestamp().Logger()
	return log.Logger
}

func signalCtx() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-ch
		cancel()
	}()
	return ctx, cancel
}

func run(ctx context.Context, cfg config.Config, logger zerolog.Logger) error {
	// Verify the snapshot directory is writable BEFORE any expensive work
	// (a 3h bootstrap scan, for example). Catches the common
	// bind-mount/UID-mismatch papercut up front with an actionable error.
	if cfg.NeedsSnapshotDir() {
		if err := ensureDirWritable(cfg.SnapshotDir); err != nil {
			return fmt.Errorf("snapshot dir %q not writable: %w", cfg.SnapshotDir, err)
		}
	}

	machine := state.New()

	// Start the Prometheus metrics endpoint unconditionally — orchestrators
	// scrape `/metrics` even before the tracker is populated.
	metricsSrv := startMetrics(cfg.MetricsListenAddr, logger)
	defer shutdownHTTP(metricsSrv)

	// Register the `nethermind_state_comp_*` gauges so the legacy bloatnet
	// Grafana dashboard (originally written against the in-process NM
	// plugin) keeps working unchanged. The refresh loop in each long-lived
	// mode below feeds it from the tracker. Bootstrap mode skips the loop
	// because it exits before any tracker is exposed.
	promExporter := promexport.Register(nil)

	switch cfg.Mode {
	case config.ModeBootstrap:
		return runBootstrap(ctx, cfg, logger, machine)
	case config.ModeServe:
		return runServe(ctx, cfg, logger, machine, promExporter)
	case config.ModeTail:
		return runTail(ctx, cfg, logger, machine, promExporter)
	case config.ModeAll:
		return runAll(ctx, cfg, logger, machine, promExporter)
	default:
		return fmt.Errorf("unknown mode %q", cfg.Mode)
	}
}

// ---------------------------------------------------------------------------
// MODE: bootstrap (one-shot scan → snapshot.bin → exit).
// ---------------------------------------------------------------------------

func runBootstrap(ctx context.Context, cfg config.Config, logger zerolog.Logger, machine *state.Machine) error {
	machine.Advance(state.PhaseBootstrap)
	h, err := openFlatDB(cfg, logger)
	if err != nil {
		return err
	}
	defer h.Close()

	result, err := scanner.Run(ctx, h, scanner.Options{
		SpillDir:     os.TempDir(),
		SkipCode:     cfg.SkipCode,
		CodeDBPath:   cfg.CodeDBPath,
		BlockCache:   cfg.BlockCacheMiB,
		UseMmap:      cfg.UseMmap,
		DedupDir:     cfg.DedupDir,
		CodeStoreDir: cfg.CodeStoreDir,
	}, logger)
	if err != nil {
		return fmt.Errorf("bootstrap scan: %w", err)
	}
	logger.Info().
		Int64("elapsed_ms", result.ElapsedMs).
		Int64("accounts", result.Counters.AccountsTotal).
		Int64("account_nodes", result.AccountNodes).
		Int64("storage_nodes", result.StorageNodes).
		Msg("bootstrap scan complete")

	out, err := snapshot.Write(cfg.SnapshotDir, result.Tracker)
	if err != nil {
		return fmt.Errorf("write snapshot: %w", err)
	}
	logger.Info().Str("path", out).Msg("snapshot written")
	return nil
}

// ---------------------------------------------------------------------------
// MODE: serve (RPC only, read-only over existing snapshot).
// ---------------------------------------------------------------------------

func runServe(ctx context.Context, cfg config.Config, logger zerolog.Logger, machine *state.Machine, prom *promexport.Metrics) error {
	var codes tracker.CodeStore
	if cfg.CodeStoreDir != "" {
		store, oerr := tracker.OpenRocksCodeStore(cfg.CodeStoreDir)
		if oerr != nil {
			return fmt.Errorf("serve: reopen codestore: %w", oerr)
		}
		codes = store
	}
	t, hdr, err := snapshot.RestoreInto(cfg.SnapshotDir, codes)
	if err != nil {
		return fmt.Errorf("serve: restore snapshot: %w", err)
	}
	logger.Info().
		Int64("block", hdr.BlockNumber).
		Int64("accounts", hdr.AccountsTotal).
		Msg("snapshot restored")
	machine.Advance(state.PhaseCatchingUp)
	machine.Advance(state.PhaseLive)

	rpcSrv := rpc.New(t, cfg.RPCListenAddr, logger)
	rpcSrv.NoteBootstrapDone()
	srv, err := rpcSrv.ListenAndServe()
	if err != nil {
		return err
	}
	defer shutdownHTTP(srv)

	go prom.Run(ctx, t, time.Second)

	<-ctx.Done()
	return ctx.Err()
}

// ---------------------------------------------------------------------------
// MODE: tail (long-running tracker; no RPC).
// ---------------------------------------------------------------------------

func runTail(ctx context.Context, cfg config.Config, logger zerolog.Logger, machine *state.Machine, prom *promexport.Metrics) error {
	var codes tracker.CodeStore
	if cfg.CodeStoreDir != "" {
		store, oerr := tracker.OpenRocksCodeStore(cfg.CodeStoreDir)
		if oerr != nil {
			return fmt.Errorf("tail: reopen codestore: %w", oerr)
		}
		codes = store
	}
	t, _, err := snapshot.RestoreInto(cfg.SnapshotDir, codes)
	if err != nil {
		return fmt.Errorf("tail: restore snapshot: %w", err)
	}
	tailHandle, err := openTailDB(cfg, logger)
	if err != nil {
		return err
	}
	defer tailHandle.Close()
	machine.Advance(state.PhaseCatchingUp)
	tlr := tailer.New(tailHandle, t, logger, time.Second)
	go runSnapshotLoop(ctx, cfg, t, nil, logger)
	go prom.Run(ctx, t, time.Second)
	machine.Advance(state.PhaseLive)
	runErr := tlr.Run(ctx)
	writeShutdownSnapshot(cfg, t, logger)
	return runErr
}

// ---------------------------------------------------------------------------
// MODE: all (production).
// ---------------------------------------------------------------------------

func runAll(ctx context.Context, cfg config.Config, logger zerolog.Logger, machine *state.Machine, prom *promexport.Metrics) error {
	// 1. If a snapshot exists, restore it. Otherwise bootstrap-scan.
	var t *tracker.Tracker
	if _, err := snapshot.ReadHeader(cfg.SnapshotDir); err == nil {
		// If the operator configured a persistent codestore, reopen it
		// before reading the snapshot so any "codes external" snapshot
		// (the bloatnet path) finds its authoritative backing on restart.
		var codes tracker.CodeStore
		if cfg.CodeStoreDir != "" {
			store, oerr := tracker.OpenRocksCodeStore(cfg.CodeStoreDir)
			if oerr != nil {
				return fmt.Errorf("all: reopen codestore at %s: %w", cfg.CodeStoreDir, oerr)
			}
			codes = store
		}
		restored, hdr, rerr := snapshot.RestoreInto(cfg.SnapshotDir, codes)
		if rerr != nil {
			return fmt.Errorf("all: restore: %w", rerr)
		}
		logger.Info().Int64("block", hdr.BlockNumber).Bool("codes_external", hdr.CodesExternal).Msg("restored prior snapshot")
		t = restored
	} else {
		machine.Advance(state.PhaseBootstrap)
		h, oerr := openFlatDB(cfg, logger)
		if oerr != nil {
			return oerr
		}
		result, serr := scanner.Run(ctx, h, scanner.Options{
			SpillDir:     os.TempDir(),
			SkipCode:     cfg.SkipCode,
			CodeDBPath:   cfg.CodeDBPath,
			BlockCache:   cfg.BlockCacheMiB,
			UseMmap:      cfg.UseMmap,
			DedupDir:     cfg.DedupDir,
			CodeStoreDir: cfg.CodeStoreDir,
		}, logger)
		h.Close()
		if serr != nil {
			return fmt.Errorf("all: bootstrap: %w", serr)
		}
		t = result.Tracker
		if _, werr := snapshot.Write(cfg.SnapshotDir, t); werr != nil {
			return fmt.Errorf("all: initial snapshot write: %w", werr)
		}
	}

	// 2. Open the BlockDiffs DB for tail. The Nethermind StateDiffsWriter
	// plugin writes blockDiffs as a standalone RocksDB (datadir/blockDiffs)
	// rather than a CF inside the FlatDb, so this is a separate open.
	tailHandle, err := openTailDB(cfg, logger)
	if err != nil {
		return err
	}
	defer tailHandle.Close()
	machine.Advance(state.PhaseCatchingUp)

	rpcSrv := rpc.New(t, cfg.RPCListenAddr, logger)
	rpcSrv.NoteBootstrapDone()
	srv, err := rpcSrv.ListenAndServe()
	if err != nil {
		return err
	}
	defer shutdownHTTP(srv)

	go runSnapshotLoop(ctx, cfg, t, rpcSrv, logger)
	go prom.Run(ctx, t, time.Second)

	tlr := tailer.New(tailHandle, t, logger, time.Second)
	machine.Advance(state.PhaseLive)
	runErr := tlr.Run(ctx)
	writeShutdownSnapshot(cfg, t, logger)
	return runErr
}

// writeShutdownSnapshot forces a final tracker dump on graceful exit so the
// snapshot.bin on disk is at most a few seconds stale rather than up to one
// SnapshotInterval tick. The codestore is already durable (RocksDB WAL), so
// this only matters for the in-RAM tracker scalars + slot map. Failures are
// logged but not propagated — we are already shutting down.
func writeShutdownSnapshot(cfg config.Config, t *tracker.Tracker, logger zerolog.Logger) {
	if t == nil {
		return
	}
	path, werr := snapshot.Write(cfg.SnapshotDir, t)
	if werr != nil {
		logger.Warn().Err(werr).Msg("shutdown snapshot write failed")
		return
	}
	logger.Info().Str("path", path).Msg("shutdown snapshot written")
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// openTailDB opens the database the tailer reads from. By default the
// Nethermind StateDiffsWriter plugin keeps blockDiffs as a standalone RocksDB
// at <datadir>/blockDiffs/. The config layer infers the path from --db when
// --blockdiffs-db is not supplied, so this function is the single point that
// turns "path exists?" into "open it" vs "log and idle".
//
// Behaviour matrix:
//
//	flag set, path exists      → open and tail
//	flag set, path missing     → return error (operator pointed at a typo)
//	flag inferred, path exists → open and tail
//	flag inferred, path missing→ log INFO (no diffs yet) and fall back to FlatDb
//	                              CF probe (pre-Phase-2 deployments still work)
func openTailDB(cfg config.Config, logger zerolog.Logger) (*db.Handle, error) {
	if cfg.BlockDiffsDB != "" {
		exists, err := pathExists(cfg.BlockDiffsDB)
		if err != nil {
			return nil, fmt.Errorf("stat blockdiffs db at %s: %w", cfg.BlockDiffsDB, err)
		}
		if !exists {
			// Path was inferred from --db AND the operator did not pass it
			// explicitly — most likely a pre-Phase-2 deployment, fine to
			// idle on the FlatDb CF until the plugin produces diffs.
			inferred := config.InferBlockDiffsDBPath(cfg.DBPath)
			if cfg.BlockDiffsDB == inferred {
				logger.Info().Str("path", cfg.BlockDiffsDB).
					Msg("no diffs to tail yet, sidecar will serve baseline only")
				return openFlatDB(cfg, logger)
			}
			return nil, fmt.Errorf("blockdiffs db not found at %s (did you pass the wrong --blockdiffs-db?)", cfg.BlockDiffsDB)
		}
		h, err := db.OpenBlockDiffsDB(cfg.BlockDiffsDB, db.OpenOptions{
			BlockCacheMiB: cfg.BlockCacheMiB,
			UseMmap:       cfg.UseMmap,
			Secondary:     cfg.UseSecondaryDB,
		})
		if err != nil {
			return nil, fmt.Errorf("open blockdiffs db at %s: %w", cfg.BlockDiffsDB, err)
		}
		if h.BlockDiffsCF == nil {
			logger.Warn().Str("path", cfg.BlockDiffsDB).Msg("blockdiffs DB opened but no per-block CF present yet")
		} else {
			logger.Info().Str("path", cfg.BlockDiffsDB).Msg("opened standalone blockdiffs db for tailing")
		}
		return h, nil
	}
	return openFlatDB(cfg, logger)
}

// pathExists returns true if path resolves to an existing inode. Distinguishes
// "missing" (false, nil) from "I/O error" (false, err).
func pathExists(path string) (bool, error) {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func openFlatDB(cfg config.Config, logger zerolog.Logger) (*db.Handle, error) {
	if cfg.DBPath == "" {
		return nil, errors.New("openFlatDB: --db is required")
	}
	h, err := db.Open(cfg.DBPath, db.CFNames{
		State:      cfg.CFStateNodes,
		Storage:    cfg.CFStorageNodes,
		Metadata:   cfg.CFMetadata,
		BlockDiffs: cfg.CFBlockDiffs,
	}, db.OpenOptions{
		BlockCacheMiB: cfg.BlockCacheMiB,
		UseMmap:       cfg.UseMmap,
		Secondary:     cfg.UseSecondaryDB,
	})
	if err != nil {
		return nil, fmt.Errorf("open flatdb: %w", err)
	}
	if h.BlockDiffsCF == nil {
		// Pre-Phase-2 layout (or Phase-2 plugin hasn't produced any diffs
		// yet). Either way, not an error: serve baseline-only until the
		// standalone BlockDiffs DB shows up.
		logger.Info().Msg("BlockDiffs CF not present in FlatDb; serving baseline only")
	}
	return h, nil
}

func runSnapshotLoop(ctx context.Context, cfg config.Config, t *tracker.Tracker, rpcSrv *rpc.Server, logger zerolog.Logger) {
	ticker := time.NewTicker(time.Duration(cfg.SnapshotInterval) * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			path, err := snapshot.Write(cfg.SnapshotDir, t)
			if err != nil {
				logger.Error().Err(err).Msg("snapshot write failed")
				continue
			}
			logger.Debug().Str("path", path).Msg("snapshot written")
			if rpcSrv != nil {
				rpcSrv.NoteSnapshotWritten()
			}
		}
	}
}

func startMetrics(addr string, logger zerolog.Logger) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Warn().Err(err).Msg("metrics server stopped")
		}
	}()
	return srv
}

// ensureDirWritable creates dir if missing then verifies the current process
// can actually write into it. On failure the returned error carries a
// remediation hint (chown/chmod) so operators don't have to guess.
func ensureDirWritable(dir string) error {
	if dir == "" {
		return errors.New("empty path")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir: %w (hint: chown %d:%d %s && chmod 0775 %s)",
			err, os.Getuid(), os.Getgid(), dir, dir)
	}
	probe, err := os.CreateTemp(dir, ".writable-probe-*")
	if err != nil {
		return fmt.Errorf("create probe: %w (hint: chown %d:%d %s && chmod 0775 %s)",
			err, os.Getuid(), os.Getgid(), dir, dir)
	}
	probePath := probe.Name()
	_ = probe.Close()
	_ = os.Remove(probePath)
	return nil
}

func shutdownHTTP(srv *http.Server) {
	if srv == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}
