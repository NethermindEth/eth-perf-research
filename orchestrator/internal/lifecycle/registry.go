package lifecycle

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ethereum/go-ethereum/common"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/atomicio"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/verbs"
)

// contractsRegistryFilename is the state-dir file recording the addresses of
// the Spamoor scenario contracts the bootstrap phase deployed.
const contractsRegistryFilename = "contracts.json"

// registryFile is the on-disk shape of the contract registry: a contract-name
// → 0x-address map.
type registryFile struct {
	Contracts map[string]string `json:"contracts"`
}

// contractsRegistryPath returns the registry file path inside stateDir.
func contractsRegistryPath(stateDir string) string {
	return filepath.Join(stateDir, contractsRegistryFilename)
}

// loadContractRegistry reads the registry file. It returns (nil, nil) when the
// file does not exist — that signals a fresh bootstrap is required.
func loadContractRegistry(stateDir string) (*verbs.ContractRegistry, error) {
	path := contractsRegistryPath(stateDir)
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("lifecycle: read contract registry: %w", err)
	}
	var rf registryFile
	if err := json.Unmarshal(raw, &rf); err != nil {
		return nil, fmt.Errorf("lifecycle: decode contract registry %q: %w", path, err)
	}
	reg := verbs.NewContractRegistry()
	for name, addrHex := range rf.Contracts {
		if !common.IsHexAddress(addrHex) {
			return nil, fmt.Errorf("lifecycle: contract registry: %q is not a valid address for %q", addrHex, name)
		}
		reg.Set(verbs.ContractName(name), common.HexToAddress(addrHex))
	}
	return reg, nil
}

// saveContractRegistry writes the registry to the state dir and fsyncs it so a
// crash immediately after bootstrap does not lose the deployed addresses. The
// write is atomic: a temp file is renamed over the target.
func saveContractRegistry(stateDir string, reg *verbs.ContractRegistry) error {
	rf := registryFile{Contracts: map[string]string{}}
	for _, name := range reg.Names() {
		addr, _ := reg.Get(name)
		rf.Contracts[string(name)] = addr.Hex()
	}
	raw, err := json.MarshalIndent(rf, "", "  ")
	if err != nil {
		return fmt.Errorf("lifecycle: encode contract registry: %w", err)
	}
	if err := atomicio.WriteFile(contractsRegistryPath(stateDir), raw, 0o644); err != nil {
		return fmt.Errorf("lifecycle: %w", err)
	}
	return nil
}

// registrySummary returns a stable, human-readable contract→address listing
// for log lines.
func registrySummary(reg *verbs.ContractRegistry) string {
	names := reg.Names()
	parts := make([]string, 0, len(names))
	for _, n := range names {
		addr, _ := reg.Get(n)
		parts = append(parts, fmt.Sprintf("%s=%s", n, addr.Hex()))
	}
	sort.Strings(parts)
	var out strings.Builder
	for i, p := range parts {
		if i > 0 {
			out.WriteString(" ")
		}
		out.WriteString(p)
	}
	return out.String()
}
