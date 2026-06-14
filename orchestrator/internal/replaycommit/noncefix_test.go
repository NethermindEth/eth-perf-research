package replaycommit

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

const testKeyHex = "bcdf20249abf0ed6d944c0288fad489e33f66b3960d9e6229c1cd214ed3bbe31"

func signedDynTx(t *testing.T, keyHex string, nonce uint64, data []byte) []byte {
	t.Helper()
	key, err := crypto.HexToECDSA(keyHex)
	if err != nil {
		t.Fatal(err)
	}
	to := common.HexToAddress("0x2203e24e6173144A55Bd9826130be878be1C148f")
	chainID := big.NewInt(1)
	tx, err := types.SignNewTx(key, types.LatestSignerForChainID(chainID), &types.DynamicFeeTx{
		ChainID: chainID, Nonce: nonce, GasTipCap: big.NewInt(1e9), GasFeeCap: big.NewInt(1e9 + 8),
		Gas: 16700000, To: &to, Value: big.NewInt(0), Data: data,
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := tx.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestNonceFixerRewritesOnlyMismatches(t *testing.T) {
	f, err := NewNonceFixer(testKeyHex)
	if err != nil {
		t.Fatal(err)
	}
	f.Seed(100)

	otherKey := "59c6995e998f97a5a0044966f0945389dc9e86dae88c7a8412f4603b6b78690d"
	in := [][]byte{
		signedDynTx(t, testKeyHex, 100, []byte{1}),  // matches -> verbatim
		signedDynTx(t, testKeyHex, 50, []byte{2}),   // too low -> rewrite to 101
		signedDynTx(t, otherKey, 7, []byte{3}),      // other sender -> untouched, no counter advance
		signedDynTx(t, testKeyHex, 9999, []byte{4}), // too high -> rewrite to 102
	}
	out, err := f.Fix(in)
	if err != nil {
		t.Fatal(err)
	}

	if string(out[0]) != string(in[0]) {
		t.Error("matching tx should pass through byte-identical")
	}
	if string(out[2]) != string(in[2]) {
		t.Error("other-sender tx should pass through byte-identical")
	}

	wantNonces := []uint64{100, 101, 7, 102}
	for i, raw := range out {
		var tx types.Transaction
		if err := tx.UnmarshalBinary(raw); err != nil {
			t.Fatalf("tx %d: %v", i, err)
		}
		if tx.Nonce() != wantNonces[i] {
			t.Errorf("tx %d: nonce=%d want %d", i, tx.Nonce(), wantNonces[i])
		}
		// Re-signed txs must recover to the right sender and keep their payload.
		from, err := types.Sender(types.LatestSignerForChainID(tx.ChainId()), &tx)
		if err != nil {
			t.Fatalf("tx %d: sender: %v", i, err)
		}
		if i != 2 && from != f.Address() {
			t.Errorf("tx %d: sender=%s want %s", i, from.Hex(), f.Address().Hex())
		}
		wantData := []byte{byte(i + 1)}
		if string(tx.Data()) != string(wantData) {
			t.Errorf("tx %d: data changed", i)
		}
	}

	rw, kept := f.Stats()
	if rw != 2 || kept != 1 {
		t.Errorf("stats rewritten=%d kept=%d, want 2/1", rw, kept)
	}
}

func TestNonceFixerUnseededFails(t *testing.T) {
	f, err := NewNonceFixer(testKeyHex)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Fix([][]byte{signedDynTx(t, testKeyHex, 0, nil)}); err == nil {
		t.Fatal("expected unseeded error")
	}
}
