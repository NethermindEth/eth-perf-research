// Package rpc provides a JSON-RPC HTTP client for Ethereum execution-layer
// nodes. It supports both the public port (8545) and the JWT-authenticated
// Engine API port (8551).
//
// The implementation wraps go-ethereum's rpc.Client and ethclient.Client so we
// inherit canonical JSON envelope handling, JWT minting, and HTTP transport
// reuse. The package surface is preserved so existing callers compile
// unchanged.
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
	Number       uint64
	Hash         common.Hash
	ParentHash   common.Hash
	StateRoot    common.Hash
	GasLimit     uint64
	GasUsed      uint64
	Timestamp    uint64
	BaseFee      *big.Int
	Transactions []common.Hash // populated when full=false
}

// clientOpts collects functional-option state before the Client is built.
type clientOpts struct {
	jwtSecret    []byte // raw 32-byte HMAC key; nil → no JWT
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

// WithMaxIdleConns sets the maximum number of idle keep-alive connections
// (default 16).
func WithMaxIdleConns(n int) Option {
	return func(o *clientOpts) { o.maxIdleConns = n }
}

// WithUserAgent sets the User-Agent header sent on every request.
func WithUserAgent(ua string) Option {
	return func(o *clientOpts) { o.userAgent = ua }
}

// Client is a JSON-RPC HTTP client. It is safe for concurrent use.
type Client struct {
	rpc *gethrpc.Client
	eth *ethclient.Client
}

// NewClient constructs a JSON-RPC client. baseURL must include scheme + host +
// port (e.g. "http://localhost:8545"). Use WithJWTSecret or WithJWTFile to
// enable JWT auth for the Engine API port.
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

// Call invokes method with params and unmarshals the result into out (must be
// a pointer or nil).
func (c *Client) Call(ctx context.Context, method string, params []any, out any) error {
	args := params
	if args == nil {
		args = []any{}
	}
	if err := c.rpc.CallContext(ctx, out, method, args...); err != nil {
		return translateError(err, method)
	}
	return nil
}

// ChainID returns the chain ID via eth_chainId.
func (c *Client) ChainID(ctx context.Context) (uint64, error) {
	id, err := c.eth.ChainID(ctx)
	if err != nil {
		return 0, fmt.Errorf("rpc: ChainID: %w", translateError(err, "eth_chainId"))
	}
	if !id.IsUint64() {
		return 0, fmt.Errorf("rpc: ChainID: %s does not fit in uint64", id.String())
	}
	return id.Uint64(), nil
}

// BlockByNumber returns the block header for number n. n < 0 fetches "latest".
func (c *Client) BlockByNumber(ctx context.Context, n int64) (*BlockHeader, error) {
	tag := "latest"
	if n >= 0 {
		tag = fmt.Sprintf("0x%x", n)
	}
	var raw json.RawMessage
	if err := c.Call(ctx, "eth_getBlockByNumber", []any{tag, false}, &raw); err != nil {
		return nil, fmt.Errorf("rpc: BlockByNumber(%d): %w", n, err)
	}
	return parseBlockHeader(raw)
}

// BlockByHash returns the block header identified by h. When full is false only
// transaction hashes are populated in BlockHeader.Transactions.
func (c *Client) BlockByHash(ctx context.Context, h common.Hash, full bool) (*BlockHeader, error) {
	var raw json.RawMessage
	if err := c.Call(ctx, "eth_getBlockByHash", []any{h.Hex(), full}, &raw); err != nil {
		return nil, fmt.Errorf("rpc: BlockByHash(%s): %w", h.Hex(), err)
	}
	return parseBlockHeader(raw)
}

// TransactionCount returns the on-chain nonce for addr at the latest block.
func (c *Client) TransactionCount(ctx context.Context, addr common.Address) (uint64, error) {
	n, err := c.eth.NonceAt(ctx, addr, nil)
	if err != nil {
		return 0, fmt.Errorf("rpc: TransactionCount(%s): %w", addr.Hex(), translateError(err, "eth_getTransactionCount"))
	}
	return n, nil
}

// TestingCommitBlockV1 submits signed transactions to Nethermind's
// testing_commitBlockV1 endpoint. signedTxs contains type-prefixed RLP-encoded
// transactions. Returns the committed block hash.
func (c *Client) TestingCommitBlockV1(ctx context.Context, signedTxs [][]byte, timestamp uint64) (common.Hash, error) {
	hexTxs := make([]string, len(signedTxs))
	for i, tx := range signedTxs {
		hexTxs[i] = "0x" + hex.EncodeToString(tx)
	}
	payloadAttrs := map[string]any{
		"timestamp":             fmt.Sprintf("0x%x", timestamp),
		"prevRandao":            "0x" + strings.Repeat("00", 32),
		"suggestedFeeRecipient": "0x" + strings.Repeat("00", 20),
		"withdrawals":           []any{},
		"parentBeaconBlockRoot": "0x" + strings.Repeat("00", 32),
	}

	var result string
	if err := c.Call(ctx, "testing_commitBlockV1", []any{payloadAttrs, hexTxs, nil}, &result); err != nil {
		return common.Hash{}, fmt.Errorf("rpc: TestingCommitBlockV1: %w", err)
	}
	return common.HexToHash(result), nil
}

// CodeAt returns the deployed bytecode at addr for the given block tag
// ("latest" when blockTag is ""). An empty slice means the account has no code.
func (c *Client) CodeAt(ctx context.Context, addr common.Address, blockTag string) ([]byte, error) {
	if blockTag == "" || blockTag == "latest" {
		b, err := c.eth.CodeAt(ctx, addr, nil)
		if err != nil {
			return nil, fmt.Errorf("rpc: CodeAt(%s): %w", addr.Hex(), translateError(err, "eth_getCode"))
		}
		return b, nil
	}
	var raw string
	if err := c.Call(ctx, "eth_getCode", []any{addr.Hex(), blockTag}, &raw); err != nil {
		return nil, fmt.Errorf("rpc: CodeAt(%s): %w", addr.Hex(), err)
	}
	raw = strings.TrimPrefix(raw, "0x")
	raw = strings.TrimPrefix(raw, "0X")
	if raw == "" {
		return nil, nil
	}
	b, err := hex.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("rpc: CodeAt(%s): decode %q: %w", addr.Hex(), raw, err)
	}
	return b, nil
}

// StatecompGet retrieves Nethermind's statecomp plugin report for the latest
// block. The raw JSON is returned as-is for the sensor to parse.
func (c *Client) StatecompGet(ctx context.Context) (json.RawMessage, error) {
	var raw json.RawMessage
	if err := c.Call(ctx, "statecomp_get", nil, &raw); err != nil {
		return nil, fmt.Errorf("rpc: StatecompGet: %w", err)
	}
	return raw, nil
}

// translateError converts a go-ethereum rpc.Error into our RpcError envelope so
// existing callers that type-assert on *RpcError keep working.
func translateError(err error, method string) error {
	if err == nil {
		return nil
	}
	var rerr gethrpc.Error
	if errors.As(err, &rerr) {
		return &RpcError{Code: rerr.ErrorCode(), Message: rerr.Error()}
	}
	return err
}

// --- wire parsing helpers ---

// wireBlock is the subset of fields returned by eth_getBlockByNumber /
// eth_getBlockByHash that the orchestrator uses.
type wireBlock struct {
	Number        string   `json:"number"`
	Hash          string   `json:"hash"`
	ParentHash    string   `json:"parentHash"`
	StateRoot     string   `json:"stateRoot"`
	GasLimit      string   `json:"gasLimit"`
	GasUsed       string   `json:"gasUsed"`
	Timestamp     string   `json:"timestamp"`
	BaseFeePerGas string   `json:"baseFeePerGas"`
	Transactions  []string `json:"transactions"`
}

func parseBlockHeader(raw json.RawMessage) (*BlockHeader, error) {
	var wb wireBlock
	if err := json.Unmarshal(raw, &wb); err != nil {
		return nil, fmt.Errorf("rpc: parse block: %w", err)
	}

	number, err := parseHexUint64(wb.Number)
	if err != nil {
		return nil, fmt.Errorf("rpc: block.number: %w", err)
	}
	gasLimit, err := parseHexUint64(wb.GasLimit)
	if err != nil {
		return nil, fmt.Errorf("rpc: block.gasLimit: %w", err)
	}
	gasUsed, err := parseHexUint64(wb.GasUsed)
	if err != nil {
		return nil, fmt.Errorf("rpc: block.gasUsed: %w", err)
	}
	ts, err := parseHexUint64(wb.Timestamp)
	if err != nil {
		return nil, fmt.Errorf("rpc: block.timestamp: %w", err)
	}

	var baseFee *big.Int
	if wb.BaseFeePerGas != "" {
		baseFee, err = parseHexBigInt(wb.BaseFeePerGas)
		if err != nil {
			return nil, fmt.Errorf("rpc: block.baseFeePerGas: %w", err)
		}
	}

	txHashes := make([]common.Hash, len(wb.Transactions))
	for i, h := range wb.Transactions {
		txHashes[i] = common.HexToHash(h)
	}

	return &BlockHeader{
		Number:       number,
		Hash:         common.HexToHash(wb.Hash),
		ParentHash:   common.HexToHash(wb.ParentHash),
		StateRoot:    common.HexToHash(wb.StateRoot),
		GasLimit:     gasLimit,
		GasUsed:      gasUsed,
		Timestamp:    ts,
		BaseFee:      baseFee,
		Transactions: txHashes,
	}, nil
}

func parseHexUint64(s string) (uint64, error) {
	s = strings.TrimPrefix(s, "0x")
	s = strings.TrimPrefix(s, "0X")
	if s == "" {
		return 0, nil
	}
	var v uint64
	if _, err := fmt.Sscanf(s, "%x", &v); err != nil {
		return 0, fmt.Errorf("parseHexUint64 %q: %w", s, err)
	}
	return v, nil
}

func parseHexBigInt(s string) (*big.Int, error) {
	s = strings.TrimPrefix(s, "0x")
	s = strings.TrimPrefix(s, "0X")
	b := new(big.Int)
	if _, ok := b.SetString(s, 16); !ok {
		return nil, fmt.Errorf("parseHexBigInt %q: invalid hex", s)
	}
	return b, nil
}

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
