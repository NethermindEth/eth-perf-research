// Package rpc provides a JSON-RPC HTTP client for Ethereum execution-layer
// nodes. It supports both the public port (8545) and the JWT-authenticated
// Engine API port (8551).
//
// Thin wrapper over go-ethereum's rpc.Client and ethclient.Client. We add
// only what they don't expose directly: a Nethermind testing_commitBlockV1
// helper and a statecomp_get convenience for the sensor.
package rpc

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/node"
	gethrpc "github.com/ethereum/go-ethereum/rpc"
)

const (
	defaultTimeout      = 30 * time.Second
	defaultMaxIdleConns = 16
	defaultUserAgent    = "eth-perf-research/orchestrator"
)

// RpcError is returned when the JSON-RPC response envelope contains an error.
type RpcError struct {
	Code    int
	Message string
}

func (e *RpcError) Error() string {
	return fmt.Sprintf("rpc error: code=%d message=%s", e.Code, e.Message)
}

// BlockHeader holds the fields the orchestrator cares about from
// eth_getBlockByNumber / eth_getBlockByHash.
type BlockHeader struct {
	Number     uint64
	Hash       common.Hash
	ParentHash common.Hash
	StateRoot  common.Hash
	GasLimit   uint64
	GasUsed    uint64
	Timestamp  uint64
	BaseFee    *big.Int
}

// clientOpts collects functional-option state before the Client is built.
type clientOpts struct {
	jwtSecret    []byte
	timeout      time.Duration
	maxIdleConns int
	userAgent    string
}

// Option configures a Client.
type Option func(*clientOpts)

// WithJWTSecret provides the HS256 secret as a hex string (0x prefix optional).
func WithJWTSecret(secretHex string) Option {
	return func(o *clientOpts) {
		b, err := decodeHex32(secretHex)
		if err != nil {
			o.jwtSecret = nil
			return
		}
		o.jwtSecret = b
	}
}

// WithJWTFile reads the hex secret from a file (one line, 64 hex chars).
func WithJWTFile(path string) Option {
	return func(o *clientOpts) {
		raw, err := os.ReadFile(path)
		if err != nil {
			return
		}
		b, err := decodeHex32(strings.TrimSpace(string(raw)))
		if err != nil {
			return
		}
		o.jwtSecret = b
	}
}

// WithTimeout sets the HTTP request timeout (default 30s).
func WithTimeout(d time.Duration) Option {
	return func(o *clientOpts) { o.timeout = d }
}

// WithMaxIdleConns sets the maximum number of idle keep-alive connections.
func WithMaxIdleConns(n int) Option {
	return func(o *clientOpts) { o.maxIdleConns = n }
}

// WithUserAgent sets the User-Agent header sent on every request.
func WithUserAgent(ua string) Option {
	return func(o *clientOpts) { o.userAgent = ua }
}

// Client is a JSON-RPC HTTP client. Safe for concurrent use.
type Client struct {
	rpc *gethrpc.Client
	eth *ethclient.Client
}

// NewClient constructs a JSON-RPC client. baseURL must include scheme + host +
// port. Use WithJWTSecret or WithJWTFile to enable JWT auth.
func NewClient(baseURL string, opts ...Option) (*Client, error) {
	o := &clientOpts{
		timeout:      defaultTimeout,
		maxIdleConns: defaultMaxIdleConns,
		userAgent:    defaultUserAgent,
	}
	for _, fn := range opts {
		fn(o)
	}

	httpClient := &http.Client{
		Transport: &http.Transport{MaxIdleConnsPerHost: o.maxIdleConns},
		Timeout:   o.timeout,
	}

	clientOpts := []gethrpc.ClientOption{
		gethrpc.WithHTTPClient(httpClient),
		gethrpc.WithHeader("User-Agent", o.userAgent),
	}
	if len(o.jwtSecret) > 0 {
		var secret [32]byte
		copy(secret[:], o.jwtSecret)
		clientOpts = append(clientOpts, gethrpc.WithHTTPAuth(node.NewJWTAuth(secret)))
	}

	rc, err := gethrpc.DialOptions(context.Background(), strings.TrimRight(baseURL, "/"), clientOpts...)
	if err != nil {
		return nil, fmt.Errorf("rpc: dial %s: %w", baseURL, err)
	}
	return &Client{rpc: rc, eth: ethclient.NewClient(rc)}, nil
}

// Call invokes method with params and unmarshals the result into out.
func (c *Client) Call(ctx context.Context, method string, params []any, out any) error {
	args := params
	if args == nil {
		args = []any{}
	}
	if err := c.rpc.CallContext(ctx, out, method, args...); err != nil {
		return translateError(err)
	}
	return nil
}

// ChainID returns the chain ID via eth_chainId.
func (c *Client) ChainID(ctx context.Context) (uint64, error) {
	id, err := c.eth.ChainID(ctx)
	if err != nil {
		return 0, fmt.Errorf("rpc: ChainID: %w", translateError(err))
	}
	if !id.IsUint64() {
		return 0, fmt.Errorf("rpc: ChainID: %s does not fit in uint64", id.String())
	}
	return id.Uint64(), nil
}

// BlockByNumber returns the block header for number n. n < 0 fetches "latest".
func (c *Client) BlockByNumber(ctx context.Context, n int64) (*BlockHeader, error) {
	var num *big.Int
	if n >= 0 {
		num = big.NewInt(n)
	}
	h, err := c.eth.HeaderByNumber(ctx, num)
	if err != nil {
		return nil, fmt.Errorf("rpc: BlockByNumber(%d): %w", n, translateError(err))
	}
	return headerToBlockHeader(h), nil
}

// BlockByHash returns the block header identified by h.
func (c *Client) BlockByHash(ctx context.Context, h common.Hash, _ bool) (*BlockHeader, error) {
	hdr, err := c.eth.HeaderByHash(ctx, h)
	if err != nil {
		return nil, fmt.Errorf("rpc: BlockByHash(%s): %w", h.Hex(), translateError(err))
	}
	return headerToBlockHeader(hdr), nil
}

// TransactionCount returns the on-chain nonce for addr at the latest block.
func (c *Client) TransactionCount(ctx context.Context, addr common.Address) (uint64, error) {
	n, err := c.eth.NonceAt(ctx, addr, nil)
	if err != nil {
		return 0, fmt.Errorf("rpc: TransactionCount(%s): %w", addr.Hex(), translateError(err))
	}
	return n, nil
}

// TestingCommitBlockV1 submits signed transactions to Nethermind's
// testing_commitBlockV1 endpoint. Returns the committed block hash.
func (c *Client) TestingCommitBlockV1(ctx context.Context, signedTxs [][]byte, timestamp uint64) (common.Hash, error) {
	txs := make([]hexutil.Bytes, len(signedTxs))
	for i, tx := range signedTxs {
		txs[i] = tx
	}
	payloadAttrs := map[string]any{
		"timestamp":             hexutil.Uint64(timestamp),
		"prevRandao":            common.Hash{},
		"suggestedFeeRecipient": common.Address{},
		"withdrawals":           []any{},
		"parentBeaconBlockRoot": common.Hash{},
	}
	var result common.Hash
	if err := c.Call(ctx, "testing_commitBlockV1", []any{payloadAttrs, txs, nil}, &result); err != nil {
		return common.Hash{}, fmt.Errorf("rpc: TestingCommitBlockV1: %w", err)
	}
	return result, nil
}

// CodeAt returns the deployed bytecode at addr for the given block tag
// ("latest" when blockTag is "").
func (c *Client) CodeAt(ctx context.Context, addr common.Address, blockTag string) ([]byte, error) {
	if blockTag == "" || blockTag == "latest" {
		b, err := c.eth.CodeAt(ctx, addr, nil)
		if err != nil {
			return nil, fmt.Errorf("rpc: CodeAt(%s): %w", addr.Hex(), translateError(err))
		}
		return b, nil
	}
	// Numeric block tag: ethclient.CodeAt would require parsing the tag to a
	// *big.Int, but the only non-"latest" caller in this repo is testing-only.
	// Fall back to the raw call to keep ethclient out of the codepath.
	var raw hexutil.Bytes
	if err := c.Call(ctx, "eth_getCode", []any{addr.Hex(), blockTag}, &raw); err != nil {
		return nil, fmt.Errorf("rpc: CodeAt(%s): %w", addr.Hex(), err)
	}
	return raw, nil
}

// StatecompGet retrieves Nethermind's statecomp plugin report.
func (c *Client) StatecompGet(ctx context.Context) (json.RawMessage, error) {
	var raw json.RawMessage
	if err := c.Call(ctx, "statecomp_get", nil, &raw); err != nil {
		return nil, fmt.Errorf("rpc: StatecompGet: %w", err)
	}
	return raw, nil
}

// translateError maps go-ethereum's rpc.Error to our RpcError envelope so
// existing callers that type-assert on *RpcError keep working.
func translateError(err error) error {
	if err == nil {
		return nil
	}
	var rerr gethrpc.Error
	if errors.As(err, &rerr) {
		return &RpcError{Code: rerr.ErrorCode(), Message: rerr.Error()}
	}
	return err
}

// headerToBlockHeader projects geth's Header onto the subset the orchestrator
// uses.
func headerToBlockHeader(h *types.Header) *BlockHeader {
	var baseFee *big.Int
	if h.BaseFee != nil {
		baseFee = new(big.Int).Set(h.BaseFee)
	}
	return &BlockHeader{
		Number:     h.Number.Uint64(),
		Hash:       h.Hash(),
		ParentHash: h.ParentHash,
		StateRoot:  h.Root,
		GasLimit:   h.GasLimit,
		GasUsed:    h.GasUsed,
		Timestamp:  h.Time,
		BaseFee:    baseFee,
	}
}

// decodeHex32 decodes a 64-char hex string (0x prefix optional) to 32 bytes.
func decodeHex32(s string) ([]byte, error) {
	s = strings.TrimPrefix(s, "0x")
	s = strings.TrimPrefix(s, "0X")
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("decodeHex32: %w", err)
	}
	if len(b) != 32 {
		return nil, fmt.Errorf("decodeHex32: expected 32 bytes, got %d", len(b))
	}
	return b, nil
}
