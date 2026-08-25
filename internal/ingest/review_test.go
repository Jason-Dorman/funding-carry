package ingest

import (
	"context"

	"github.com/prometheus/client_golang/prometheus"

	"testing"
	"time"

	"github.com/Jason-Dorman/funding-carry/internal/coinbase"
	"github.com/Jason-Dorman/funding-carry/internal/db"
)

// Regression tests from the adversarial review of 2026-08-20. Each one was run
// against the unfixed code and observed to fail there before its fix was made.

// A book that stops being updated must stop being persisted.
//
// Tick is driven by frame arrival, and every connection carries heartbeats, so a
// level2 channel that goes silent while the socket stays up leaves Tick firing
// once a second against a book frozen at the moment the updates stopped. Every
// boundary then writes a cb_book_snapshots row whose best bid, depth arrays,
// imbalance and impact prices are all the same stale book — presented as a fresh
// observation of the market at that timestamp. It is the same failure the
// venue-state sampler already refuses to commit (see maxQuoteAge): a missing row
// is a visible gap, a repeated row is a lie.
func TestBookSnapshotsStopWhenTheBookStopsUpdating(t *testing.T) {
	sink := &fakeSink{}
	const interval = 10 * time.Second
	h := NewLevel2Handler(testPerp, interval, sink)
	ctx := context.Background()

	if err := h.Handle(ctx, message(t, channelLevel2Data, epoch, l2Frame(testPerp, "snapshot",
		lvl("bid", "100", "2"),
		lvl("offer", "101", "1"),
	))); err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	// Boundaries keep arriving — heartbeats guarantee it — but no l2_data does.
	for i := 1; i <= 40; i++ {
		if err := h.Tick(ctx, epoch.Add(time.Duration(i)*interval)); err != nil {
			t.Fatalf("tick %d: %v", i, err)
		}
	}

	rows := bookSnapshots(t, sink)
	if len(rows) == 0 {
		t.Fatal("no snapshot was written at all")
	}
	last := rows[len(rows)-1].TS
	if cutoff := epoch.Add(maxBookAge); last.After(cutoff) {
		t.Errorf("wrote a snapshot at %s from a book last updated at %s: %d rows of a frozen book "+
			"presented as fresh observations (newest allowed %s)", last, epoch, len(rows), cutoff)
	}
}

// A quote going stale must not take the maintenance flag down with it.
//
// maintenance_window does not come from the ticker. It comes from the configured
// venue calendar and from the status stream, which is a separate connection with
// its own health. Gating the whole row on quote freshness drops the one
// observation the column exists to record, at exactly the moment it matters: a
// halt is precisely when the ticker is most likely to have stopped too.
func TestMaintenanceIsRecordedEvenWithoutAFreshQuote(t *testing.T) {
	sink := &fakeSink{}
	v := newSampler(t, sink)

	quotedAt := epoch
	v.Observe(testPerp, quote(t, "99.5", "100.5", "100", quotedAt))
	v.ObserveStatus(testPerp, false) // the venue says the product is not tradable

	at := quotedAt.Add(time.Duration(maxQuoteAge+1) * sampleEvery)
	if err := v.sample(context.Background(), at); err != nil {
		t.Fatalf("sample: %v", err)
	}

	rows := venueRows(t, sink)
	row, ok := rows[testPerp]
	if !ok {
		t.Fatal("no cb_venue_state row for the perp: a halt that also stopped the ticker went unrecorded")
	}
	if !row.MaintenanceWindow {
		t.Error("maintenance_window = false while the venue reported the product offline")
	}
	// The stale quote is still not written. NULL is "not observed"; a carried
	// forward mid is a frozen price the basis would be computed from.
	if row.Mid.Valid {
		t.Errorf("mid = %v, want NULL: the quote behind it is stale", row.Mid)
	}
	// The spot product has no maintenance flag of its own and no fresh quote, so
	// it has nothing to say and must produce no row.
	if _, ok := rows[testSpot]; ok {
		t.Error("wrote a spot row with nothing observable in it")
	}
}

// mid is a midpoint or it is NULL.
//
// Falling back to the ticker's last trade price puts a print into a column
// documented as a mid, with no marker of the substitution: spread_bps goes NULL
// beside it, which is a hint and not a statement. The freshness rule cannot
// catch it either, because it measures the age of the ticker message, not the
// age of the trade the price came from.
func TestMidIsNullOnAOneSidedBook(t *testing.T) {
	sink := &fakeSink{}
	v := newSampler(t, sink)

	v.Observe(testPerp, quote(t, "", "100.5", "100.25", epoch))
	if err := v.sample(context.Background(), epoch); err != nil {
		t.Fatalf("sample: %v", err)
	}

	row := venueRows(t, sink)[testPerp]
	if row.Mid.Valid {
		t.Errorf("mid = %v, want NULL: only one side was quoted, so there is no midpoint", row.Mid)
	}
	// A spread needs both sides. Reporting the half-spread, or zero, would be a
	// tighter market than the one that exists.
	if row.SpreadBps.Valid {
		t.Errorf("spread_bps = %v, want NULL with only one side quoted", row.SpreadBps)
	}
}

// Two samples inside one boundary must produce one row.
//
// The row's identity is (product_id, ts) and every insert is ON CONFLICT DO
// NOTHING, so a second row on a boundary already written is discarded by the
// database — silently, and counted as a conflict, which is also what a healthy
// retry looks like. Level2Handler.Tick guards exactly this; the sampler did not,
// so ticker jitter could hand the fresher observation to a boundary that was
// already spoken for.
func TestVenueStateWritesOneRowPerBoundary(t *testing.T) {
	sink := &fakeSink{}
	v := newSampler(t, sink)
	ctx := context.Background()

	v.Observe(testPerp, quote(t, "99.5", "100.5", "100", epoch))
	if err := v.sample(ctx, epoch.Add(time.Second)); err != nil {
		t.Fatalf("first sample: %v", err)
	}
	// A tick that arrives late enough to land in the same boundary bucket.
	v.Observe(testPerp, quote(t, "99", "101", "100", epoch.Add(3*time.Second)))
	if err := v.sample(ctx, epoch.Add(4*time.Second)); err != nil {
		t.Fatalf("second sample: %v", err)
	}

	if rows := sink.collected(); len(rows) != 1 {
		t.Fatalf("rows = %d, want 1 per boundary: the second would be dropped by the database "+
			"and counted as a conflict", len(rows))
	}
}

// The trade bucket lost across a reconnect must be counted as a gap.
//
// Reset discards the bucket that was open when the socket dropped, so at least
// one cb_trades_agg row per product is never written. None of the three gap
// detectors sees it: the sequence check is disarmed by the reconnect, the
// silence threshold for this stream is two minutes and a reconnect takes a
// second, and Reset itself reported nothing. The only signal was
// ingest_ws_reconnects_total, which cannot distinguish this from a level2
// reconnect that loses nothing because the venue resends a full snapshot.
func TestADroppedTradeBucketIsReportedAsAGap(t *testing.T) {
	sink := &fakeSink{}
	h := admittedHandler(t, sink, testPerp)
	ctx := context.Background()

	if err := h.Handle(ctx, message(t, channelMarketTrades, epoch, tradesFrame(
		trade("1", testPerp, "100", "5", "BUY", epoch.Add(time.Second)),
	))); err != nil {
		t.Fatalf("handle: %v", err)
	}

	h.Reset()

	// The first tick of the new connection re-admits, and must say what the
	// reconnect cost. The stream turns a handler error into ingest_ws_gaps_total.
	err := h.Tick(ctx, epoch.Add(30*time.Second))
	if err == nil {
		t.Fatal("a trade bucket was discarded across a reconnect and no gap was reported")
	}
	// It is a report, not a failure: aggregation must carry on.
	if err2 := h.Tick(ctx, epoch.Add(2*time.Minute+lateGrace)); err2 != nil {
		t.Fatalf("the gap report stopped aggregation: %v", err2)
	}
	if rows := aggRows(t, sink); len(rows) == 0 {
		t.Fatal("no bucket was written after the reconnect")
	}
}

// The Tick path needs the same writer-gone escape the Handle path has.
//
// Deleting the errors.Is check from dispatch's Tick branch left the whole suite
// green: TestStreamStopsWhenTheWriterIsGone drives the error through Handle
// only. With the escape gone and the writer dead, the level2 and market_trades
// streams reconnect and tick forever, incrementing ingest_ws_gaps_total once a
// second against a writer that accepts nothing — a permanently rising gap rate
// that reads as a venue problem.
func TestStreamStopsWhenTheWriterIsGoneOnTheTickPath(t *testing.T) {
	conn := &scriptedConn{frames: [][]byte{heartbeat(t, 0, epoch)}}
	dialer := &scriptedDialer{conns: []*scriptedConn{conn}, dialed: make(chan struct{}, 4)}
	h := &fakeHandler{
		channel: channelTicker, data: channelTicker,
		ticked:  make(chan struct{}, 4),
		tickErr: errWriterGone,
	}

	s := NewStream("ticker", tickingHandler{h}, dialer, &fakeRecorder{}, testLogger(),
		StreamOptions{Backoff: fastBackoff, now: newClock(epoch).now})

	// No cancellation: the stream must return on its own.
	done := make(chan struct{})
	go func() { defer close(done); s.Run(context.Background()) }()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stream kept ticking after the writer stopped")
	}
}

// A candle must be persisted when a later one is observed, not when a later one
// happens to share a message with it.
//
// Found by the 1-hour soak, not by reading: cb_bars gained one row in thirty
// minutes where it should have gained twelve. Measuring the channel then
// explained it — the venue sends a 100-candle `snapshot` on subscribe and then
// `update` messages carrying exactly one candle, the one currently forming. The
// original rule looked for a newer candle inside the same message, so on the
// five-minute roll the update carried only the new candle, the one that had just
// closed was never seen again, and it was never written. Nothing downstream
// could tell: cb_bars just quietly stopped growing.
//
// The unit tests missed it because every fixture message carried several
// candles, which is the shape of a snapshot and not of an update, and the
// 150-second live test missed it because it never crossed a boundary.
func TestACandleIsPersistedWhenTheNextOneAppearsInALaterMessage(t *testing.T) {
	sink := &fakeSink{}
	h := NewCandlesHandler([]string{testPerp}, sink)
	ctx := context.Background()

	first, second, third := epoch, epoch.Add(CandleInterval), epoch.Add(2*CandleInterval)

	// Subscribe snapshot: two candles, the second still forming.
	if err := h.Handle(ctx, message(t, channelCandles, epoch, candlesFrame(
		candleAt(testPerp, first, "101"),
		candleAt(testPerp, second, "102"),
	))); err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	// Updates to the forming candle, one candle per message, as the venue sends
	// them. Its close moves as it forms.
	for _, px := range []string{"103", "104"} {
		if err := h.Handle(ctx, message(t, channelCandles, epoch, candlesFrame(
			candleAt(testPerp, second, px),
		))); err != nil {
			t.Fatalf("update: %v", err)
		}
	}

	// The roll: the first message of the new candle, carrying only the new one.
	if err := h.Handle(ctx, message(t, channelCandles, epoch, candlesFrame(
		candleAt(testPerp, third, "105"),
	))); err != nil {
		t.Fatalf("roll: %v", err)
	}

	rows := barRows(t, sink)
	if len(rows) != 2 {
		t.Fatalf("bars = %d, want 2: the candle that closed on the roll was never written", len(rows))
	}
	if want := second.Add(CandleInterval); !rows[1].TS.Equal(want) {
		t.Fatalf("second bar ts = %s, want %s", rows[1].TS, want)
	}
	// And it carries the last values it was seen with, not the ones it happened
	// to have in the message that first mentioned it.
	if got := rows[1].Close.String(); got != "104" {
		t.Errorf("close = %s, want 104 (the final update), not the value from the snapshot", got)
	}
}

// A trade whose aggressor side the venue does not know is still a trade.
//
// The live feed sends side="UNKNOWN_ORDER_SIDE" — 330 trades over 22 hours of
// the Part 4 soak, in bursts, most of them on the replay that follows the
// Friday reopen. Rejecting the whole trade dropped its price and size along with
// its side, so trade_count, vwap and max_single_sz all understated the market
// for that bucket, and the loss showed up only as a gap counter moving.
//
// Only the buy/sell split is unknowable. Everything else about the trade is on
// the wire.
func TestATradeWithAnUnknownSideStillCounts(t *testing.T) {
	sink := &fakeSink{}
	h := admittedHandler(t, sink, testPerp)
	ctx := context.Background()

	if err := h.Handle(ctx, message(t, channelMarketTrades, epoch, tradesFrame(
		trade("1", testPerp, "100", "2", "BUY", epoch.Add(time.Second)),
		trade("2", testPerp, "110", "6", "UNKNOWN_ORDER_SIDE", epoch.Add(2*time.Second)),
	))); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if err := h.Tick(ctx, epoch.Add(time.Minute+lateGrace)); err != nil {
		t.Fatalf("tick: %v", err)
	}

	rows := aggRows(t, sink)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	row := rows[0]
	if row.TradeCount != 2 {
		t.Errorf("trade_count = %d, want 2: the unknown-side trade happened", row.TradeCount)
	}
	// (100*2 + 110*6) / 8 = 860/8
	if got := row.VWAP.Decimal.String(); got != "107.5" {
		t.Errorf("vwap = %s, want 107.5 — the unknown-side trade's price and size are on the wire", got)
	}
	if got := row.MaxSingleSz.Decimal.String(); got != "6" {
		t.Errorf("max_single_sz = %s, want 6", got)
	}
	// The split is the only thing that cannot be known, so it alone omits it.
	if got := row.BuyVol.String(); got != "2" {
		t.Errorf("buy_vol = %s, want 2", got)
	}
	if got := row.SellVol.String(); got != "0" {
		t.Errorf("sell_vol = %s, want 0: an unknown side is not a sell", got)
	}
}

// A side the venue has never sent is still an error, because it means the
// vocabulary moved and the split can no longer be trusted.
func TestATradeWithAnUnrecognisedSideIsStillReported(t *testing.T) {
	sink := &fakeSink{}
	h := admittedHandler(t, sink, testPerp)

	err := h.Handle(context.Background(), message(t, channelMarketTrades, epoch, tradesFrame(
		trade("1", testPerp, "100", "1", "buy", epoch.Add(time.Second)),
	)))
	if err == nil {
		t.Fatal("a side outside the venue's vocabulary was accepted silently")
	}
}

// Every sampling boundary must produce exactly one row.
//
// Observed in production, not in a test: over 19 hours the sampler skipped 914
// of its 5-second boundaries — always exactly one at a time, 6.4% of the series.
// The cause was scheduling, not sampling. A free-running time.Ticker holds its
// period but not its phase relative to the wall clock, and the boundary was
// derived by truncating whatever time the tick happened to arrive at. With the
// phase sitting near a boundary edge, jitter of a millisecond was enough for a
// tick to land just *below* the boundary it was meant for, truncate to the
// previous one, be rejected as a duplicate, and take that boundary with it.
//
// Waiting for the boundary itself rather than for a period removes the class:
// a timer fires at or after its deadline, never before.
func TestSamplerVisitsEveryBoundary(t *testing.T) {
	const interval = 5 * time.Second
	start := epoch.Add(1500 * time.Millisecond)

	// Worst case for the old scheme: a phase that sits a hair under the
	// boundary, with jitter either side of it.
	jitter := []time.Duration{0, 1200 * time.Microsecond, -0, 800 * time.Microsecond, 0}

	now := start
	var seen []time.Time
	for i := range 200 {
		now = now.Add(untilNextBoundary(now, interval))
		// The timer fires at or after the deadline; model both.
		now = now.Add(jitter[i%len(jitter)])
		seen = append(seen, now.Truncate(interval))
	}

	for i := 1; i < len(seen); i++ {
		if gap := seen[i].Sub(seen[i-1]); gap != interval {
			t.Fatalf("boundary %s followed %s: gap of %s, want exactly %s "+
				"(a skipped boundary is a hole in cb_venue_state)", seen[i], seen[i-1], gap, interval)
		}
	}
}

func TestUntilNextBoundaryNeverWakesEarly(t *testing.T) {
	const interval = 5 * time.Second
	base := epoch
	for _, offset := range []time.Duration{
		0, time.Nanosecond, time.Millisecond, 2500 * time.Millisecond,
		interval - time.Millisecond, interval - time.Nanosecond,
	} {
		at := base.Add(offset)
		woke := at.Add(untilNextBoundary(at, interval))
		if !woke.After(at) {
			t.Errorf("at %s the wait was %s: a non-positive wait spins", at, woke.Sub(at))
		}
		// The wake time must be exactly a boundary, so truncating it cannot
		// land on the previous one.
		if !woke.Equal(woke.Truncate(interval)) {
			t.Errorf("waking at %s, which is not a boundary", woke)
		}
	}
}

// ---------------------------------------------------------------------------
// Part 5 review regressions (2026-08-24)
// ---------------------------------------------------------------------------

// Every Part 5 goroutine must observe cancellation.
//
// The only shutdown test in this package built Ingest with a nil RESTClient and
// a nil Account, so `New` constructed none of the marks sampler, the funding
// runner, the REST poller or the account poller — and the four Run loops Part 5
// added were never started by any test, let alone asserted to stop.
func TestPartFiveGoroutinesStopOnCancellation(t *testing.T) {
	opts := testOptions(t, &scriptedDialer{})
	opts.RESTClient = NewRESTClient("http://127.0.0.1:1/", nil) // refuses instantly
	opts.Account = stubAccount{}
	opts.Backfill = 0 // the backfill has its own tests; this one is about the loops

	in := New(opts, &fakeSink{}, NewMetrics(prometheus.NewRegistry()), testLogger())

	if in.marks == nil || in.funding == nil || in.poller == nil || in.account == nil {
		t.Fatalf("Part 5 components not constructed: marks=%v funding=%v poller=%v account=%v",
			in.marks != nil, in.funding != nil, in.poller != nil, in.account != nil)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); in.Run(ctx) }()

	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancellation: a Part 5 goroutine ignores the root context, " +
			"so the writer could never drain safely")
	}
}

// stubAccount answers the account poller without a credential or a network.
type stubAccount struct{}

func (stubAccount) BalanceSummary(ctx context.Context) (coinbase.Account, error) {
	return coinbase.Account{}, ctx.Err()
}
func (stubAccount) Positions(ctx context.Context) ([]coinbase.Position, error) { return nil, ctx.Err() }
func (stubAccount) IntradayMarginEnabled(ctx context.Context) (bool, error)    { return false, ctx.Err() }

// A stale trading session must stop asserting maintenance.
//
// The session used to be a bare bool that, once set, was never cleared. A REST
// outage beginning while the market was shut would have pinned
// maintenance_window true for as long as the poller stayed down — blocking every
// entry on a reading of a market that had since reopened.
func TestAStaleSessionStopsAssertingMaintenance(t *testing.T) {
	sink := &fakeSink{}
	v := newSampler(t, sink)

	at := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC) // a Thursday, outside the break
	v.now = func() time.Time { return at }
	v.ObserveSession(testPerp, Session{IsOpen: false})
	v.Observe(testPerp, quote(t, "99.5", "100.5", "100", at))

	if err := v.sample(context.Background(), at); err != nil {
		t.Fatalf("sample: %v", err)
	}
	if !venueRows(t, sink)[testPerp].MaintenanceWindow {
		t.Fatal("a fresh closed session did not set maintenance_window")
	}

	later := at.Add(maxSessionAge + time.Minute)
	sink2 := &fakeSink{}
	v.sink = sink2
	v.Observe(testPerp, quote(t, "99.5", "100.5", "100", later))
	if err := v.sample(context.Background(), later); err != nil {
		t.Fatalf("sample: %v", err)
	}
	if venueRows(t, sink2)[testPerp].MaintenanceWindow {
		t.Error("a stale session still asserted maintenance_window: entries would stay blocked " +
			"on a reading of a market that has since reopened")
	}
}

// The venue publishes a funding rate, and when it does it wins.
//
// This system read perpetual_details.funding_rate — permanently empty for this
// contract — and concluded the venue published nothing, for two build parts. The
// populated field is one level up. The estimate stays in funding_rate_est as the
// independent cross-check the reconciliation measures.
func TestTheVenuesRateTakesPrecedenceOverTheEstimate(t *testing.T) {
	sink := &fakeSink{}
	v := newSampler(t, sink)

	at := epoch
	v.Observe(testPerp, quote(t, "99.5", "100.5", "100", at))
	v.ObserveFunding(testPerp, Funding{
		HourStart: HourOf(at), Rate: dec(t, "0.0001"), Annualized: dec(t, "0.876"),
	}, "computed")
	v.ObserveVenueFunding(testPerp, db.Num(dec(t, "0.000019")), at, at)

	if err := v.sample(context.Background(), at); err != nil {
		t.Fatalf("sample: %v", err)
	}

	row := venueRows(t, sink)[testPerp]
	if got := row.FundingRateHourly.Decimal.String(); got != "0.000019" {
		t.Errorf("funding_rate_hourly = %s, want the venue's 0.000019", got)
	}
	if row.FundingSource != db.FundingSourceVenue {
		t.Errorf("funding_source = %q, want %q", row.FundingSource, db.FundingSourceVenue)
	}
	if got := row.FundingRateEst.Decimal.String(); got != "0.0001" {
		t.Errorf("funding_rate_est = %s, want the local estimate 0.0001 preserved", got)
	}
}

// signalSink reports each write so a test can wait on a channel rather than spin.
type signalSink struct {
	inner Sink
	wrote chan struct{}
}

func (s signalSink) Submit(ctx context.Context, r db.Row) error {
	err := s.inner.Submit(ctx, r)
	if err == nil {
		select {
		case s.wrote <- struct{}{}:
		default:
		}
	}
	return err
}

// The funding runner must actually emit at an hour boundary.
//
// Every other test drove Emit directly, so nothing exercised Run — and in
// production the first hour boundary after a restart passed with no row and no
// log line at all.
func TestFundingRunnerEmitsAtTheHourBoundary(t *testing.T) {
	sink := &fakeSink{}
	state := newSampler(t, sink)

	// A clock offset so that "now" sits just before the top of the hour and then
	// advances with real time. Nudging a frozen clock from the test goroutine
	// races the runner's own first read of it: lose that race and the runner
	// computes its wait from the far side of the boundary and sleeps an hour.
	hour := time.Date(2026, 8, 24, 13, 0, 0, 0, time.UTC)
	base, started := hour.Add(-60*time.Millisecond), time.Now()
	now := func() time.Time { return base.Add(time.Since(started)) }
	r := NewFundingRunner(testPerp, maintenanceWindow(t), state, sink, testLogger(), now)

	wrote := make(chan struct{}, 1)
	r.sink = signalSink{inner: sink, wrote: wrote}

	// A full previous hour of marks, so the boundary has something to compute.
	prev := hour.Add(-time.Hour)
	spot := dec(t, "2000")
	for i := 1; i <= SamplesPerHour; i++ {
		f := spot.Add(spot.Mul(dec(t, "0.0024")))
		r.Observe(MarkSample{At: prev.Add(time.Duration(i) * SampleInterval), FuturesMark: f, SpotMark: spot})
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); r.Run(ctx) }()

	select {
	case <-wrote:
	case <-time.After(10 * time.Second):
		cancel()
		<-done
		t.Fatal("the hour boundary passed and the runner wrote nothing: Run never called Emit")
	}
	cancel()
	<-done

	rows := accrualRows(t, sink)
	if len(rows) == 0 {
		t.Fatal("no accrual row was written")
	}
	if !rows[0].TS.Equal(prev) {
		t.Errorf("emitted hour %s, want the hour that just ended, %s", rows[0].TS, prev)
	}
	if rows[0].FundingSource != db.FundingSourceComputed {
		t.Errorf("funding_source = %q, want computed", rows[0].FundingSource)
	}
}
