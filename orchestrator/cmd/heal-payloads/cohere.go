package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/ethereum/go-ethereum/common"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/payloads"
)

// HashFetcher is a BlockFetcher that can also return a block's canonical hash
// cheaply (header only, no per-tx fetch). Used to seed the linkage chain.
type HashFetcher interface {
	BlockFetcher
	BlockHashAt(ctx context.Context, blockNumber uint64) (common.Hash, error)
}

// CohereConfig parameterises a canonical rebuild.
type CohereConfig struct {
	InputPath  string
	OutputPath string
	From       uint64 // first block to include (inclusive)
	Head       uint64 // last block to include (inclusive); 0 = use node head
	Fetcher    HashFetcher
}

// CohereSummary reports what the rebuild did.
type CohereSummary struct {
	From         uint64
	Head         uint64
	Reused       uint64 // input frames copied verbatim (canonical, linked)
	Fetched      uint64 // blocks absent from input, fetched from the node
	Relinked     uint64 // input frames that failed linkage, replaced from node
	OutputFrames uint64
}

// Cohere rebuilds a canonical, contiguous, hash-linked payloads file over
// [From, Head]. It reuses recorded frames where the recorded block links to the
// running canonical chain (last-occurrence-per-block wins, selecting the block
// re-committed after any reorg), copying their raw bytes; any block missing
// from the input, or whose recorded frame is orphaned (parentHash does not
// match the prior canonical block), is fetched fresh from the node. The output
// is verified contiguous and hash-linked before returning.
func Cohere(ctx context.Context, cfg CohereConfig) (*CohereSummary, error) {
	if cfg.InputPath == "" || cfg.OutputPath == "" {
		return nil, errors.New("cohere: input and output paths required")
	}
	if cfg.Fetcher == nil {
		return nil, errors.New("cohere: fetcher required")
	}

	head := cfg.Head
	if head == 0 {
		h, err := cfg.Fetcher.HeadBlock(ctx)
		if err != nil {
			return nil, fmt.Errorf("cohere: head: %w", err)
		}
		head = h
	}
	if cfg.From == 0 || cfg.From > head {
		return nil, fmt.Errorf("cohere: invalid range from=%d head=%d", cfg.From, head)
	}

	refs, err := payloads.ScanIndex(cfg.InputPath)
	if err != nil {
		return nil, err
	}
	last := make(map[uint64]payloads.FrameRef, len(refs))
	for _, r := range refs {
		last[r.Number] = r // last occurrence wins: the block re-committed after a reorg
	}

	in, err := os.Open(cfg.InputPath)
	if err != nil {
		return nil, fmt.Errorf("cohere: open input: %w", err)
	}
	defer in.Close()

	w, err := payloads.OpenWriter(cfg.OutputPath)
	if err != nil {
		return nil, fmt.Errorf("cohere: open output: %w", err)
	}
	defer w.Close()

	// Seed the linkage chain from the canonical block just below From so the
	// first emitted frame's parentHash is verified too.
	baseHash, err := cfg.Fetcher.BlockHashAt(ctx, cfg.From-1)
	if err != nil {
		return nil, fmt.Errorf("cohere: baseline hash %d: %w", cfg.From-1, err)
	}

	summary := &CohereSummary{From: cfg.From, Head: head}
	prevHash := baseHash
	for bn := cfg.From; bn <= head; bn++ {
		var p *payloads.ExecutionPayloadV3
		if ref, ok := last[bn]; ok {
			raw, err := payloads.ReadFrameAt(in, ref)
			if err != nil {
				return nil, err
			}
			dp, err := payloads.DecodeFrame(raw)
			if err != nil {
				return nil, fmt.Errorf("cohere: decode block %d: %w", bn, err)
			}
			if dp.ParentHash == prevHash {
				if err := w.AppendRaw(raw); err != nil {
					return nil, err
				}
				summary.Reused++
				p = dp
			} else {
				p, err = cfg.Fetcher.PayloadAt(ctx, bn)
				if err != nil {
					return nil, fmt.Errorf("cohere: relink fetch %d: %w", bn, err)
				}
				if err := w.Append(p); err != nil {
					return nil, err
				}
				summary.Relinked++
				slog.Info("cohere: relinked orphan frame", "block", bn)
			}
		} else {
			p, err = cfg.Fetcher.PayloadAt(ctx, bn)
			if err != nil {
				return nil, fmt.Errorf("cohere: fetch missing %d: %w", bn, err)
			}
			if err := w.Append(p); err != nil {
				return nil, err
			}
			summary.Fetched++
		}
		prevHash = p.BlockHash
		summary.OutputFrames++
		if (bn-cfg.From)%5000 == 0 {
			slog.Info("cohere: progress", "block", bn, "reused", summary.Reused,
				"fetched", summary.Fetched, "relinked", summary.Relinked)
		}
	}

	if err := w.Sync(); err != nil {
		return nil, fmt.Errorf("cohere: fsync: %w", err)
	}

	// A chain that links continuously from the canonical baseline to the
	// canonical head cannot contain a non-canonical frame (a fork would never
	// reconverge to the same head hash). Assert both endpoints are canonical.
	headHash, err := cfg.Fetcher.BlockHashAt(ctx, head)
	if err != nil {
		return nil, fmt.Errorf("cohere: head hash %d: %w", head, err)
	}
	if prevHash != headHash {
		return nil, fmt.Errorf("cohere: terminal block %d hash %x != node canonical %x", head, prevHash, headHash)
	}

	if err := verifyChain(cfg.OutputPath, cfg.From, head, baseHash); err != nil {
		return nil, err
	}
	return summary, nil
}

// verifyChain re-reads path and asserts block numbers run From..Head with +1
// steps and every parentHash matches the prior frame's blockHash (the first
// frame is checked against baseHash).
func verifyChain(path string, from, head uint64, baseHash common.Hash) error {
	r, err := payloads.OpenReader(path)
	if err != nil {
		return fmt.Errorf("cohere: reopen output: %w", err)
	}
	defer r.Close()
	prevHash := baseHash
	var prevNum uint64
	var have bool
	for {
		p, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("cohere: verify read: %w", err)
		}
		if !have {
			if p.Number != from {
				return fmt.Errorf("cohere: verify: first block %d != from %d", p.Number, from)
			}
		} else if p.Number != prevNum+1 {
			return fmt.Errorf("cohere: verify: gap %d -> %d", prevNum, p.Number)
		}
		if p.ParentHash != prevHash {
			return fmt.Errorf("cohere: verify: broken link at %d: parent=%x prev=%x", p.Number, p.ParentHash, prevHash)
		}
		prevHash = p.BlockHash
		prevNum = p.Number
		have = true
	}
	if !have || prevNum != head {
		return fmt.Errorf("cohere: verify: last block %d != head %d", prevNum, head)
	}
	return nil
}
