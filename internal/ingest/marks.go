package ingest

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/shopspring/decimal"

	"github.com/Jason-Dorman/funding-carry/internal/db"
)

// Marks builds the three-minute marks the funding estimator consumes.
//
// The venue's definition, followed exactly (venue doc section 3):
//
//	futures_mark = 3-min VWAP of the future
//	             | if no trades, 3-min TWAP of the futures mid
//	             | if no quotes, spot mark + the previous (futures - spot) gap
//	spot_mark    = the same three, with the products exchanged
//
// The fallbacks are the interesting part and they are ordered by how much they
// assume. A VWAP is what traded. A mid TWAP is what was quoted but did not
// trade, which is weaker but still an observation. Carrying the previous gap
// assumes only that the basis moves slowly, and it is the last resort — it is
// how a mark survives a product going quiet without the premium collapsing to
// zero, which is what a missing mark would otherwise imply.
type Marks struct {
	perp, spot string
	interval   time.Duration
	log        *slog.Logger
	now        func() time.Time

	// out receives each completed sample. It is a function rather than a channel
	// so that the backfill can drive the same computation synchronously.
	out func(MarkSample)

	// mu guards everything below: trade buckets arrive on the market_trades
	// stream's goroutine, quotes on the ticker stream's, and the sample runs on
	// this component's own.
	mu      sync.Mutex
	buckets map[string][]bucketMark
	mids    map[string][]midMark
	// gap is the previous (futures - spot), which is what the last-resort
	// fallback carries forward.
	gap          decimal.NullDecimal
	lastBoundary time.Time
}

type bucketMark struct {
	end    time.Time
	vwap   decimal.NullDecimal
	volume decimal.Decimal
}

type midMark struct {
	at  time.Time
	mid decimal.Decimal
}

// markDelay is how long after a boundary the mark for the window that just
// closed is taken.
//
// It exists because two clocks have to agree. A trade bucket is stamped with
// its end but is not closed until lateGrace afterwards, since trade timestamps
// run a little behind the socket. Sampling exactly on the boundary therefore
// asked for a window whose final minute had not been aggregated yet: the mark
// silently dropped a third of its own window, and the more the price moved in
// that last minute the more it dropped. Waiting out lateGrace plus a margin
// makes the bucket the mark needs already closed.
const markDelay = lateGrace + time.Second

// retain is how much history each product keeps: enough to cover one sample
// window with room for late arrivals, and no more. The estimator only ever
// looks back three minutes.
const retain = 3

// NewMarks builds the mark sampler. out is called once per completed sample.
func NewMarks(perp, spot string, out func(MarkSample), log *slog.Logger, now func() time.Time) *Marks {
	if now == nil {
		now = time.Now
	}
	return &Marks{
		perp:     perp,
		spot:     spot,
		interval: SampleInterval,
		log:      log.With("component", "marks"),
		now:      now,
		out:      out,
		buckets:  map[string][]bucketMark{},
		mids:     map[string][]midMark{},
	}
}

// ObserveBucket records a closed trade bucket. Called from the market_trades
// stream when it writes a cb_trades_agg row, so the VWAP the estimator marks
// against is the same number that was persisted.
func (m *Marks) ObserveBucket(row db.TradesAggRow) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.buckets[row.ProductID] = trim(append(m.buckets[row.ProductID], bucketMark{
		end:    row.TS,
		vwap:   row.VWAP,
		volume: row.BuyVol.Add(row.SellVol),
	}), retain*int(m.interval/time.Minute)+2)
}

// ObserveQuote records a mid, which is what the no-trades fallback averages.
func (m *Marks) ObserveQuote(product string, q Quote) {
	mid := midpoint(q)
	if !mid.Valid {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// One sample window's worth of mids at the 5-second cadence, plus slack.
	m.mids[product] = trim(append(m.mids[product], midMark{at: q.At, mid: mid.Decimal}), 64)
}

func trim[T any](s []T, max int) []T {
	if len(s) <= max {
		return s
	}
	return s[len(s)-max:]
}

// Run samples on the three-minute boundary until ctx is canceled.
func (m *Marks) Run(ctx context.Context) {
	for {
		timer := time.NewTimer(untilNextBoundary(m.now(), m.interval) + markDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		m.Sample(m.now())
	}
}

// Sample emits the mark for the window that has just closed, if it can be
// marked at all.
//
// The boundary is the window's end, and the sample describes the three minutes
// before it — so a sample stamped 12:03 is what happened between 12:00 and
// 12:03, which is what RateForHour then files under the hour containing 12:03.
func (m *Marks) Sample(at time.Time) {
	end := at.UTC().Truncate(m.interval)

	m.mu.Lock()
	if !end.After(m.lastBoundary) {
		m.mu.Unlock()
		return
	}
	m.lastBoundary = end
	futures, spot, why := m.markPairLocked(end.Add(-m.interval), end)
	out := m.out
	m.mu.Unlock()

	if !futures.Valid || !spot.Valid {
		// Unmarkable. The hour will come up short on samples and be recorded as
		// a gap rather than being filled in.
		m.log.Debug("window unmarkable", "window_end", end, "why", why)
		return
	}
	if out != nil {
		out(MarkSample{At: end, FuturesMark: futures.Decimal, SpotMark: spot.Decimal})
	}
}

// markPairLocked marks both products over one window, applying the cross-product
// fallback and updating the carried gap. The caller holds mu.
func (m *Marks) markPairLocked(start, end time.Time) (futures, spot decimal.NullDecimal, why string) {
	futures, fSource := m.markLocked(m.perp, start, end)
	spot, sSource := m.markLocked(m.spot, start, end)

	// The last resort: a product with neither trades nor quotes is marked from
	// the other one and the gap they had when both were known. It assumes the
	// basis moves slowly, which is far weaker than assuming it went to zero.
	switch {
	case !futures.Valid && spot.Valid && m.gap.Valid:
		futures = decimal.NullDecimal{Decimal: spot.Decimal.Add(m.gap.Decimal), Valid: true}
		fSource = "carried"
	case !spot.Valid && futures.Valid && m.gap.Valid:
		spot = decimal.NullDecimal{Decimal: futures.Decimal.Sub(m.gap.Decimal), Valid: true}
		sSource = "carried"
	}

	if futures.Valid && spot.Valid {
		m.gap = decimal.NullDecimal{Decimal: futures.Decimal.Sub(spot.Decimal), Valid: true}
	}
	return futures, spot, "futures=" + fSource + " spot=" + sSource
}

// markLocked computes one product's mark over [start, end). The caller holds mu.
// The returned string names which rule produced it, for the log.
func (m *Marks) markLocked(product string, start, end time.Time) (decimal.NullDecimal, string) {
	if v, ok := vwapOver(m.buckets[product], start, end); ok {
		return v, "vwap"
	}
	if v, ok := midTWAPOver(m.mids[product], start, end); ok {
		return v, "mid_twap"
	}
	return decimal.NullDecimal{}, "none"
}

// vwapOver is the volume-weighted price of the buckets closing inside the
// window. A bucket is stamped with its end, so it belongs to this window when
// that end falls inside it.
func vwapOver(buckets []bucketMark, start, end time.Time) (decimal.NullDecimal, bool) {
	notional, volume := decimal.Zero, decimal.Zero
	for _, b := range buckets {
		if !b.vwap.Valid || !b.volume.IsPositive() {
			continue
		}
		if b.end.After(start) && !b.end.After(end) {
			notional = notional.Add(b.vwap.Decimal.Mul(b.volume))
			volume = volume.Add(b.volume)
		}
	}
	if !volume.IsPositive() {
		return decimal.NullDecimal{}, false
	}
	return decimal.NullDecimal{Decimal: notional.Div(volume), Valid: true}, true
}

// midTWAPOver is the mean quoted mid over the window: what the market was
// showing when nothing traded.
func midTWAPOver(mids []midMark, start, end time.Time) (decimal.NullDecimal, bool) {
	sum, n := decimal.Zero, 0
	for _, q := range mids {
		if !q.at.Before(start) && q.at.Before(end) {
			sum = sum.Add(q.mid)
			n++
		}
	}
	if n == 0 {
		return decimal.NullDecimal{}, false
	}
	return decimal.NullDecimal{Decimal: sum.Div(decimal.NewFromInt(int64(n))), Valid: true}, true
}
