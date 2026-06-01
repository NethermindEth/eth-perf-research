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
	cmd.AddCommand(newCohereCmd())
	return cmd
}

type cohereFlags struct {
	in     string
	out    string
	rpcURL string
	from   uint64
	head   uint64
}

// newCohereCmd is the reorg-tolerant rebuild: it produces a canonical,
// contiguous, hash-linked payloads file over [--from, --head], reusing
// recorded frames and fetching anything missing or orphaned from the node.
// Use this (not the default heal) when the input has a head shortfall,
// non-monotonic reorg frames, or a head pointer behind the committed state.
func newCohereCmd() *cobra.Command {
	var f cohereFlags
	cmd := &cobra.Command{
		Use:           "cohere",
		Short:         "Rebuild a canonical, contiguous, hash-linked payloads.rlp over [--from,--head]",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if f.in == "" || f.rpcURL == "" || f.from == 0 {
				return errors.New("--in, --rpc-url and --from are required")
			}
			out := f.out
			if out == "" {
				out = f.in + ".cohered"
			}
			client, err := rpc.NewClient(f.rpcURL)
			if err != nil {
				return err
			}
			summary, err := Cohere(cmd.Context(), CohereConfig{
				InputPath:  f.in,
				OutputPath: out,
				From:       f.from,
				Head:       f.head,
				Fetcher:    newDebugBlockFetcher(client),
			})
			if err != nil {
				return err
			}
			slog.Info("cohere complete",
				"from", summary.From, "head", summary.Head,
				"output_frames", summary.OutputFrames,
				"reused", summary.Reused, "fetched", summary.Fetched,
				"relinked", summary.Relinked,
			)
			return nil
		},
	}
	cmd.Flags().StringVar(&f.in, "in", "", "Path to existing payloads.rlp (read-only)")
	cmd.Flags().StringVar(&f.out, "out", "", "Path to rebuilt output file (default <in>.cohered)")
	cmd.Flags().StringVar(&f.rpcURL, "rpc-url", "", "Nethermind JSON-RPC URL (e.g. http://localhost:8545)")
	cmd.Flags().Uint64Var(&f.from, "from", 0, "First block to include (inclusive)")
	cmd.Flags().Uint64Var(&f.head, "head", 0, "Last block to include (inclusive); 0 = use node head")
	return cmd
}
