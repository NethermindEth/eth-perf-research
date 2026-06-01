// Command verify-payloads walks a payloads.rlp recorded by the orchestrator
// (read-only) and reports the block-number range, any gaps, non-monotonic or
// duplicate frames, and — when --from/--to are given — coverage against the
// expected inclusive range. It never writes; use heal-payloads to repair.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/payloads"
)

var version = "dev"

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "verify-payloads:", err)
		os.Exit(1)
	}
}

type gap struct {
	from, to uint64 // inclusive missing range [from,to]
}

func (g gap) count() uint64 { return g.to - g.from + 1 }

func newRootCmd() *cobra.Command {
	var (
		in   string
		from uint64
		to   uint64
		dump string
	)
	cmd := &cobra.Command{
		Use:           "verify-payloads",
		Short:         "Report block-number range, gaps and coverage of a payloads.rlp (read-only)",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			if in == "" {
				return errors.New("--in is required")
			}
			return run(in, from, to, dump)
		},
	}
	cmd.Flags().StringVar(&in, "in", "", "Path to payloads.rlp (read-only)")
	cmd.Flags().Uint64Var(&from, "from", 0, "Expected first block (inclusive); 0 = skip coverage check")
	cmd.Flags().Uint64Var(&to, "to", 0, "Expected last block (inclusive); 0 = skip coverage check")
	cmd.Flags().StringVar(&dump, "dump", "", "Optional path to write 'frameIdx blockNumber blockHash' per frame")
	return cmd
}

func run(path string, from, to uint64, dumpPath string) error {
	r, err := payloads.OpenReader(path)
	if err != nil {
		return err
	}
	defer r.Close()

	var dumpW *os.File
	if dumpPath != "" {
		dumpW, err = os.Create(dumpPath)
		if err != nil {
			return err
		}
		defer dumpW.Close()
	}

	var (
		frames     uint64
		first      uint64
		last       uint64
		havePrev   bool
		prev       uint64
		gaps       []gap
		nonMono    uint64
		dups       uint64
		totMissing uint64
		prevTs     uint64
		tsNonInc   uint64
		firstTs    uint64
		lastTs     uint64
	)

	for {
		p, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("frame %d (after block %d): %w", frames, prev, err)
		}
		frames++
		if dumpW != nil {
			fmt.Fprintf(dumpW, "%d %d %x\n", frames-1, p.Number, p.BlockHash[:])
		}
		if !havePrev {
			first = p.Number
			last = p.Number
			prev = p.Number
			firstTs = p.Timestamp
			lastTs = p.Timestamp
			prevTs = p.Timestamp
			havePrev = true
			continue
		}
		if p.Timestamp <= prevTs {
			tsNonInc++
			if tsNonInc <= 5 {
				fmt.Printf("  TS NON-INCREASING at frame %d (block %d): prev_ts=%d cur_ts=%d\n", frames-1, p.Number, prevTs, p.Timestamp)
			}
		}
		prevTs = p.Timestamp
		lastTs = p.Timestamp
		switch {
		case p.Number == prev+1:
			// contiguous
		case p.Number > prev+1:
			g := gap{from: prev + 1, to: p.Number - 1}
			gaps = append(gaps, g)
			totMissing += g.count()
		case p.Number == prev:
			dups++
			fmt.Printf("  DUP at frame %d: block %d repeated\n", frames-1, p.Number)
		default: // p.Number < prev
			nonMono++
			fmt.Printf("  NON-MONOTONIC at frame %d: prev=%d -> cur=%d (back %d)\n", frames-1, prev, p.Number, prev-p.Number)
		}
		last = p.Number
		prev = p.Number
	}

	fmt.Printf("file:           %s\n", path)
	fmt.Printf("frames:         %d\n", frames)
	if frames == 0 {
		fmt.Println("EMPTY FILE")
		return nil
	}
	fmt.Printf("first block:    %d\n", first)
	fmt.Printf("last block:     %d\n", last)
	fmt.Printf("span:           %d blocks\n", last-first+1)
	fmt.Printf("internal gaps:  %d (%d blocks missing)\n", len(gaps), totMissing)
	fmt.Printf("duplicates:     %d\n", dups)
	fmt.Printf("non-monotonic:  %d\n", nonMono)
	fmt.Printf("first ts:       %d\n", firstTs)
	fmt.Printf("last ts:        %d\n", lastTs)
	fmt.Printf("ts non-incr:    %d (must be 0 for replay-safe)\n", tsNonInc)
	for _, g := range gaps {
		if g.from == g.to {
			fmt.Printf("  gap: %d\n", g.from)
		} else {
			fmt.Printf("  gap: %d..%d (%d)\n", g.from, g.to, g.count())
		}
	}

	healthy := len(gaps) == 0 && dups == 0 && nonMono == 0 && tsNonInc == 0
	if from != 0 || to != 0 {
		fmt.Printf("\nexpected range: %d..%d\n", from, to)
		if from != 0 && first > from {
			fmt.Printf("  HEAD-MISSING: file starts at %d, expected %d (missing %d..%d)\n", first, from, from, first-1)
			healthy = false
		}
		if to != 0 && last < to {
			fmt.Printf("  TAIL-MISSING: file ends at %d, expected %d (missing %d..%d)\n", last, to, last+1, to)
			healthy = false
		}
	}

	if healthy {
		fmt.Println("\nRESULT: OK (contiguous, no gaps)")
		return nil
	}
	fmt.Println("\nRESULT: NEEDS HEALING")
	return nil
}
