package ingest

import (
	"context"

	"log/slog"
	"sort"
	"time"

	"github.com/shopspring/decimal"

	"github.com/Jason-Dorman/funding-carry/internal/config"
	"github.com/Jason-Dorman/funding-carry/internal/db"
)

// Backfill reconstructs history the system was not running for.
//
// The plan called for pulling `fundingHistory`. That endpoint does not publish
// for this product — the venue's own rate field comes back empty (venue doc
// section 3) — so there is no funding history to pull. There is, however,
// candle history: the REST endpoint serves one-minute candles for both products
// back to the perp's launch. Since the estimator's only inputs are a futures
// mark and a spot mark, the series can be reconstructed with the venue's own
// formula rather than waited for, which is the difference between the funding
// z-score being usable today and in thirty days.
//
// What it is not is the same measurement. A live mark is a three-minute VWAP of
// trades; a reconstructed one is built from one-minute candles. Same formula,
// coarser inputs, different error profile — so every row it writes is marked
// funding_source='backfilled' and nothing downstream has to guess (ADR-0015).
//
// Every insert is ON CONFLICT DO NOTHING against the table's natural key, which
// gives idempotency for free and settles the precedence question in the right
// direction: a recorded row always beats a reconstructed one, because the
// recorded row is already there when the backfill tries.
type Backfill struct {
	perp, spot  string
	client      *RESTClient
	store       BarStore
	sink        Sink
	maintenance config.MaintenanceWindow
	log         *slog.Logger
}

// BarStore is what the backfill needs to know about what is already recorded.
//
// Declared at the consumer, and it is what turns this from "download forty-five
// days on every start" into "download what is missing". Two things follow from
// having it, and the second matters more than the first:
//
//   - A restart with the history already stored costs one query instead of
//     several hundred HTTP requests, so the automatic gap-filling below is
//     something that can run on every boot rather than something to be avoided.
//   - The reconstruction is computed from the STORED bars, so it is a function
//     of the database rather than of whatever a particular download returned.
//     That is what makes it reproducible: anyone can recompute the series from
//     cb_bars and get the same answer. It also makes it strictly more complete,
//     because stored bars accumulate across runs while any single fetch is only
//     as good as that one call.
type BarStore interface {
	ReadBars(ctx context.Context, product, tf string, from, to time.Time) ([]db.StoredBar, error)
	ReadFundingHours(ctx context.Context, product string, from, to time.Time) ([]time.Time, error)
	LatestFundingRate(ctx context.Context, product string, before time.Time) (decimal.Decimal, time.Time, bool, error)
}

// NewBackfill builds the backfiller. store may be nil, in which case every run
// downloads the whole window and computes from what it downloaded.
func NewBackfill(perp, spot string, client *RESTClient, store BarStore, sink Sink,
	w config.MaintenanceWindow, log *slog.Logger,
) *Backfill {
	return &Backfill{
		perp: perp, spot: spot, client: client, store: store, sink: sink,
		maintenance: w, log: log.With("component", "backfill"),
	}
}

// Result reports what one run recovered.
//
// Fetched counts only the bars this run downloaded; Bars counts everything it
// computed from, stored and fetched together. A healthy steady-state restart has
// Fetched at or near zero and Bars at the full window.
//
// Last is the newest hour reconstructed. It is what the live runner seeds its
// smoothing from: without it the first live hour has no previous rate and is
// published as its own raw premium, which both overstates it and starts the
// recursion again from nothing after every restart.
type Result struct {
	Bars     int
	Fetched  int
	Gaps     int
	Hours    int
	Skipped  int
	From, To time.Time
	Last     Funding
	HaveLast bool
}

// Run brings the stored history up to date over `window` ending at `end`.
//
// It reads what is already there, downloads only the ranges that are missing,
// and reconstructs the funding series from the two together. A restart with
// nothing missing does no network work at all, which is what makes it safe to
// run unconditionally on every container start.
func (b *Backfill) Run(ctx context.Context, end time.Time, window time.Duration) (Result, error) {
	start := end.Add(-window).Truncate(time.Hour)
	end = end.Truncate(time.Hour)
	res := Result{From: start, To: end}

	perpBars, spotBars, err := b.gather(ctx, start, end, &res)
	if err != nil {
		return res, err
	}
	res.Bars = len(perpBars) + len(spotBars)

	series, skipped, err := b.funding(ctx, perpBars, spotBars, start, end)
	res.Hours, res.Skipped = len(series), skipped
	switch {
	case len(series) > 0:
		res.Last, res.HaveLast = series[len(series)-1], true
	case b.store != nil:
		// Nothing new was computed, which is the ordinary restart: the history is
		// already complete. The live runner still needs a rate to smooth its
		// first hour against, so it comes from the newest one recorded rather
		// than from this run's output.
		rate, hour, ok, rerr := b.store.LatestFundingRate(ctx, b.perp, end)
		if rerr != nil {
			return res, rerr
		}
		if ok {
			res.Last, res.HaveLast = Funding{HourStart: hour, Rate: rate}, true
		}
	}
	return res, err
}

// gather assembles the fullest candle set available: what is stored, plus what
// has to be downloaded to fill the holes in it.
func (b *Backfill) gather(ctx context.Context, start, end time.Time, res *Result) (perp, spot map[time.Time]Candle, err error) {
	perp, err = b.stored(ctx, b.perp, start, end)
	if err != nil {
		return nil, nil, err
	}
	spot, err = b.stored(ctx, b.spot, start, end)
	if err != nil {
		return nil, nil, err
	}

	// Coverage is judged on the SPOT product and applied to both. Spot trades
	// every minute, so a minute missing there is a range that was never
	// downloaded. The perp is sparse — a minute missing there usually means
	// nobody traded — so absence proves nothing about coverage and cannot drive
	// the decision. Both products are then fetched over whatever ranges spot
	// says are unexplored.
	gaps := missingRanges(spot, start, end)
	res.Gaps = len(gaps)
	if len(gaps) == 0 {
		b.log.Info("history already complete; nothing to download",
			"perp_bars", len(perp), "spot_bars", len(spot), "from", start, "to", end)
		return perp, spot, nil
	}
	b.log.Info("filling gaps in stored history",
		"ranges", len(gaps), "perp_bars", len(perp), "spot_bars", len(spot),
		"first_gap", gaps[0].from, "last_gap", gaps[len(gaps)-1].to)

	for _, g := range gaps {
		for product, into := range map[string]map[time.Time]Candle{b.perp: perp, b.spot: spot} {
			fetched, ferr := b.bars(ctx, product, g.from, g.to)
			if ferr != nil {
				return nil, nil, ferr
			}
			for at, c := range fetched {
				if _, have := into[at]; !have {
					res.Fetched++
				}
				into[at] = c
				if serr := submit(ctx, b.sink, c.Row(product, OneMinute)); serr != nil {
					return nil, nil, serr
				}
			}
		}
	}
	return perp, spot, nil
}

// stored loads the bars already recorded, keyed by candle start.
func (b *Backfill) stored(ctx context.Context, product string, start, end time.Time) (map[time.Time]Candle, error) {
	out := map[time.Time]Candle{}
	if b.store == nil {
		return out, nil
	}
	// cb_bars.ts is the bar's close, so the window is shifted by one interval to
	// select the bars whose candles START inside it.
	bars, err := b.store.ReadBars(ctx, product, OneMinute.TF(),
		start.Add(OneMinute.Interval()), end.Add(OneMinute.Interval()))
	if err != nil {
		return nil, err
	}
	for _, s := range bars {
		at := s.TS.Add(-OneMinute.Interval())
		out[at] = Candle{Start: at, Open: s.Open, High: s.High, Low: s.Low, Close: s.Close, Volume: s.Volume}
	}
	return out, nil
}

// timeRange is one half-open span that has to be downloaded.
type timeRange struct{ from, to time.Time }

// missingRanges finds the minutes with no stored candle and coalesces them into
// spans, so that a thousand scattered holes do not become a thousand requests.
func missingRanges(have map[time.Time]Candle, start, end time.Time) []timeRange {
	var out []timeRange
	var open *timeRange

	for at := start; at.Before(end); at = at.Add(OneMinute.Interval()) {
		if _, ok := have[at]; ok {
			if open != nil {
				out = append(out, *open)
				open = nil
			}
			continue
		}
		if open == nil {
			open = &timeRange{from: at}
		}
		// The end is exclusive and always one interval past the last missing
		// minute, so a single missing minute is still a fetchable span.
		open.to = at.Add(OneMinute.Interval())
	}
	if open != nil {
		out = append(out, *open)
	}
	return out
}

// bars pages backwards through the candle endpoint, which returns at most
// MaxCandlesPerRequest per call.
//
// Candles are keyed by their start rather than accumulated positionally,
// because a window with no trades yields no candle at all — the perp skips
// whole minutes overnight — so the response is not a dense array.
func (b *Backfill) bars(ctx context.Context, product string, start, end time.Time) (map[time.Time]Candle, error) {
	out := map[time.Time]Candle{}
	page := time.Duration(MaxCandlesPerRequest) * OneMinute.Interval()

	for to := end; to.After(start); to = to.Add(-page) {
		from := to.Add(-page)
		if from.Before(start) {
			from = start
		}
		candles, err := b.client.Candles(ctx, product, from, to, OneMinute)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			// A page that will not load is a hole in the reconstruction, not a
			// reason to abandon the rest of it.
			b.log.Warn("candle page failed", "product", product, "from", from, "to", to, "error", err)
			continue
		}
		for _, c := range candles {
			out[c.Start] = c
		}
	}
	b.log.Info("candles fetched", "product", product, "candles", len(out), "from", start, "to", end)
	return out, nil
}

// funding reconstructs the hourly series from the bars just written.
func (b *Backfill) funding(ctx context.Context, perp, spot map[time.Time]Candle, start, end time.Time) ([]Funding, int, error) {
	samples := MarkSamplesFromCandles(perp, spot)
	if len(samples) == 0 {
		b.log.Warn("no mark samples could be built; nothing to reconstruct")
		return nil, 0, nil
	}

	hours, recorded, err := b.hoursToCompute(ctx, start, end)
	if err != nil {
		return nil, 0, err
	}
	if len(hours) == 0 {
		b.log.Info("funding series already complete", "recorded_hours", recorded)
		return nil, 0, nil
	}

	series, skipped := Series(hours, samples, decimal.NullDecimal{})
	for _, f := range series {
		row := AccrualRow(b.perp, f, db.FundingSourceBackfilled, decimal.NullDecimal{})
		if err := submit(ctx, b.sink, row); err != nil {
			return series, len(skipped), err
		}
	}
	b.log.Info("funding reconstructed",
		"hours", len(series), "unmarkable", len(skipped),
		"from", start, "to", end)
	return series, len(skipped), nil
}

// hoursToCompute lists the hours in the window that still need a rate, and how
// many already had one.
//
// An hour already recorded is skipped: the natural key would discard the repeat
// anyway, and re-deriving it would burn the arithmetic for nothing. The
// maintenance hour is skipped for a different reason — the venue publishes no
// rate for an hour it was shut, so filling it would invent the one hour a week
// that is definitionally absent.
func (b *Backfill) hoursToCompute(ctx context.Context, start, end time.Time) ([]time.Time, int, error) {
	done := map[time.Time]struct{}{}
	if b.store != nil {
		recorded, err := b.store.ReadFundingHours(ctx, b.perp, start, end)
		if err != nil {
			return nil, 0, err
		}
		for _, h := range recorded {
			done[h] = struct{}{}
		}
	}

	var hours []time.Time
	for h := start; h.Before(end); h = h.Add(time.Hour) {
		if b.maintenance.Contains(h) {
			continue
		}
		if _, ok := done[h]; ok {
			continue
		}
		hours = append(hours, h)
	}
	return hours, len(done), nil
}

// MarkSamplesFromCandles builds three-minute marks out of one-minute candles.
//
// A candle's contribution is its close weighted by its volume, which is the
// closest thing to a VWAP that one-minute OHLCV supports: the true VWAP needs
// trade-level data, and asking the trades endpoint for thirteen months of
// history is not a thing it will do. A three-minute window is marked only when
// **both** products have volume in it — a mark for one and not the other would
// produce a premium against a price that was not observed.
//
// This coarsening is exactly why these rows are 'backfilled' and not 'computed'.
func MarkSamplesFromCandles(perp, spot map[time.Time]Candle) []MarkSample {
	windows := map[time.Time]struct{}{}
	for s := range perp {
		windows[s.Truncate(SampleInterval)] = struct{}{}
	}

	out := make([]MarkSample, 0, len(windows))
	for _, w := range sortedSet(windows) {
		f, okF := candleMark(perp, w)
		s, okS := candleMark(spot, w)
		if !okF || !okS {
			continue
		}
		// The sample is stamped with the window's end, matching the live path,
		// so an hour's twenty samples are the same twenty either way.
		out = append(out, MarkSample{At: w.Add(SampleInterval), FuturesMark: f, SpotMark: s})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
}

// candleMark is the volume-weighted close of the candles inside one window.
func candleMark(bars map[time.Time]Candle, windowStart time.Time) (decimal.Decimal, bool) {
	notional, volume := decimal.Zero, decimal.Zero
	for i := range int(SampleInterval / OneMinute.Interval()) {
		c, ok := bars[windowStart.Add(time.Duration(i)*OneMinute.Interval())]
		if !ok || !c.Volume.IsPositive() {
			continue
		}
		notional = notional.Add(c.Close.Mul(c.Volume))
		volume = volume.Add(c.Volume)
	}
	if !volume.IsPositive() {
		return decimal.Decimal{}, false
	}
	return notional.Div(volume), true
}

func sortedSet(m map[time.Time]struct{}) []time.Time {
	out := make([]time.Time, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Before(out[j]) })
	return out
}
