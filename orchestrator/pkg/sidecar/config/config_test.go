package config

import (
	"testing"
)

func TestInferBlockDiffsDBPath(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "nm canonical layout",
			in:   "/data/nethermind/state/FlatDb",
			want: "/data/nethermind/blockDiffs",
		},
		{
			name: "trailing slash tolerated",
			in:   "/data/nethermind/state/FlatDb/",
			want: "/data/nethermind/blockDiffs",
		},
		{
			name: "nested under state",
			in:   "/var/lib/nm/state/inner/FlatDb",
			want: "/var/lib/nm/blockDiffs",
		},
		{
			name: "no state segment falls back to parent",
			in:   "/srv/customdir/FlatDb",
			want: "/srv/customdir/blockDiffs",
		},
		{
			name: "empty input",
			in:   "",
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := InferBlockDiffsDBPath(tc.in)
			if got != tc.want {
				t.Errorf("InferBlockDiffsDBPath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestFromFlagsInfersBlockDiffsDB(t *testing.T) {
	c, err := FromFlags([]string{"--db=/data/nm/state/FlatDb", "--mode=tail"})
	if err != nil {
		t.Fatalf("FromFlags: %v", err)
	}
	want := "/data/nm/blockDiffs"
	if c.BlockDiffsDB != want {
		t.Errorf("BlockDiffsDB = %q, want %q", c.BlockDiffsDB, want)
	}
}

func TestFromFlagsExplicitOverridesInference(t *testing.T) {
	c, err := FromFlags([]string{
		"--db=/data/nm/state/FlatDb",
		"--blockdiffs-db=/explicit/path",
		"--mode=tail",
	})
	if err != nil {
		t.Fatalf("FromFlags: %v", err)
	}
	if c.BlockDiffsDB != "/explicit/path" {
		t.Errorf("explicit --blockdiffs-db got overwritten by inference: %q", c.BlockDiffsDB)
	}
}

func TestDefaultsSnapshotIntervalIs30s(t *testing.T) {
	d := Defaults()
	if d.SnapshotInterval != 30 {
		t.Errorf("default SnapshotInterval = %d, want 30", d.SnapshotInterval)
	}
}
