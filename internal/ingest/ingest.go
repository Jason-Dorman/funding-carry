package ingest

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/Jason-Dorman/funding-carry/internal/config"
)

// Silence thresholds: how long each stream's own channel may go quiet before a
// gap is recorded. They differ because "quiet" means something different on each
// channel, and a single threshold would either miss a dead ticker or cry gap
// every time the perp went a minute without a print.
//
// The read deadline is a separate mechanism and is uniform: heartbeats keep
// every socket talking once a second, so a read that times out means the
// connection is dead, whatever the market is doing.
const (
	tickerSilence = 30 * time.Second
	// Measured, not assumed. Over the first thirty minutes of the Part 4 soak
	// the perp book was fresh in 95% of ten-second samples but went quiet for as
	// long as **51 seconds** three times: a nano contract's book genuinely stops
	// changing when its market makers are idle. Thirty seconds — the first guess
	// — counted two false gaps in half an hour and, through the book-staleness
	// bound derived from it, punched holes in cb_book_snapshots during perfectly
	// healthy periods. Two minutes clears the observed quiet by better than 2x
	// and still notices a dead subscription well inside the window that matters;
	// FeedStale reads last_seen at sixty seconds and is the faster signal anyway.
	level2Silence = Level2Quiet
	// Trades have real droughts. Two minutes is long enough not to fire on a
	// quiet overnight stretch, and short enough that the 60-second buckets going
	// empty for two in a row is noticed.
	tradesSilence = 2 * time.Minute
	// Two candle intervals: one missed close is a gap.
	candlesSilence = 2 * CandleInterval
	// Zero, deliberately. The status channel speaks when the product's state
	// changes and is silent for days otherwise, so silence carries no
	// information and a threshold would only produce noise.
	statusSilence = 0

	// Level2Quiet is how long the perp book may go without an update before this
	// binary stops believing it. It bounds two things that must agree: the gap
	// threshold above, and how stale a book may be and still be snapshotted
	// (see maxBookAge). Splitting them is how a feed ends up counted as broken
	// while its rows keep being written, or the reverse.
	Level2Quiet = 2 * time.Minute
)

// Options configures the WebSocket ingest.
type Options struct {
	PerpProduct string
	SpotProduct string

	// SampleInterval is the cadence of cb_venue_state rows (POLL_REST_SECS).
	// It is shared with the REST poller in Part 5 on purpose: both feed one
	// sampler, so both must agree on where a boundary is.
	SampleInterval time.Duration

	// BookSnapInterval is BOOK_SNAP_SECS.
	BookSnapInterval time.Duration

	Maintenance config.MaintenanceWindow

	// Dialer opens each stream's socket. Every stream gets its own connection,
	// so a level2 snapshot storm cannot delay a ticker frame and one channel's
	// failure reconnects one channel.
	Dialer Dialer

	// RESTClient enables the Part 5 poller, the funding estimator and the
	// backfill. Nil runs the WebSocket half alone, which is what Part 4 was.
	RESTClient *RESTClient

	// Account enables cb_account_state polling. Nil when no CDP credential is
	// configured: the public stack must start and run without one, so the
	// account half is absent rather than broken.
	Account AccountSource

	// Chain enables the Base wallet poller (Part 6). Nil runs without it, which
	// is what the stack does with no BASE_RPC_URL or no wallet address
	// configured: base_state goes unwritten rather than half-written.
	Chain ChainReader

	// BaseAddresses are the wallet, the two tokens and the pool the Base poller
	// reads. Ignored when Chain is nil.
	BaseAddresses BaseAddresses

	// BaseInterval is POLL_BASE_SECS. It is its own cadence rather than the
	// sampler's because base_state is keyed on (ts) alone and has no boundary to
	// share with anything.
	BaseInterval time.Duration

	// Backfill, when positive, is how far back to reconstruct history on
	// startup. Zero skips it.
	//
	// It runs on every start and costs nothing when there is nothing to do: the
	// backfiller reads what is already stored and downloads only the ranges that
	// are missing. That is what makes it safe to leave on — a container that has
	// been down for an hour recovers that hour by itself, and one that restarts
	// twice in a minute does no network work the second time.
	Backfill time.Duration

	// BarStore lets the backfill see what is already recorded. Nil makes every
	// run download the whole window.
	BarStore BarStore

	// Backoff and now are overridden by tests; production leaves them zero.
	Backoff Backoff
	now     func() time.Time
}

// Ingest is the WebSocket half of cmd/ingest: five streams and the venue-state
// sampler they feed.
type Ingest struct {
	streams  []*Stream
	state    *VenueState
	marks    *Marks
	funding  *FundingRunner
	poller   *Poller
	account  *AccountPoller
	base     *BasePoller
	backfill *Backfill
	window   time.Duration
	log      *slog.Logger
}

// New builds the streams. It starts nothing; Run does that.
func New(opts Options, sink Sink, m *Metrics, log *slog.Logger) *Ingest {
	now := opts.now
	if now == nil {
		now = time.Now
	}

	state := NewVenueState(opts.PerpProduct, opts.SpotProduct, opts.SampleInterval,
		opts.Maintenance, sink, log, now)

	// Both products on the channels where the spot reference carries information:
	// the quote the basis is measured against, its trades (the spot mark the
	// funding estimator needs is a VWAP of them), and its candles. level2 and
	// status are perp-only — the spot leg executes on Base, not here, and the
	// spot market does not close.
	both := []string{opts.PerpProduct, opts.SpotProduct}

	in := &Ingest{state: state, log: log, window: opts.Backfill}

	// The Part 5 chain, built only when there is a REST client to feed it:
	// marks -> funding -> the venue-state sampler and the funding ledger.
	if opts.RESTClient != nil {
		in.funding = NewFundingRunner(opts.PerpProduct, opts.Maintenance, state, sink, log, now)
		in.marks = NewMarks(opts.PerpProduct, opts.SpotProduct, func(s MarkSample) {
			state.ObserveMarks(opts.PerpProduct, s.FuturesMark, s.SpotMark, s.At)
			in.funding.Observe(s)
		}, log, now)
		in.poller = NewPoller(opts.PerpProduct, opts.RESTClient, state, sink, opts.SampleInterval, log, now)
		if opts.Backfill > 0 {
			in.backfill = NewBackfill(opts.PerpProduct, opts.SpotProduct, opts.RESTClient,
				opts.BarStore, sink, opts.Maintenance, log)
		}
	}
	if opts.Account != nil {
		in.account = NewAccountPoller(opts.Account, opts.PerpProduct, sink, opts.SampleInterval, log, now)
	}
	// The venue-state sampler is the poller's spot fallback: the Coinbase mid it
	// already holds costs no request, so a failing pool read degrades to a price
	// from a different market rather than to no price at all.
	if opts.Chain != nil {
		in.base = NewBasePoller(opts.Chain, opts.BaseAddresses, state, sink,
			opts.BaseInterval, m.Base(), log, now)
	}

	specs := []struct {
		name    string
		handler Handler
		silence time.Duration
	}{
		{channelTicker, NewTickerHandler(both, state, in.marks), tickerSilence},
		{channelLevel2, NewLevel2Handler(opts.PerpProduct, opts.BookSnapInterval, sink), level2Silence},
		{channelMarketTrades, NewTradesHandler(both, TradeBucketSecs*time.Second, sink, in.marks), tradesSilence},
		{channelCandles, NewCandlesHandler(both, sink), candlesSilence},
		{channelStatus, NewStatusHandler(opts.PerpProduct, state, log), statusSilence},
	}

	for _, s := range specs {
		in.streams = append(in.streams, NewStream(s.name, s.handler, opts.Dialer, m.Stream(s.name), log,
			StreamOptions{Silence: s.silence, Backoff: opts.Backoff, now: now}))
	}
	return in
}

// Run starts every stream and the sampler, and returns once they have all
// stopped — which happens only when ctx is canceled. Nothing here returns an
// error: a feed degrades to staleness (architecture section 8), and the failure
// that is fatal to this binary is the writer's, reported by whoever runs it.
//
// Returning only after the last goroutine has stopped is what lets the caller
// close the writer safely: the writer's drain assumes its producers are done.
func (in *Ingest) Run(ctx context.Context) {
	in.runBackfill(ctx)
	if ctx.Err() != nil {
		return
	}

	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		in.state.Run(ctx)
	}()

	for _, r := range []struct {
		name string
		run  func(context.Context)
	}{
		{"marks", runOf(in.marks)},
		{"funding", runOf(in.funding)},
		{"rest_poller", runOf(in.poller)},
		{"account_poller", runOf(in.account)},
		{"base_poller", runOf(in.base)},
	} {
		if r.run == nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.run(ctx)
		}()
	}

	for _, s := range in.streams {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Run(ctx)
		}()
	}

	in.log.Info("ingest streams started", "streams", in.names())
	wg.Wait()
	in.log.Info("ingest streams stopped")
}

func (in *Ingest) names() []string {
	names := make([]string, len(in.streams))
	for i, s := range in.streams {
		names[i] = s.Name()
	}
	return names
}

// runBackfill recovers history before the live streams start, and hands the
// newest reconstructed rate to the funding runner.
//
// It blocks on purpose: it is a one-shot recovery, and letting the live streams
// start first would only race it for the same rows. Every insert is idempotent,
// so a recorded row beats a reconstructed one whichever order they arrive in —
// this is about not doing the work twice, not about correctness.
func (in *Ingest) runBackfill(ctx context.Context) {
	if in.backfill == nil {
		return
	}
	res, err := in.backfill.Run(ctx, time.Now().UTC(), in.window)
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		in.log.Warn("backfill incomplete", "error", err, "bars", res.Bars, "hours", res.Hours)
	} else {
		in.log.Info("backfill complete",
			"bars_considered", res.Bars, "bars_downloaded", res.Fetched,
			"gap_ranges", res.Gaps, "funding_hours", res.Hours,
			"unmarkable_hours", res.Skipped, "from", res.From, "to", res.To)
	}

	// Seed the smoothing so the first live hour continues the series instead of
	// starting it again from its own raw premium.
	if res.HaveLast && in.funding != nil {
		in.funding.Seed(res.Last.Rate, res.Last.HourStart)
		in.log.Info("funding smoothing seeded from the backfill",
			"hour", res.Last.HourStart, "rate", res.Last.Rate.StringFixed(10))
	}
}

// runner is anything with a Run loop bound to the root context.
type runner interface{ Run(ctx context.Context) }

// runOf returns the Run method of a possibly-nil component. A nil typed pointer
// in an interface is not a nil interface, so the nil-ness has to be checked on
// the concrete value before it is wrapped — the classic Go trap, and the reason
// this helper exists rather than a plain interface field.
func runOf[T runner](v T) func(context.Context) {
	var zero T
	if any(v) == any(zero) {
		return nil
	}
	return v.Run
}
