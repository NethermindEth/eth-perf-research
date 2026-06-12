package replaycommit

import (
	"crypto/ecdsa"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

// NonceFixer rewrites the nonces of recorded transactions from one sender so
// they fit the clean chain, re-signing with the sender's key.
//
// Why this exists: the recorded x5 chain carries a wedge artifact deeper than
// the poisoned roots — after the 2026-05-28 state wedge, Nethermind's persisted
// state silently lost the effects of ~76k already-included master txs (the
// phantom-root trajectory). When the orchestrator restarted it re-queried the
// master nonce from that phantom state and continued ~76k nonces BELOW the true
// trajectory. Those recorded txs were includable on the poisoned chain
// (NoValidation + phantom state agreed) but are nonce-too-low on a clean
// re-execution. Rewriting ONLY the nonce — calldata, target, gas, fee fields and
// value stay byte-identical — preserves every storage/balance effect the bloat
// intended while making the blocks fully valid for both Nethermind and Geth.
type NonceFixer struct {
	key  *ecdsa.PrivateKey
	addr common.Address
	// next is the clean chain's expected nonce for addr; advanced per fixed tx.
	next uint64
	// initialized guards lazy nonce seeding via the EL on first use.
	initialized bool

	rewritten int
	kept      int
}

// NewNonceFixer builds a fixer for the single bloat sender key (hex, no 0x).
func NewNonceFixer(keyHex string) (*NonceFixer, error) {
	key, err := crypto.HexToECDSA(keyHex)
	if err != nil {
		return nil, fmt.Errorf("noncefix: parse key: %w", err)
	}
	return &NonceFixer{key: key, addr: crypto.PubkeyToAddress(key.PublicKey)}, nil
}

// Seed sets the expected next nonce (the clean chain's eth_getTransactionCount
// at latest, fetched by the driver after head detection).
func (f *NonceFixer) Seed(next uint64) {
	f.next = next
	f.initialized = true
}

// Address returns the sender address whose txs are fixed.
func (f *NonceFixer) Address() common.Address { return f.addr }

// Stats reports how many txs were rewritten vs kept verbatim.
func (f *NonceFixer) Stats() (rewritten, kept int) { return f.rewritten, f.kept }

// Fix returns the tx list with sender-matching txs renumbered onto the clean
// nonce trajectory. Txs whose recorded nonce already matches are passed through
// byte-identical (hash preserved); only mismatches are rebuilt and re-signed.
// Txs from other senders pass through untouched and do not advance the counter.
func (f *NonceFixer) Fix(txs [][]byte) ([][]byte, error) {
	if !f.initialized {
		return nil, fmt.Errorf("noncefix: not seeded")
	}
	out := make([][]byte, len(txs))
	for i, raw := range txs {
		var tx types.Transaction
		if err := tx.UnmarshalBinary(raw); err != nil {
			return nil, fmt.Errorf("noncefix: decode tx %d: %w", i, err)
		}
		signer := types.LatestSignerForChainID(tx.ChainId())
		from, err := types.Sender(signer, &tx)
		if err != nil {
			return nil, fmt.Errorf("noncefix: recover sender tx %d: %w", i, err)
		}
		if from != f.addr {
			out[i] = raw
			continue
		}
		if tx.Nonce() == f.next {
			out[i] = raw
			f.kept++
			f.next++
			continue
		}
		fixed, err := f.resign(&tx, signer)
		if err != nil {
			return nil, fmt.Errorf("noncefix: re-sign tx %d (nonce %d -> %d): %w", i, tx.Nonce(), f.next, err)
		}
		out[i] = fixed
		f.rewritten++
		f.next++
	}
	return out, nil
}

// resign rebuilds tx with nonce f.next, preserving every other field, and signs
// it with the fixer key.
func (f *NonceFixer) resign(tx *types.Transaction, signer types.Signer) ([]byte, error) {
	var inner types.TxData
	switch tx.Type() {
	case types.DynamicFeeTxType:
		inner = &types.DynamicFeeTx{
			ChainID:    tx.ChainId(),
			Nonce:      f.next,
			GasTipCap:  tx.GasTipCap(),
			GasFeeCap:  tx.GasFeeCap(),
			Gas:        tx.Gas(),
			To:         tx.To(),
			Value:      tx.Value(),
			Data:       tx.Data(),
			AccessList: tx.AccessList(),
		}
	case types.AccessListTxType:
		inner = &types.AccessListTx{
			ChainID:    tx.ChainId(),
			Nonce:      f.next,
			GasPrice:   tx.GasPrice(),
			Gas:        tx.Gas(),
			To:         tx.To(),
			Value:      tx.Value(),
			Data:       tx.Data(),
			AccessList: tx.AccessList(),
		}
	case types.LegacyTxType:
		inner = &types.LegacyTx{
			Nonce:    f.next,
			GasPrice: tx.GasPrice(),
			Gas:      tx.Gas(),
			To:       tx.To(),
			Value:    tx.Value(),
			Data:     tx.Data(),
		}
	default:
		return nil, fmt.Errorf("unsupported tx type %d", tx.Type())
	}
	signed, err := types.SignNewTx(f.key, signer, inner)
	if err != nil {
		return nil, err
	}
	return signed.MarshalBinary()
}
