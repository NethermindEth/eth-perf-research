package lifecycle

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/journal"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/orchpb"
)

func TestPerBatchPendingPath(t *testing.T) {
	if got, want := perBatchPendingPath("/state/pending-batch", 42), "/state/pending-batch.42"; got != want {
		t.Errorf("perBatchPendingPath = %q, want %q", got, want)
	}
}

// TestClearAllPending verifies the shutdown drain removes every per-batch
// sidecar ("<base>.<id>") and nothing else.
func TestClearAllPending(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "pending-batch")

	if err := clearAllPending(base); err != nil {
		t.Fatalf("clearAllPending on empty dir: %v", err)
	}

	for _, id := range []uint64{1, 2, 3} {
		if err := os.WriteFile(perBatchPendingPath(base, id), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	unrelated := filepath.Join(dir, "journal.bin")
	if err := os.WriteFile(unrelated, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := clearAllPending(base); err != nil {
		t.Fatalf("clearAllPending: %v", err)
	}
	for _, id := range []uint64{1, 2, 3} {
		if _, err := os.Stat(perBatchPendingPath(base, id)); !os.IsNotExist(err) {
			t.Errorf("sidecar %d not removed", id)
		}
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Errorf("clearAllPending removed an unrelated file: %v", err)
	}
}

// TestReconcilePendingNoFiles: a clean shutdown left no sidecars, so a resume
// reconciles to nil.
func TestReconcilePendingNoFiles(t *testing.T) {
	d := &startupDecision{PendingPath: filepath.Join(t.TempDir(), "pending-batch")}
	if err := reconcilePending(context.Background(), nil, d); err != nil {
		t.Errorf("reconcilePending with no sidecars = %v, want nil", err)
	}
}

// TestReconcilePendingStillActive: with no journal tail the chain head is
// unknowable, so a leftover sidecar cannot be proven stale — reconcile must
// refuse to start and name the offending batch rather than silently drop it.
func TestReconcilePendingStillActive(t *testing.T) {
	base := filepath.Join(t.TempDir(), "pending-batch")
	if err := journal.WritePending(perBatchPendingPath(base, 7),
		&orchpb.PendingBatch{BatchId: 7, Verb: "eoatx"}); err != nil {
		t.Fatal(err)
	}
	err := reconcilePending(context.Background(), nil, &startupDecision{PendingPath: base})
	if err == nil {
		t.Fatal("reconcilePending = nil, want still-active error")
	}
	if !strings.Contains(err.Error(), "batch=7") || !strings.Contains(err.Error(), "still active") {
		t.Errorf("error = %q, want it to name batch=7 and 'still active'", err)
	}
}
