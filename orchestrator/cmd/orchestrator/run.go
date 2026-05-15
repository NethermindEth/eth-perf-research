package main

import (
	"cmp"
	"context"
	"errors"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/lifecycle"
)

type runFlags struct {
	rpcURL               string
	stateDir             string
	targetYAML           string
	genesisSHA256        string
	pluginGitSHA         string
	nethermindCommitSHA  string
	dotnetRuntimeMajor   string
	jwtPath              string
	sensorRPCURL         string
	referenceFPath       string
	manifestPath         string
	maxBatches           int
	targetTotalBytesOver int64
	deployPrivateKey     string
	builderWorkerCmd     string
	builderWorkers       int
	enableProbe          bool
	metricsAddr          string
	epsilon              float64
	planners             int
}

func newRunCmd() *cobra.Command {
	var f runFlags
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run the orchestrator main loop against a Nethermind RPC",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runOrchestrator(cmd.Context(), f)
		},
	}
	cmd.Flags().StringVar(&f.rpcURL, "rpc-url", "", "Nethermind JSON-RPC URL (required)")
	cmd.Flags().StringVar(&f.stateDir, "state-dir", "", "State directory for journal/manifest/payloads (required)")
	cmd.Flags().StringVar(&f.targetYAML, "target-yaml", "", "Path to target.yaml (required)")
	cmd.Flags().StringVar(&f.genesisSHA256, "genesis-sha256", "", "Hex sha256 of genesis.json (required)")
	cmd.Flags().StringVar(&f.pluginGitSHA, "plugin-git-sha", "", "StateComp plugin git SHA")
	cmd.Flags().StringVar(&f.nethermindCommitSHA, "nethermind-commit-sha", "", "Nethermind commit SHA")
	cmd.Flags().StringVar(&f.dotnetRuntimeMajor, "dotnet-runtime-major", "", ".NET runtime major version")
	cmd.Flags().StringVar(&f.jwtPath, "jwt-path", "", "Path to JWT secret file (Engine API)")
	cmd.Flags().StringVar(&f.sensorRPCURL, "sensor-rpc-url", "", "Override RPC URL for sensor (defaults to --rpc-url)")
	cmd.Flags().StringVar(&f.referenceFPath, "reference-f", "", "Path to REFERENCE_F YAML")
	cmd.Flags().StringVar(&f.manifestPath, "manifest", "", "Manifest output path (defaults to state-dir/run-manifest.json)")
	cmd.Flags().IntVar(&f.maxBatches, "max-batches", 0, "Stop after N batches (0=unbounded)")
	cmd.Flags().Int64Var(&f.targetTotalBytesOver, "target-total-bytes-override", 0, "Override target.total_bytes")
	cmd.Flags().StringVar(&f.deployPrivateKey, "deploy-private-key", "", "Hex private key (0x-prefix optional); also reads ORCH_DEPLOY_PRIVATE_KEY")
	cmd.Flags().StringVar(&f.builderWorkerCmd, "builder-worker-cmd", "python -m builder_worker", "Command to spawn each Python builder worker")
	cmd.Flags().IntVar(&f.builderWorkers, "builder-workers", 0, "Number of Python builder workers (0=NumCPU)")
	cmd.Flags().BoolVar(&f.enableProbe, "probe", true, "Run probe phase on fresh start")
	cmd.Flags().StringVar(&f.metricsAddr, "metrics-addr", ":9101", "TCP address for the Prometheus /metrics endpoint")
	cmd.Flags().Float64Var(&f.epsilon, "epsilon", 0.5, "ε-greedy exploration rate for verb selection")
	cmd.Flags().IntVar(&f.planners, "planners", 4, "Number of parallel planner goroutines (1=sequential)")
	return cmd
}

func (f runFlags) validate() error {
	if f.rpcURL == "" {
		return errors.New("--rpc-url is required")
	}
	if f.stateDir == "" {
		return errors.New("--state-dir is required")
	}
	if f.targetYAML == "" {
		return errors.New("--target-yaml is required")
	}
	if f.genesisSHA256 == "" {
		return errors.New("--genesis-sha256 is required")
	}
	return nil
}

func runOrchestrator(ctx context.Context, f runFlags) error {
	if err := f.validate(); err != nil {
		return err
	}

	cfg := lifecycle.Config{
		RPCURL:           f.rpcURL,
		JWTPath:          f.jwtPath,
		StateDir:         f.stateDir,
		TargetYAMLPath:   f.targetYAML,
		GenesisSHA256:    f.genesisSHA256,
		ReferenceFPath:   f.referenceFPath,
		ManifestPath:     f.manifestPath,
		MaxBatches:       f.maxBatches,
		Planners:         f.planners,
		EnableProbe:      f.enableProbe,
		DeployPrivateKey: cmp.Or(f.deployPrivateKey, os.Getenv("ORCH_DEPLOY_PRIVATE_KEY")),
		BuilderWorkerCmd: strings.Fields(f.builderWorkerCmd),
		BuilderWorkers:   f.builderWorkers,
		Epsilon:          f.epsilon,
		TotalBatchBytes:  resolveTotalBatchBytes(),
		Verbs:            resolveVerbs(),
		PluginGitSHA:        f.pluginGitSHA,
		NethermindCommitSHA: f.nethermindCommitSHA,
		DotnetRuntimeMajor:  f.dotnetRuntimeMajor,
		MetricsAddr:         f.metricsAddr,
	}
	return lifecycle.Run(ctx, cfg)
}

const defaultRunTotalBatchBytes = 5 * 1024 * 1024

func resolveTotalBatchBytes() int {
	raw := os.Getenv("ORCH_TOTAL_BATCH_BYTES")
	if raw == "" {
		return defaultRunTotalBatchBytes
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < 1024 {
		return defaultRunTotalBatchBytes
	}
	return v
}

func resolveVerbs() []string {
	raw := os.Getenv("ORCH_VERBS")
	if raw == "" {
		return []string{
			"eoatx", "calltx", "deploytx", "factorydeploytx",
			"storagespam", "erc20_bloater", "erc20tx", "uniswap_swaps",
			"storagerefundtx", "gasburnertx", "evm_fuzz", "noop",
		}
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return []string{"eoatx"}
	}
	return out
}
