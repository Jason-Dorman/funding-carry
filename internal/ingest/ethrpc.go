package ingest

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/sha3"
)

// EthClient is a minimal Ethereum JSON-RPC client: the four methods the Base
// read path needs and nothing else.
//
// It is hand-rolled rather than go-ethereum's ethclient (ADR-0017) for the same
// reason the Coinbase client is (ADR-0016): the read path is three RPC methods
// and one ERC-20 call, and the ~100 lines below are cheaper to explain than a
// dependency tree the rest of the system never touches. Part 17 makes its own
// choice when it needs signing.
//
// It holds no credential. BASE_RPC_URL carries the Alchemy key in its path, so
// the URL itself is the secret and is never logged; see cmd/ingest.
type EthClient struct {
	url  string
	http Doer
}

// rpcTimeout bounds one JSON-RPC round trip. It is deliberately shorter than
// POLL_BASE_SECS: a call that has not answered by then has missed its poll, and
// letting it run into the next one would queue reads behind a dead endpoint
// rather than degrade to staleness.
const rpcTimeout = 15 * time.Second

// NewEthClient builds the client. url is BASE_RPC_URL.
func NewEthClient(url string, doer Doer) *EthClient {
	if doer == nil {
		doer = &http.Client{Timeout: rpcTimeout}
	}
	return &EthClient{url: url, http: doer}
}

// ---------------------------------------------------------------------------
// Address
// ---------------------------------------------------------------------------

// Address is a 20-byte account or contract address.
//
// It is a parsed type rather than a string because of one asymmetry in this API:
// eth_getBalance against an address that does not exist returns 0, not an error,
// and balanceOf against a contract that is not a token returns nothing rather
// than complaining. A typo in WALLET_ADDRESS would therefore poll happily
// forever and record a wallet that holds nothing. The parse is the only place
// that mistake can be caught, so there is a type whose only way in is a parse.
type Address [20]byte

// ParseAddress reads a hex address, with or without the 0x prefix and in any
// case. Mixed-case EIP-55 checksums parse; the checksum itself is not verified,
// because a wrong one is not what this guards against.
func ParseAddress(s string) (Address, error) {
	var a Address
	t := strings.TrimPrefix(strings.TrimPrefix(s, "0x"), "0X")
	if len(t) != 2*len(a) {
		return a, fmt.Errorf("address %q: want %d hex digits, got %d", s, 2*len(a), len(t))
	}
	if _, err := hex.Decode(a[:], []byte(t)); err != nil {
		return a, fmt.Errorf("address %q: %w", s, err)
	}
	return a, nil
}

// String renders the lowercase 0x form, which is what the RPC expects.
func (a Address) String() string { return "0x" + hex.EncodeToString(a[:]) }

// LogValue is what makes an Address readable in a log line, and it is not
// optional decoration.
//
// slog resolves LogValuer, not Stringer, so without it the JSON handler — the
// format containers log in — renders an Address as twenty JSON numbers. The
// first live run printed `"wallet":[216,218,107,242,…]` on the line whose whole
// purpose is to say which wallet the rows describe. Same trap as the Secret
// redaction gap found in the 2026-08-19 review, arrived at from the opposite
// direction: there a value was meant to be hidden and was not, here one is meant
// to be readable and was not.
func (a Address) LogValue() slog.Value { return slog.StringValue(a.String()) }

// ---------------------------------------------------------------------------
// Errors
// ---------------------------------------------------------------------------

// RPCError is a refusal from the endpoint, whether it arrived as a non-2xx
// status or as an error object inside a 200.
//
// Both are one type because they are one thing to the caller — the read did not
// happen — and both have to answer the question the poller asks before it
// decides how loudly to complain: is waiting worth it?
type RPCError struct {
	Method  string
	Status  int // HTTP status; 0 when the endpoint answered 200 with an error object
	Code    int // JSON-RPC error code; 0 when the failure was the HTTP status
	Message string
}

func (e *RPCError) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("%s: http %d: %s", e.Method, e.Status, e.Message)
	}
	return fmt.Sprintf("%s: rpc error %d: %s", e.Method, e.Code, e.Message)
}

// JSON-RPC error codes worth distinguishing. Everything else is the request
// being wrong, which will be wrong again next poll.
const (
	rpcInternalError = -32603 // the node failed, not the request
	rpcLimitExceeded = -32005 // Alchemy's rate limit, when it does not use 429
)

// Retryable reports whether waiting could change the answer.
func (e *RPCError) Retryable() bool {
	if e.Status != 0 {
		return e.Status == http.StatusTooManyRequests || e.Status >= http.StatusInternalServerError
	}
	return e.Code == rpcInternalError || e.Code == rpcLimitExceeded
}

// ---------------------------------------------------------------------------
// Transport
// ---------------------------------------------------------------------------

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  []any  `json:"params"`
}

type rpcResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// call performs one JSON-RPC request and decodes result into out.
func (c *EthClient) call(ctx context.Context, method string, out any, params ...any) error {
	req, err := c.request(ctx, method, params)
	if err != nil {
		return err
	}

	resp, err := c.http.Do(req)
	if err != nil {
		// The URL carries the API key in its path and *url.Error quotes the URL
		// it failed on, so the transport error is reported by method rather than
		// wrapped. This is the one error path that would otherwise put the
		// credential in a log line, and
		// TestATransportFailureDoesNotLeakTheURL is what keeps it that way.
		return fmt.Errorf("post %s: transport failure", method)
	}
	defer func() { _ = resp.Body.Close() }()

	return decodeRPC(method, resp, out)
}

func (c *EthClient) request(ctx context.Context, method string, params []any) (*http.Request, error) {
	if params == nil {
		params = []any{}
	}
	body, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: 1, Method: method, Params: params})
	if err != nil {
		return nil, fmt.Errorf("encode %s: %w", method, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		// Deliberately not wrapped, for the same reason as the transport failure
		// below and found the same way: this error is a *url.Error, and *url.Error
		// quotes the URL it failed on — which here is the credential. An endpoint
		// that net/url rejects (a control character from a CRLF-edited env file, a
		// space in the host, a missing scheme) would otherwise put the key into a
		// WARN line on the poller's first read.
		return nil, fmt.Errorf("build request for %s: the configured endpoint is not a valid URL", method)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	return req, nil
}

// decodeRPC turns one response into either out or an error. A refusal reaches
// here as a status or as an error object inside a 200, and both leave as the
// same type.
func decodeRPC(method string, resp *http.Response, out any) error {
	if resp.StatusCode != http.StatusOK {
		// Bounded: an error page can be arbitrarily large and none of it is
		// worth more than a line in a log.
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return &RPCError{Method: method, Status: resp.StatusCode, Message: string(snippet)}
	}

	var rr rpcResponse
	if err := json.NewDecoder(resp.Body).Decode(&rr); err != nil {
		return fmt.Errorf("decode %s: %w", method, err)
	}
	if rr.Error != nil {
		return &RPCError{Method: method, Code: rr.Error.Code, Message: rr.Error.Message}
	}
	if len(rr.Result) == 0 || string(rr.Result) == "null" {
		return fmt.Errorf("%s: no result", method)
	}
	if err := json.Unmarshal(rr.Result, out); err != nil {
		return fmt.Errorf("decode %s result: %w", method, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Block tags
// ---------------------------------------------------------------------------

// BlockTag names the block a read is answered at.
type BlockTag string

// LatestBlock is the tag for "whatever the node has now". It is used only for
// the first read of a poll; the rest are pinned (see BlockAt).
const LatestBlock BlockTag = "latest"

// BlockAt pins a read to one block height.
//
// Every balance and price in a base_state row is read at the same block, so the
// row is a snapshot of one moment on the chain rather than a smear across three.
// Without it a row could show a wallet before a swap and a price after it.
func BlockAt(n uint64) BlockTag { return BlockTag("0x" + strconv.FormatUint(n, 16)) }

// ---------------------------------------------------------------------------
// Methods
// ---------------------------------------------------------------------------

// BlockNumber returns the height of the newest block the node has.
func (c *EthClient) BlockNumber(ctx context.Context) (uint64, error) {
	var raw string
	if err := c.call(ctx, "eth_blockNumber", &raw); err != nil {
		return 0, err
	}
	n, err := decodeQuantity(raw)
	if err != nil {
		return 0, fmt.Errorf("eth_blockNumber: %w", err)
	}
	if !n.IsUint64() {
		return 0, fmt.Errorf("eth_blockNumber: %s does not fit a uint64", n)
	}
	return n.Uint64(), nil
}

// Balance returns an address's native ETH balance in wei.
func (c *EthClient) Balance(ctx context.Context, a Address, at BlockTag) (*big.Int, error) {
	var raw string
	if err := c.call(ctx, "eth_getBalance", &raw, a.String(), string(at)); err != nil {
		return nil, err
	}
	n, err := decodeQuantity(raw)
	if err != nil {
		return nil, fmt.Errorf("eth_getBalance: %w", err)
	}
	return n, nil
}

// GasPrice returns the node's current gas price estimate, in wei.
//
// It takes no block tag: it is the node's view of what a transaction would have
// to pay now, not a property of a past block, so it is the one field in a
// base_state row that cannot be pinned with the others.
func (c *EthClient) GasPrice(ctx context.Context) (*big.Int, error) {
	var raw string
	if err := c.call(ctx, "eth_gasPrice", &raw); err != nil {
		return nil, err
	}
	n, err := decodeQuantity(raw)
	if err != nil {
		return nil, fmt.Errorf("eth_gasPrice: %w", err)
	}
	return n, nil
}

// CallContract runs a read-only contract call and returns the raw return data.
func (c *EthClient) CallContract(ctx context.Context, to Address, data []byte, at BlockTag) ([]byte, error) {
	args := map[string]string{"to": to.String(), "data": "0x" + hex.EncodeToString(data)}
	var raw string
	if err := c.call(ctx, "eth_call", &raw, args, string(at)); err != nil {
		return nil, err
	}
	out, err := decodeData(raw)
	if err != nil {
		return nil, fmt.Errorf("eth_call %s: %w", to, err)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Hex
// ---------------------------------------------------------------------------

// decodeQuantity parses a JSON-RPC QUANTITY: 0x-prefixed, minimal-length hex.
func decodeQuantity(s string) (*big.Int, error) {
	t, ok := strings.CutPrefix(s, "0x")
	if !ok || t == "" {
		return nil, fmt.Errorf("quantity %q: not 0x-prefixed hex", s)
	}
	// The sign check is not redundant: big.Int.SetString accepts a leading "-" or
	// "+", so "0x-1" parsed cleanly and became a balance of minus one wei. A
	// JSON-RPC QUANTITY is unsigned by definition, and every caller here reads a
	// balance, a gas price or a block height — none of which can be negative, so
	// a signed one is a broken endpoint rather than a small number.
	if t[0] == '-' || t[0] == '+' {
		return nil, fmt.Errorf("quantity %q: signed", s)
	}
	n, ok := new(big.Int).SetString(t, 16)
	if !ok {
		return nil, fmt.Errorf("quantity %q: not hex", s)
	}
	return n, nil
}

// decodeData parses a JSON-RPC DATA value: 0x-prefixed, even-length hex. Empty
// (a bare "0x") is allowed and returns no bytes — that is what a call to an
// address holding no code answers, and it is the caller's job to say so.
func decodeData(s string) ([]byte, error) {
	t, ok := strings.CutPrefix(s, "0x")
	if !ok {
		return nil, fmt.Errorf("data %q: not 0x-prefixed", s)
	}
	b, err := hex.DecodeString(t)
	if err != nil {
		return nil, fmt.Errorf("data %q: %w", s, err)
	}
	return b, nil
}

// ---------------------------------------------------------------------------
// ABI
//
// Enough of the contract ABI to make four read calls and take them apart: a
// selector, address arguments, and fixed 32-byte return words. Nothing here
// handles dynamic types, because nothing this system reads has one.
// ---------------------------------------------------------------------------

// wordSize is the ABI's fixed slot width.
const wordSize = 32

// selector is the first four bytes of the keccak-256 hash of a function's
// canonical signature — the ABI's method id.
//
// It is computed rather than pasted. The four ids this file needs are famous
// constants and pasting them would work, but deriving them makes the known
// values a test (TestSelectorsMatchThePublishedMethodIds) instead of four magic
// numbers nobody can check by reading.
func selector(sig string) []byte {
	h := sha3.NewLegacyKeccak256()
	// hash.Hash documents that Write never returns an error.
	_, _ = h.Write([]byte(sig))
	return h.Sum(nil)[:4]
}

// callData builds the calldata for a function taking only address arguments.
func callData(sig string, args ...Address) []byte {
	data := selector(sig)
	for _, a := range args {
		var word [wordSize]byte
		copy(word[wordSize-len(a):], a[:]) // left-padded, as the ABI requires
		data = append(data, word[:]...)
	}
	return data
}

// returnWord returns the i'th 32-byte word of a return value.
//
// A short return is an error rather than a zero value on purpose: a call to a
// contract that does not implement the function answers with no data at all, and
// reading that as zero would record a wallet holding no USDC, or a pool priced
// at nothing, with no sign that anything went wrong.
func returnWord(b []byte, i int) ([]byte, error) {
	lo, hi := i*wordSize, (i+1)*wordSize
	if len(b) < hi {
		return nil, fmt.Errorf("return value is %d bytes, want at least %d", len(b), hi)
	}
	return b[lo:hi], nil
}

// uintWord reads word i as an unsigned integer.
func uintWord(b []byte, i int) (*big.Int, error) {
	w, err := returnWord(b, i)
	if err != nil {
		return nil, err
	}
	return new(big.Int).SetBytes(w), nil
}

// addressWord reads word i as an address, rejecting anything in the twelve bytes
// of padding an address is not allowed to occupy.
func addressWord(b []byte, i int) (Address, error) {
	var a Address
	w, err := returnWord(b, i)
	if err != nil {
		return a, err
	}
	if !allZero(w[:wordSize-len(a)]) {
		return a, fmt.Errorf("word %d is not an address: padding is not zero", i)
	}
	copy(a[:], w[wordSize-len(a):])
	return a, nil
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}
