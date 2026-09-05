package ingest

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/shopspring/decimal"

	"github.com/Jason-Dorman/funding-carry/internal/db"
)

// Base mainnet addresses, which are also the configured defaults. Using the real
// ones keeps the fixtures honest about which side of the pool ETH is on: WETH
// sorts below USDC by address, so token0 is WETH, and a test that invented
// addresses could get that backwards without noticing.
var (
	testWallet = mustAddress("0x1111111111111111111111111111111111111111")
	testUSDC   = mustAddress("0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913")
	testWETH   = mustAddress("0x4200000000000000000000000000000000000006")
	testPool   = mustAddress("0xd0b53D9277642d899DF5C87A3966A349A798F224")
)

func mustAddress(s string) Address {
	a, err := ParseAddress(s)
	if err != nil {
		panic(err)
	}
	return a
}

func testAddresses() BaseAddresses {
	return BaseAddresses{Wallet: testWallet, USDC: testUSDC, WETH: testWETH, Pool: testPool}
}

const pollBaseEvery = 30 * time.Second

// ---------------------------------------------------------------------------
// A scripted chain
// ---------------------------------------------------------------------------

func uint256Word(n *big.Int) []byte {
	b := make([]byte, wordSize)
	n.FillBytes(b)
	return b
}

func addressWordBytes(a Address) []byte {
	b := make([]byte, wordSize)
	copy(b[wordSize-len(a):], a[:])
	return b
}

type chainCall struct {
	to     Address
	sig    string
	tag    BlockTag
	arg    Address // the single address argument, when the call has one
	hasArg bool
}

// fakeChain answers the four RPC methods from a script and records what it was
// asked. Like every fake in this package it honours the context it is handed: a
// fake that ignored cancellation would make a shutdown test pass over a real
// leak.
type fakeChain struct {
	mu sync.Mutex

	block    uint64
	blockErr error

	balance    *big.Int
	balanceErr error

	gas    *big.Int
	gasErr error

	// calls is keyed by contract address and selector.
	calls   map[string][]byte
	callErr map[string]error

	seen []chainCall
}

func newFakeChain() *fakeChain {
	c := &fakeChain{
		block:   0x2160ec0,
		balance: big.NewInt(100000000000000000), // 0.1 ETH
		gas:     big.NewInt(10000000),           // 0.01 gwei, a plausible Base fee
		calls:   map[string][]byte{},
		callErr: map[string]error{},
	}
	c.respond(testUSDC, sigDecimals, uint256Word(big.NewInt(6)))
	c.respond(testWETH, sigDecimals, uint256Word(big.NewInt(18)))
	c.respond(testPool, sigToken0, addressWordBytes(testWETH))
	c.respond(testPool, sigToken1, addressWordBytes(testUSDC))
	c.respond(testPool, sigSlot0, uint256Word(sqrtPriceFor2500))
	c.respond(testUSDC, sigBalanceOf, uint256Word(big.NewInt(1234560000))) // 1234.56 USDC
	return c
}

func callKey(to Address, sig string) string {
	return to.String() + ":" + sig
}

func (c *fakeChain) respond(to Address, sig string, out []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls[callKey(to, sig)] = out
}

func (c *fakeChain) fail(to Address, sig string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.callErr[callKey(to, sig)] = err
}

func (c *fakeChain) BlockNumber(ctx context.Context) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.block, c.blockErr
}

func (c *fakeChain) Balance(ctx context.Context, a Address, at BlockTag) (*big.Int, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seen = append(c.seen, chainCall{to: a, sig: "eth_getBalance", tag: at})
	return c.balance, c.balanceErr
}

func (c *fakeChain) GasPrice(ctx context.Context) (*big.Int, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gas, c.gasErr
}

func (c *fakeChain) CallContract(ctx context.Context, to Address, data []byte, at BlockTag) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	sig, ok := c.signatureOf(data)
	if !ok {
		return nil, fmt.Errorf("unscripted selector 0x%s", hex.EncodeToString(data[:4]))
	}
	// The ARGUMENT is recorded, not just the callee. Without it, swapping
	// p.addr.Wallet for p.addr.Pool in the balanceOf call passed every test in
	// this file while recording the pool's balance as the wallet's — an
	// adversarial review's finding, and the reason a fake has to enforce
	// everything the real client does.
	call := chainCall{to: to, sig: sig, tag: at}
	if len(data) >= 4+wordSize {
		if a, err := addressWord(data[4:], 0); err == nil {
			call.arg = a
			call.hasArg = true
		}
	}
	c.seen = append(c.seen, call)

	key := callKey(to, sig)
	if err := c.callErr[key]; err != nil {
		return nil, err
	}
	out, ok := c.calls[key]
	if !ok {
		return nil, fmt.Errorf("unscripted call %s", key)
	}
	return out, nil
}

// signatureOf reverses a selector back to the signature that produced it, so the
// recorded calls read as names rather than as hex.
func (c *fakeChain) signatureOf(data []byte) (string, bool) {
	for _, sig := range []string{sigBalanceOf, sigDecimals, sigToken0, sigToken1, sigSlot0} {
		if len(data) >= 4 && string(data[:4]) == string(selector(sig)) {
			return sig, true
		}
	}
	return "", false
}

func (c *fakeChain) calledWith(sig string) []chainCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []chainCall
	for _, call := range c.seen {
		if call.sig == sig {
			out = append(out, call)
		}
	}
	return out
}

// sqrtPriceFor2500 puts the pool at 2500 USDC per ETH.
//
// slot0 reports sqrt(token1 per token0) in smallest units, scaled by 2^96. At
// 2500 USDC (6 decimals) per ETH (18 decimals) the raw ratio is 2500e-12, whose
// square root is exactly 5e-5, so the fixed-point value is 2^96/20000 — floored
// to an integer here, which moves the recovered price by about one part in
// 10^25 and so is invisible at any precision this system keeps.
var sqrtPriceFor2500 = new(big.Int).Div(new(big.Int).Lsh(big.NewInt(1), 96), big.NewInt(20000))

func newBasePoller(t *testing.T, chain ChainReader, fallback SpotReference, sink Sink, clock *fakeClock) *BasePoller {
	t.Helper()
	return NewBasePoller(chain, testAddresses(), fallback, sink, pollBaseEvery,
		newBaseMetrics(prometheus.NewRegistry()), testLogger(), clock.now)
}

func baseRows(t *testing.T, sink *fakeSink) []db.BaseStateRow {
	t.Helper()
	var rows []db.BaseStateRow
	for _, r := range sink.collected() {
		if row, ok := r.(db.BaseStateRow); ok {
			rows = append(rows, row)
		}
	}
	return rows
}

func onlyBaseRow(t *testing.T, sink *fakeSink) db.BaseStateRow {
	t.Helper()
	rows := baseRows(t, sink)
	if len(rows) != 1 {
		t.Fatalf("want exactly one base_state row, got %d", len(rows))
	}
	return rows[0]
}

func wantNum(t *testing.T, got decimal.NullDecimal, want, what string) {
	t.Helper()
	if !got.Valid {
		t.Fatalf("%s is NULL, want %s", what, want)
	}
	if got.Decimal.String() != want {
		t.Fatalf("%s = %s, want %s", what, got.Decimal, want)
	}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestBasePollWritesTheWholeRow(t *testing.T) {
	sink := &fakeSink{}
	clock := newClock(epoch.Add(17 * time.Second))
	p := newBasePoller(t, newFakeChain(), nil, sink, clock)

	if err := p.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}

	row := onlyBaseRow(t, sink)
	// The row is stamped on the sampling boundary, not at the clock reading:
	// (ts) is base_state's identity, and a clock reading would make every row
	// unique and the constraint decorative.
	if want := epoch; !row.TS.Equal(want) {
		t.Fatalf("ts = %s, want %s", row.TS, want)
	}
	wantNum(t, row.WalletETH, "0.1", "wallet_eth")
	wantNum(t, row.WalletUSDC, "1234.56", "wallet_usdc")
	wantNum(t, row.GasGwei, "0.01", "gas_gwei")
	wantNum(t, row.SpotPx, "2500", "spot_px")
	// The source travels with the price: the two markets are different, and a
	// row that could not say which one it holds is a price whose market is
	// unknowable after the fact.
	if row.SpotPxSource != db.SpotSourceDEX {
		t.Fatalf("spot_px_source = %q, want %q", row.SpotPxSource, db.SpotSourceDEX)
	}
}

// Every balance and price in a row is read at one block, so the row is one
// moment on the chain rather than a smear across three: a wallet read before a
// swap and a price read after it would reconcile against nothing.
func TestEveryReadIsPinnedToTheSameBlock(t *testing.T) {
	chain := newFakeChain()
	sink := &fakeSink{}
	p := newBasePoller(t, chain, nil, sink, newClock(epoch))

	if err := p.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}

	want := BlockAt(chain.block)
	if want == LatestBlock {
		t.Fatal("the fixture is not exercising a pinned tag")
	}
	chain.mu.Lock()
	defer chain.mu.Unlock()
	if len(chain.seen) == 0 {
		t.Fatal("no reads were recorded")
	}
	for _, call := range chain.seen {
		if call.tag != want {
			t.Fatalf("%s on %s read at %q, want %q", call.sig, call.to, call.tag, want)
		}
	}
}

// Each column has its own source, so each fails on its own. A gas price the node
// will not quote is not a reason to throw away a wallet balance that was read
// perfectly well.
func TestAColumnThatCannotBeReadGoesNullAndTheRestSurvive(t *testing.T) {
	chain := newFakeChain()
	chain.gasErr = errors.New("node refused eth_gasPrice")
	sink := &fakeSink{}
	p := newBasePoller(t, chain, nil, sink, newClock(epoch))

	if err := p.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}

	row := onlyBaseRow(t, sink)
	if row.GasGwei.Valid {
		t.Fatalf("gas_gwei = %s, want NULL", row.GasGwei.Decimal)
	}
	wantNum(t, row.WalletETH, "0.1", "wallet_eth")
	wantNum(t, row.SpotPx, "2500", "spot_px")
}

// A missing row is what "we were not looking" should read as. A row of NULLs
// claims an observation that did not happen, and it is the kind of row a
// coverage query counts as present.
func TestAPollThatCanReadNothingWritesNoRow(t *testing.T) {
	chain := newFakeChain()
	chain.balanceErr = errors.New("down")
	chain.gasErr = errors.New("down")
	chain.fail(testUSDC, sigBalanceOf, errors.New("down"))
	chain.fail(testPool, sigSlot0, errors.New("down"))
	sink := &fakeSink{}
	p := newBasePoller(t, chain, nil, sink, newClock(epoch))

	if err := p.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if rows := baseRows(t, sink); len(rows) != 0 {
		t.Fatalf("wrote %d rows with nothing readable: %+v", len(rows), rows)
	}
}

// Without a block height there is nothing coherent to pin the other reads to.
// Falling back to "latest" would silently produce the mixed snapshot the pin
// exists to prevent, so the whole row is skipped instead.
func TestABlockNumberFailureSkipsTheRowEntirely(t *testing.T) {
	chain := newFakeChain()
	chain.blockErr = errors.New("node is syncing")
	sink := &fakeSink{}
	p := newBasePoller(t, chain, nil, sink, newClock(epoch))

	if err := p.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if rows := baseRows(t, sink); len(rows) != 0 {
		t.Fatalf("wrote %d rows without a block: %+v", len(rows), rows)
	}
	if calls := chain.calledWith(sigSlot0); len(calls) != 0 {
		t.Fatalf("read the pool anyway: %+v", calls)
	}
}

type fixedSpot struct {
	px decimal.Decimal
	ok bool
}

func (f fixedSpot) SpotMid(time.Time) (decimal.Decimal, bool) { return f.px, f.ok }

func TestSpotPriceFallsBackToTheCoinbaseMid(t *testing.T) {
	chain := newFakeChain()
	chain.fail(testPool, sigSlot0, errors.New("pool read failed"))
	sink := &fakeSink{}
	p := newBasePoller(t, chain, fixedSpot{px: decimal.RequireFromString("2499.5"), ok: true},
		sink, newClock(epoch))

	if err := p.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	row := onlyBaseRow(t, sink)
	wantNum(t, row.SpotPx, "2499.5", "spot_px")
	if row.SpotPxSource != db.SpotSourceCoinbase {
		t.Fatalf("spot_px_source = %q, want %q", row.SpotPxSource, db.SpotSourceCoinbase)
	}
}

// The fallback is a courtesy, not a licence to invent a price. A stale Coinbase
// mid is refused by the sampler, and spot_px is then NULL — the honest answer —
// while the columns that were readable still land.
func TestSpotPriceIsNullWhenNeitherSourceCanAnswer(t *testing.T) {
	chain := newFakeChain()
	chain.fail(testPool, sigSlot0, errors.New("pool read failed"))
	sink := &fakeSink{}
	p := newBasePoller(t, chain, fixedSpot{}, sink, newClock(epoch))

	if err := p.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	row := onlyBaseRow(t, sink)
	if row.SpotPx.Valid {
		t.Fatalf("spot_px = %s, want NULL", row.SpotPx.Decimal)
	}
	// The schema pairs the two, so a NULL price must leave a NULL source: the
	// migration's CHECK would reject the row otherwise.
	if row.SpotPxSource != "" {
		t.Fatalf("spot_px_source = %q with a NULL price", row.SpotPxSource)
	}
	wantNum(t, row.WalletETH, "0.1", "wallet_eth")
}

// A pool holding some other pair answers slot0 perfectly happily, and the number
// it returns would land in spot_px looking exactly like a price. This is the one
// failure in this file that is silent unless it is checked for.
func TestAPoolHoldingTheWrongPairIsNeverPriced(t *testing.T) {
	chain := newFakeChain()
	other := mustAddress("0x2222222222222222222222222222222222222222")
	chain.respond(testPool, sigToken1, addressWordBytes(other))
	// The impostor answers decimals() and the pool answers slot0, exactly as a
	// real pool of some other pair would. Nothing about this call sequence fails
	// on its own; only the token check stands between it and a price.
	chain.respond(other, sigDecimals, uint256Word(big.NewInt(18)))
	sink := &fakeSink{}
	p := newBasePoller(t, chain, fixedSpot{px: decimal.RequireFromString("2499.5"), ok: true},
		sink, newClock(epoch))

	if err := p.Poll(context.Background()); err != nil {
		t.Fatalf("first poll: %v", err)
	}
	// The price came from Coinbase, not from a pool holding the wrong pair, and
	// the row says so.
	row := onlyBaseRow(t, sink)
	wantNum(t, row.SpotPx, "2499.5", "spot_px")
	if row.SpotPxSource != db.SpotSourceCoinbase {
		t.Fatalf("spot_px_source = %q, want %q", row.SpotPxSource, db.SpotSourceCoinbase)
	}
	if calls := chain.calledWith(sigSlot0); len(calls) != 0 {
		t.Fatalf("priced a mismatched pool: %+v", calls)
	}

	// The verdict is permanent: a wrong address is wrong every thirty seconds,
	// so the pool is not re-read on the next poll.
	if err := p.Poll(context.Background()); err != nil {
		t.Fatalf("second poll: %v", err)
	}
	if calls := chain.calledWith(sigToken0); len(calls) != 1 {
		t.Fatalf("token0 was read %d times, want 1", len(calls))
	}
}

// The pool's tokens and their decimals are properties of two contracts, not of
// the market. Re-reading them every poll would be four RPC calls a minute to
// learn what cannot change.
func TestPoolMetadataIsReadOnce(t *testing.T) {
	chain := newFakeChain()
	sink := &fakeSink{}
	p := newBasePoller(t, chain, nil, sink, newClock(epoch))

	for i := range 3 {
		if err := p.Poll(context.Background()); err != nil {
			t.Fatalf("poll %d: %v", i, err)
		}
	}

	for _, sig := range []string{sigToken0, sigToken1, sigDecimals} {
		if got := len(chain.calledWith(sig)); sig == sigDecimals {
			// Three: USDC for the wallet scale, then token0 and token1 for the
			// pool layout. All on the first poll.
			if got != 3 {
				t.Fatalf("%s was read %d times, want 3", sig, got)
			}
		} else if got != 1 {
			t.Fatalf("%s was read %d times, want 1", sig, got)
		}
	}
	if got := len(chain.calledWith(sigSlot0)); got != 3 {
		t.Fatalf("slot0 was read %d times, want one per poll", got)
	}
}

// USDC's scale is read from the contract rather than assumed at six. Six is
// right for the native USDC this system holds and wrong for the bridged USDbC
// beside it on the same chain, and a wrong scale is a balance off by orders of
// magnitude that still looks like a plausible number.
func TestTheTokenScaleComesFromTheContract(t *testing.T) {
	chain := newFakeChain()
	chain.respond(testUSDC, sigDecimals, uint256Word(big.NewInt(18)))
	sink := &fakeSink{}
	p := newBasePoller(t, chain, nil, sink, newClock(epoch))

	if err := p.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	wantNum(t, onlyBaseRow(t, sink).WalletUSDC, "0.00000000123456", "wallet_usdc")
}

func TestAnImplausibleTokenScaleIsRefused(t *testing.T) {
	chain := newFakeChain()
	chain.respond(testUSDC, sigDecimals, uint256Word(big.NewInt(maxTokenDecimals+1)))
	sink := &fakeSink{}
	p := newBasePoller(t, chain, nil, sink, newClock(epoch))

	if err := p.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if row := onlyBaseRow(t, sink); row.WalletUSDC.Valid {
		t.Fatalf("wallet_usdc = %s, want NULL", row.WalletUSDC.Decimal)
	}
}

func TestPriceFromSqrtX96(t *testing.T) {
	for _, tc := range []struct {
		name   string
		sqrt   *big.Int
		layout poolLayout
		want   string
	}{
		{
			// sqrt = 2^96 means a raw ratio of exactly 1 token1-unit per
			// token0-unit. With 18 decimals against 6, one wei of ETH buys one
			// millionth of a USDC, so an ETH is 10^12 USDC.
			name:   "eth is token0",
			sqrt:   new(big.Int).Lsh(big.NewInt(1), 96),
			layout: poolLayout{ethIsToken0: true, dec0: 18, dec1: 6},
			want:   "1000000000000",
		},
		{
			// The same pool with its tokens the other way round has to give the
			// same answer, which is what makes the inversion branch a check
			// rather than a restatement.
			name:   "eth is token1",
			sqrt:   new(big.Int).Lsh(big.NewInt(1), 96),
			layout: poolLayout{ethIsToken0: false, dec0: 6, dec1: 18},
			want:   "1000000000000",
		},
		{
			// Doubling the sqrt price quadruples the price.
			name:   "the sqrt is squared",
			sqrt:   new(big.Int).Lsh(big.NewInt(1), 97),
			layout: poolLayout{ethIsToken0: true, dec0: 18, dec1: 18},
			want:   "4",
		},
		{
			name:   "a realistic pool",
			sqrt:   sqrtPriceFor2500,
			layout: poolLayout{ethIsToken0: true, dec0: 18, dec1: 6},
			want:   "2500",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := priceFromSqrtX96(tc.sqrt, tc.layout)
			if err != nil {
				t.Fatalf("price: %v", err)
			}
			if got.String() != tc.want {
				t.Fatalf("price = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestAnUninitializedPoolIsNotAPriceOfZero(t *testing.T) {
	if _, err := priceFromSqrtX96(big.NewInt(0), poolLayout{ethIsToken0: true, dec0: 18, dec1: 6}); err == nil {
		t.Fatal("a zero sqrt price was accepted")
	}
}

// The poller stops when the writer has gone: there is nothing left to record
// into, and continuing to poll would be requests against a venue for rows that
// have nowhere to land.
func TestBasePollerStopsWhenTheWriterIsGone(t *testing.T) {
	sink := &fakeSink{}
	sink.stop()
	p := newBasePoller(t, newFakeChain(), nil, sink, newClock(epoch))

	err := p.Poll(context.Background())
	if !errors.Is(err, errWriterGone) {
		t.Fatalf("poll returned %v, want %v", err, errWriterGone)
	}
}

func TestBasePollerRunStopsOnCancellation(t *testing.T) {
	sink := &fakeSink{}
	p := newBasePoller(t, newFakeChain(), nil, sink, newClock(epoch))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.Run(ctx)
	}()

	// Run polls immediately, so a row proves the loop is up before it is asked
	// to stop.
	deadline := time.After(5 * time.Second)
	for len(baseRows(t, sink)) == 0 {
		select {
		case <-deadline:
			t.Fatal("the first poll never happened")
		default:
		}
	}
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return on cancellation")
	}
}

// A refusing endpoint refuses every thirty seconds. One log line per transition
// into failure, and one when it comes back — not one per tick, which is what
// turns an outage into a log nobody reads.
func TestAFailingReadIsReportedOnceAndItsRecoveryIsReportedToo(t *testing.T) {
	chain := newFakeChain()
	chain.gasErr = errors.New("node refused eth_gasPrice")

	var log bytes.Buffer
	p := NewBasePoller(chain, testAddresses(), nil, &fakeSink{}, pollBaseEvery,
		newBaseMetrics(prometheus.NewRegistry()),
		slog.New(slog.NewJSONHandler(&log, nil)), newClock(epoch).now)

	for range 3 {
		if err := p.Poll(context.Background()); err != nil {
			t.Fatalf("poll: %v", err)
		}
	}
	if got := strings.Count(log.String(), "base read failed"); got != 1 {
		t.Fatalf("three failing polls produced %d warnings, want 1", got)
	}

	chain.mu.Lock()
	chain.gasErr = nil
	chain.mu.Unlock()
	if err := p.Poll(context.Background()); err != nil {
		t.Fatalf("poll after recovery: %v", err)
	}
	if got := strings.Count(log.String(), "base read recovered"); got != 1 {
		t.Fatalf("recovery was reported %d times, want 1", got)
	}

	// And the failure is reportable again, so a flapping endpoint is not silent
	// after its first outage.
	chain.mu.Lock()
	chain.gasErr = errors.New("gone again")
	chain.mu.Unlock()
	if err := p.Poll(context.Background()); err != nil {
		t.Fatalf("poll after the second failure: %v", err)
	}
	if got := strings.Count(log.String(), "base read failed"); got != 2 {
		t.Fatalf("the second outage was reported %d times in total, want 2", got)
	}
}

// Both balances must be read for the WALLET, and nothing else.
//
// Neither read was constrained by any test: the fake keyed its script on the
// callee, so the eth_call's `to` was pinned, but the balanceOf ARGUMENT and the
// eth_getBalance address were free. Swapping either for the pool address left
// the whole suite green while base_state recorded the pool's holdings as the
// wallet's — large, plausible, monotonic numbers that Part 18's treasury
// reconciliation would then balance against the wrong account.
func TestBothBalancesAreReadForTheWallet(t *testing.T) {
	chain := newFakeChain()
	p := newBasePoller(t, chain, nil, &fakeSink{}, newClock(epoch))

	if err := p.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}

	native := chain.calledWith("eth_getBalance")
	if len(native) != 1 {
		t.Fatalf("eth_getBalance called %d times, want 1", len(native))
	}
	if native[0].to != testWallet {
		t.Errorf("native balance read for %s, want the wallet %s", native[0].to, testWallet)
	}

	erc20 := chain.calledWith(sigBalanceOf)
	if len(erc20) != 1 {
		t.Fatalf("balanceOf called %d times, want 1", len(erc20))
	}
	if erc20[0].to != testUSDC {
		t.Errorf("balanceOf called on %s, want the USDC contract %s", erc20[0].to, testUSDC)
	}
	if !erc20[0].hasArg {
		t.Fatal("balanceOf carried no address argument")
	}
	if erc20[0].arg != testWallet {
		t.Errorf("balanceOf asked about %s, want the wallet %s", erc20[0].arg, testWallet)
	}
}

// A row with no price at all is its own event and must say so.
//
// It is reached from inside an already-warned pool failure, so the warn-once map
// swallowed it: the pool fails (one warning, rows carry the Coinbase mid), then
// hours later the ticker goes quiet and every row's spot_px goes NULL — the
// reference price the spot leg is marked against — with no new log line, a
// frozen source counter, and ingest_base_last_block still climbing because the
// block read is fine. Nothing alerted.
func TestLosingBothPriceSourcesIsAnnounced(t *testing.T) {
	chain := newFakeChain()
	chain.fail(testPool, sigSlot0, errors.New("pool read failed"))
	fallback := &togglingSpot{px: decimal.RequireFromString("2499.5"), ok: true}

	var log bytes.Buffer
	reg := prometheus.NewRegistry()
	p := NewBasePoller(chain, testAddresses(), fallback, &fakeSink{}, pollBaseEvery,
		newBaseMetrics(reg), slog.New(slog.NewJSONHandler(&log, nil)), newClock(epoch).now)

	if err := p.Poll(context.Background()); err != nil {
		t.Fatalf("poll on the fallback: %v", err)
	}
	if strings.Contains(log.String(), "spot_px is NULL") {
		t.Fatal("announced a missing price while the fallback was still answering")
	}

	fallback.ok = false
	if err := p.Poll(context.Background()); err != nil {
		t.Fatalf("poll with both sources gone: %v", err)
	}
	if !strings.Contains(log.String(), "spot_px is NULL") {
		t.Fatalf("losing both price sources was silent:\n%s", log.String())
	}
	if got := testutil.ToFloat64(p.metrics.spotSource.WithLabelValues(spotSourceNone)); got != 1 {
		t.Errorf("the none-source counter is %v, want 1", got)
	}
}

type togglingSpot struct {
	px decimal.Decimal
	ok bool
}

func (s *togglingSpot) SpotMid(time.Time) (decimal.Decimal, bool) { return s.px, s.ok }
