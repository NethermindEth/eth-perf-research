package main

import (
	"errors"
	"time"

	"github.com/spf13/cobra"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/manifest"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/replay"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/rpc"
)

type replayFlags struct {
	payloadsPath string
	manifestPath string
	rpcURL       string
	jwtPath      string

	// Memory-gated backpressure: poll the EL's debug_memStats and pause dispatch
	// when its resident heap is high, so replay never outruns the client's
	// state-flush throughput (the diff-layer OOM guard). Empty URL disables it.
	memGateURL    string
	memGateHighGB float64
	memGateLowGB  float64
	memGateEvery  int

	// follow tails a payloads file a live bloat run is still appending to, so the
	// replay streams blocks as they are produced rather than stopping at EOF.
	follow         bool
	followPollSecs float64
}

func newReplayCmd() *cobra.Command {
	var f replayFlags
	cmd := &cobra.Command{
		Use:   "replay",
		Short: "Replay payloads.rlp against an EL client via Engine API",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if f.payloadsPath == "" || f.manifestPath == "" || f.rpcURL == "" {
				return errors.New("--payloads, --manifest, --rpc-url required")
			}

			opts := []rpc.Option{}
			if f.jwtPath != "" {
				opts = append(opts, rpc.WithJWTFile(f.jwtPath))
			}
			client, err := rpc.NewClient(f.rpcURL, opts...)
			if err != nil {
				return err
			}
			m, err := manifest.Load(f.manifestPath)
			if err != nil {
				return err
			}
			d := &replay.Driver{Client: client, Manifest: m, Follow: f.follow}
			if f.follow {
				poll := f.followPollSecs
				if poll <= 0 {
					poll = 2
				}
				d.FollowPoll = time.Duration(poll * float64(time.Second))
			}

			if f.memGateURL != "" {
				if f.memGateLowGB <= 0 || f.memGateHighGB <= f.memGateLowGB {
					return errors.New("--mem-gate-high-gb must be greater than --mem-gate-low-gb > 0")
				}
				if f.memGateEvery <= 0 {
					return errors.New("--mem-gate-every must be > 0")
				}
				gateClient, err := rpc.NewClient(f.memGateURL)
				if err != nil {
					return err
				}
				d.MemGate = &replay.MemGate{
					Client:     gateClient,
					High:       uint64(f.memGateHighGB * 1e9),
					Low:        uint64(f.memGateLowGB * 1e9),
					CheckEvery: f.memGateEvery,
					Poll:       10 * time.Second,
					MaxWait:    10 * time.Minute,
				}
			}

			return d.Replay(cmd.Context(), f.payloadsPath)
		},
	}
	cmd.Flags().StringVar(&f.payloadsPath, "payloads", "", "Path to payloads.rlp")
	cmd.Flags().StringVar(&f.manifestPath, "manifest", "", "Path to run-manifest.json")
	cmd.Flags().StringVar(&f.rpcURL, "rpc-url", "", "Engine API URL (e.g. http://localhost:8551)")
	cmd.Flags().StringVar(&f.jwtPath, "jwt-path", "", "Path to JWT secret file")
	cmd.Flags().StringVar(&f.memGateURL, "mem-gate-url", "", "EL debug RPC URL (e.g. http://127.0.0.1:8545) for memStats backpressure; empty disables the gate")
	cmd.Flags().Float64Var(&f.memGateHighGB, "mem-gate-high-gb", 38, "pause replay when EL resident heap exceeds this many GB")
	cmd.Flags().Float64Var(&f.memGateLowGB, "mem-gate-low-gb", 26, "resume replay once EL resident heap drops below this many GB")
	cmd.Flags().IntVar(&f.memGateEvery, "mem-gate-every", 8, "check EL heap every N blocks")
	cmd.Flags().BoolVar(&f.follow, "follow", false, "tail the payloads file (stream blocks as a live bloat run records them, never stop at EOF)")
	cmd.Flags().Float64Var(&f.followPollSecs, "follow-poll-secs", 2, "in --follow mode, seconds to wait for new frames when at end of file")
	return cmd
}
