package lifecycle

import (
	"sort"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/verbs"
)

// deployedRegistry returns a registry populated with the four catalog
// contracts the bootstrap actually deploys on a bare devnet.
func deployedRegistry() *verbs.ContractRegistry {
	reg := verbs.NewContractRegistry()
	reg.Set(verbs.ContractStorageSpam, common.HexToAddress("0x01"))
	reg.Set(verbs.ContractTestToken, common.HexToAddress("0x02"))
	reg.Set(verbs.ContractStorageRefund, common.HexToAddress("0x03"))
	reg.Set(verbs.ContractGasBurner, common.HexToAddress("0x04"))
	return reg
}

// TestComputeEligibleVerbsExcludesUndeployedContractVerb is the regression
// test for the verb-selection bug: uniswap_swaps depends on a Uniswap router
// the bootstrap never deploys, so it must be excluded while every verb whose
// contract IS deployed (or which needs no contract) stays eligible.
func TestComputeEligibleVerbsExcludesUndeployedContractVerb(t *testing.T) {
	configured := []string{
		"eoatx", "calltx", "deploytx", "factorydeploytx",
		"storagespam", "erc20_bloater", "erc20tx", "uniswap_swaps",
		"storagerefundtx", "gasburnertx",
	}
	res := computeEligibleVerbs(configured, deployedRegistry())

	gotEligible := append([]string(nil), res.eligible...)
	sort.Strings(gotEligible)
	wantEligible := []string{
		"calltx", "deploytx", "eoatx", "erc20_bloater", "erc20tx",
		"factorydeploytx", "gasburnertx", "storagerefundtx", "storagespam",
	}
	if strings.Join(gotEligible, ",") != strings.Join(wantEligible, ",") {
		t.Fatalf("eligible = %v, want %v", gotEligible, wantEligible)
	}

	if len(res.exclusions) != 1 {
		t.Fatalf("exclusions = %v, want exactly 1 (uniswap_swaps)", res.exclusions)
	}
	if res.exclusions[0].verb != "uniswap_swaps" {
		t.Fatalf("excluded verb = %q, want uniswap_swaps", res.exclusions[0].verb)
	}
	if !strings.Contains(res.exclusions[0].reason, string(verbs.ContractUniswapRouter)) {
		t.Fatalf("exclusion reason %q must name the missing contract %q",
			res.exclusions[0].reason, verbs.ContractUniswapRouter)
	}
}

// TestComputeEligibleVerbsAllEligibleWhenContractsDeployed verifies a verb
// becomes eligible once its dependency is in the deployed set — the gate keys
// off the registry, it is not a hardcoded ban.
func TestComputeEligibleVerbsAllEligibleWhenContractsDeployed(t *testing.T) {
	reg := deployedRegistry()
	reg.Set(verbs.ContractUniswapRouter, common.HexToAddress("0x99"))

	res := computeEligibleVerbs([]string{"uniswap_swaps", "eoatx"}, reg)
	if len(res.exclusions) != 0 {
		t.Fatalf("exclusions = %v, want none once UniswapRouter is deployed", res.exclusions)
	}
	if len(res.eligible) != 2 {
		t.Fatalf("eligible = %v, want both verbs", res.eligible)
	}
}

// TestComputeEligibleVerbsEmptySetWhenNoContractsDeployed verifies the
// fail-loud precondition: when every configured verb depends on an undeployed
// contract the eligible set is empty, which Run treats as a fatal error.
func TestComputeEligibleVerbsEmptySetWhenNoContractsDeployed(t *testing.T) {
	emptyReg := verbs.NewContractRegistry()
	// Every verb here depends on a contract; with an empty registry none can run.
	res := computeEligibleVerbs([]string{"storagespam", "uniswap_swaps", "erc20tx"}, emptyReg)
	if len(res.eligible) != 0 {
		t.Fatalf("eligible = %v, want empty (no contracts deployed)", res.eligible)
	}
	if len(res.exclusions) != 3 {
		t.Fatalf("exclusions = %d, want 3", len(res.exclusions))
	}
}

// TestComputeEligibleVerbsNoDependencyAlwaysEligible verifies a verb with no
// contract dependency is eligible even against a completely empty registry.
func TestComputeEligibleVerbsNoDependencyAlwaysEligible(t *testing.T) {
	res := computeEligibleVerbs([]string{"eoatx", "deploytx", "factorydeploytx"}, verbs.NewContractRegistry())
	if len(res.eligible) != 3 || len(res.exclusions) != 0 {
		t.Fatalf("contract-free verbs must always be eligible: eligible=%v exclusions=%v",
			res.eligible, res.exclusions)
	}
}
