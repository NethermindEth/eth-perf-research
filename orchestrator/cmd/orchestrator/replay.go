package main

import (
	"errors"

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
			d := &replay.Driver{Client: client, Manifest: m}
			return d.Replay(cmd.Context(), f.payloadsPath)
		},
	}
	cmd.Flags().StringVar(&f.payloadsPath, "payloads", "", "Path to payloads.rlp")
	cmd.Flags().StringVar(&f.manifestPath, "manifest", "", "Path to run-manifest.json")
	cmd.Flags().StringVar(&f.rpcURL, "rpc-url", "", "Engine API URL (e.g. http://localhost:8551)")
	cmd.Flags().StringVar(&f.jwtPath, "jwt-path", "", "Path to JWT secret file")
	return cmd
}
