package main

import (
	"cmp"
	"context"
	"errors"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/config"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/lifecycle"
)

type runFlags struct {
	rpcURL                string
	stateDir              string
	targetYAML            string
	genesisSHA256         string
	pluginGitSHA          string
	nethermindCommitSHA   string
	dotnetRuntimeMajor    string
	jwtPath               string
	sensorRPCURL          string
	referenceFPath        string
	manifestPath          string
	maxBatches            int
	deployPrivateKey      string
	metricsAddr           string
	overshootThreshold    float64
	useRatioScoring       bool
	epsilon               float64
	allowNonZeroFreshHead bool
	debugPick             bool
}

func newRunCmd() *cobra.Command {
	var f runFlags
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run the orchestrator main loop against a Nethermind RPC",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runOrchestrator(cmd.Context(), cmd, f)
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
	cmd.Flags().StringVar(&f.deployPrivateKey, "deploy-private-key", "", "Hex private key (0x-prefix optional); also reads ORCH_DEPLOY_PRIVATE_KEY")
	cmd.Flags().StringVar(&f.metricsAddr, "metrics-addr", ":9101", "TCP address for the Prometheus /metrics endpoint")
	cmd.Flags().Float64Var(&f.overshootThreshold, "overshoot-threshold", 0, "Override controller overshoot trip ratio (0=use target.yaml/default)")
	cmd.Flags().BoolVar(&f.useRatioScoring, "use-ratio-scoring", false, "Score verbs by mainnet-ratio deficit instead of absolute residual")
	cmd.Flags().Float64Var(&f.epsilon, "epsilon", -1, "ε-greedy exploration probability (-1=use target.yaml/default)")
	cmd.Flags().BoolVar(&f.allowNonZeroFreshHead, "allow-non-zero-fresh-head", false, "Allow empty journal against a non-genesis chain (re-baseline runs)")
	cmd.Flags().BoolVar(&f.debugPick, "debug-pick", false, "Emit per-batch per-verb controller debug logs")
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

func runOrchestrator(ctx context.Context, cmd *cobra.Command, f runFlags) error {
	if err := f.validate(); err != nil {
		return err
	}

	rc, err := config.Load(f.targetYAML)
	if err != nil {
		return err
	}
	// CLI flags override config only when explicitly set — unset flags must not
	// clobber target.yaml values with cobra defaults.
	if cmd.Flags().Changed("metrics-addr") {
		rc.Run.MetricsAddr = f.metricsAddr
	}
	if cmd.Flags().Changed("overshoot-threshold") {
		rc.Control.OvershootThreshold = f.overshootThreshold
	}
	if cmd.Flags().Changed("use-ratio-scoring") {
		rc.Control.UseRatioScoring = f.useRatioScoring
	}
	if cmd.Flags().Changed("epsilon") {
		rc.Control.Epsilon = f.epsilon
	}
	if cmd.Flags().Changed("debug-pick") {
		rc.Control.DebugPick = f.debugPick
	}
	if err := config.Validate(rc); err != nil {
		return err
	}

	cfg := lifecycle.Config{
		RPCURL:                f.rpcURL,
		SensorRPCURL:          f.sensorRPCURL,
		JWTPath:               f.jwtPath,
		StateDir:              f.stateDir,
		TargetYAMLPath:        f.targetYAML,
		GenesisSHA256:         f.genesisSHA256,
		ReferenceFPath:        f.referenceFPath,
		ManifestPath:          f.manifestPath,
		MaxBatches:            f.maxBatches,
		DeployPrivateKey:      cmp.Or(f.deployPrivateKey, os.Getenv("ORCH_DEPLOY_PRIVATE_KEY")),
		AllowNonZeroFreshHead: f.allowNonZeroFreshHead,
		Verbs:                 resolveVerbs(),
		PluginGitSHA:          f.pluginGitSHA,
		NethermindCommitSHA:   f.nethermindCommitSHA,
		DotnetRuntimeMajor:    f.dotnetRuntimeMajor,
		Run:                   rc,
	}
	return lifecycle.Run(ctx, cfg)
}

func resolveVerbs() []string {
	raw := os.Getenv("ORCH_VERBS")
	if raw == "" {
		return []string{
			"eoatx", "calltx", "deploytx", "factorydeploytx",
			"storagespam", "erc20_bloater", "erc20tx", "uniswap_swaps",
			"gasburnertx",
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
