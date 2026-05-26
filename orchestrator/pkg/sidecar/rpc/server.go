// Package rpc serves the sidecar's JSON-RPC endpoints over HTTP. The schema
// mirrors Nethermind.StateComposition.Rpc.StateCompositionRpcModule so that
// orchestrators can flip --sensor-rpc-url to the sidecar without code changes.
//
// Supported methods:
//
//	statecomp_lite   — tier-1 counters + lag (production-critical fast path).
//	statecomp_get    — full report (tier-2 too).
//	statecomp_health — sidecar-specific liveness / lag / RSS report.
package rpc

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"

	"github.com/NethermindEth/eth-perf-research/orchestrator/pkg/sidecar/tracker"
)

// Server is the HTTP JSON-RPC frontend.
type Server struct {
	tracker *tracker.Tracker
	log     zerolog.Logger
	addr    string

	mu                 sync.RWMutex
	chainHeadBlock     int64 // updated by the tailer; used for blocksBehind
	startedAt          time.Time
	lastSnapshotAt     atomic.Int64 // unix-ns
	bootstrapCompleted atomic.Bool
}

// New constructs a Server bound to addr (e.g. "0.0.0.0:9001").
func New(t *tracker.Tracker, addr string, log zerolog.Logger) *Server {
	return &Server{
		tracker:   t,
		log:       log,
		addr:      addr,
		startedAt: time.Now(),
	}
}

// SetChainHead records the orchestrator's view of the current chain head.
// Phase 2 will populate this from the tailer's last-seen BlockDiffs key.
func (s *Server) SetChainHead(b int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b > s.chainHeadBlock {
		s.chainHeadBlock = b
	}
}

// NoteSnapshotWritten stamps the time of the most-recent snapshot publish.
func (s *Server) NoteSnapshotWritten() { s.lastSnapshotAt.Store(time.Now().UnixNano()) }

// NoteBootstrapDone flips the bootstrap-complete flag.
func (s *Server) NoteBootstrapDone() { s.bootstrapCompleted.Store(true) }

// ListenAndServe blocks. Call Shutdown via http.Server.Shutdown on a cloned
// http.Server if you need graceful cancellation; otherwise just close the
// parent context's listener.
func (s *Server) ListenAndServe() (*http.Server, error) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handle)
	srv := &http.Server{
		Addr:              s.addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			s.log.Error().Err(err).Msg("rpc server error")
		}
	}()
	return srv, nil
}

// jsonRPCRequest mirrors the JSON-RPC 2.0 request shape we accept. We only
// support `id`, `method`, and (ignored) `params`.
type jsonRPCRequest struct {
	Jsonrpc string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// jsonRPCResponse is the standard 2.0 envelope.
type jsonRPCResponse struct {
	Jsonrpc string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// ServeHTTP makes Server an http.Handler. Useful for tests that want to drive
// the JSON-RPC code path via httptest without binding a TCP port.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handle(w, r)
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()

	var req jsonRPCRequest
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&req); err != nil {
		writeRPCError(w, nil, -32700, fmt.Sprintf("parse error: %v", err))
		return
	}
	if req.Jsonrpc != "" && req.Jsonrpc != "2.0" {
		writeRPCError(w, req.ID, -32600, "only jsonrpc 2.0 supported")
		return
	}

	result, rpcErr := s.dispatch(req.Method, req.Params)
	if rpcErr != nil {
		writeRPCError(w, req.ID, rpcErr.Code, rpcErr.Message)
		return
	}
	writeRPCOK(w, req.ID, result)
}

func (s *Server) dispatch(method string, params json.RawMessage) (any, *jsonRPCError) {
	switch method {
	case "statecomp_lite":
		return s.statecompLite(), nil
	case "statecomp_get":
		return s.statecompGet(), nil
	case "statecomp_health":
		return s.statecompHealth(), nil
	default:
		return nil, &jsonRPCError{Code: -32601, Message: "method not found: " + method}
	}
}

func writeRPCOK(w http.ResponseWriter, id json.RawMessage, result any) {
	resp := jsonRPCResponse{Jsonrpc: "2.0", ID: id, Result: result}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func writeRPCError(w http.ResponseWriter, id json.RawMessage, code int, msg string) {
	resp := jsonRPCResponse{Jsonrpc: "2.0", ID: id, Error: &jsonRPCError{Code: code, Message: msg}}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func hex32(b [32]byte) string {
	if b == ([32]byte{}) {
		return ""
	}
	return "0x" + hex.EncodeToString(b[:])
}

// readRSSMB returns the process RSS in MiB. Cross-platform best effort:
// Linux reads /proc/self/status; Darwin returns 0 (Go MemStats already
// reports Alloc).
func readRSSMB() int64 {
	// Avoid syscall.Getrusage Darwin/Linux unit differences — keep it simple.
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return int64(ms.Sys / (1024 * 1024))
}
