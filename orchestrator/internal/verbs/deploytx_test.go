package verbs

import (
	"bytes"
	"encoding/hex"
	"testing"
)

// expectedDeployInitLen is the size of the full init code produced by
// buildUniqueStorageBurnerInit: 14 init-prefix + 34 unique-tag + 104 runtime.
const expectedDeployInitLen = 14 + 34 + 104

// TestBuildUniqueStorageBurnerInit checks the init code is well-formed: it
// has the expected length, its first 14 bytes (the CODECOPY+RETURN prefix)
// are identical across idx, the next 34 bytes (the unique tag) differ across
// idx, and the trailing 104 bytes (the storage-burner runtime) are identical
// across idx. This is what makes every deploytx emit a distinct codehash.
func TestBuildUniqueStorageBurnerInit(t *testing.T) {
	a := buildUniqueStorageBurnerInit(0)
	b := buildUniqueStorageBurnerInit(1)

	if len(a) != expectedDeployInitLen {
		t.Fatalf("init len(idx=0) = %d, want %d", len(a), expectedDeployInitLen)
	}
	if len(b) != expectedDeployInitLen {
		t.Fatalf("init len(idx=1) = %d, want %d", len(b), expectedDeployInitLen)
	}
	if bytes.Equal(a, b) {
		t.Fatal("init(idx=0) == init(idx=1) — unique tag not differentiating")
	}
	if !bytes.Equal(a[:14], b[:14]) {
		t.Errorf("init prefix (bytes 0..14) differs across idx — want identical")
	}
	if bytes.Equal(a[14:48], b[14:48]) {
		t.Errorf("unique tag (bytes 14..48) identical across idx — want different")
	}
	if !bytes.Equal(a[48:], b[48:]) {
		t.Errorf("runtime (bytes 48..end) differs across idx — want identical")
	}

	// PUSH2 0x008A = 138 (= 34-byte tag + 104-byte runtime). First 3 bytes of
	// the init prefix are PUSH2 0x008A.
	if a[0] != 0x61 || a[1] != 0x00 || a[2] != 0x8A {
		t.Errorf("init prefix opcode 0..3 = % x, want 61 00 8A (PUSH2 138)", a[:3])
	}
}

// TestDeploytxBuildTx checks the deploytx verb emits a CREATE tx carrying
// unique storage-burner init code.
func TestDeploytxBuildTx(t *testing.T) {
	tx, err := verbDeploytx{}.BuildTx(7, BuildCtx{})
	if err != nil {
		t.Fatalf("BuildTx: %v", err)
	}
	if tx.To != nil {
		t.Errorf("To = %v, want nil (CREATE tx)", tx.To)
	}
	if tx.Gas != deploytxGas {
		t.Errorf("Gas = %d, want %d", tx.Gas, deploytxGas)
	}
	if len(tx.Data) != expectedDeployInitLen {
		t.Errorf("data len = %d, want %d", len(tx.Data), expectedDeployInitLen)
	}
	oldDefault, _ := hex.DecodeString("6001600055")
	if bytes.Equal(tx.Data, oldDefault) {
		t.Fatal("BuildTx still returning the old empty-runtime default 0x6001600055")
	}
}
