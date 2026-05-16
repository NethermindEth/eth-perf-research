package verbs

import (
	"encoding/hex"
	"fmt"
	"sort"

	"github.com/ethereum/go-ethereum/common"
)

// ContractName identifies a Spamoor scenario contract the orchestrator must
// deploy once at startup before any contract-calling verb can do real work.
type ContractName string

const (
	// ContractStorageSpam backs the storagespam verb (and the erc20_bloater
	// and uniswap_swaps stand-ins). setRandomForGas writes pseudo-random
	// storage slots — the primary storage bloater.
	ContractStorageSpam ContractName = "StorageSpam"
	// ContractTestToken backs the erc20tx verb. transferMint grows the
	// token's balance mapping.
	ContractTestToken ContractName = "TestToken"
	// ContractStorageRefund backs the storagerefundtx verb. execute writes
	// and clears storage windows to exercise SSTORE refunds.
	ContractStorageRefund ContractName = "StorageRefund"
	// ContractGasBurner backs the gasburnertx verb. Its runtime loops,
	// burning gas until a remainder threshold, then emits one LOG1.
	ContractGasBurner ContractName = "GasBurner"
)

// ContractSpec describes one contract the bootstrap phase deploys.
type ContractSpec struct {
	Name ContractName
	// InitCode is the CREATE-tx calldata (creation/init bytecode).
	InitCode []byte
	// Gas is the gas limit for the bootstrap CREATE transaction.
	Gas uint64
}

// contractCatalog returns the deterministic, ordered list of contracts the
// bootstrap phase deploys. Order is fixed so CreateAddress nonce arithmetic is
// reproducible across runs. The gas burner init code is assembled at call time
// from the geas templates (gasBurnerCreationCode).
func contractCatalog() []ContractSpec {
	return []ContractSpec{
		{Name: ContractStorageSpam, InitCode: mustHex(storageSpamInitHex), Gas: 2_000_000},
		{Name: ContractTestToken, InitCode: mustHex(testTokenInitHex), Gas: 2_000_000},
		{Name: ContractStorageRefund, InitCode: mustHex(storageRefundInitHex), Gas: 2_000_000},
		{Name: ContractGasBurner, InitCode: gasBurnerCreationCode(), Gas: 2_000_000},
	}
}

// ContractRegistry maps a deployed contract name to its on-chain address. It
// is produced by the bootstrap phase, persisted to the state dir, and threaded
// into every contract-calling verb via BuildCtx.
type ContractRegistry struct {
	Contracts map[ContractName]common.Address
}

// NewContractRegistry returns an empty registry.
func NewContractRegistry() *ContractRegistry {
	return &ContractRegistry{Contracts: map[ContractName]common.Address{}}
}

// Set records a deployed contract address.
func (r *ContractRegistry) Set(name ContractName, addr common.Address) {
	r.Contracts[name] = addr
}

// Get returns the deployed address for name. The bool is false when the
// contract is absent — callers that need the address must fail loudly.
func (r *ContractRegistry) Get(name ContractName) (common.Address, bool) {
	if r == nil {
		return common.Address{}, false
	}
	a, ok := r.Contracts[name]
	return a, ok
}

// MustGet returns the deployed address for name or panics. Verbs call this
// after the lifecycle's fail-loud guard has already asserted every contract
// has code, so a missing entry here is a programming error.
func (r *ContractRegistry) MustGet(name ContractName) common.Address {
	a, ok := r.Get(name)
	if !ok {
		panic(fmt.Sprintf("verbs: contract %q not in registry", name))
	}
	return a
}

// Names returns the registry's contract names in sorted order.
func (r *ContractRegistry) Names() []ContractName {
	out := make([]ContractName, 0, len(r.Contracts))
	for n := range r.Contracts {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// contractVerbTargets maps each contract-calling verb to the contract it must
// target. Verbs absent from this map (eoatx, noop, deploytx, factorydeploytx,
// evm_fuzz, blob_combined, uniswap_swaps) need no pre-deployed contract.
//
//   - storagespam      → StorageSpam     (setRandomForGas — real storage write)
//   - erc20tx          → TestToken       (transferMint — real mint/transfer)
//   - storagerefundtx  → StorageRefund   (execute — real refund method)
//   - gasburnertx      → GasBurner       (geas gas-burner)
//   - calltx           → StorageSpam     (getStorage — non-reverting touch)
//   - erc20_bloater    → StorageSpam     (setRandomForGas — bulk storage write;
//                                         shares StorageSpam with storagespam,
//                                         it has no dedicated Spamoor contract)
//
// uniswap_swaps is absent: the EELS build_uniswap_swaps_transactions
// adaptation targets a fixed router placeholder address, not a contract the
// bootstrap phase deploys, so the verb resolves its target internally.
var contractVerbTargets = map[string]ContractName{
	"storagespam":     ContractStorageSpam,
	"erc20tx":         ContractTestToken,
	"storagerefundtx": ContractStorageRefund,
	"gasburnertx":     ContractGasBurner,
	"calltx":          ContractStorageSpam,
	"erc20_bloater":   ContractStorageSpam,
}

// ContractVerbTarget returns the contract a verb must target, or false when
// the verb needs no pre-deployed contract.
func ContractVerbTarget(verb string) (ContractName, bool) {
	c, ok := contractVerbTargets[verb]
	return c, ok
}

// RequiredContracts returns the set of contracts the given verbs require, in
// catalog order. Used by the bootstrap phase to deploy only what is needed and
// by the fail-loud guard to know which addresses must have code.
func RequiredContracts(verbs []string) []ContractName {
	want := map[ContractName]bool{}
	for _, v := range verbs {
		if c, ok := contractVerbTargets[v]; ok {
			want[c] = true
		}
	}
	out := make([]ContractName, 0, len(want))
	for _, spec := range contractCatalog() {
		if want[spec.Name] {
			out = append(out, spec.Name)
		}
	}
	return out
}

// ContractSpecFor returns the catalog spec for a contract name.
func ContractSpecFor(name ContractName) (ContractSpec, bool) {
	for _, spec := range contractCatalog() {
		if spec.Name == name {
			return spec, true
		}
	}
	return ContractSpec{}, false
}

// ContractInitCodeHex returns a contract's init code as a 0x-prefixed hex
// string — used by the bootstrap deployer's logging so the exact deploy tx
// payload is recoverable from the run log.
func ContractInitCodeHex(spec ContractSpec) string {
	return "0x" + hex.EncodeToString(spec.InitCode)
}

// verbTarget resolves the deployed contract address a verb must call. It
// returns an error when the registry is missing the verb's contract — verbs
// surface this so the dispatcher fails the batch loudly rather than sending a
// no-op tx to a dead address.
func verbTarget(ctx BuildCtx, verb string) (common.Address, error) {
	name, ok := ContractVerbTarget(verb)
	if !ok {
		return common.Address{}, fmt.Errorf("verbs: %s is not a contract-calling verb", verb)
	}
	addr, ok := ctx.Contracts.Get(name)
	if !ok {
		return common.Address{}, fmt.Errorf("verbs: %s target contract %q not in registry", verb, name)
	}
	return addr, nil
}
