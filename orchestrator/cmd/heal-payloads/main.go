// Command heal-payloads repairs a payloads.rlp recorded by the orchestrator.
//
// The recorder commits a block then appends its frame; a SIGKILL between the
// two leaves the file behind the chain. heal-payloads walks the input file,
// detects gaps in block numbers, fetches the missing blocks from Nethermind
// over JSON-RPC, rebuilds canonical ExecutionPayloadV3 frames, and writes a
// contiguous output file. Optionally it also appends missing tail blocks if
// NM's head has advanced past the last input frame.
package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/rpc"
)

var version = "dev"

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := newRootCmd().ExecuteContext(ctx); err != nil {
		slog.Error("heal-payloads failed", "err", err)
		os.Exit(1)
	}
}

type healFlags struct {
	in          string
	out         string
	rpcURL      string
	includeTail bool
}

func newRootCmd() *cobra.Command {
	var f healFlags
	cmd := &cobra.Command{
		Use:           "heal-payloads",
		Short:         "Fill gaps in a payloads.rlp by fetching missing blocks from Nethermind",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if f.in == "" || f.rpcURL == "" {
				return errors.New("--in and --rpc-url are required")
			}
			out := f.out
			if out == "" {
				out = f.in + ".healed"
			}

			client, err := rpc.NewClient(f.rpcURL)
			if err != nil {
				return err
			}
			fetcher := newRPCBlockFetcher(client)
			summary, err := Heal(cmd.Context(), HealConfig{
				InputPath:   f.in,
				OutputPath:  out,
				Fetcher:     fetcher,
				IncludeTail: f.includeTail,
			})
			if err != nil {
				return err
			}
			slog.Info("heal complete",
				"input_frames", summary.InputFrames,
				"output_frames", summary.OutputFrames,
				"gaps_filled", summary.GapsFilled,
				"tail_filled", summary.TailFilled,
				"first_bn", summary.FirstBlock,
				"last_bn", summary.LastBlock,
			)
			return nil
		},
	}
	cmd.Flags().StringVar(&f.in, "in", "", "Path to existing payloads.rlp (read-only)")
	cmd.Flags().StringVar(&f.out, "out", "", "Path to healed output file (default <in>.healed)")
	cmd.Flags().StringVar(&f.rpcURL, "rpc-url", "", "Nethermind JSON-RPC URL (e.g. http://localhost:8545)")
	cmd.Flags().BoolVar(&f.includeTail, "include-tail", true, "Append missing tail blocks if NM head is ahead of input")
	return cmd
}
