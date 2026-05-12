package journal

import (
	"fmt"
	"os"
	"path/filepath"

	"google.golang.org/protobuf/proto"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/orchpb"
)

// WritePending atomically writes a PendingBatch sidecar to path using
// tmp-file + fsync + rename, then fsyncs the containing directory.
func WritePending(path string, pb *orchpb.PendingBatch) error {
	data, err := proto.Marshal(pb)
	if err != nil {
		return fmt.Errorf("journal: marshal pending batch: %w", err)
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".pending-batch-*.tmp")
	if err != nil {
		return fmt.Errorf("journal: create tmp for pending batch: %w", err)
	}
	tmpName := tmp.Name()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("journal: write pending batch tmp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("journal: fsync pending batch tmp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("journal: close pending batch tmp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("journal: rename pending batch: %w", err)
	}
	// fsync the directory so the rename is durable.
	df, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("journal: open dir for fsync: %w", err)
	}
	defer df.Close()
	if err := df.Sync(); err != nil {
		return fmt.Errorf("journal: fsync dir: %w", err)
	}
	return nil
}

// ReadPending reads the PendingBatch sidecar at path.
// Returns os.ErrNotExist when the sidecar is absent.
func ReadPending(path string) (*orchpb.PendingBatch, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err // preserves os.ErrNotExist via errors.Is
	}
	var pb orchpb.PendingBatch
	if err := proto.Unmarshal(data, &pb); err != nil {
		return nil, fmt.Errorf("journal: unmarshal pending batch: %w", err)
	}
	return &pb, nil
}

// ClearPending removes the sidecar at path. No error if already absent.
func ClearPending(path string) error {
	err := os.Remove(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("journal: clear pending: %w", err)
	}
	return nil
}
