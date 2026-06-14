package lifecycle

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/verbs"
)

// TestContractRegistryRoundTrip asserts a registry survives a save/load cycle
// through the state-dir JSON file.
func TestContractRegistryRoundTrip(t *testing.T) {
	dir := t.TempDir()

	reg := verbs.NewContractRegistry()
	reg.Set(verbs.ContractStorageSpam, common.HexToAddress("0x00000000000000000000000000000000c0117ac1"))
	reg.Set(verbs.ContractTestToken, common.HexToAddress("0x00000000000000000000000000000000c0117ac2"))

	if err := saveContractRegistry(dir, reg); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, err := loadContractRegistry(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got == nil {
		t.Fatal("load returned nil registry")
	}
	for _, name := range reg.Names() {
		want, _ := reg.Get(name)
		have, ok := got.Get(name)
		if !ok {
			t.Fatalf("loaded registry missing %q", name)
		}
		if have != want {
			t.Fatalf("%q: want %s, got %s", name, want.Hex(), have.Hex())
		}
	}
}

// TestLoadContractRegistryAbsent asserts a missing registry file yields
// (nil, nil) — the signal that a fresh bootstrap is required.
func TestLoadContractRegistryAbsent(t *testing.T) {
	reg, err := loadContractRegistry(t.TempDir())
	if err != nil {
		t.Fatalf("load absent: unexpected error %v", err)
	}
	if reg != nil {
		t.Fatalf("load absent: want nil registry, got %v", reg)
	}
}
