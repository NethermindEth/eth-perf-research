package builderpool

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"

	"google.golang.org/protobuf/proto"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/orchpb"
)

// workerState tracks whether a worker is alive.
type workerState int32

const (
	stateRunning workerState = iota
	stateDead
)

type pendingCall struct {
	resp chan *orchpb.BuildBatchResponse
	err  chan error
}

// worker wraps a single subprocess with framed-proto I/O.
type worker struct {
	idx    int
	cmd    []string
	env    []string
	mu     sync.Mutex // serialises framed I/O (one in-flight per worker)
	proc   *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	state  atomic.Int32 // workerState
}

func newWorker(idx int, cmd []string, env []string) *worker {
	return &worker{idx: idx, cmd: cmd, env: env}
}

func (w *worker) start() error {
	c := exec.Command(w.cmd[0], w.cmd[1:]...) //nolint:gosec
	c.Env = append(os.Environ(), w.env...)

	stdin, err := c.StdinPipe()
	if err != nil {
		return fmt.Errorf("worker[%d]: stdin pipe: %w", w.idx, err)
	}
	stdout, err := c.StdoutPipe()
	if err != nil {
		return fmt.Errorf("worker[%d]: stdout pipe: %w", w.idx, err)
	}
	c.Stderr = &prefixWriter{prefix: fmt.Sprintf("[builder-worker-%d] ", w.idx), dst: os.Stderr}

	if err := c.Start(); err != nil {
		return fmt.Errorf("worker[%d]: start: %w", w.idx, err)
	}
	w.proc = c
	w.stdin = stdin
	w.stdout = stdout
	w.state.Store(int32(stateRunning))
	return nil
}

// call sends one request and blocks until the response arrives.
// The caller must hold w.mu.
func (w *worker) callLocked(req *orchpb.BuildBatchRequest) (*orchpb.BuildBatchResponse, error) {
	if err := writeFramedProto(w.stdin, req); err != nil {
		return nil, fmt.Errorf("worker[%d]: send request: %w", w.idx, err)
	}
	resp := &orchpb.BuildBatchResponse{}
	if err := readFramedProto(w.stdout, resp); err != nil {
		return nil, fmt.Errorf("worker[%d]: read response: %w", w.idx, err)
	}
	return resp, nil
}

func (w *worker) kill() {
	w.state.Store(int32(stateDead))
	if w.stdin != nil {
		w.stdin.Close()
	}
	if w.proc != nil && w.proc.Process != nil {
		w.proc.Process.Kill()
		w.proc.Wait()
	}
}

func (w *worker) isAlive() bool {
	return workerState(w.state.Load()) == stateRunning
}

// prefixWriter prefixes every line written to it before forwarding to dst.
type prefixWriter struct {
	prefix string
	dst    io.Writer
	mu     sync.Mutex
	buf    []byte
}

func (pw *prefixWriter) Write(p []byte) (int, error) {
	pw.mu.Lock()
	defer pw.mu.Unlock()
	pw.buf = append(pw.buf, p...)
	for {
		idx := -1
		for i, b := range pw.buf {
			if b == '\n' {
				idx = i
				break
			}
		}
		if idx < 0 {
			break
		}
		line := pw.buf[:idx+1]
		fmt.Fprintf(pw.dst, "%s%s", pw.prefix, line)
		pw.buf = append(pw.buf[:0], pw.buf[idx+1:]...)
	}
	return len(p), nil
}

// writeFramedProto writes [4-byte BE length][proto bytes] to w.
func writeFramedProto(w io.Writer, m proto.Message) error {
	data, err := proto.Marshal(m)
	if err != nil {
		return fmt.Errorf("marshal proto: %w", err)
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(data)))
	if _, err := w.Write(hdr[:]); err != nil {
		return fmt.Errorf("write length header: %w", err)
	}
	if _, err := w.Write(data); err != nil {
		return fmt.Errorf("write payload: %w", err)
	}
	return nil
}

// readFramedProto reads [4-byte BE length][proto bytes] from r into m.
func readFramedProto(r io.Reader, m proto.Message) error {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return fmt.Errorf("read length header: %w", err)
	}
	n := binary.BigEndian.Uint32(hdr[:])
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return fmt.Errorf("read payload (%d bytes): %w", n, err)
	}
	return proto.Unmarshal(buf, m)
}

