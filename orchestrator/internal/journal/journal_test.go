package journal

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/orchpb"
)

func makeRecord(i int) *orchpb.Record {
	return &orchpb.Record{
		SessionId: "test-session",
		BatchId:   uint64(i),
		TsIso:     "2026-01-01T00:00:00Z",
		ReplayCore: &orchpb.ReplayCore{
			Schema:           1,
			Verb:             "transfer",
			BlockNumber:      uint64(1000 + i),
			BlockTimestamp:   uint64(1700000000 + i),
			SaltCursorBefore: uint64(i * 10),
			SaltCursorAfter:  uint64(i*10 + 5),
			StartAddress:     []byte{byte(i), 0x00, 0xAB},
			EndAddress:       []byte{byte(i), 0xFF, 0xAB},
			Status:           "ok",
		},
		Observability: &orchpb.Observability{
			GasUsed:  uint64(21000 + i*100),
			TxCount:  uint32(i + 1),
			Epsilon:  0.001 * float64(i+1),
		},
	}
}

func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "journal.binlog")

	w, err := OpenWriter(path)
	if err != nil {
		t.Fatalf("OpenWriter: %v", err)
	}

	const n = 10
	var lastHash [32]byte
	for i := range n {
		rec := makeRecord(i)
		h, err := w.Append(rec)
		if err != nil {
			t.Fatalf("Append[%d]: %v", i, err)
		}
		lastHash = h
	}
	if w.CurrentChainHash() != lastHash {
		t.Fatal("CurrentChainHash() does not match last Append return value")
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	r, err := OpenReader(path)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	defer r.Close()

	count := 0
	for {
		rec, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next[%d]: %v", count, err)
		}
		if rec.BatchId != uint64(count) {
			t.Errorf("record[%d] BatchId = %d, want %d", count, rec.BatchId, count)
		}
		count++
	}
	if count != n {
		t.Fatalf("read %d records, want %d", count, n)
	}
	if r.prevHash != lastHash {
		t.Fatal("reader final hash does not match writer hash")
	}
}

func TestTamperDetected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tamper.binlog")

	w, err := OpenWriter(path)
	if err != nil {
		t.Fatalf("OpenWriter: %v", err)
	}
	for i := range 5 {
		if _, err := w.Append(makeRecord(i)); err != nil {
			t.Fatalf("Append[%d]: %v", i, err)
		}
	}
	w.Close()

	// Flip a byte inside the third record's payload.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Skip past the first two records to reach the third record's payload area.
	// Each frame: 4-byte length + N-byte payload. We just flip a byte in the
	// middle of the file where the payload bytes definitely live.
	mid := len(raw) / 2
	raw[mid] ^= 0xFF
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	r, err := OpenReader(path)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	defer r.Close()

	gotErr := false
	for {
		_, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if !errors.Is(err, ErrChainHashMismatch) {
				// Could also be an unmarshal error from the corrupted bytes — acceptable.
				t.Logf("non-chain-mismatch error (acceptable): %v", err)
			}
			gotErr = true
			break
		}
	}
	if !gotErr {
		t.Fatal("expected error reading tampered journal, got none")
	}
}

func TestEmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.binlog")

	// Create an empty file.
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()

	r, err := OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	_, err = r.Next()
	if !errors.Is(err, io.EOF) {
		t.Fatalf("empty file Next() = %v, want io.EOF", err)
	}

	rec, hash, err := Tail(path)
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if rec != nil {
		t.Fatalf("Tail on empty file returned non-nil record")
	}
	if hash != zeroHash {
		t.Fatalf("Tail on empty file returned non-zero hash")
	}
}

func TestPendingBatchRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pending-batch.bin")

	pb := &orchpb.PendingBatch{
		BatchId:   42,
		Verb:      "erc20-transfer",
		SessionId: "s1",
		Count:     100,
	}
	if err := WritePending(path, pb); err != nil {
		t.Fatalf("WritePending: %v", err)
	}

	got, err := ReadPending(path)
	if err != nil {
		t.Fatalf("ReadPending: %v", err)
	}
	if got.BatchId != pb.BatchId || got.Verb != pb.Verb || got.Count != pb.Count {
		t.Fatalf("round-trip mismatch: %+v", got)
	}

	if err := ClearPending(path); err != nil {
		t.Fatalf("ClearPending: %v", err)
	}
	_, err = ReadPending(path)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("after ClearPending, ReadPending = %v, want os.ErrNotExist", err)
	}
	// Second clear should not error.
	if err := ClearPending(path); err != nil {
		t.Fatalf("second ClearPending: %v", err)
	}
}

func TestVerifyAll(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "verify.binlog")

	w, err := OpenWriter(path)
	if err != nil {
		t.Fatal(err)
	}
	const n = 5
	for i := range n {
		if _, err := w.Append(makeRecord(i)); err != nil {
			t.Fatalf("Append[%d]: %v", i, err)
		}
	}
	writerHash := w.CurrentChainHash()
	w.Close()

	count, finalHash, err := VerifyAll(path)
	if err != nil {
		t.Fatalf("VerifyAll: %v", err)
	}
	if count != n {
		t.Fatalf("VerifyAll count = %d, want %d", count, n)
	}
	if finalHash != writerHash {
		t.Fatalf("VerifyAll finalHash mismatch")
	}
}
