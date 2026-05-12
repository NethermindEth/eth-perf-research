package builderpool

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/orchpb"
)

// echoWorkerBin builds the echo_worker binary once per test run and returns
// its path.
func echoWorkerBin(t *testing.T) string {
	t.Helper()
	// Locate the module root relative to this file.
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// thisFile = .../orchestrator/internal/builderpool/pool_test.go
	moduleRoot := filepath.Join(filepath.Dir(thisFile), "..", "..")
	srcPkg := filepath.Join(moduleRoot, "testdata", "echo_worker")

	bin := filepath.Join(t.TempDir(), "echo_worker")
	cmd := exec.Command("go", "build", "-o", bin, srcPkg)
	cmd.Dir = moduleRoot
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("build echo_worker: %v", err)
	}
	return bin
}

func TestPool_ConcurrentBuilds(t *testing.T) {
	bin := echoWorkerBin(t)
	cfg := Config{
		Cmd:     []string{bin},
		Workers: 4,
	}
	ctx := context.Background()
	p, err := Open(ctx, cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()

	const total = 100
	var wg sync.WaitGroup
	errs := make([]error, total)
	for i := range total {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := &orchpb.BuildBatchRequest{
				Id:       uint64(i),
				StartIdx: uint64(i * 10),
				Verb:     "transfer",
			}
			resp, err := p.Build(ctx, req)
			if err != nil {
				errs[i] = fmt.Errorf("Build[%d]: %w", i, err)
				return
			}
			if resp.Id != req.Id {
				errs[i] = fmt.Errorf("Build[%d]: response id %d, want %d", i, resp.Id, req.Id)
			}
		}(i)
	}
	wg.Wait()
	for _, e := range errs {
		if e != nil {
			t.Error(e)
		}
	}
}

func TestPool_WorkerCrashRespawn(t *testing.T) {
	bin := echoWorkerBin(t)
	// Worker dies after 1 request; pool must respawn and retry.
	cfg := Config{
		Cmd:     []string{bin, "--die-after-n=1"},
		Workers: 1,
	}
	ctx := context.Background()
	p, err := Open(ctx, cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()

	// First call — worker echoes and then dies.
	req1 := &orchpb.BuildBatchRequest{Id: 1, StartIdx: 10}
	if _, err := p.Build(ctx, req1); err != nil {
		t.Logf("first call returned error (may be ok after respawn): %v", err)
	}

	// Second call — worker should have been respawned; must succeed.
	req2 := &orchpb.BuildBatchRequest{Id: 2, StartIdx: 20}
	resp, err := p.Build(ctx, req2)
	if err != nil {
		t.Fatalf("second Build after crash: %v", err)
	}
	if resp.Id != req2.Id {
		t.Fatalf("response id %d, want %d", resp.Id, req2.Id)
	}
}

func TestPool_CloseGrace(t *testing.T) {
	bin := echoWorkerBin(t)
	cfg := Config{
		Cmd:     []string{bin},
		Workers: 2,
	}
	ctx := context.Background()
	p, err := Open(ctx, cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	start := time.Now()
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	elapsed := time.Since(start)
	// Should close well within the 6s requirement (workers get SIGTERM immediately).
	if elapsed > 6*time.Second {
		t.Fatalf("Close took %v, want <6s", elapsed)
	}
}
