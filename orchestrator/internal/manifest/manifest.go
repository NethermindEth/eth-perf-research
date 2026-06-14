// Package manifest reads and writes the run-manifest.json run descriptor.
package manifest

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/atomicio"
)

const SchemaVersion = 2

type Manifest struct {
	SchemaVersion           int           `json:"schema_version"`
	SessionID               string        `json:"session_id"`
	StartedAtISO            string        `json:"started_at_iso"`
	FinishedAtISO           string        `json:"finished_at_iso,omitempty"`
	GenesisSHA256           string        `json:"genesis_sha256"`
	PluginGitSHA            string        `json:"plugin_git_sha,omitempty"`
	NethermindCommitSHA     string        `json:"nethermind_commit_sha,omitempty"`
	DotnetRuntimeMajor      string        `json:"dotnet_runtime_major,omitempty"`
	ChainIdentityHash       string        `json:"chain_identity_hash"`
	TargetHistory           []TargetEntry `json:"target_history"`
	FinalStateRoot          string        `json:"final_state_root,omitempty"`
	JournalSHA256           string        `json:"journal_sha256,omitempty"`
	LastChainHashCheckpoint string        `json:"last_chain_hash_checkpoint,omitempty"`
	BatchCount              int           `json:"batch_count"`
	Terminated              string        `json:"terminated,omitempty"`
}

type TargetEntry struct {
	AppliedAtBatch int                `json:"applied_at_batch"`
	AppliedAtISO   string             `json:"applied_at_iso"`
	TargetSHA256   string             `json:"target_sha256"`
	Shares         map[string]float64 `json:"shares"`
	TotalBytes     int64              `json:"total_bytes"`
}

func Load(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("manifest: read %s: %w", path, err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("manifest: parse %s: %w", path, err)
	}
	return &m, nil
}

func (m *Manifest) Save(path string) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("manifest: marshal: %w", err)
	}
	if err := atomicio.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("manifest: %w", err)
	}
	return nil
}
