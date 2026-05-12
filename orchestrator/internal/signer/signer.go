package signer

import (
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"fmt"
	"math/big"
	"runtime"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"golang.org/x/sync/errgroup"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/orchpb"
)

// Signer holds a parsed ECDSA private key and signs EIP-1559 transactions.
type Signer struct {
	privKey *ecdsa.PrivateKey
}

// New parses a hex private key (with or without 0x prefix) and returns a Signer.
func New(hexKey string) (*Signer, error) {
	hexKey = strings.TrimPrefix(hexKey, "0x")
	keyBytes, err := hex.DecodeString(hexKey)
	if err != nil {
		return nil, fmt.Errorf("signer: decode hex key: %w", err)
	}
	priv, err := crypto.ToECDSA(keyBytes)
	if err != nil {
		return nil, fmt.Errorf("signer: parse ecdsa key: %w", err)
	}
	return &Signer{privKey: priv}, nil
}

// Address returns the public Ethereum address derived from the signer's key.
func (s *Signer) Address() common.Address {
	return crypto.PubkeyToAddress(s.privKey.PublicKey)
}

// SignBatch signs txs in parallel (bounded by GOMAXPROCS workers) and returns
// signed RLP bytes in input order. Cancels early if ctx is done.
func (s *Signer) SignBatch(ctx context.Context, txs []*orchpb.TxIn) ([][]byte, error) {
	results := make([][]byte, len(txs))
	sem := make(chan struct{}, runtime.GOMAXPROCS(0))

	g, ctx := errgroup.WithContext(ctx)
	for i, tx := range txs {
		i, tx := i, tx
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case sem <- struct{}{}:
		}
		g.Go(func() error {
			defer func() { <-sem }()
			raw, err := signOne(tx, s.privKey)
			if err != nil {
				return fmt.Errorf("tx[%d]: %w", i, err)
			}
			results[i] = raw
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return results, nil
}

// signOne converts a protobuf TxIn to a signed EIP-1559 transaction and
// returns the canonical type-2 prefixed RLP bytes.
func signOne(tx *orchpb.TxIn, key *ecdsa.PrivateKey) ([]byte, error) {
	chainID := new(big.Int).SetUint64(tx.ChainId)

	var to *common.Address
	switch len(tx.To) {
	case 0:
		// contract creation
	case 20:
		a := common.BytesToAddress(tx.To)
		to = &a
	default:
		return nil, fmt.Errorf("to: expected 0 or 20 bytes, got %d", len(tx.To))
	}

	al := make(types.AccessList, 0, len(tx.AccessList))
	for _, e := range tx.AccessList {
		if len(e.Address) != 20 {
			return nil, fmt.Errorf("accessList.address: expected 20 bytes, got %d", len(e.Address))
		}
		keys := make([]common.Hash, 0, len(e.StorageKeys))
		for _, k := range e.StorageKeys {
			if len(k) != 32 {
				return nil, fmt.Errorf("accessList.storageKey: expected 32 bytes, got %d", len(k))
			}
			keys = append(keys, common.BytesToHash(k))
		}
		al = append(al, types.AccessTuple{
			Address:     common.BytesToAddress(e.Address),
			StorageKeys: keys,
		})
	}

	inner := &types.DynamicFeeTx{
		ChainID:    chainID,
		Nonce:      tx.Nonce,
		GasTipCap:  new(big.Int).SetBytes(tx.MaxPriorityFeePerGas),
		GasFeeCap:  new(big.Int).SetBytes(tx.MaxFeePerGas),
		Gas:        tx.Gas,
		To:         to,
		Value:      new(big.Int).SetBytes(tx.Value),
		Data:       tx.Data,
		AccessList: al,
	}
	unsigned := types.NewTx(inner)
	signer := types.LatestSignerForChainID(chainID)
	signed, err := types.SignTx(unsigned, signer, key)
	if err != nil {
		return nil, fmt.Errorf("sign: %w", err)
	}
	return signed.MarshalBinary()
}
