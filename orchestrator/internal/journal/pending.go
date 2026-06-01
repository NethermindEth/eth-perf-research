package journal

import (
	"fmt"
	"os"

	"google.golang.org/protobuf/proto"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/atomicio"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/orchpb"
)

// WritePending atomically writes a PendingBatch sidecar to path using
// tmp-file + fsync + rename, then fsyncs the containing directory.
func WritePending(path string, pb *orchpb.PendingBatch) error {
	data, err := proto.Marshal(pb)
	if err != nil {
		return fmt.Errorf("journal: marshal pending batch: %w", err)
	}
	if err := atomicio.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("journal: %w", err)
	}
	return nil
}

// ReadPending returns os.ErrNotExist when the sidecar is absent.
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
