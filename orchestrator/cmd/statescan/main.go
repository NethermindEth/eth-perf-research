//go:build !nogrocksdb

// statescan walks a Nethermind FlatDb's four trie-node column families
// (StateTopNodes, StateNodes, StorageNodes, FallbackNodes) read-only and emits
// EXACT node-type counts and a per-depth distribution for the account and
// storage tries. Depth is decoded from the node key's path-length field
// (see Nethermind BaseTriePersistence key layout); node type from rlp.Classify.
//
// Unlike the incremental sidecar tracker (which approximates node-type deltas),
// this is a full deterministic walk → exact figures, no preimages needed.
//
// Usage: statescan <flatdb-path>   (opens as RocksDB secondary; never writes the source)
package main

import (
	"fmt"
	"os"
	"sort"

	rlppkg "github.com/NethermindEth/eth-perf-research/orchestrator/pkg/sidecar/rlp"
	gethrlp "github.com/ethereum/go-ethereum/rlp"
	"github.com/linxGnu/grocksdb"
)

const maxDepth = 64

type trieAgg struct {
	branches   [maxDepth + 1]int64
	extensions [maxDepth + 1]int64
	leaves     [maxDepth + 1]int64
	bytes      [maxDepth + 1]int64
	childSum   int64 // total non-empty children across branch nodes (for occupancy)
}

func (a *trieAgg) add(depth int, kind rlppkg.NodeKind, val []byte) {
	if depth < 0 || depth > maxDepth {
		depth = maxDepth
	}
	a.bytes[depth] += int64(len(val))
	switch kind {
	case rlppkg.NodeBranch:
		a.branches[depth]++
		a.childSum += branchChildren(val)
	case rlppkg.NodeExtension:
		a.extensions[depth]++
	case rlppkg.NodeLeaf:
		a.leaves[depth]++
	}
}

func sum(a [maxDepth + 1]int64) int64 {
	var t int64
	for _, v := range a {
		t += v
	}
	return t
}

// branchChildren counts non-empty entries in a 17-element branch node RLP.
func branchChildren(nodeRLP []byte) int64 {
	content, _, err := gethrlp.SplitList(nodeRLP)
	if err != nil {
		return 0
	}
	var n int64
	rest := content
	for i := 0; i < 16; i++ {
		_, c, r, e := gethrlp.Split(rest)
		if e != nil {
			break
		}
		if len(c) > 0 {
			n++
		}
		rest = r
	}
	return n
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: statescan <flatdb-path>")
		os.Exit(2)
	}
	path := os.Args[1]
	opts := grocksdb.NewDefaultOptions()
	opts.SetCreateIfMissing(false)
	listed, err := grocksdb.ListColumnFamilies(opts, path)
	must(err, "list cfs")
	cfOpts := make([]*grocksdb.Options, len(listed))
	for i := range cfOpts {
		cfOpts[i] = opts
	}
	secDir, err := os.MkdirTemp("", "statescan-secondary-")
	must(err, "mk secondary dir")
	defer os.RemoveAll(secDir)
	db, handles, err := grocksdb.OpenDbAsSecondaryColumnFamilies(opts, path, secDir, listed, cfOpts)
	must(err, "open secondary")
	defer db.Close()
	_ = db.TryCatchUpWithPrimary()
	cf := map[string]*grocksdb.ColumnFamilyHandle{}
	for i, name := range listed {
		cf[name] = handles[i]
	}
	fmt.Fprintf(os.Stderr, "CFs: %v\n", listed)

	var acct, stor trieAgg
	// account: StateTopNodes (depth 0-5), StateNodes (depth 6-15)
	scanCF(db, cf["StateTopNodes"], &acct, depthStateTop)
	scanCF(db, cf["StateNodes"], &acct, depthStateNodes)
	// storage: StorageNodes (depth 0-15)
	scanCF(db, cf["StorageNodes"], &stor, depthStorageNodes)
	// FallbackNodes (depth 16+): mixed; route by prefix byte
	scanFallback(db, cf["FallbackNodes"], &acct, &stor)

	report("ACCOUNT", &acct)
	report("STORAGE", &stor)
}

type depthFn func(key []byte) int

func depthStateTop(k []byte) int { // 3-byte key, length in low nibble of byte[2]
	if len(k) < 3 {
		return len(k) * 2
	}
	return int(k[2] & 0x0F)
}

func depthStateNodes(k []byte) int { // 8-byte key, length in low nibble of byte[7]
	if len(k) < 8 {
		return -1
	}
	return int(k[7] & 0x0F)
}

func depthStorageNodes(k []byte) int { // 28-byte: addr[0..4] | 8B path | addr[4..20]; length nibble at byte[11]
	if len(k) < 12 {
		return -1
	}
	return int(k[11] & 0x0F)
}

func scanCF(db *grocksdb.DB, h *grocksdb.ColumnFamilyHandle, agg *trieAgg, df depthFn) {
	if h == nil {
		return
	}
	ro := grocksdb.NewDefaultReadOptions()
	ro.SetFillCache(false)
	ro.SetReadaheadSize(4 * 1024 * 1024)
	defer ro.Destroy()
	it := db.NewIteratorCF(ro, h)
	defer it.Close()
	var n int64
	for it.SeekToFirst(); it.Valid(); it.Next() {
		k := it.Key()
		v := it.Value()
		kind, leaf := rlppkg.Classify(v.Data())
		agg.add(df(k.Data()), kind, v.Data())
		_ = leaf
		k.Free()
		v.Free()
		n++
		if n%50_000_000 == 0 {
			fmt.Fprintf(os.Stderr, "  ...%d nodes\n", n)
		}
	}
}

// FallbackNodes mixes account (prefix 0x00, depth=key[33]) and storage
// (prefix 0x01, depth=key[37]).
func scanFallback(db *grocksdb.DB, h *grocksdb.ColumnFamilyHandle, acct, stor *trieAgg) {
	if h == nil {
		return
	}
	ro := grocksdb.NewDefaultReadOptions()
	ro.SetFillCache(false)
	defer ro.Destroy()
	it := db.NewIteratorCF(ro, h)
	defer it.Close()
	for it.SeekToFirst(); it.Valid(); it.Next() {
		k := it.Key()
		v := it.Value()
		key := k.Data()
		kind, _ := rlppkg.Classify(v.Data())
		if len(key) > 0 && key[0] == 0x01 {
			d := -1
			if len(key) > 37 {
				d = int(key[37])
			}
			stor.add(d, kind, v.Data())
		} else {
			d := -1
			if len(key) > 33 {
				d = int(key[33])
			}
			acct.add(d, kind, v.Data())
		}
		k.Free()
		v.Free()
	}
}

func report(name string, a *trieAgg) {
	tb, te, tl := sum(a.branches), sum(a.extensions), sum(a.leaves)
	tot := tb + te + tl
	totBytes := sum(a.bytes)
	var depthWeighted, maxD int64
	for d := 0; d <= maxDepth; d++ {
		c := a.branches[d] + a.extensions[d] + a.leaves[d]
		depthWeighted += int64(d) * c
		if c > 0 && int64(d) > maxD {
			maxD = int64(d)
		}
	}
	avgD := 0.0
	if tot > 0 {
		avgD = float64(depthWeighted) / float64(tot)
	}
	occ := 0.0
	if tb > 0 {
		occ = float64(a.childSum) / float64(tb)
	}
	fmt.Printf("\n=== %s TRIE ===\n", name)
	fmt.Printf("branches=%d extensions=%d leaves=%d totalNodes=%d totalBytes=%d (%.2f GiB)\n",
		tb, te, tl, tot, totBytes, float64(totBytes)/(1<<30))
	fmt.Printf("avgPathDepth=%.2f maxDepth=%d avgBranchOccupancy=%.2f/16\n", avgD, maxD, occ)
	fmt.Printf("depth | branches | extensions | leaves | size(GiB)\n")
	depths := []int{}
	for d := 0; d <= maxDepth; d++ {
		if a.branches[d]+a.extensions[d]+a.leaves[d] > 0 {
			depths = append(depths, d)
		}
	}
	sort.Ints(depths)
	for _, d := range depths {
		fmt.Printf("%5d | %d | %d | %d | %.3f\n", d, a.branches[d], a.extensions[d], a.leaves[d],
			float64(a.bytes[d])/(1<<30))
	}
}

func must(err error, ctx string) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL %s: %v\n", ctx, err)
		os.Exit(1)
	}
}
