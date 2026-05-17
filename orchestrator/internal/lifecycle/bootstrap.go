package lifecycle

import (
	"context"
	"fmt"
	"log/slog"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/crypto"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/facade"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/orchpb"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/rpc"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/signer"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/verbs"
)

// bootstrapDeps is the narrow set of subsystems the bootstrap phase needs.
// deployGas is the resolved RunConfig gas limit for each bootstrap CREATE tx.
type bootstrapDeps struct {
	rpc       *rpc.Client
	signer    *signer.Signer
	facadeCtx *facade.Context
	stateDir  string
	chainID   uint64
	deployGas uint64
}

// bootstrapContracts ensures every Spamoor scenario contract the run's verbs
// require is deployed and recorded in the state-dir registry, then returns the
// registry. It is the one-time deploy phase that runs before the main bloat
// loop.
//
// Resume case: if the registry file exists AND eth_getCode is non-empty for
// every registered address, bootstrap is skipped and the existing registry is
// returned. This makes a resumed run after an EL-client restart re-use the
// contracts it already deployed.
//
// Fresh case: each required contract is deployed from the master signer as a
// CREATE transaction. The deploys are committed through testing_commitBlockV1
// in a single dedicated bootstrap block; each contract's address is computed
// deterministically as CreateAddress(signer, nonce). The registry is then
// fsynced to the state dir.
//
// After deployment (fresh or resume) a fail-loud guard asserts every required
// contract address has code; if any is empty it returns an error so the run
// halts rather than silently sending no-op txs to dead addresses.
func bootstrapContracts(ctx context.Context, deps *bootstrapDeps, runVerbs []string) (*verbs.ContractRegistry, error) {
	required := verbs.RequiredContracts(runVerbs)
	if len(required) == 0 {
		slog.Info("lifecycle: bootstrap: no contract-calling verbs enabled, skipping deploy")
		return verbs.NewContractRegistry(), nil
	}

	if reg, ok, err := tryResumeRegistry(ctx, deps, required); err != nil {
		return nil, err
	} else if ok {
		slog.Info("lifecycle: bootstrap: reusing deployed contracts", "registry", registrySummary(reg))
		return reg, nil
	}

	reg, err := deployContracts(ctx, deps, required)
	if err != nil {
		return nil, err
	}
	if err := saveContractRegistry(deps.stateDir, reg); err != nil {
		return nil, err
	}
	slog.Info("lifecycle: bootstrap: contracts deployed and registry written",
		"registry", registrySummary(reg), "path", contractsRegistryPath(deps.stateDir))

	if err := assertContractsHaveCode(ctx, deps.rpc, reg, required); err != nil {
		return nil, err
	}
	return reg, nil
}

// tryResumeRegistry loads the registry file and, if present, verifies every
// required contract has code on-chain. Returns (reg, true, nil) when the
// registry is complete and valid for resume.
func tryResumeRegistry(ctx context.Context, deps *bootstrapDeps, required []verbs.ContractName) (*verbs.ContractRegistry, bool, error) {
	reg, err := loadContractRegistry(deps.stateDir)
	if err != nil {
		return nil, false, err
	}
	if reg == nil {
		return nil, false, nil
	}
	for _, name := range required {
		addr, ok := reg.Get(name)
		if !ok {
			slog.Warn("lifecycle: bootstrap: registry missing a required contract, redeploying",
				"contract", name)
			return nil, false, nil
		}
		code, err := deps.rpc.CodeAt(ctx, addr, "latest")
		if err != nil {
			return nil, false, fmt.Errorf("lifecycle: bootstrap: getCode %s: %w", addr.Hex(), err)
		}
		if len(code) == 0 {
			slog.Warn("lifecycle: bootstrap: registered contract has no code, redeploying",
				"contract", name, "address", addr.Hex())
			return nil, false, nil
		}
	}
	return reg, true, nil
}

// deployContracts deploys each required contract from the master signer in a
// single bootstrap block and returns the resulting registry.
//
// The bootstrap nonces start at the facade context's current AddressCursor
// (primed from the chain's master-signer nonce). The CREATE addresses are
// computed deterministically from (signer, nonce); after the commit, the
// fail-loud guard cross-checks them against eth_getCode. The cursor is then
// advanced past the bootstrap nonces so the main loop's first batch does not
// collide with them.
func deployContracts(ctx context.Context, deps *bootstrapDeps, required []verbs.ContractName) (*verbs.ContractRegistry, error) {
	startNonce := deps.facadeCtx.LoadAddressCursor()
	signerAddr := deps.signer.Address()

	maxFee, maxPri := deps.facadeCtx.LoadFeePolicy()
	if maxFee == nil {
		maxFee = new(big.Int)
	}
	if maxPri == nil {
		maxPri = new(big.Int)
	}

	reg := verbs.NewContractRegistry()
	txIns := make([]*orchpb.TxIn, 0, len(required))
	for i, name := range required {
		spec, ok := verbs.ContractSpecFor(name, deps.deployGas)
		if !ok {
			return nil, fmt.Errorf("lifecycle: bootstrap: no catalog spec for %q", name)
		}
		nonce := startNonce + uint64(i)
		addr := crypto.CreateAddress(signerAddr, nonce)
		reg.Set(name, addr)
		txIns = append(txIns, &orchpb.TxIn{
			ChainId:              deps.chainID,
			Nonce:                nonce,
			Gas:                  spec.Gas,
			To:                   nil, // contract creation
			Data:                 spec.InitCode,
			MaxFeePerGas:         maxFee.Bytes(),
			MaxPriorityFeePerGas: maxPri.Bytes(),
		})
		slog.Info("lifecycle: bootstrap: prepared deploy",
			"contract", name, "nonce", nonce, "address", addr.Hex(),
			"init_code_len", len(spec.InitCode), "init_code", verbs.ContractInitCodeHex(spec))
	}

	raws, err := deps.signer.SignBatch(ctx, txIns)
	if err != nil {
		return nil, fmt.Errorf("lifecycle: bootstrap: sign deploy batch: %w", err)
	}

	blockTS := uint64(time.Now().Unix())
	blockHash, err := deps.rpc.TestingCommitBlockV1(ctx, raws, blockTS)
	if err != nil {
		return nil, fmt.Errorf("lifecycle: bootstrap: commit deploy block: %w", err)
	}
	slog.Info("lifecycle: bootstrap: deploy block committed",
		"block_hash", blockHash.Hex(), "contracts", len(required))

	// Advance the master-nonce cursor past the bootstrap deploys so the main
	// loop's first batch starts on the next free nonce.
	deps.facadeCtx.ReserveAddresses(uint64(len(required)))
	return reg, nil
}

// assertContractsHaveCode is the fail-loud guard: it asserts every required
// contract address has non-empty code on-chain. Any empty result is a fatal
// error — the orchestrator must never silently send no-op txs to a dead
// address again.
func assertContractsHaveCode(ctx context.Context, cli *rpc.Client, reg *verbs.ContractRegistry, required []verbs.ContractName) error {
	for _, name := range required {
		addr, ok := reg.Get(name)
		if !ok {
			return fmt.Errorf("lifecycle: bootstrap guard: required contract %q absent from registry", name)
		}
		code, err := cli.CodeAt(ctx, addr, "latest")
		if err != nil {
			return fmt.Errorf("lifecycle: bootstrap guard: getCode %s (%s): %w", name, addr.Hex(), err)
		}
		if len(code) == 0 {
			return fmt.Errorf(
				"lifecycle: bootstrap guard: contract %q at %s has NO code after deploy; "+
					"refusing to start the bloat loop (verbs would no-op)",
				name, addr.Hex())
		}
		slog.Info("lifecycle: bootstrap guard: contract has code",
			"contract", name, "address", addr.Hex(), "code_len", len(code))
	}
	return nil
}

// assertVerbContractsHaveCode runs the fail-loud guard for the contracts the
// run's verbs require, using an already-built registry. It is called from Run
// just before the hot loop starts so a resumed run also fails loudly if a
// previously-deployed contract vanished.
func assertVerbContractsHaveCode(ctx context.Context, cli *rpc.Client, reg *verbs.ContractRegistry, runVerbs []string) error {
	required := verbs.RequiredContracts(runVerbs)
	if len(required) == 0 {
		return nil
	}
	return assertContractsHaveCode(ctx, cli, reg, required)
}

