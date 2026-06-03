//go:build !nogrocksdb

// statescan walks a Nethermind FlatDb's trie-node column families STRUCTURALLY
// (root → leaf, following parent→child edges) and emits exact per-depth
// branch/extension/leaf/byte counts for the account and storage tries.
//
// Depth semantics — CRITICAL. The published #5 reference (statecomp_get
// trieDistribution) is produced by Nethermind.StateComposition's TrieDiffWalker,
// which assigns each node a STRUCTURAL depth: the number of branch+extension
// edges from the trie root. An extension that consumes several nibbles still
// advances depth by exactly ONE (TrieDiffWalker.Collection.WalkStructure:
// childDepth = depth + 1 for both branch and extension). Depth is clamped to 15.
//
// This is NOT the same as the nibble path-length stored in the FlatDb key. A
// flat per-key scan that reads the key's path-length field over-counts depth
// (leaf reached via extensions appears 1-4 levels too deep), which is the v19
// "phantom depth 13-16" bug. The only way to reproduce the reference exactly is
// to walk the trie structurally, which is what this scanner does.
//
// Node lookup uses the FlatDb's path-keyed layout (BaseTriePersistence): a node
// at TreePath p is fetched by encoding p into the column-family key — no node
// hashes are needed for resolution because FlatInTrie keys nodes by path.
//
// Usage: statescan <flatdb-path>   (opens as RocksDB secondary; never writes the source)
package main

import (
	"fmt"
	"os"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	rlppkg "github.com/NethermindEth/eth-perf-research/orchestrator/pkg/sidecar/rlp"
	gethrlp "github.com/ethereum/go-ethereum/rlp"
	"github.com/linxGnu/grocksdb"
)

const (
	maxDepth     = 64
	clampDepth   = 15 // reference clamps structural depth to [0,15]
	fullPathLen  = 32
	storagePfx   = 4  // address prefix bytes kept in front
	storageHash  = 20 // truncated address-hash bytes used in storage keys
	shortPathLen = 8  // EncodeWith8Byte output length
)

type trieAgg struct {
	branches   [maxDepth + 1]int64
	extensions [maxDepth + 1]int64
	leaves     [maxDepth + 1]int64
	bytes      [maxDepth + 1]int64
	childSum   int64
}

func (a *trieAgg) addBranch(depth int, n int64, children, bytes int64) {
	if depth > maxDepth {
		depth = maxDepth
	}
	a.branches[depth] += n
	a.childSum += children
	a.bytes[depth] += bytes
}

func (a *trieAgg) addExt(depth int, bytes int64) {
	if depth > maxDepth {
		depth = maxDepth
	}
	a.extensions[depth]++
	a.bytes[depth] += bytes
}

func (a *trieAgg) addLeaf(depth int, bytes int64) {
	if depth > maxDepth {
		depth = maxDepth
	}
	a.leaves[depth]++
	a.bytes[depth] += bytes
}

func (a *trieAgg) mergeFrom(o *trieAgg) {
	for d := 0; d <= maxDepth; d++ {
		a.branches[d] += o.branches[d]
		a.extensions[d] += o.extensions[d]
		a.leaves[d] += o.leaves[d]
		a.bytes[d] += o.bytes[d]
	}
	a.childSum += o.childSum
}

func sumArr(a [maxDepth + 1]int64) int64 {
	var t int64
	for _, v := range a {
		t += v
	}
	return t
}

// ---- TreePath: 64-nibble path packed 2/byte (even idx → hi nibble) ----

type treePath struct {
	bytes  [fullPathLen]byte
	length int // number of valid nibbles [0..64]
}

func (p *treePath) nibble(i int) byte {
	b := p.bytes[i/2]
	if i&1 == 0 {
		return b >> 4
	}
	return b & 0x0f
}

func (p *treePath) setNibble(i int, v byte) {
	off := i / 2
	if i&1 == 0 {
		p.bytes[off] = (p.bytes[off] & 0x0f) | (v&0x0f)<<4
	} else {
		p.bytes[off] = (p.bytes[off] & 0xf0) | (v & 0x0f)
	}
}

func (p treePath) appendNibble(v byte) treePath {
	p.setNibble(p.length, v)
	p.length++
	return p
}

func (p treePath) appendNibbles(ns []byte) treePath {
	for _, n := range ns {
		p.setNibble(p.length, n)
		p.length++
	}
	return p
}

// ---- FlatDb key encoders (mirror BaseTriePersistence exactly) ----

func encodeStateKey(p treePath) []byte {
	switch {
	case p.length <= 5: // StateNodesTop, 3 bytes
		k := make([]byte, 3)
		copy(k, p.bytes[:3])
		k[2] = (k[2] & 0xf0) | byte(p.length&0x0f)
		return k
	case p.length <= 15: // StateNodes, 8 bytes
		k := make([]byte, 8)
		copy(k, p.bytes[:8])
		k[7] = (k[7] & 0xf0) | byte(p.length&0x0f)
		return k
	default: // FallbackNodes, 34 bytes, prefix 0x00
		k := make([]byte, 1+fullPathLen+1)
		k[0] = 0
		copy(k[1:], p.bytes[:])
		k[1+fullPathLen] = byte(p.length)
		return k
	}
}

// encodeStorageKey encodes a storage node path under a given 20-byte addr hash.
func encodeStorageKey(addr20 []byte, p treePath) []byte {
	if p.length <= 15 { // StorageNodes, 28 bytes
		k := make([]byte, storagePfx+shortPathLen+(storageHash-storagePfx))
		copy(k[0:storagePfx], addr20[:storagePfx])
		// EncodeWith8Byte: first 8 path bytes, length in low nibble of byte 7
		copy(k[storagePfx:storagePfx+shortPathLen], p.bytes[:shortPathLen])
		k[storagePfx+shortPathLen-1] = (k[storagePfx+shortPathLen-1] & 0xf0) | byte(p.length&0x0f)
		copy(k[storagePfx+shortPathLen:], addr20[storagePfx:storageHash])
		return k
	}
	// FallbackNodes, 54 bytes, prefix 0x01
	k := make([]byte, 1+storagePfx+fullPathLen+1+(storageHash-storagePfx))
	k[0] = 1
	copy(k[1:1+storagePfx], addr20[:storagePfx])
	copy(k[1+storagePfx:1+storagePfx+fullPathLen], p.bytes[:])
	k[1+storagePfx+fullPathLen] = byte(p.length)
	copy(k[1+storagePfx+fullPathLen+1:], addr20[storagePfx:storageHash])
	return k
}

// ---- Trie node parsing ----

type nodeKind int

const (
	kBranch nodeKind = iota
	kExtension
	kLeaf
	kInvalid
)

// childRef is one branch/extension child slot: either a 32-byte hash reference
// (resolve by path) or an inline node RLP (recurse directly, never stored by
// path). inline is the raw RLP item (list) when the child is embedded.
type childRef struct {
	nibble byte   // branch slot index (0..15); 0 for extension
	inline []byte // non-nil ⇒ embedded node RLP to recurse directly
	isRef  bool   // true ⇒ 32-byte hash reference, resolve child by path
}

// parseNode classifies a node's RLP and extracts what the walker needs:
//   - branch: childRefs for each non-empty slot 0..15 (inline vs hash-ref)
//   - extension: keyNibbles + the single child ref (inline vs hash-ref)
//   - leaf: keyNibbles (decoded hex-prefix path) + inner value content
func parseNode(rlp []byte) (kind nodeKind, children []childRef, keyNibbles []byte, value []byte) {
	listContent, _, err := gethrlp.SplitList(rlp)
	if err != nil {
		return kInvalid, nil, nil, nil
	}
	count, err := gethrlp.CountValues(listContent)
	if err != nil {
		return kInvalid, nil, nil, nil
	}
	if count == 17 {
		rest := listContent
		var refs []childRef
		for i := 0; i < 16; i++ {
			kind, c, r, e := gethrlp.Split(rest)
			if e != nil {
				return kInvalid, nil, nil, nil
			}
			if len(c) > 0 {
				refs = append(refs, classifyChild(byte(i), kind, c, rest))
			}
			rest = r
		}
		return kBranch, refs, nil, nil
	}
	if count != 2 {
		return kInvalid, nil, nil, nil
	}
	pkind, encPath, rest, err := gethrlp.Split(listContent)
	if err != nil {
		return kInvalid, nil, nil, nil
	}
	_ = pkind
	if len(encPath) == 0 {
		return kInvalid, nil, nil, nil
	}
	nibs, isLeaf := decodeHexPrefix(encPath)
	if isLeaf {
		_, val, _, e := gethrlp.Split(rest)
		if e != nil {
			val = nil
		}
		return kLeaf, nil, nibs, val
	}
	// extension: second item is the child (hash ref or inline node)
	ckind, cc, _, e := gethrlp.Split(rest)
	if e != nil {
		return kInvalid, nil, nil, nil
	}
	return kExtension, []childRef{classifyChild(0, ckind, cc, rest)}, nibs, nil
}

// classifyChild distinguishes a 32-byte hash reference (string of len 32) from
// an inline node (an RLP list embedded in the parent). rawItem is the full RLP
// encoding of this child item (needed to recurse on an inline list).
func classifyChild(nibble byte, kind gethrlp.Kind, content, rawRemaining []byte) childRef {
	if kind == gethrlp.List {
		// Inline node: recover the item's full encoding (prefix + content).
		itemLen := len(rawRemaining)
		if _, _, rest, err := gethrlp.Split(rawRemaining); err == nil {
			itemLen = len(rawRemaining) - len(rest)
		}
		return childRef{nibble: nibble, inline: append([]byte(nil), rawRemaining[:itemLen]...)}
	}
	// String: a 32-byte keccak reference (resolve by path).
	return childRef{nibble: nibble, isRef: true}
}

// decodeHexPrefix decodes a compact (hex-prefix) encoded path into nibbles and
// reports whether the terminator (leaf) flag is set.
func decodeHexPrefix(enc []byte) (nibbles []byte, isLeaf bool) {
	if len(enc) == 0 {
		return nil, false
	}
	flag := enc[0] >> 4
	isLeaf = flag&0x2 != 0
	odd := flag&0x1 != 0
	var nibs []byte
	if odd {
		nibs = append(nibs, enc[0]&0x0f)
	}
	for _, b := range enc[1:] {
		nibs = append(nibs, b>>4, b&0x0f)
	}
	return nibs, isLeaf
}

// ---- Walker ----

type walker struct {
	db        *grocksdb.DB
	stateTop  *grocksdb.ColumnFamilyHandle
	stateMid  *grocksdb.ColumnFamilyHandle
	storage   *grocksdb.ColumnFamilyHandle
	fallback  *grocksdb.ColumnFamilyHandle
	ro        *grocksdb.ReadOptions
	nodeCount atomic.Int64
}

func (w *walker) getState(p treePath) []byte {
	var cf *grocksdb.ColumnFamilyHandle
	if p.length <= 15 {
		if p.length <= 5 {
			cf = w.stateTop
		} else {
			cf = w.stateMid
		}
	} else {
		cf = w.fallback
	}
	v, err := w.db.GetCF(w.ro, cf, encodeStateKey(p))
	if err != nil || v == nil {
		return nil
	}
	defer v.Free()
	if !v.Exists() {
		return nil
	}
	return append([]byte(nil), v.Data()...)
}

func (w *walker) getStorage(addr20 []byte, p treePath) []byte {
	cf := w.storage
	if p.length > 15 {
		cf = w.fallback
	}
	v, err := w.db.GetCF(w.ro, cf, encodeStorageKey(addr20, p))
	if err != nil || v == nil {
		return nil
	}
	defer v.Free()
	if !v.Exists() {
		return nil
	}
	return append([]byte(nil), v.Data()...)
}

// walkState resolves an account node at path p (by key lookup) and recurses.
func (w *walker) walkState(p treePath, depth int, agg *trieAgg, storeSink func(addr20 []byte)) {
	rlp := w.getState(p)
	if rlp == nil {
		return
	}
	w.recurse(rlp, p, depth, agg, false, nil, storeSink)
}

// walkStorage resolves a storage node at path p for addr20 and recurses.
func (w *walker) walkStorage(addr20 []byte, p treePath, depth int, agg *trieAgg) {
	rlp := w.getStorage(addr20, p)
	if rlp == nil {
		return
	}
	w.recurse(rlp, p, depth, agg, true, addr20, nil)
}

// recurse records the node at structural `depth` and descends into its children.
// Hash-referenced children are resolved by path; inline children are recursed
// directly (they are never stored under their own key).
func (w *walker) recurse(rlp []byte, p treePath, depth int, agg *trieAgg,
	isStorage bool, addr20 []byte, storeSink func(addr20 []byte),
) {
	w.nodeCount.Add(1)
	d := depth
	if d > clampDepth {
		d = clampDepth
	}
	kind, children, keyNibs, value := parseNode(rlp)
	switch kind {
	case kBranch:
		agg.addBranch(d, 1, int64(len(children)), int64(len(rlp)))
		for _, ch := range children {
			cp := p.appendNibble(ch.nibble)
			w.descend(ch, cp, depth+1, agg, isStorage, addr20, storeSink)
		}
	case kExtension:
		agg.addExt(d, int64(len(rlp)))
		cp := p.appendNibbles(keyNibs)
		if len(children) == 1 {
			w.descend(children[0], cp, depth+1, agg, isStorage, addr20, storeSink)
		}
	case kLeaf:
		agg.addLeaf(d, int64(len(rlp)))
		if !isStorage && storeSink != nil {
			full := p.appendNibbles(keyNibs)
			if full.length == 64 && accountHasStorage(value) {
				storeSink(full.bytes[:storageHash])
			}
		}
	}
}

// descend follows one child edge: inline children recurse on embedded RLP,
// hash-referenced children resolve by path.
func (w *walker) descend(ch childRef, cp treePath, depth int, agg *trieAgg,
	isStorage bool, addr20 []byte, storeSink func(addr20 []byte),
) {
	if ch.inline != nil {
		w.recurse(ch.inline, cp, depth, agg, isStorage, addr20, storeSink)
		return
	}
	if isStorage {
		w.walkStorage(addr20, cp, depth, agg)
	} else {
		w.walkState(cp, depth, agg, storeSink)
	}
}

// accountHasStorage decodes the leaf's inner account RLP and reports whether the
// account has a non-empty storage trie.
func accountHasStorage(leafValue []byte) bool {
	info, ok := rlppkg.DecodeAccount(leafValue)
	return ok && info.HasStorage
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: statescan <flatdb-path>")
		os.Exit(2)
	}
	path := os.Args[1]

	blockCacheMiB := 2048
	if v := os.Getenv("STATESCAN_BLOCK_CACHE_MIB"); v != "" {
		fmt.Sscanf(v, "%d", &blockCacheMiB)
	}
	parallel := runtime.NumCPU()
	if v := os.Getenv("STATESCAN_PARALLEL"); v != "" {
		fmt.Sscanf(v, "%d", &parallel)
	}
	accountOnly := os.Getenv("STATESCAN_ACCOUNT_ONLY") == "1"

	cache := grocksdb.NewLRUCache(uint64(blockCacheMiB) * 1024 * 1024)
	bbto := grocksdb.NewDefaultBlockBasedTableOptions()
	bbto.SetBlockCache(cache)
	bbto.SetCacheIndexAndFilterBlocks(true)
	opts := grocksdb.NewDefaultOptions()
	opts.SetBlockBasedTableFactory(bbto)
	opts.SetCreateIfMissing(false)
	opts.SetMaxOpenFiles(512)

	listed, err := grocksdb.ListColumnFamilies(opts, path)
	must(err, "list cfs")
	cfOpts := make([]*grocksdb.Options, len(listed))
	for i := range cfOpts {
		cfOpts[i] = opts
	}
	secDir, err := os.MkdirTemp("", "statescan-secondary-")
	must(err, "mk secondary dir")
	defer os.RemoveAll(secDir)
	gdb, handles, err := grocksdb.OpenDbAsSecondaryColumnFamilies(opts, path, secDir, listed, cfOpts)
	must(err, "open secondary")
	defer gdb.Close()
	_ = gdb.TryCatchUpWithPrimary()
	cf := map[string]*grocksdb.ColumnFamilyHandle{}
	for i, name := range listed {
		cf[name] = handles[i]
	}
	fmt.Fprintf(os.Stderr, "CFs: %v | parallel=%d blockCacheMiB=%d accountOnly=%v\n",
		listed, parallel, blockCacheMiB, accountOnly)

	ro := grocksdb.NewDefaultReadOptions()
	ro.SetFillCache(true)
	w := &walker{
		db:       gdb,
		stateTop: cf["StateTopNodes"],
		stateMid: cf["StateNodes"],
		storage:  cf["StorageNodes"],
		fallback: cf["FallbackNodes"],
		ro:       ro,
	}

	start := time.Now()
	progressDone := make(chan struct{})
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-progressDone:
				return
			case <-t.C:
				fmt.Fprintf(os.Stderr, "  ...%d nodes (%.0fs)\n",
					w.nodeCount.Load(), time.Since(start).Seconds())
			}
		}
	}()

	var acct, stor trieAgg

	// Phase 1: walk the account trie with N workers fanned out over the root's
	// children. Collect (addrHash, hasStorage) for the storage phase.
	var contractsMu sync.Mutex
	var contracts [][storageHash]byte
	storeSink := func(addr20 []byte) {
		var c [storageHash]byte
		copy(c[:], addr20)
		contractsMu.Lock()
		contracts = append(contracts, c)
		contractsMu.Unlock()
	}

	// Resolve root node, then parallelize over its children subtrees.
	rootRlp := w.getState(treePath{})
	if rootRlp == nil {
		fmt.Fprintln(os.Stderr, "FATAL: no account root node found")
		os.Exit(1)
	}
	walkAccountParallel(w, rootRlp, parallel, &acct, storeSink)

	report("ACCOUNT", &acct)
	fmt.Fprintf(os.Stderr, "account walk done in %.0fs, contracts-with-storage=%d\n",
		time.Since(start).Seconds(), len(contracts))

	if !accountOnly {
		// Phase 2: walk each contract's storage trie. Parallelize across contracts.
		storStart := time.Now()
		walkStorageParallel(w, contracts, parallel, &stor)
		report("STORAGE", &stor)
		fmt.Fprintf(os.Stderr, "storage walk done in %.0fs\n", time.Since(storStart).Seconds())
	}

	close(progressDone)
}

// walkAccountParallel records the root node then fans out its child subtrees
// across `parallel` workers. Each job is one child edge of the root.
func walkAccountParallel(w *walker, rootRlp []byte, parallel int, agg *trieAgg,
	storeSink func(addr20 []byte),
) {
	w.nodeCount.Add(1)
	kind, children, keyNibs, value := parseNode(rootRlp)
	root := treePath{}

	type job struct {
		ch    childRef
		p     treePath
		depth int
	}
	var jobs []job

	switch kind {
	case kBranch:
		agg.addBranch(0, 1, int64(len(children)), int64(len(rootRlp)))
		for _, ch := range children {
			jobs = append(jobs, job{ch, root.appendNibble(ch.nibble), 1})
		}
	case kExtension:
		agg.addExt(0, int64(len(rootRlp)))
		if len(children) == 1 {
			jobs = append(jobs, job{children[0], root.appendNibbles(keyNibs), 1})
		}
	case kLeaf:
		agg.addLeaf(0, int64(len(rootRlp)))
		full := root.appendNibbles(keyNibs)
		if full.length == 64 && accountHasStorage(value) {
			storeSink(full.bytes[:storageHash])
		}
		return
	default:
		return
	}

	jobCh := make(chan job, len(jobs))
	for _, j := range jobs {
		jobCh <- j
	}
	close(jobCh)

	aggs := make([]trieAgg, parallel)
	var wg sync.WaitGroup
	for i := 0; i < parallel; i++ {
		wg.Add(1)
		go func(local *trieAgg) {
			defer wg.Done()
			for j := range jobCh {
				w.descend(j.ch, j.p, j.depth, local, false, nil, storeSink)
			}
		}(&aggs[i])
	}
	wg.Wait()
	for i := range aggs {
		agg.mergeFrom(&aggs[i])
	}
}

func walkStorageParallel(w *walker, contracts [][20]byte, parallel int, agg *trieAgg) {
	jobCh := make(chan [20]byte, parallel*4)
	aggs := make([]trieAgg, parallel)
	var wg sync.WaitGroup
	for i := 0; i < parallel; i++ {
		wg.Add(1)
		go func(local *trieAgg) {
			defer wg.Done()
			for addr := range jobCh {
				a := addr
				w.walkStorage(a[:], treePath{}, 0, local)
			}
		}(&aggs[i])
	}
	for _, c := range contracts {
		jobCh <- c
	}
	close(jobCh)
	wg.Wait()
	for i := range aggs {
		agg.mergeFrom(&aggs[i])
	}
}

func report(name string, a *trieAgg) {
	tb, te, tl := sumArr(a.branches), sumArr(a.extensions), sumArr(a.leaves)
	tot := tb + te + tl
	totBytes := sumArr(a.bytes)
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
