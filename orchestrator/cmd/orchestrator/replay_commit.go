package main

import (
	"errors"

	"github.com/spf13/cobra"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/replaycommit"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/rpc"
)

type replayCommitFlags struct {
	payloadsPath string
	outPath      string
	rpcURL       string
}

func newReplayCommitCmd() *cobra.Command {
	var f replayCommitFlags
	cmd := &cobra.Command{
		Use:   "replay-commit",
		Short: "Re-execute recorded payloads via testing_commitBlockV1, recording NM's CLEAN computed blocks",
		Long: "Re-executes a recorded payloads.rlp through Nethermind via testing_commitBlockV1, which " +
			"EXECUTES the recorded transactions and commits with NM's OWN computed state root (NoValidation). " +
			"Poisoned recorded roots are discarded; NM produces correct clean roots. The resulting clean blocks " +
			"are written to --out, reaching the same block height as the input.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if f.payloadsPath == "" || f.outPath == "" || f.rpcURL == "" {
				return errors.New("--payloads, --out, --rpc-url required")
			}
			// testing_commitBlockV1 is a plain JSON-RPC method (Testing module); no
			// JWT/engine auth needed.
			client, err := rpc.NewClient(f.rpcURL)
			if err != nil {
				return err
			}
			d := &replaycommit.Driver{Client: client}
			return d.RunFiles(cmd.Context(), f.payloadsPath, f.outPath)
		},
	}
	cmd.Flags().StringVar(&f.payloadsPath, "payloads", "", "Path to input payloads.rlp (recorded, possibly poison-tailed)")
	cmd.Flags().StringVar(&f.outPath, "out", "", "Path to output clean payloads.rlp (O_APPEND; resumes if non-empty)")
	cmd.Flags().StringVar(&f.rpcURL, "rpc-url", "", "Nethermind JSON-RPC URL with the Testing module (e.g. http://localhost:8545)")
	return cmd
}
