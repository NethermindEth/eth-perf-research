package rpc

import "time"

// StateCompositionLite mirrors Nethermind.StateComposition.Data.StateCompositionLite.
// JSON field names match the C# record's camelCase serialisation so the
// orchestrator's JSON deserialiser keeps working unchanged.
type StateCompositionLite struct {
	ChainHeadBlockNumber     int64 `json:"chainHeadBlockNumber"`
	IncrementalBlockNumber   int64 `json:"incrementalBlockNumber"`
	BlocksBehind             int64 `json:"blocksBehind"`
	DiffsSinceBaseline       int   `json:"diffsSinceBaseline"`
	HasIncrementalBaseline   bool  `json:"hasIncrementalBaseline"`
	IncrementalEnabled       bool  `json:"incrementalEnabled"`
	IncrementalDetailEnabled bool  `json:"incrementalDetailEnabled"`

	AccountsTotal     int64 `json:"accountsTotal"`
	ContractsTotal    int64 `json:"contractsTotal"`
	StorageSlotsTotal int64 `json:"storageSlotsTotal"`
	AccountTrieBytes  int64 `json:"accountTrieBytes"`
	StorageTrieBytes  int64 `json:"storageTrieBytes"`
	CodeBytesTotal    int64 `json:"codeBytesTotal"`

	ExactCountersActive      bool  `json:"exactCountersActive"`
	ExactCountersBlockNumber int64 `json:"exactCountersBlockNumber"`
}

// CumulativeTrieStats mirrors Nethermind.StateComposition.Data.CumulativeTrieStats.
// It is the nested payload of StateCompositionGet.TrieStats — the orchestrator's
// sensor.go reads `result.trieStats.{accountTrieBytes,storageTrieBytes,codeBytesTotal}`
// straight off this object.
type CumulativeTrieStats struct {
	AccountsTotal         int64   `json:"accountsTotal"`
	ContractsTotal        int64   `json:"contractsTotal"`
	StorageSlotsTotal     int64   `json:"storageSlotsTotal"`
	AccountTrieBranches   int64   `json:"accountTrieBranches"`
	AccountTrieExtensions int64   `json:"accountTrieExtensions"`
	AccountTrieLeaves     int64   `json:"accountTrieLeaves"`
	AccountTrieBytes      int64   `json:"accountTrieBytes"`
	StorageTrieBranches   int64   `json:"storageTrieBranches"`
	StorageTrieExtensions int64   `json:"storageTrieExtensions"`
	StorageTrieLeaves     int64   `json:"storageTrieLeaves"`
	StorageTrieBytes      int64   `json:"storageTrieBytes"`
	ContractsWithStorage  int64   `json:"contractsWithStorage"`
	EmptyAccounts         int64   `json:"emptyAccounts"`
	CodeBytesTotal        int64   `json:"codeBytesTotal"`
	SlotCountHistogram    []int64 `json:"slotCountHistogram"`
}

// StateCompositionGet is the verbose payload returned by statecomp_get. The
// schema matches the legacy NM Nethermind.StateComposition.Data.StateCompositionReport
// shape — nested `trieStats` plus top-level `blockNumber` — so the orchestrator's
// sensor.go works against either backend with no code change.
//
// All flat fields (accountsTotal, storageTrieBytes, slotCountHistogram, …) are
// kept as well for back-compat with existing dashboards and integration tests
// that pre-dated the wrapper. They are simply duplicated from trieStats.
type StateCompositionGet struct {
	BlockNumber int64               `json:"blockNumber"`
	StateRoot   string              `json:"stateRoot"`
	TrieStats   CumulativeTrieStats `json:"trieStats"`

	AccountsTotal        int64 `json:"accountsTotal"`
	ContractsTotal       int64 `json:"contractsTotal"`
	StorageSlotsTotal    int64 `json:"storageSlotsTotal"`
	CodeBytesTotal       int64 `json:"codeBytesTotal"`
	UniqueCodeHashes     int64 `json:"uniqueCodeHashes"`
	ContractsWithStorage int64 `json:"contractsWithStorage"`
	EmptyAccounts        int64 `json:"emptyAccounts"`

	AccountTrieBranches   int64 `json:"accountTrieBranches"`
	AccountTrieExtensions int64 `json:"accountTrieExtensions"`
	AccountTrieLeaves     int64 `json:"accountTrieLeaves"`
	AccountTrieBytes      int64 `json:"accountTrieBytes"`

	StorageTrieBranches   int64 `json:"storageTrieBranches"`
	StorageTrieExtensions int64 `json:"storageTrieExtensions"`
	StorageTrieLeaves     int64 `json:"storageTrieLeaves"`
	StorageTrieBytes      int64 `json:"storageTrieBytes"`

	SlotCountHistogram []int64 `json:"slotCountHistogram"`
}

// HealthReport is the sidecar-specific health probe. The orchestrator polls
// `lag_blocks` for back-pressure decisions.
type HealthReport struct {
	UpSeconds          int64 `json:"upSeconds"`
	BootstrapCompleted bool  `json:"bootstrapCompleted"`
	LastBlock          int64 `json:"lastBlock"`
	ChainHead          int64 `json:"chainHead"`
	LagBlocks          int64 `json:"lagBlocks"`
	LastSnapshotAgeS   int64 `json:"lastSnapshotAgeS"`
	PeakRSSMb          int64 `json:"peakRssMb"`
	GoroutineCount     int   `json:"goroutineCount"`
}

func (s *Server) statecompLite() StateCompositionLite {
	snap := s.tracker.SnapshotCounters()
	s.mu.RLock()
	head := s.chainHeadBlock
	s.mu.RUnlock()
	if head < snap.LastBlock {
		head = snap.LastBlock
	}
	return StateCompositionLite{
		ChainHeadBlockNumber:     head,
		IncrementalBlockNumber:   snap.LastBlock,
		BlocksBehind:             head - snap.LastBlock,
		DiffsSinceBaseline:       0,
		HasIncrementalBaseline:   s.bootstrapCompleted.Load(),
		IncrementalEnabled:       true,
		IncrementalDetailEnabled: true,
		AccountsTotal:            snap.AccountsTotal,
		ContractsTotal:           snap.ContractsTotal,
		StorageSlotsTotal:        snap.StorageSlotsTotal,
		AccountTrieBytes:         snap.AccountTrieBytes,
		StorageTrieBytes:         snap.StorageTrieBytes,
		CodeBytesTotal:           snap.CodeBytesTotal,
		ExactCountersActive:      s.bootstrapCompleted.Load(),
		ExactCountersBlockNumber: snap.LastBlock,
	}
}

func (s *Server) statecompGet() StateCompositionGet {
	snap := s.tracker.SnapshotCounters()
	hist := make([]int64, len(snap.SlotHistogram))
	for i, v := range snap.SlotHistogram {
		hist[i] = v
	}
	trieStats := CumulativeTrieStats{
		AccountsTotal:         snap.AccountsTotal,
		ContractsTotal:        snap.ContractsTotal,
		StorageSlotsTotal:     snap.StorageSlotsTotal,
		AccountTrieBranches:   snap.AccountTrieBranches,
		AccountTrieExtensions: snap.AccountTrieExtensions,
		AccountTrieLeaves:     snap.AccountTrieLeaves,
		AccountTrieBytes:      snap.AccountTrieBytes,
		StorageTrieBranches:   snap.StorageTrieBranches,
		StorageTrieExtensions: snap.StorageTrieExtensions,
		StorageTrieLeaves:     snap.StorageTrieLeaves,
		StorageTrieBytes:      snap.StorageTrieBytes,
		ContractsWithStorage:  snap.ContractsWithStorage,
		EmptyAccounts:         snap.EmptyAccounts,
		CodeBytesTotal:        snap.CodeBytesTotal,
		SlotCountHistogram:    hist,
	}
	return StateCompositionGet{
		BlockNumber:           snap.LastBlock,
		StateRoot:             hex32(snap.StateRoot),
		TrieStats:             trieStats,
		AccountsTotal:         snap.AccountsTotal,
		ContractsTotal:        snap.ContractsTotal,
		StorageSlotsTotal:     snap.StorageSlotsTotal,
		CodeBytesTotal:        snap.CodeBytesTotal,
		UniqueCodeHashes:      snap.UniqueCodeHashes,
		ContractsWithStorage:  snap.ContractsWithStorage,
		EmptyAccounts:         snap.EmptyAccounts,
		AccountTrieBranches:   snap.AccountTrieBranches,
		AccountTrieExtensions: snap.AccountTrieExtensions,
		AccountTrieLeaves:     snap.AccountTrieLeaves,
		AccountTrieBytes:      snap.AccountTrieBytes,
		StorageTrieBranches:   snap.StorageTrieBranches,
		StorageTrieExtensions: snap.StorageTrieExtensions,
		StorageTrieLeaves:     snap.StorageTrieLeaves,
		StorageTrieBytes:      snap.StorageTrieBytes,
		SlotCountHistogram:    hist,
	}
}

func (s *Server) statecompHealth() HealthReport {
	s.mu.RLock()
	head := s.chainHeadBlock
	startedAt := s.startedAt
	s.mu.RUnlock()
	last := s.tracker.LastBlock()
	if head < last {
		head = last
	}
	var snapAge int64
	if last := s.lastSnapshotAt.Load(); last != 0 {
		snapAge = int64(time.Since(time.Unix(0, last)).Seconds())
	}
	return HealthReport{
		UpSeconds:          int64(time.Since(startedAt).Seconds()),
		BootstrapCompleted: s.bootstrapCompleted.Load(),
		LastBlock:          last,
		ChainHead:          head,
		LagBlocks:          head - last,
		LastSnapshotAgeS:   snapAge,
		PeakRSSMb:          readRSSMB(),
	}
}
