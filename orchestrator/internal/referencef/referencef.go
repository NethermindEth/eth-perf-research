package referencef

import (
	"fmt"
	"os"

	"sigs.k8s.io/yaml"
)

type ReferenceF struct {
	Verbs    map[string]map[string]float64 `json:"verbs"`
	AvgTxRLP map[string]float64            `json:"avg_tx_rlp"`
}

func Load(path string) (*ReferenceF, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("referencef: read %s: %w", path, err)
	}
	var rf ReferenceF
	if err := yaml.Unmarshal(data, &rf); err != nil {
		return nil, fmt.Errorf("referencef: parse %s: %w", path, err)
	}
	return &rf, nil
}
