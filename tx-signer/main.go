// Command tx-signer is a long-lived stdio worker that signs EIP-1559
// transactions on behalf of the Python orchestrator.
//
// Wire protocol: 4-byte big-endian length prefix + Protobuf SignRequest/SignResponse.
// Master private key is loaded once from SIGNER_PRIVATE_KEY at startup.
// Each batch fans out across goroutines (one per core); results stream back
// in input order so the orchestrator's nonce sequence is preserved.
package main

import (
	"bufio"
	"crypto/ecdsa"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"math/big"
	"os"
	"runtime"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"google.golang.org/protobuf/proto"

	pb "github.com/NethermindEth/eth-perf-research/tx-signer/pb"
)

var (
	privKey *ecdsa.PrivateKey
)

// signOne builds an EIP-1559 (type-2) tx from the protobuf message and signs
// it with the global private key. Returns the canonical RLP bytes (with the
// 0x02 type prefix) ready for testing_commitBlockV1.
func signOne(tx *pb.TxIn) ([]byte, error) {
	chainID := new(big.Int).SetUint64(tx.ChainId)
	maxPri := new(big.Int).SetBytes(tx.MaxPriorityFeePerGas)
	maxFee := new(big.Int).SetBytes(tx.MaxFeePerGas)
	value := new(big.Int).SetBytes(tx.Value)

	var to *common.Address
	if len(tx.To) == 20 {
		a := common.BytesToAddress(tx.To)
		to = &a
	} else if len(tx.To) != 0 {
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
				return nil, fmt.Errorf("accessList.storageKeys: expected 32 bytes, got %d", len(k))
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
		GasTipCap:  maxPri,
		GasFeeCap:  maxFee,
		Gas:        tx.Gas,
		To:         to,
		Value:      value,
		Data:       tx.Data,
		AccessList: al,
	}
	unsigned := types.NewTx(inner)
	signer := types.LatestSignerForChainID(chainID)
	signed, err := types.SignTx(unsigned, signer, privKey)
	if err != nil {
		return nil, err
	}
	return signed.MarshalBinary()
}

// signBatch fans out signOne across runtime.NumCPU() goroutines.
// Results preserve input order; per-tx errors collect into a slice but do
// not abort the batch (the orchestrator decides what to do with partial fails).
func signBatch(req *pb.SignRequest) *pb.SignResponse {
	results := make([][]byte, len(req.Txs))
	errs := make([]string, 0)
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, runtime.NumCPU())

	for i := range req.Txs {
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int) {
			defer wg.Done()
			defer func() { <-sem }()
			raw, err := signOne(req.Txs[idx])
			if err != nil {
				mu.Lock()
				errs = append(errs, fmt.Sprintf("tx[%d]: %v", idx, err))
				mu.Unlock()
				return
			}
			results[idx] = raw
		}(i)
	}
	wg.Wait()

	return &pb.SignResponse{Id: req.Id, Raw: results, Errors: errs}
}

func main() {
	privHex := os.Getenv("SIGNER_PRIVATE_KEY")
	if privHex == "" {
		fmt.Fprintln(os.Stderr, "tx-signer: SIGNER_PRIVATE_KEY env required")
		os.Exit(2)
	}
	if len(privHex) >= 2 && privHex[:2] == "0x" {
		privHex = privHex[2:]
	}
	keyBytes, err := hex.DecodeString(privHex)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tx-signer: SIGNER_PRIVATE_KEY decode: %v\n", err)
		os.Exit(2)
	}
	privKey, err = crypto.ToECDSA(keyBytes)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tx-signer: priv key parse: %v\n", err)
		os.Exit(2)
	}
	addr := crypto.PubkeyToAddress(privKey.PublicKey)
	fmt.Fprintf(os.Stderr, "tx-signer: ready, signer=%s, cores=%d\n", addr.Hex(), runtime.NumCPU())

	in := bufio.NewReaderSize(os.Stdin, 1<<20)
	out := bufio.NewWriterSize(os.Stdout, 1<<20)
	var lenBuf [4]byte

	for {
		if _, err := io.ReadFull(in, lenBuf[:]); err != nil {
			if err == io.EOF {
				return
			}
			fmt.Fprintf(os.Stderr, "tx-signer: read length: %v\n", err)
			os.Exit(1)
		}
		n := binary.BigEndian.Uint32(lenBuf[:])
		body := make([]byte, n)
		if _, err := io.ReadFull(in, body); err != nil {
			fmt.Fprintf(os.Stderr, "tx-signer: read body: %v\n", err)
			os.Exit(1)
		}

		var req pb.SignRequest
		if err := proto.Unmarshal(body, &req); err != nil {
			fmt.Fprintf(os.Stderr, "tx-signer: unmarshal: %v\n", err)
			os.Exit(1)
		}

		resp := signBatch(&req)
		outBody, err := proto.Marshal(resp)
		if err != nil {
			fmt.Fprintf(os.Stderr, "tx-signer: marshal response: %v\n", err)
			os.Exit(1)
		}
		binary.BigEndian.PutUint32(lenBuf[:], uint32(len(outBody)))
		if _, err := out.Write(lenBuf[:]); err != nil {
			os.Exit(1)
		}
		if _, err := out.Write(outBody); err != nil {
			os.Exit(1)
		}
		if err := out.Flush(); err != nil {
			os.Exit(1)
		}
	}
}
