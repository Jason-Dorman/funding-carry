package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/shopspring/decimal"

	"github.com/Jason-Dorman/funding-carry/internal/db"
)

const (
	// TradeBucketSecs is the aggregation window. It is part of the row's
	// identity, not just a column, because the funding estimator marks against a
	// 3-minute VWAP (venue doc section 3) and has to know what it is summing:
	// one minute divides into three minutes exactly, so the mark is three whole
	// buckets rather than a re-derivation from raw trades.
	TradeBucketSecs = 60

	// lateGrace is how long a bucket stays open past its end, waiting for trades
	// whose venue timestamp falls inside it but which arrived after the boundary.
	// Buckets are keyed by trade time, and trade time is always a little behind
	// the socket.
	lateGrace = 2 * time.Second

	// sweepWindow is the gap that separates one aggressive burst from the next.
	// A sweep here is a run of two or more trades on the same side within this
	// window of each other: one taker clearing several resting orders, which is
	// what the sweep-intensity feature is looking for. It is deliberately a
	// property of timing and side rather than of size, because size alone cannot
	// tell a large resting fill from an aggressive one.
	sweepWindow = 250 * time.Millisecond

	// minSweepTrades is the run length that counts as a sweep.
	minSweepTrades = 2
)

// TradesHandler aggregates the market_trades channel into fixed buckets.
//
// Raw trades are not persisted: at the perp's rate that is millions of rows a
// month to answer questions — VWAP, imbalance, sweep intensity — that are all
// asked of the aggregate. What is persisted is one row per product per bucket,
// including buckets in which nothing traded, so that a silent minute is
// distinguishable from a minute this process was not listening.
type TradesHandler struct {
	products []string
	interval time.Duration
	// bucketSecs is the interval as the schema stores it. It is derived once
	// here rather than at every close, so the one narrowing conversion in this
	// file happens in a place where the bound is obvious.
	bucketSecs int32
	sink       Sink

	state map[string]*productTrades
}

// productTrades is one product's aggregation state.
type productTrades struct {
	// next is the start of the oldest bucket not yet written. Zero means the
	// product has not been admitted yet: aggregation begins at the next whole
	// boundary, because the bucket in progress when this handler started is
	// missing however much of itself elapsed before then, and a partial
	// aggregate written under a complete bucket's key can never be corrected —
	// ON CONFLICT DO NOTHING would drop the correction.
	next time.Time

	// open holds buckets still accepting trades. In steady state this is one
	// entry, briefly two around a boundary.
	open map[time.Time]*tradeBucket

	// batchMax is the newest trade time of the previous message, which is what
	// trade-time regression is measured against. Within a single message trades
	// arrive newest first, so "older than the one before it" is normal there and
	// only the batch high-water mark carries the signal.
	batchMax time.Time

	// lostFrom is the boundary aggregation had reached when a reconnect
	// discarded the bucket in progress. It is reported as a gap on the next
	// tick; see Reset.
	lostFrom time.Time
}

type tradeBucket struct {
	trades []tradeSample
	seen   map[string]struct{}
}

type tradeSample struct {
	at time.Time
	px decimal.Decimal
	sz decimal.Decimal
	// side is the aggressor. The venue publishes a third value beside BUY and
	// SELL — UNKNOWN_ORDER_SIDE — and such a trade is still a trade: its price
	// and size are on the wire and belong in the count, the VWAP and the largest
	// print. Only the buy/sell split is unknowable, so only the split omits it.
	side aggressor
}

// aggressor is the taker's side, including the case where the venue does not
// say.
type aggressor int

const (
	sideUnknown aggressor = iota
	sideBuy
	sideSell
)

// NewTradesHandler builds the trade aggregator for the given products.
func NewTradesHandler(products []string, interval time.Duration, sink Sink) *TradesHandler {
	h := &TradesHandler{
		products:   products,
		interval:   interval,
		bucketSecs: clampInt32(int(interval / time.Second)),
		sink:       sink,
		state:      make(map[string]*productTrades, len(products)),
	}
	for _, p := range products {
		h.state[p] = &productTrades{open: map[time.Time]*tradeBucket{}}
	}
	return h
}

// Subscribe names the channel and the products this handler wants.
func (h *TradesHandler) Subscribe() (string, []string) { return channelMarketTrades, h.products }

// DataChannel is where trade data arrives; here it matches the subscription.
func (h *TradesHandler) DataChannel() string { return channelMarketTrades }

// Reset drops every in-progress bucket and re-arms admission.
//
// A bucket that was accumulating when the socket dropped is missing the trades
// that happened while it was down, and there is nothing in the aggregate that
// would reveal that. Writing it would put a wrong VWAP under a key that can
// never be rewritten, so the bucket spanning a reconnect is skipped instead and
// shows up as a hole in the series. Refilling those holes from the REST
// market_trades endpoint is Part 5's backfill.
func (h *TradesHandler) Reset() {
	for _, p := range h.state {
		// Remember what the drop cost, so the next tick can report it. None of
		// the stream's three gap detectors can see this one: the sequence check
		// is disarmed by the reconnect itself, this stream's silence threshold is
		// two minutes and a reconnect takes a second, and a discarded bucket
		// leaves no trace in the data. Without this the only signal would be
		// ingest_ws_reconnects_total, which cannot tell a reconnect that lost a
		// row from one that lost nothing because the venue resent a snapshot.
		if !p.next.IsZero() {
			p.lostFrom = p.next
		}
		p.next = time.Time{}
		clear(p.open)
		p.batchMax = time.Time{}
	}
}

// Handle files each trade under its bucket and reports a time regression.
func (h *TradesHandler) Handle(_ context.Context, msg Message) error {
	var events []tradesEvent
	if err := json.Unmarshal(msg.Events, &events); err != nil {
		return fmt.Errorf("decode trade events: %w", err)
	}

	var errs []error
	// batchMax is collected per product across the whole message before being
	// compared, so the regression check sees one high-water mark per message.
	seenMax := map[string]time.Time{}

	for _, e := range events {
		for _, t := range e.Trades {
			p, ok := h.state[t.ProductID]
			if !ok {
				continue
			}
			if err := h.admitTrade(p, t); err != nil {
				errs = append(errs, fmt.Errorf("%s trade %s: %w", t.ProductID, t.TradeID, err))
				continue
			}
			at := t.Time.UTC()
			if at.After(seenMax[t.ProductID]) {
				seenMax[t.ProductID] = at
			}
		}
	}

	return errors.Join(append(errs, h.checkBatchOrder(seenMax)...)...)
}

// checkBatchOrder compares each product's newest trade in this message with the
// newest in the previous one. Within a single message trades arrive newest
// first, so "older than the one before it" is normal there; only the batch
// high-water mark going backwards means this stream is being fed a stale or
// reordered view. The aggregate is still written — the gap is what says not to
// trust it.
func (h *TradesHandler) checkBatchOrder(seenMax map[string]time.Time) []error {
	var errs []error
	for product, high := range seenMax {
		p := h.state[product]
		if !p.batchMax.IsZero() && high.Before(p.batchMax) {
			errs = append(errs, fmt.Errorf("%s: trade time regressed from %s to %s",
				product, p.batchMax.Format(time.RFC3339Nano), high.Format(time.RFC3339Nano)))
		}
		if high.After(p.batchMax) {
			p.batchMax = high
		}
	}
	return errs
}

// admitTrade files one trade under its bucket, dropping the ones that belong to
// a bucket already written or not yet opened.
func (h *TradesHandler) admitTrade(p *productTrades, t tradeWire) error {
	px, err := requireDecimal("price", t.Price)
	if err != nil {
		return err
	}
	sz, err := requireDecimal("size", t.Size)
	if err != nil {
		return err
	}
	side, err := parseAggressor(t.Side)
	if err != nil {
		return err
	}

	at := t.Time.UTC()
	start := at.Truncate(h.interval)
	if p.next.IsZero() || start.Before(p.next) {
		// Before admission, or late for a bucket already written. The history the
		// venue replays on every subscribe lands here, which is the point: it is
		// what makes a re-subscription free of half-filled buckets.
		return nil
	}

	b, ok := p.open[start]
	if !ok {
		b = &tradeBucket{seen: map[string]struct{}{}}
		p.open[start] = b
	}
	// The venue replays recent trades on subscribe and can repeat one across
	// messages; the trade id is what makes a repeat free.
	if _, dup := b.seen[t.TradeID]; dup {
		return nil
	}
	b.seen[t.TradeID] = struct{}{}
	b.trades = append(b.trades, tradeSample{at: at, px: px, sz: sz, side: side})
	return nil
}

// parseAggressor maps the venue's side vocabulary. UNKNOWN_ORDER_SIDE is a value
// the venue really sends — 330 trades in 22 hours of the Part 4 soak, in bursts,
// mostly on the replay after the Friday reopen — so it is a known member of the
// set and not an error. Anything outside the set is: it means the vocabulary
// moved, and a split derived from it can no longer be trusted.
func parseAggressor(side string) (aggressor, error) {
	switch side {
	case "BUY":
		return sideBuy, nil
	case "SELL":
		return sideSell, nil
	case "UNKNOWN_ORDER_SIDE":
		return sideUnknown, nil
	default:
		return sideUnknown, fmt.Errorf("side=%q: outside the venue's vocabulary", side)
	}
}

// Tick closes every bucket whose window has elapsed, and admits products that
// have not started aggregating yet.
//
// Admission happens here rather than on the first trade so that a product which
// has not traded at all still produces rows. Driving it from frame arrival is
// what the heartbeat subscription buys: this runs about once a second on the
// stream's own goroutine, with no timer and no lock.
func (h *TradesHandler) Tick(ctx context.Context, now time.Time) error {
	now = now.UTC()
	var errs []error
	for _, product := range h.products {
		p := h.state[product]
		if p.next.IsZero() {
			p.next = now.Truncate(h.interval).Add(h.interval)
			if !p.lostFrom.IsZero() {
				errs = append(errs, fmt.Errorf("%s: buckets from %s to %s were dropped across a reconnect",
					product, p.lostFrom.Format(time.RFC3339), p.next.Format(time.RFC3339)))
				p.lostFrom = time.Time{}
			}
			continue
		}
		for h.elapsed(p.next, now) {
			if err := h.closeBucket(ctx, product, p); err != nil {
				// A writer that has gone is not a gap; it stops the stream.
				return err
			}
		}
	}
	return errors.Join(errs...)
}

// elapsed reports whether the bucket starting at start is finished, allowing for
// trades that arrive a little after their own boundary.
func (h *TradesHandler) elapsed(start, now time.Time) bool {
	return !start.Add(h.interval).Add(lateGrace).After(now)
}

func (h *TradesHandler) closeBucket(ctx context.Context, product string, p *productTrades) error {
	start := p.next
	b := p.open[start]
	delete(p.open, start)
	p.next = start.Add(h.interval)

	return submit(ctx, h.sink, aggregate(product, start.Add(h.interval), h.bucketSecs, b))
}

// aggregate reduces one bucket to its row. A bucket with no trades is still a
// row: zero volume in a minute this process was listening is an observation, and
// leaving it out would make it indistinguishable from a minute that was missed.
func aggregate(product string, end time.Time, bucketSecs int32, b *tradeBucket) db.TradesAggRow {
	row := db.TradesAggRow{
		TS:         end,
		ProductID:  product,
		BucketSecs: bucketSecs,
		BuyVol:     decimal.Zero,
		SellVol:    decimal.Zero,
	}
	if b == nil || len(b.trades) == 0 {
		row.SweepCount = db.Opt(int32(0))
		return row
	}

	// Sorted by time because the sweep run-length depends on order and the venue
	// delivers each message newest first.
	sort.Slice(b.trades, func(i, j int) bool { return b.trades[i].at.Before(b.trades[j].at) })

	notional, volume, maxSize := decimal.Zero, decimal.Zero, decimal.Zero
	for _, t := range b.trades {
		notional = notional.Add(t.px.Mul(t.sz))
		volume = volume.Add(t.sz)
		if t.sz.GreaterThan(maxSize) {
			maxSize = t.sz
		}
		// An unknown side adds to neither, which is why buy_vol + sell_vol is
		// not guaranteed to equal the bucket's total volume.
		switch t.side {
		case sideBuy:
			row.BuyVol = row.BuyVol.Add(t.sz)
		case sideSell:
			row.SellVol = row.SellVol.Add(t.sz)
		case sideUnknown:
		}
	}

	row.TradeCount = clampInt32(len(b.trades))
	row.MaxSingleSz = db.Num(maxSize)
	if !volume.IsZero() {
		row.VWAP = db.Num(notional.Div(volume))
	}
	row.SweepCount = db.Opt(countSweeps(b.trades))
	return row
}

// clampInt32 narrows to the width the schema's integer columns have. Neither
// caller can realistically reach the ceiling — a bucket is a minute long — but
// a silent wrap would turn a trade count into a negative one, so the bound is
// checked rather than assumed.
func clampInt32(n int) int32 {
	if n > math.MaxInt32 {
		return math.MaxInt32
	}
	if n < math.MinInt32 {
		return math.MinInt32
	}
	return int32(n)
}

// countSweeps counts runs of same-side trades spaced no further apart than
// sweepWindow. Trades must already be in time order.
func countSweeps(trades []tradeSample) int32 {
	var sweeps, run int32
	for i, t := range trades {
		// A run needs a known, matching side: a sweep is one taker clearing
		// several levels, and an unknown side cannot be shown to be the same
		// taker as the print before it.
		continues := i > 0 &&
			t.side != sideUnknown &&
			trades[i-1].side == t.side &&
			t.at.Sub(trades[i-1].at) <= sweepWindow
		if !continues {
			run = 1
			continue
		}
		run++
		if run == minSweepTrades {
			sweeps++
		}
	}
	return sweeps
}
