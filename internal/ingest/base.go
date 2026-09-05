package ingest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"time"

	"github.com/shopspring/decimal"

	"github.com/Jason-Dorman/funding-carry/internal/db"
)

// ChainReader is the Base chain as this poller needs it.
//
// Declared at the consumer (architecture section 12) so the poll logic is
// testable against a scripted chain rather than a node, and so the four RPC
// methods this system uses are written down in one place that a reviewer can
// check against the JSON-RPC spec.
type ChainReader interface {
	BlockNumber(ctx context.Context) (uint64, error)
	Balance(ctx context.Context, a Address, at BlockTag) (*big.Int, error)
	GasPrice(ctx context.Context) (*big.Int, error)
	CallContract(ctx context.Context, to Address, data []byte, at BlockTag) ([]byte, error)
}

// SpotReference is the Coinbase spot mid, which stands in for the on-chain price
// when the pool read fails — the "Coinbase spot as configured fallback" the Part
// 6 deliverable asks for.
//
// It is an interface for the usual consumer-side reason, but also because the
// implementation is the venue-state sampler in this same package: the mid is
// already in memory from the ticker stream, so the fallback costs no request and
// cannot itself fail over the network.
type SpotReference interface {
	SpotMid(at time.Time) (decimal.Decimal, bool)
}

// BaseAddresses are the four addresses a poll reads.
//
// They are configuration rather than constants because all four are chain-scoped:
// the same system pointed at Base Sepolia (spec section 1, where the Base leg
// starts) needs a different USDC, a different WETH and a different pool, and
// hard-coding mainnet would make the testnet run silently read nothing.
type BaseAddresses struct {
	Wallet Address
	USDC   Address
	WETH   Address
	Pool   Address
}

// Read-only calls this poller makes. Nothing here writes, and nothing here is
// signed: the whole Part 6 surface is four RPC methods and these five calls.
const (
	sigBalanceOf = "balanceOf(address)"
	sigDecimals  = "decimals()"
	sigToken0    = "token0()"
	sigToken1    = "token1()"
	sigSlot0     = "slot0()"
)

// Decimal places for the two native units. ETH is quoted in wei and gas in gwei,
// and both conversions are exact shifts rather than divisions.
const (
	weiPerETH  = 18
	weiPerGwei = 9
)

// BasePoller writes base_state: the spot side of the book — wallet inventory,
// the ETH/USDC reference price, and what gas costs.
//
// It is the one component that reads a chain rather than a venue, and it follows
// the same rule as every other feed: an RPC failure degrades to staleness, never
// to a guess (architecture section 8). Each of the four columns fails
// independently, because they have independent sources; a poll that could read
// nothing at all writes no row, since a missing row is a visible gap and a row
// of NULLs is noise.
//
// There is no backfill. Balances only exist from when polling starts (build plan
// Part 6, architecture section 7.1).
type BasePoller struct {
	chain    ChainReader
	addr     BaseAddresses
	fallback SpotReference
	sink     Sink
	interval time.Duration
	metrics  *BaseMetrics
	log      *slog.Logger
	now      func() time.Time

	// layout and usdcDecimals are read from the chain once and kept. They are
	// properties of two contracts, not of the market, so re-reading them every
	// thirty seconds would be two RPC calls a poll to learn what cannot change.
	layout       *poolLayout
	layoutErr    error
	usdcDecimals int32
	haveDecimals bool

	// warned keeps a repeating failure to one log line per transition rather
	// than one per tick, per column: an RPC endpoint that is refusing is
	// refusing every thirty seconds.
	warned map[string]bool
	// priceless tracks the both-sources-gone state separately, because it is
	// reached from inside an already-warned pool failure and would otherwise be
	// swallowed by warned["spot_px"].
	priceless bool
}

// NewBasePoller builds the poller. fallback may be nil, which leaves spot_px
// NULL when the pool cannot be read.
func NewBasePoller(chain ChainReader, addr BaseAddresses, fallback SpotReference, sink Sink,
	interval time.Duration, m *BaseMetrics, log *slog.Logger, now func() time.Time,
) *BasePoller {
	if now == nil {
		now = time.Now
	}
	return &BasePoller{
		chain: chain, addr: addr, fallback: fallback, sink: sink,
		interval: interval, metrics: m,
		log: log.With("component", "base_poller"), now: now,
		warned: make(map[string]bool, 4),
	}
}

// Run polls on every boundary until ctx is canceled.
//
// The first poll is immediate: a restart should not leave the wallet unobserved
// for a whole interval, and thirty seconds is a long time to be blind to a
// balance a manual carry may have just moved. It is also the only poll whose
// timestamp can collide with one already recorded, which the natural key
// absorbs.
//
// After that it waits for each boundary rather than ticking on a period
// (runOnBoundary), for the reason recorded there: (ts) is this row's identity,
// so a tick landing a hair below its boundary truncates onto the previous one
// and is dropped as a duplicate, taking its own boundary with it.
func (p *BasePoller) Run(ctx context.Context) {
	if err := p.Poll(ctx); err != nil {
		if ctx.Err() == nil {
			p.log.Info("base poller stopping", "reason", err)
		}
		return
	}
	if err := runOnBoundary(ctx, p.interval, p.now, p.Poll); err != nil && ctx.Err() == nil {
		p.log.Info("base poller stopping", "reason", err)
	}
}

// Poll writes one snapshot. It returns an error only when the writer has gone or
// the context has ended; every chain failure is logged and left for the next
// tick.
//
// The row timestamp is the sampling boundary, not the moment the poll ran: (ts)
// is base_state's identity (ADR-0012) and a clock reading would make every row
// unique and the constraint decorative.
func (p *BasePoller) Poll(ctx context.Context) error {
	at := p.now().UTC()

	// One poll gets one interval, total. rpcTimeout bounds a single round trip,
	// and a poll makes up to five of them in sequence — so without this a slow
	// endpoint could keep a 30-second poll running for 75 seconds and skip the
	// boundaries it ran through. Overrunning is not a crash and not a wrong
	// number; it is silent gaps in the series, which is why it is bounded here
	// rather than left to the RPC timeout. Measured on the account poller, whose
	// 5-second cadence has the least headroom: 2 of 128 boundaries.
	ctx, cancel := context.WithTimeout(ctx, p.interval)
	defer cancel()

	// The block height is read first and every other read is pinned to it, so
	// one row is one moment on the chain rather than a smear across three. It is
	// also the only failure that skips the whole row: without a block there is
	// nothing coherent to pin, and reading the columns at "latest" instead would
	// quietly produce exactly the mixed snapshot this avoids.
	block, err := p.chain.BlockNumber(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		p.warn("block_number", err)
		return nil
	}
	p.cleared("block_number")

	row, ok := p.read(ctx, BlockAt(block), at)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if !ok {
		// Nothing was readable. A missing row is what "we were not looking"
		// should read as; a row of NULLs claims an observation that did not
		// happen.
		return nil
	}
	row.TS = at.Truncate(p.interval)
	p.metrics.observed(block)

	return submit(ctx, p.sink, row)
}

// read gathers the four columns at one block, reporting false when none of them
// could be read.
func (p *BasePoller) read(ctx context.Context, tag BlockTag, at time.Time) (db.BaseStateRow, bool) {
	var row db.BaseStateRow
	var observed bool
	if v, ok := p.walletETH(ctx, tag); ok {
		row.WalletETH, observed = v, true
	}
	if v, ok := p.walletUSDC(ctx, tag); ok {
		row.WalletUSDC, observed = v, true
	}
	if v, ok := p.gasGwei(ctx); ok {
		row.GasGwei, observed = v, true
	}
	if v, source, ok := p.spotPx(ctx, tag, at); ok {
		row.SpotPx, row.SpotPxSource, observed = v, source, true
	}
	return row, observed
}

// walletETH reads the native balance, in whole ETH.
func (p *BasePoller) walletETH(ctx context.Context, tag BlockTag) (decimal.NullDecimal, bool) {
	wei, err := p.chain.Balance(ctx, p.addr.Wallet, tag)
	if err != nil {
		p.warn("wallet_eth", err)
		return decimal.NullDecimal{}, false
	}
	p.cleared("wallet_eth")
	return db.Num(decimal.NewFromBigInt(wei, -weiPerETH)), true
}

// walletUSDC reads the ERC-20 balance, in whole USDC.
func (p *BasePoller) walletUSDC(ctx context.Context, tag BlockTag) (decimal.NullDecimal, bool) {
	balance, err := p.usdcBalance(ctx, tag)
	if err != nil {
		p.warn("wallet_usdc", err)
		return decimal.NullDecimal{}, false
	}
	p.cleared("wallet_usdc")
	return db.Num(balance), true
}

func (p *BasePoller) usdcBalance(ctx context.Context, tag BlockTag) (decimal.Decimal, error) {
	scale, err := p.tokenDecimals(ctx, tag)
	if err != nil {
		return decimal.Zero, err
	}
	out, err := p.chain.CallContract(ctx, p.addr.USDC, callData(sigBalanceOf, p.addr.Wallet), tag)
	if err != nil {
		return decimal.Zero, err
	}
	raw, err := uintWord(out, 0)
	if err != nil {
		return decimal.Zero, fmt.Errorf("balanceOf: %w", err)
	}
	return decimal.NewFromBigInt(raw, -scale), nil
}

// gasGwei reads the node's current gas price estimate.
func (p *BasePoller) gasGwei(ctx context.Context) (decimal.NullDecimal, bool) {
	wei, err := p.chain.GasPrice(ctx)
	if err != nil {
		p.warn("gas_gwei", err)
		return decimal.NullDecimal{}, false
	}
	p.cleared("gas_gwei")
	return db.Num(decimal.NewFromBigInt(wei, -weiPerGwei)), true
}

// spotPx is the ETH/USDC reference price: the Base pool's own mid, falling back
// to the Coinbase spot mid when the pool cannot be read.
//
// The source travels with the price rather than beside it, because the two are
// different markets and a row that could not say which one it held would be a
// price whose market is unknowable after the fact. It is recorded twice, for two
// different readers: in the row, for anything reading the series back, and in
// ingest_base_spot_px_source_total, for the alert that notices the fallback
// carrying rows it was never meant to carry.
func (p *BasePoller) spotPx(ctx context.Context, tag BlockTag, at time.Time) (decimal.NullDecimal, string, bool) {
	px, err := p.poolPrice(ctx, tag)
	if err == nil {
		p.cleared("spot_px")
		p.metrics.spot(db.SpotSourceDEX)
		return db.Num(px), db.SpotSourceDEX, true
	}

	mid, haveMid := decimal.Zero, false
	if p.fallback != nil {
		mid, haveMid = p.fallback.SpotMid(at)
	}
	p.warn("spot_px", err, "fallback", fallbackName(haveMid))
	if !haveMid {
		// Both sources gone. This is its own state, not a continuation of the
		// pool failure already warned about: rows stop carrying a price at all,
		// and without a signal here that transition was silent — no new log
		// line, a frozen source counter, and ingest_base_last_block still
		// climbing because the block read is fine.
		p.metrics.spot(spotSourceNone)
		if !p.priceless {
			p.priceless = true
			p.log.Warn("spot_px is NULL: the pool cannot be read and the Coinbase mid is stale or absent",
				"pool_error", err)
		}
		return decimal.NullDecimal{}, "", false
	}
	p.priceless = false
	p.metrics.spot(db.SpotSourceCoinbase)
	return db.Num(mid), db.SpotSourceCoinbase, true
}

func fallbackName(haveMid bool) string {
	if haveMid {
		return db.SpotSourceCoinbase
	}
	return "none; spot_px is NULL"
}

// ---------------------------------------------------------------------------
// The pool
// ---------------------------------------------------------------------------

// poolLayout is which side of the configured pool is ETH, and how the two tokens
// scale. Both are read from the pool itself rather than assumed.
type poolLayout struct {
	ethIsToken0 bool
	dec0, dec1  int32
}

// errPoolMismatch is the configured pool not holding the configured pair.
//
// It is permanent: it can only be a wrong BASE_SPOT_POOL, and no amount of
// waiting fixes a wrong address. It is also the one failure here that would be
// silent if it were not checked — a pool holding some other pair answers slot0
// perfectly happily, and the number it returns would land in spot_px looking
// exactly like a price. So the layout is verified once against the configured
// WETH and USDC, the failure is cached rather than retried, and spot_px falls
// back to Coinbase rather than recording a price for the wrong pair.
var errPoolMismatch = errors.New("configured pool does not hold the configured WETH/USDC pair")

// poolPrice reads slot0 and converts it to ETH priced in USDC.
func (p *BasePoller) poolPrice(ctx context.Context, tag BlockTag) (decimal.Decimal, error) {
	layout, err := p.poolLayout(ctx, tag)
	if err != nil {
		return decimal.Zero, err
	}
	out, err := p.chain.CallContract(ctx, p.addr.Pool, callData(sigSlot0), tag)
	if err != nil {
		return decimal.Zero, err
	}
	sqrtPriceX96, err := uintWord(out, 0)
	if err != nil {
		return decimal.Zero, fmt.Errorf("slot0: %w", err)
	}
	return priceFromSqrtX96(sqrtPriceX96, layout)
}

// poolLayout is the cache in front of resolveLayout. It keeps the answer,
// including when the answer is that the pool is the wrong one — a wrong address
// is wrong every thirty seconds, so retrying it forever would be four RPC calls
// a poll to be told the same thing.
func (p *BasePoller) poolLayout(ctx context.Context, tag BlockTag) (poolLayout, error) {
	switch {
	case p.layout != nil:
		return *p.layout, nil
	case p.layoutErr != nil:
		return poolLayout{}, p.layoutErr
	}
	layout, err := p.resolveLayout(ctx, tag)
	if err != nil {
		return poolLayout{}, err
	}
	p.layout = &layout
	return layout, nil
}

// resolveLayout reads the pool's two tokens and their scales, and checks that
// they are the pair this system was configured to price.
func (p *BasePoller) resolveLayout(ctx context.Context, tag BlockTag) (poolLayout, error) {
	token0, err := p.tokenOf(ctx, sigToken0, tag)
	if err != nil {
		return poolLayout{}, err
	}
	token1, err := p.tokenOf(ctx, sigToken1, tag)
	if err != nil {
		return poolLayout{}, err
	}

	var layout poolLayout
	switch {
	case token0 == p.addr.WETH && token1 == p.addr.USDC:
		layout.ethIsToken0 = true
	case token0 == p.addr.USDC && token1 == p.addr.WETH:
		layout.ethIsToken0 = false
	default:
		return poolLayout{}, p.rejectPool(token0, token1)
	}

	if layout.dec0, err = p.decimalsOf(ctx, token0, tag); err != nil {
		return poolLayout{}, err
	}
	if layout.dec1, err = p.decimalsOf(ctx, token1, tag); err != nil {
		return poolLayout{}, err
	}

	p.log.Info("Base spot pool verified",
		"pool", p.addr.Pool, "token0", token0, "token1", token1,
		"decimals0", layout.dec0, "decimals1", layout.dec1, "eth_is_token0", layout.ethIsToken0)
	return layout, nil
}

// rejectPool records the mismatch permanently and says so once, loudly.
func (p *BasePoller) rejectPool(token0, token1 Address) error {
	p.layoutErr = fmt.Errorf("%w: pool %s holds %s/%s", errPoolMismatch, p.addr.Pool, token0, token1)
	p.log.Error("BASE_SPOT_POOL is not the configured pair; spot_px will not be read from it",
		"pool", p.addr.Pool, "token0", token0, "token1", token1,
		"want_weth", p.addr.WETH, "want_usdc", p.addr.USDC)
	return p.layoutErr
}

// tokenOf calls token0() or token1() on the pool.
func (p *BasePoller) tokenOf(ctx context.Context, sig string, tag BlockTag) (Address, error) {
	out, err := p.chain.CallContract(ctx, p.addr.Pool, callData(sig), tag)
	if err != nil {
		return Address{}, err
	}
	a, err := addressWord(out, 0)
	if err != nil {
		return Address{}, fmt.Errorf("%s: %w", sig, err)
	}
	return a, nil
}

// tokenDecimals is the USDC contract's own decimals, read once.
//
// It is read rather than assumed at six. Six is right for the USDC this system
// holds, but the constant would be wrong for the bridged USDbC beside it on the
// same chain, and a wrong scale is a wallet balance off by orders of magnitude
// that still looks like a plausible number.
func (p *BasePoller) tokenDecimals(ctx context.Context, tag BlockTag) (int32, error) {
	if p.haveDecimals {
		return p.usdcDecimals, nil
	}
	d, err := p.decimalsOf(ctx, p.addr.USDC, tag)
	if err != nil {
		return 0, err
	}
	p.usdcDecimals, p.haveDecimals = d, true
	return d, nil
}

// maxTokenDecimals bounds what decimals() may claim. ERC-20 declares it a uint8,
// so anything past this is a contract that is not a token — and letting it
// through would shift a balance by an arbitrary power of ten.
const maxTokenDecimals = 36

func (p *BasePoller) decimalsOf(ctx context.Context, token Address, tag BlockTag) (int32, error) {
	out, err := p.chain.CallContract(ctx, token, callData(sigDecimals), tag)
	if err != nil {
		return 0, err
	}
	d, err := uintWord(out, 0)
	if err != nil {
		return 0, fmt.Errorf("decimals of %s: %w", token, err)
	}
	// The bound is what makes the narrowing conversion below safe, and it is
	// also the check that matters: a contract that is not a token can answer
	// anything, and an unbounded value would shift a balance by an arbitrary
	// power of ten rather than fail.
	if !d.IsInt64() {
		return 0, fmt.Errorf("decimals of %s: %s is not a token's decimals", token, d)
	}
	n := d.Int64()
	if n < 0 || n > maxTokenDecimals {
		return 0, fmt.Errorf("decimals of %s: %d is not a token's decimals", token, n)
	}
	return int32(n), nil
}

// twoTo192 is 2^192, the scale slot0's sqrt price is fixed-point in.
var twoTo192 = decimal.NewFromBigInt(new(big.Int).Lsh(big.NewInt(1), 192), 0)

// priceFromSqrtX96 converts a Uniswap v3 slot0 sqrt price into ETH priced in
// USDC.
//
// slot0 reports sqrt(token1 per token0) scaled by 2^96, in the tokens' own
// smallest units. Squaring and dividing by 2^192 undoes both, and shifting by
// the difference of the two decimals turns smallest units into whole tokens.
// Which way up that leaves the answer depends on the pool's token ordering,
// which is fixed by address and so is a property of the deployment rather than
// something to assume: the second branch is the same identity inverted, arranged
// so that each case is one division at full precision rather than a division
// followed by a reciprocal.
func priceFromSqrtX96(sqrtPriceX96 *big.Int, l poolLayout) (decimal.Decimal, error) {
	if sqrtPriceX96.Sign() <= 0 {
		return decimal.Zero, errors.New("slot0: sqrt price is zero; the pool is uninitialized")
	}
	squared := new(big.Int).Mul(sqrtPriceX96, sqrtPriceX96)
	if l.ethIsToken0 {
		return decimal.NewFromBigInt(squared, l.dec0-l.dec1).Div(twoTo192), nil
	}
	return twoTo192.Shift(l.dec1 - l.dec0).Div(decimal.NewFromBigInt(squared, 0)), nil
}

// ---------------------------------------------------------------------------
// Degradation
// ---------------------------------------------------------------------------

// warn reports a column going NULL, once per transition into failure.
func (p *BasePoller) warn(what string, err error, attrs ...any) {
	if p.warned[what] {
		return
	}
	// A shutdown fails every read that is in flight, and each one used to report
	// its column going NULL — four warnings per graceful stop, describing a
	// process that was asked to stop rather than a feed that broke.
	if errors.Is(err, context.Canceled) {
		return
	}
	p.warned[what] = true

	// A mismatched pool is the one failure here that waiting cannot fix, and it
	// is not an *RPCError — so the default-to-retryable rule below would have
	// labelled the only permanent failure as worth retrying.
	var rpc *RPCError
	retryable := !errors.Is(err, errPoolMismatch) &&
		(!errors.As(err, &rpc) || rpc.Retryable())
	p.log.Warn("base read failed; the column is going NULL",
		append([]any{"what", what, "error", err, "retryable", retryable}, attrs...)...)
}

// cleared reports a column coming back, so an outage has a visible end as well
// as a visible start.
func (p *BasePoller) cleared(what string) {
	if p.warned[what] {
		delete(p.warned, what)
		p.log.Info("base read recovered", "what", what)
	}
}
