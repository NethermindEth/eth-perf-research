package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/payloads"
)

// BlockFetcher abstracts the RPC dependency for testability.
type BlockFetcher interface {
	// PayloadAt returns the canonical ExecutionPayloadV3 for the given block
	// number, materialised from eth_getBlockByNumber and per-tx
	// eth_getRawTransactionByHash calls.
	PayloadAt(ctx context.Context, blockNumber uint64) (*payloads.ExecutionPayloadV3, error)
	// HeadBlock returns the current head block number reported by the node.
	HeadBlock(ctx context.Context) (uint64, error)
}

// HealConfig parameterises the heal pass.
type HealConfig struct {
	InputPath   string
	OutputPath  string
	Fetcher     BlockFetcher
	IncludeTail bool
}

// HealSummary is the verification report emitted at the end of Heal.
type HealSummary struct {
	InputFrames  uint64
	OutputFrames uint64
	GapsFilled   uint64
	TailFilled   uint64
	FirstBlock   uint64
	LastBlock    uint64
}

// Heal copies frames from cfg.InputPath to cfg.OutputPath, filling any
// block-number gaps by fetching the missing blocks from cfg.Fetcher. When
// cfg.IncludeTail is set and the node's head exceeds the last input block,
// the trailing blocks are appended too. Heal returns an error if the output
// is not contiguous after writing.
func Heal(ctx context.Context, cfg HealConfig) (*HealSummary, error) {
	if cfg.InputPath == "" || cfg.OutputPath == "" {
		return nil, errors.New("heal: input and output paths required")
	}
	if cfg.Fetcher == nil {
		return nil, errors.New("heal: fetcher required")
	}

	r, err := payloads.OpenReader(cfg.InputPath)
	if err != nil {
		return nil, fmt.Errorf("heal: open input: %w", err)
	}
	defer r.Close()

	w, err := payloads.OpenWriter(cfg.OutputPath)
	if err != nil {
		return nil, fmt.Errorf("heal: open output: %w", err)
	}
	defer w.Close()

	summary := &HealSummary{}
	var prev uint64
	var havePrev bool

	for {
		p, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("heal: read frame %d: %w", summary.InputFrames, err)
		}
		summary.InputFrames++

		if havePrev && p.Number > prev+1 {
			for missing := prev + 1; missing < p.Number; missing++ {
				filled, err := cfg.Fetcher.PayloadAt(ctx, missing)
				if err != nil {
					return nil, fmt.Errorf("heal: fetch gap block %d: %w", missing, err)
				}
				if err := w.Append(filled); err != nil {
					return nil, fmt.Errorf("heal: write gap block %d: %w", missing, err)
				}
				summary.GapsFilled++
				summary.OutputFrames++
				slog.Info("heal: filled gap block", "block", missing)
			}
		} else if havePrev && p.Number <= prev {
			return nil, fmt.Errorf("heal: non-monotonic input at frame %d: prev=%d current=%d", summary.InputFrames, prev, p.Number)
		}

		if err := w.Append(p); err != nil {
			return nil, fmt.Errorf("heal: write frame %d (block %d): %w", summary.InputFrames, p.Number, err)
		}
		summary.OutputFrames++
		if summary.FirstBlock == 0 {
			summary.FirstBlock = p.Number
		}
		summary.LastBlock = p.Number
		prev = p.Number
		havePrev = true
	}

	if cfg.IncludeTail && havePrev {
		head, err := cfg.Fetcher.HeadBlock(ctx)
		if err != nil {
			return nil, fmt.Errorf("heal: head block: %w", err)
		}
		for missing := summary.LastBlock + 1; missing <= head; missing++ {
			filled, err := cfg.Fetcher.PayloadAt(ctx, missing)
			if err != nil {
				return nil, fmt.Errorf("heal: fetch tail block %d: %w", missing, err)
			}
			if err := w.Append(filled); err != nil {
				return nil, fmt.Errorf("heal: write tail block %d: %w", missing, err)
			}
			summary.TailFilled++
			summary.OutputFrames++
			summary.LastBlock = missing
		}
	}

	if err := w.Sync(); err != nil {
		return nil, fmt.Errorf("heal: fsync output: %w", err)
	}

	if err := verifyContiguous(cfg.OutputPath); err != nil {
		return nil, err
	}

	return summary, nil
}

// verifyContiguous walks the healed file and fails if the block-number
// sequence is not strictly +1 between frames.
func verifyContiguous(path string) error {
	r, err := payloads.OpenReader(path)
	if err != nil {
		return fmt.Errorf("heal: reopen output: %w", err)
	}
	defer r.Close()
	var prev uint64
	var havePrev bool
	var idx uint64
	for {
		p, err := r.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("heal: verify frame %d: %w", idx, err)
		}
		if havePrev && p.Number != prev+1 {
			return fmt.Errorf("heal: output non-contiguous at frame %d: prev=%d current=%d", idx, prev, p.Number)
		}
		prev = p.Number
		havePrev = true
		idx++
	}
}
