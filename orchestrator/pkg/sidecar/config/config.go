// Package config carries the runtime configuration for the sidecar.
//
// All knobs are exposed as flags on the entry binary; this package exists so
// individual sub-packages can be unit-tested without re-parsing flag.CommandLine.
package config

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Mode selects the high-level lifecycle of the sidecar process.
type Mode string

const (
	ModeBootstrap Mode = "bootstrap"
	ModeTail      Mode = "tail"
	ModeServe     Mode = "serve"
	ModeAll       Mode = "all"
)

// ValidModes lists every recognised --mode value.
var ValidModes = []Mode{ModeBootstrap, ModeTail, ModeServe, ModeAll}

// Config is the immutable settings struct passed to every module.
type Config struct {
	Mode Mode

	// FlatDb (NM-side RocksDB) inputs.
	DBPath         string
	CodeDBPath     string
	BlockDiffsDB   string // standalone NM blockDiffs RocksDB (separate from FlatDb)
	CFStateNodes   string
	CFStorageNodes string
	CFMetadata     string
	CFBlockDiffs   string
	BlockCacheMiB  int
	UseMmap        bool
	UseSecondaryDB bool

	// Snapshot directory (output of bootstrap, input of serve/tail).
	SnapshotDir      string
	SnapshotInterval int // seconds between periodic snapshots in tail/all mode.

	// Networking.
	RPCListenAddr     string
	MetricsListenAddr string

	// Scanner tunables.
	SkipCode bool
	// DedupDir is the on-disk location for the bootstrap scanner's temporary
	// codehash-dedup RocksDB. Empty ⇒ <spillDir>/codehash-dedup. At 10x state
	// scale the dedup DB can reach ~700 GB, so prefer a roomy volume.
	DedupDir string

	// CodeStoreDir is the on-disk location of the persistent per-codehash
	// store (refcount + codeSize). When non-empty the bootstrap scanner
	// streams merge-join output directly into a RocksDB at this path
	// instead of building an in-memory []CodeSeed (which OOMs at 1.4 B
	// codehashes; see v19 sidecar bootstrap regression). The same directory
	// is reopened on subsequent boots so tail-mode lookups never need to
	// rescan. Empty ⇒ in-memory CodeStore (suitable for tests and
	// deployments under ~50 M codehashes).
	CodeStoreDir string

	// Tracker memory cap (MiB). Hot tier is capped here; rest spills to mmap.
	TrackerHotMiB int

	LogLevel string
}

// Defaults returns the production default configuration.
func Defaults() Config {
	return Config{
		Mode:           ModeAll,
		CFStateNodes:   "StateNodes",
		CFStorageNodes: "StorageNodes",
		CFMetadata:     "Metadata",
		CFBlockDiffs:   "BlockDiffs",
		BlockCacheMiB:  128,
		UseMmap:        false,
		UseSecondaryDB: true,
		SnapshotDir:    "/var/lib/sidecar",
		// 30s: snapshot writer is O(hot-tier-size) — not state-size — so
		// cold-start cost (~3h bootstrap) dominates loss-on-crash math.
		SnapshotInterval:  30,
		RPCListenAddr:     "0.0.0.0:9001",
		MetricsListenAddr: "0.0.0.0:9090",
		SkipCode:          false,
		TrackerHotMiB:     300,
		LogLevel:          "info",
	}
}

// FromFlags parses CLI flags into a Config. The returned error is non-nil
// for hard parse failures only; missing-but-required fields are signalled by
// Validate() so callers can format their own usage message.
func FromFlags(args []string) (Config, error) {
	c := Defaults()
	fs := flag.NewFlagSet("sidecar", flag.ContinueOnError)

	var modeStr string
	fs.StringVar(&modeStr, "mode", string(c.Mode), "lifecycle mode: bootstrap|tail|serve|all")
	fs.StringVar(&c.DBPath, "db", c.DBPath, "path to FlatDb RocksDB root")
	fs.StringVar(&c.CodeDBPath, "code-db", c.CodeDBPath, "path to Code RocksDB root (optional)")
	fs.StringVar(&c.BlockDiffsDB, "blockdiffs-db", c.BlockDiffsDB, "path to standalone BlockDiffs RocksDB root written by the NM StateDiffsWriter plugin (optional; tail/all modes idle without it)")
	fs.StringVar(&c.CFStateNodes, "cf-state", c.CFStateNodes, "state-nodes CF name")
	fs.StringVar(&c.CFStorageNodes, "cf-storage", c.CFStorageNodes, "storage-nodes CF name")
	fs.StringVar(&c.CFMetadata, "cf-metadata", c.CFMetadata, "metadata CF name")
	fs.StringVar(&c.CFBlockDiffs, "cf-blockdiffs", c.CFBlockDiffs, "BlockDiffs CF name")
	fs.IntVar(&c.BlockCacheMiB, "block-cache-mb", c.BlockCacheMiB, "RocksDB block cache size (MiB)")
	fs.BoolVar(&c.UseMmap, "use-mmap", c.UseMmap, "enable mmap reads")
	fs.BoolVar(&c.UseSecondaryDB, "secondary", c.UseSecondaryDB, "open RocksDB in secondary mode (live NM)")
	fs.StringVar(&c.SnapshotDir, "snapshot-dir", c.SnapshotDir, "directory for sidecar snapshots")
	fs.IntVar(&c.SnapshotInterval, "snapshot-interval-s", c.SnapshotInterval, "snapshot write interval seconds")
	fs.StringVar(&c.RPCListenAddr, "rpc-addr", c.RPCListenAddr, "JSON-RPC listen address")
	fs.StringVar(&c.MetricsListenAddr, "metrics-addr", c.MetricsListenAddr, "Prometheus metrics listen address")
	fs.BoolVar(&c.SkipCode, "skip-code", c.SkipCode, "skip Code-DB lookup pass (bootstrap)")
	fs.StringVar(&c.DedupDir, "dedup-dir", c.DedupDir, "directory for the temp codehash-dedup RocksDB (default: <spill>/codehash-dedup; use a roomy volume at 10x scale)")
	fs.StringVar(&c.CodeStoreDir, "codestore-dir", c.CodeStoreDir, "directory for the persistent per-codehash store (default: empty/in-memory; set at bloatnet scale to avoid bootstrap OOM)")
	fs.IntVar(&c.TrackerHotMiB, "tracker-hot-mb", c.TrackerHotMiB, "tracker hot-tier RAM ceiling (MiB)")
	fs.StringVar(&c.LogLevel, "log-level", c.LogLevel, "debug|info|warn|error")

	var outPath string
	fs.StringVar(&outPath, "out", "", "bootstrap mode: output JSON path; serve mode: snapshot blob path")

	fs.SetOutput(os.Stderr)
	if err := fs.Parse(args); err != nil {
		return Config{}, fmt.Errorf("parse flags: %w", err)
	}

	c.Mode = Mode(strings.ToLower(modeStr))
	if outPath != "" {
		c.SnapshotDir = filepath.Dir(outPath)
		if c.SnapshotDir == "." {
			c.SnapshotDir = "."
		}
	}
	// Infer blockDiffs path from the FlatDb path when not set explicitly.
	// The NM StateDiffsWriter plugin writes to <datadir>/blockDiffs, where
	// <datadir> is the parent of the FlatDb root.
	if c.BlockDiffsDB == "" && c.DBPath != "" {
		c.BlockDiffsDB = InferBlockDiffsDBPath(c.DBPath)
	}
	return c, nil
}

// InferBlockDiffsDBPath returns the default sibling location of the
// BlockDiffs RocksDB given the FlatDb path. The NM StateDiffsWriter plugin
// writes to "<datadir>/blockDiffs", where <datadir> is the parent of the
// FlatDb root (e.g. "<datadir>/state/FlatDb" → "<datadir>/blockDiffs").
func InferBlockDiffsDBPath(dbPath string) string {
	if dbPath == "" {
		return ""
	}
	cleaned := filepath.Clean(dbPath)
	dir := cleaned
	for {
		parent := filepath.Dir(dir)
		if parent == dir || parent == "." || parent == "/" {
			break
		}
		if filepath.Base(dir) == "state" {
			return filepath.Join(parent, "blockDiffs")
		}
		dir = parent
	}
	return filepath.Join(filepath.Dir(cleaned), "blockDiffs")
}

// Validate returns a friendly error for unusable configurations.
func (c Config) Validate() error {
	if !c.isKnownMode() {
		return fmt.Errorf("unknown --mode=%q; want one of %v", c.Mode, ValidModes)
	}
	if c.NeedsDB() && c.DBPath == "" {
		return errors.New("--db is required for bootstrap/tail/all modes")
	}
	if c.NeedsSnapshotDir() && c.SnapshotDir == "" {
		return errors.New("--snapshot-dir is required for serve/tail/all modes")
	}
	if c.BlockCacheMiB <= 0 || c.BlockCacheMiB > 4096 {
		return fmt.Errorf("--block-cache-mb must be in (0,4096], got %d", c.BlockCacheMiB)
	}
	if c.SnapshotInterval <= 0 {
		return fmt.Errorf("--snapshot-interval-s must be positive, got %d", c.SnapshotInterval)
	}
	if c.TrackerHotMiB <= 0 {
		return fmt.Errorf("--tracker-hot-mb must be positive, got %d", c.TrackerHotMiB)
	}
	return nil
}

func (c Config) isKnownMode() bool {
	for _, m := range ValidModes {
		if c.Mode == m {
			return true
		}
	}
	return false
}

func (c Config) NeedsDB() bool {
	return c.Mode == ModeBootstrap || c.Mode == ModeTail || c.Mode == ModeAll
}

func (c Config) NeedsSnapshotDir() bool {
	return c.Mode == ModeServe || c.Mode == ModeTail || c.Mode == ModeAll
}
