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

	// Backoff and now are overridden by tests; production leaves them zero.
	Backoff Backoff
	now     func() time.Time
}

// Ingest is the WebSocket half of cmd/ingest: five streams and the venue-state
// sampler they feed.
type Ingest struct {
	streams []*Stream
	state   *VenueState
	log     *slog.Logger
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

	specs := []struct {
		name    string
		handler Handler
		silence time.Duration
	}{
		{channelTicker, NewTickerHandler(both, state), tickerSilence},
		{channelLevel2, NewLevel2Handler(opts.PerpProduct, opts.BookSnapInterval, sink), level2Silence},
		{channelMarketTrades, NewTradesHandler(both, TradeBucketSecs*time.Second, sink), tradesSilence},
		{channelCandles, NewCandlesHandler(both, sink), candlesSilence},
		{channelStatus, NewStatusHandler(opts.PerpProduct, state, log), statusSilence},
	}

	in := &Ingest{state: state, log: log}
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
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		in.state.Run(ctx)
	}()

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
