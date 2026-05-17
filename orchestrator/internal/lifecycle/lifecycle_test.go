package lifecycle

import (
	"bytes"
	"math"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/config"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/controller"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/orchpb"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/referencef"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/rpc"
)

func TestReachedTargetAllAxesMet(t *testing.T) {
	tgt := &controller.Target{
		TotalBytes: 1000,
		Shares: map[controller.Axis]float64{
			controller.AxisAccounts: 0.5,
			controller.AxisStorage:  0.3,
			controller.AxisCode:     0.2,
		},
	}
	obs := &controller.Observation{
		AccountTrieBytes: 500,
		StorageTrieBytes: 300,
		CodeBytesTotal:   200,
	}
	if !reachedTarget(obs, tgt) {
		t.Fatalf("expected target reached when every axis at its share, got false")
	}
}

func TestReachedTargetOneAxisShort(t *testing.T) {
	tgt := &controller.Target{
		TotalBytes: 1000,
		Shares: map[controller.Axis]float64{
			controller.AxisAccounts: 0.5,
			controller.AxisStorage:  0.3,
			controller.AxisCode:     0.2,
		},
	}
	obs := &controller.Observation{
		AccountTrieBytes: 500,
		StorageTrieBytes: 299, // one byte short
		CodeBytesTotal:   200,
	}
	if reachedTarget(obs, tgt) {
		t.Fatalf("expected NOT reached when storage axis short, got true")
	}
}

func TestReachedTargetOvershootStillReached(t *testing.T) {
	tgt := &controller.Target{
		TotalBytes: 1000,
		Shares: map[controller.Axis]float64{
			controller.AxisAccounts: 0.5,
			controller.AxisStorage:  0.3,
			controller.AxisCode:     0.2,
		},
	}
	obs := &controller.Observation{
		AccountTrieBytes: 2000,
		StorageTrieBytes: 500,
		CodeBytesTotal:   300,
	}
	if !reachedTarget(obs, tgt) {
		t.Fatalf("expected reached when every axis exceeds its share")
	}
}

func TestReachedTargetNilInputs(t *testing.T) {
	if reachedTarget(nil, nil) {
		t.Fatalf("nil inputs should not be reached")
	}
}

func TestBuildExecutionPayloadV3Fields(t *testing.T) {
	parent := common.HexToHash("0x0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20")
	hash := common.HexToHash("0xabcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789")
	stateRoot := common.HexToHash("0xfeedfacedeadbeefcafebabe00000000000000000000000000000000aaaaaaaa")
	header := &rpc.BlockHeader{
		Number:     42,
		Hash:       hash,
		ParentHash: parent,
		StateRoot:  stateRoot,
		GasLimit:   30_000_000,
		GasUsed:    21_000,
		Timestamp:  1_700_000_000,
		BaseFee:    big.NewInt(7),
	}
	txs := [][]byte{
		{0x02, 0xaa, 0xbb},
		{0x02, 0xcc, 0xdd},
	}

	p := buildExecutionPayloadV3(header, txs)
	if p == nil {
		t.Fatalf("nil payload")
	}
	if p.BlockNumber != 42 {
		t.Errorf("BlockNumber=%d want 42", p.BlockNumber)
	}
	if p.GasLimit != 30_000_000 {
		t.Errorf("GasLimit=%d", p.GasLimit)
	}
	if p.GasUsed != 21_000 {
		t.Errorf("GasUsed=%d", p.GasUsed)
	}
	if p.Timestamp != 1_700_000_000 {
		t.Errorf("Timestamp=%d", p.Timestamp)
	}
	if p.BaseFeePerGas == nil || p.BaseFeePerGas.Int64() != 7 {
		t.Errorf("BaseFeePerGas=%v want 7", p.BaseFeePerGas)
	}
	if p.ParentHash != parent {
		t.Errorf("ParentHash mismatch")
	}
	if p.BlockHash != hash {
		t.Errorf("BlockHash mismatch")
	}
	if p.StateRoot != stateRoot {
		t.Errorf("StateRoot mismatch")
	}
	if len(p.Transactions) != 2 || !bytes.Equal(p.Transactions[0], txs[0]) {
		t.Errorf("Transactions not threaded through")
	}
	// LogsBloom must be 256 zero bytes (the zero-value array).
	for i, b := range p.LogsBloom {
		if b != 0 {
			t.Fatalf("LogsBloom[%d]=%x want 0", i, b)
		}
	}
	// Withdrawals + extraData are nil/empty by default.
	if p.Withdrawals != nil {
		t.Errorf("Withdrawals must be nil for synthetic commits, got %v", p.Withdrawals)
	}
	if p.ExtraData != nil {
		t.Errorf("ExtraData must be nil, got %v", p.ExtraData)
	}
}

func TestBuildExecutionPayloadV3NilBaseFee(t *testing.T) {
	header := &rpc.BlockHeader{Number: 1}
	p := buildExecutionPayloadV3(header, nil)
	if p.BaseFeePerGas == nil || p.BaseFeePerGas.Sign() != 0 {
		t.Fatalf("BaseFeePerGas must default to zero, got %v", p.BaseFeePerGas)
	}
}

func TestSplitFlatKey(t *testing.T) {
	cases := []struct {
		k      string
		verb   string
		ax     controller.Axis
		ok     bool
	}{
		{"eoatx.accounts", "eoatx", controller.AxisAccounts, true},
		{"erc20_bloater.storage", "erc20_bloater", controller.AxisStorage, true},
		{"calltx.code", "calltx", controller.AxisCode, true},
		{"calltx.unknown", "", "", false},
		{"no_separator", "", "", false},
	}
	for _, tc := range cases {
		v, a, ok := splitFlatKey(tc.k)
		if v != tc.verb || a != tc.ax || ok != tc.ok {
			t.Errorf("splitFlatKey(%q)=(%q,%q,%v), want (%q,%q,%v)", tc.k, v, a, ok, tc.verb, tc.ax, tc.ok)
		}
	}
}

// newColdState builds a cold-started controller State seeded from reference-F
// for the hydrate tests.
func newColdState(verbs []string) *controller.State {
	ref := &referencef.ReferenceF{
		Verbs:    map[string]map[string]float64{},
		AvgTxRLP: map[string]float64{},
	}
	var identity [32]byte
	return controller.NewState(config.Defaults(), verbs, ref, identity)
}

// TestHydrateStateFromTailReconstructsCoefficients verifies the resume path
// seeds the controller's F / σ / α from the journal tail's Observability block.
func TestHydrateStateFromTailReconstructsCoefficients(t *testing.T) {
	state := newColdState([]string{"eoatx", "storagespam"})

	tail := &orchpb.Record{
		BatchId: 41,
		Observability: &orchpb.Observability{
			CoeffsAfter: map[string]float64{
				"eoatx.accounts":       123.0,
				"storagespam.storage":  456.0,
			},
			AlphaCurrent: map[string]float64{"eoatx.accounts": 0.07},
			SigmaInnov:   map[string]float64{"storagespam.storage": 2.5},
		},
	}

	res := hydrateStateFromTail(state, tail)
	if !res.Reconstructed {
		t.Fatalf("Reconstructed=false, want true")
	}
	if res.CoeffCells != 2 || res.AlphaCells != 1 || res.SigmaCells != 1 {
		t.Errorf("counts = coeff:%d alpha:%d sigma:%d, want 2/1/1",
			res.CoeffCells, res.AlphaCells, res.SigmaCells)
	}
	if got := state.F["eoatx"][controller.AxisAccounts]; got != 123.0 {
		t.Errorf("F[eoatx][accounts]=%v, want 123", got)
	}
	if got := state.F["storagespam"][controller.AxisStorage]; got != 456.0 {
		t.Errorf("F[storagespam][storage]=%v, want 456", got)
	}
	if got := state.Alpha["eoatx"][controller.AxisAccounts]; got != 0.07 {
		t.Errorf("Alpha[eoatx][accounts]=%v, want 0.07", got)
	}
	if got := state.Sigma["storagespam"][controller.AxisStorage]; got != 2.5 {
		t.Errorf("Sigma[storagespam][storage]=%v, want 2.5", got)
	}
	if state.BatchID != 42 {
		t.Errorf("BatchID=%d, want 42 (tail.BatchId+1)", state.BatchID)
	}
}

// TestHydrateStateFromTailColdStartFallback verifies that a tail with no
// Observability block leaves the State at its cold-start seed and reports
// Reconstructed=false — the safe fall-through the resume path relies on.
func TestHydrateStateFromTailColdStartFallback(t *testing.T) {
	state := newColdState([]string{"eoatx"})
	seedF := state.F["eoatx"][controller.AxisAccounts]

	cases := []struct {
		name string
		tail *orchpb.Record
	}{
		{"nil tail", nil},
		{"nil observability", &orchpb.Record{BatchId: 7}},
		{"empty coefficient maps", &orchpb.Record{
			BatchId:       7,
			Observability: &orchpb.Observability{},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newColdState([]string{"eoatx"})
			res := hydrateStateFromTail(st, tc.tail)
			if res.Reconstructed {
				t.Errorf("Reconstructed=true, want false for %s", tc.name)
			}
			if got := st.F["eoatx"][controller.AxisAccounts]; got != seedF {
				t.Errorf("F mutated on cold-start fallback: got %v, want seed %v", got, seedF)
			}
		})
	}
}

// TestHydrateStateFromTailRejectsCorruptCoefficients guards FIX 2: a journal
// tail carrying a divergent F coefficient (the production bug persisted
// F[eoatx][storage] ≈ 6.48e13) must be rejected wholesale. The reconstruction
// is discarded and the State keeps its sane cold-start reference-F seed —
// a corrupt journal must never override it.
func TestHydrateStateFromTailRejectsCorruptCoefficients(t *testing.T) {
	verbs := []string{"eoatx", "storagespam"}

	cases := []struct {
		name string
		obs  *orchpb.Observability
	}{
		{
			name: "divergent F coefficient",
			obs: &orchpb.Observability{
				CoeffsAfter: map[string]float64{
					"eoatx.accounts": 160.0,
					"eoatx.storage":  6.48e13, // garbage from the divergence bug
				},
			},
		},
		{
			name: "non-finite F coefficient",
			obs: &orchpb.Observability{
				CoeffsAfter: map[string]float64{"eoatx.accounts": math.Inf(1)},
			},
		},
		{
			name: "divergent sigma",
			obs: &orchpb.Observability{
				CoeffsAfter: map[string]float64{"eoatx.accounts": 160.0},
				SigmaInnov:  map[string]float64{"eoatx.storage": 4.15e14},
			},
		},
		{
			name: "non-finite alpha",
			obs: &orchpb.Observability{
				CoeffsAfter:  map[string]float64{"eoatx.accounts": 160.0},
				AlphaCurrent: map[string]float64{"eoatx.accounts": math.NaN()},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state := newColdState(verbs)
			// Capture the cold-start seed for every cell so we can assert the
			// reconstruction left the State entirely untouched.
			seedF := make(map[string]map[controller.Axis]float64)
			for _, v := range verbs {
				seedF[v] = map[controller.Axis]float64{}
				for _, ax := range controller.Axes {
					seedF[v][ax] = state.F[v][ax]
				}
			}

			tail := &orchpb.Record{BatchId: 99, Observability: tc.obs}
			res := hydrateStateFromTail(state, tail)

			if res.Reconstructed {
				t.Fatalf("Reconstructed=true, want false (corrupt tail must be rejected)")
			}
			if res.CoeffCells != 0 || res.SigmaCells != 0 || res.AlphaCells != 0 {
				t.Errorf("cells committed on rejection: coeff:%d sigma:%d alpha:%d, want 0/0/0",
					res.CoeffCells, res.SigmaCells, res.AlphaCells)
			}
			for _, v := range verbs {
				for _, ax := range controller.Axes {
					if got := state.F[v][ax]; got != seedF[v][ax] {
						t.Errorf("F[%s][%s] mutated by rejected reconstruction: got %v, want seed %v",
							v, ax, got, seedF[v][ax])
					}
				}
			}
			if state.BatchID != 0 {
				t.Errorf("BatchID advanced on rejection: got %d, want 0 (cold start)", state.BatchID)
			}
		})
	}
}

func TestEncodeAddr(t *testing.T) {
	got := encodeAddr(0x0102030405060708)
	want := []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}
	if !bytes.Equal(got, want) {
		t.Fatalf("encodeAddr=%x want %x", got, want)
	}
}
