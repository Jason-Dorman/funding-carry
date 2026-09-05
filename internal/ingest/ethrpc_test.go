package ingest

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The four-byte method ids are published constants — anyone can look up
// 0x70a08231 and find balanceOf(address). This is the test that lets the code
// derive them from their signatures instead of pasting them: the famous values
// are asserted once, here, rather than sitting in the source as numbers a
// reviewer would have to take on faith.
func TestSelectorsMatchThePublishedMethodIds(t *testing.T) {
	for _, tc := range []struct {
		sig  string
		want string
	}{
		{sigBalanceOf, "70a08231"},
		{sigDecimals, "313ce567"},
		{sigToken0, "0dfe1681"},
		{sigToken1, "d21220a7"},
		{sigSlot0, "3850c7bd"},
	} {
		t.Run(tc.sig, func(t *testing.T) {
			if got := hex.EncodeToString(selector(tc.sig)); got != tc.want {
				t.Fatalf("selector(%q) = %s, want %s", tc.sig, got, tc.want)
			}
		})
	}
}

// An address that fails to parse is the only chance to notice a typo: every
// read this system makes answers a wrong address with a plausible zero rather
// than an error.
func TestParseAddressRejectsWhatIsNotAnAddress(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		ok   bool
	}{
		{"lowercase", "0x4200000000000000000000000000000000000006", true},
		{"eip55 mixed case", "0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913", true},
		{"no prefix", "4200000000000000000000000000000000000006", true},
		{"one digit short", "0x420000000000000000000000000000000000000", false},
		{"one digit long", "0x42000000000000000000000000000000000000067", false},
		{"not hex", "0x42000000000000000000000000000000000000zz", false},
		{"empty", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseAddress(tc.in)
			if tc.ok && err != nil {
				t.Fatalf("ParseAddress(%q): %v", tc.in, err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("ParseAddress(%q) was accepted", tc.in)
			}
		})
	}
}

func TestParseAddressRoundTripsToTheLowercaseForm(t *testing.T) {
	const checksummed = "0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913"
	a, err := ParseAddress(checksummed)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if want := strings.ToLower(checksummed); a.String() != want {
		t.Fatalf("String() = %s, want %s", a, want)
	}
}

// scriptedRPC answers JSON-RPC calls from a map of method to result, recording
// the params it was sent so a test can assert on what was actually asked.
type scriptedRPC struct {
	t       *testing.T
	results map[string]any
	status  int
	rpcErr  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}

	params map[string][]any
}

func newScriptedRPC(t *testing.T) *scriptedRPC {
	t.Helper()
	return &scriptedRPC{t: t, results: map[string]any{}, params: map[string][]any{}}
}

func (s *scriptedRPC) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req rpcRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.t.Errorf("decode request: %v", err)
		return
	}
	s.params[req.Method] = req.Params

	if s.status != 0 && s.status != http.StatusOK {
		w.WriteHeader(s.status)
		_, _ = w.Write([]byte(`{"error":"nope"}`))
		return
	}

	body := map[string]any{"jsonrpc": "2.0", "id": req.ID}
	if s.rpcErr != nil {
		body["error"] = s.rpcErr
	} else {
		body["result"] = s.results[req.Method]
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(body); err != nil {
		s.t.Errorf("encode response: %v", err)
	}
}

func (s *scriptedRPC) client(t *testing.T) *EthClient {
	t.Helper()
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return NewEthClient(srv.URL, srv.Client())
}

func TestEthClientReadsTheFourMethods(t *testing.T) {
	s := newScriptedRPC(t)
	s.results["eth_blockNumber"] = "0x2160ec0"
	s.results["eth_getBalance"] = "0x16345785d8a0000" // 0.1 ETH in wei
	s.results["eth_gasPrice"] = "0x2540be400"         // 10 gwei
	s.results["eth_call"] = "0x" + hex.EncodeToString(uint256Word(big.NewInt(6)))

	c := s.client(t)
	ctx := context.Background()

	block, err := c.BlockNumber(ctx)
	if err != nil {
		t.Fatalf("block number: %v", err)
	}
	if block != 0x2160ec0 {
		t.Fatalf("block = %d, want %d", block, 0x2160ec0)
	}

	wei, err := c.Balance(ctx, testWallet, BlockAt(block))
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	if want := big.NewInt(100000000000000000); wei.Cmp(want) != 0 {
		t.Fatalf("balance = %s, want %s", wei, want)
	}
	// The address is sent lowercase and the block tag is the pinned one, not
	// "latest": the pin is what makes a base_state row one moment on the chain.
	if got := s.params["eth_getBalance"]; len(got) != 2 ||
		got[0] != testWallet.String() || got[1] != string(BlockAt(block)) {
		t.Fatalf("eth_getBalance params = %v", got)
	}

	gas, err := c.GasPrice(ctx)
	if err != nil {
		t.Fatalf("gas price: %v", err)
	}
	if want := big.NewInt(10000000000); gas.Cmp(want) != 0 {
		t.Fatalf("gas = %s, want %s", gas, want)
	}

	out, err := c.CallContract(ctx, testUSDC, callData(sigDecimals), BlockAt(block))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	got, err := uintWord(out, 0)
	if err != nil {
		t.Fatalf("decode call: %v", err)
	}
	if got.Int64() != 6 {
		t.Fatalf("decimals = %s, want 6", got)
	}
}

func TestRPCErrorsAreClassified(t *testing.T) {
	for _, tc := range []struct {
		name      string
		err       RPCError
		retryable bool
	}{
		{"rate limited", RPCError{Status: http.StatusTooManyRequests}, true},
		{"gateway down", RPCError{Status: http.StatusBadGateway}, true},
		{"bad request", RPCError{Status: http.StatusBadRequest}, false},
		{"unauthorized key", RPCError{Status: http.StatusUnauthorized}, false},
		{"node internal", RPCError{Code: rpcInternalError}, true},
		{"limit exceeded", RPCError{Code: rpcLimitExceeded}, true},
		{"method not found", RPCError{Code: -32601}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.Retryable(); got != tc.retryable {
				t.Fatalf("Retryable() = %v, want %v", got, tc.retryable)
			}
		})
	}
}

func TestANonOKStatusIsAnRPCError(t *testing.T) {
	s := newScriptedRPC(t)
	s.status = http.StatusTooManyRequests

	_, err := s.client(t).BlockNumber(context.Background())
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("error is %T, want *RPCError: %v", err, err)
	}
	if !rpcErr.Retryable() {
		t.Fatalf("a 429 should be retryable: %v", rpcErr)
	}
}

func TestAnErrorObjectInsideA200IsAnRPCError(t *testing.T) {
	s := newScriptedRPC(t)
	s.rpcErr = &struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}{Code: rpcLimitExceeded, Message: "your app has exceeded its capacity"}

	_, err := s.client(t).GasPrice(context.Background())
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("error is %T, want *RPCError: %v", err, err)
	}
	if rpcErr.Code != rpcLimitExceeded || !rpcErr.Retryable() {
		t.Fatalf("misclassified: %+v", rpcErr)
	}
}

// BASE_RPC_URL carries the Alchemy key in its path, so the transport error is
// the one place a credential could reach a log line: *url.Error quotes the URL
// it failed on, and the poller logs the error it is handed.
func TestATransportFailureDoesNotLeakTheURL(t *testing.T) {
	const key = "s3cret-alchemy-key"
	// A port nothing is listening on, so the request fails in the transport
	// rather than at the server.
	c := NewEthClient("http://127.0.0.1:1/v2/"+key, nil)

	_, err := c.BlockNumber(context.Background())
	if err == nil {
		t.Fatal("expected a transport failure")
	}
	if strings.Contains(err.Error(), key) || strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("the endpoint leaked into the error: %v", err)
	}
}

// The request-build path is the one *url.Error this client used to wrap, and it
// is the one the leak test above could not reach: that test uses a well-formed
// URL on a dead port, so it only ever exercises http.Do. An endpoint net/url
// rejects — a control character from a CRLF-edited env file, a space in the
// host, a missing scheme — failed earlier, at http.NewRequestWithContext, and
// *url.Error quotes the URL it failed on. Four independent reviewers found this
// on the same line.
func TestAMalformedEndpointDoesNotLeakTheURLEither(t *testing.T) {
	const key = "s3cret-alchemy-key"
	for _, tc := range []struct {
		name string
		url  string
	}{
		{"trailing carriage return", "https://base-mainnet.example/v2/" + key + "\r"},
		{"space in the host", "https://base mainnet.example/v2/" + key},
		{"no scheme", "://base-mainnet.example/v2/" + key},
		{"invalid port", "https://base-mainnet.example:8545x/v2/" + key},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewEthClient(tc.url, nil).BlockNumber(context.Background())
			if err == nil {
				t.Fatal("a malformed endpoint was accepted")
			}
			if strings.Contains(err.Error(), key) {
				t.Fatalf("the endpoint leaked into the error: %v", err)
			}
		})
	}
}

// A JSON-RPC QUANTITY is unsigned, and big.Int.SetString is not: it accepts a
// leading sign, so "0x-1" decoded to minus one and would have been recorded as a
// wallet balance, a gas price or a block height.
func TestASignedQuantityIsRefused(t *testing.T) {
	for _, s := range []string{"0x-1", "0x+5", "0x-1a2b"} {
		if n, err := decodeQuantity(s); err == nil {
			t.Errorf("decodeQuantity(%q) = %s, want an error", s, n)
		}
	}
}

func TestQuantitiesAndDataAreDecodedStrictly(t *testing.T) {
	if _, err := decodeQuantity("2160ec0"); err == nil {
		t.Fatal("a quantity without 0x was accepted")
	}
	if _, err := decodeQuantity("0x"); err == nil {
		t.Fatal("an empty quantity was accepted")
	}
	if _, err := decodeQuantity("0xnothex"); err == nil {
		t.Fatal("a non-hex quantity was accepted")
	}
	// "0x" is a legitimate DATA value: it is what a call to an address holding
	// no code answers, and the caller has to be able to tell that apart from a
	// zero.
	empty, err := decodeData("0x")
	if err != nil {
		t.Fatalf("decode empty data: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("0x decoded to %d bytes", len(empty))
	}
}

// A call to a contract that does not implement the function answers with no
// data at all. Reading that as zero would record a wallet holding no USDC, or a
// pool priced at nothing, with nothing on the row to say anything went wrong.
func TestAnEmptyReturnIsAnErrorNotAZero(t *testing.T) {
	if _, err := uintWord(nil, 0); err == nil {
		t.Fatal("an empty return decoded as a number")
	}
	if _, err := uintWord(make([]byte, 31), 0); err == nil {
		t.Fatal("a short word decoded as a number")
	}
	if _, err := returnWord(make([]byte, 32), 1); err == nil {
		t.Fatal("a missing second word decoded")
	}
}

func TestAddressWordRejectsNonZeroPadding(t *testing.T) {
	good := addressWordBytes(testUSDC)
	got, err := addressWord(good, 0)
	if err != nil {
		t.Fatalf("decode address word: %v", err)
	}
	if got != testUSDC {
		t.Fatalf("address = %s, want %s", got, testUSDC)
	}

	bad := addressWordBytes(testUSDC)
	bad[0] = 1
	if _, err := addressWord(bad, 0); err == nil {
		t.Fatal("a word with a dirty high byte decoded as an address")
	}
}

func TestBlockTagsAreMinimalHex(t *testing.T) {
	if got := BlockAt(0x2160ec0); got != "0x2160ec0" {
		t.Fatalf("BlockAt = %q", got)
	}
	if got := BlockAt(0); got != "0x0" {
		t.Fatalf("BlockAt(0) = %q", got)
	}
}

// An Address is [20]byte, and slog's JSON handler resolves LogValuer — not
// Stringer — so without LogValue below it renders as twenty JSON numbers.
// Observed in the first live run: the line that exists to say which wallet the
// rows describe printed `"wallet":[216,218,107,242,...]`, which is the log
// defeating its own purpose. Same class as the Secret redaction gap found in the
// 2026-08-19 review, and for exactly the same reason.
func TestAnAddressLogsAsHexNotAsBytes(t *testing.T) {
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("poll", "wallet", testUSDC)

	got := buf.String()
	if !strings.Contains(got, testUSDC.String()) {
		t.Fatalf("the address is not in the log line as hex: %s", got)
	}
	if strings.Contains(got, "[131,") {
		t.Fatalf("the address logged as a byte array: %s", got)
	}
}
