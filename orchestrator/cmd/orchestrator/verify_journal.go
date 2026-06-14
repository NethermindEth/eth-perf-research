package main

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/journal"
)

func newVerifyJournalCmd() *cobra.Command {
	var path string
	cmd := &cobra.Command{
		Use:   "verify-journal",
		Short: "Re-derive the BLAKE3 chain hash over a journal binlog and print the tail summary",
		RunE: func(_ *cobra.Command, _ []string) error {
			if path == "" {
				return errors.New("--path required")
			}
			count, hash, err := journal.VerifyAll(path)
			if err != nil {
				return err
			}
			fmt.Printf("verified %d records; final chain hash: %x\n", count, hash)
			return nil
		},
	}
	cmd.Flags().StringVar(&path, "path", "", "Path to journal.binlog")
	return cmd
}
