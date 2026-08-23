package ingest

import (
	"context"
	"testing"
	"time"

	"github.com/Jason-Dorman/funding-carry/internal/db"
)

const testSpot = "ETH-USD"

func tradesFrame(trades ...tradeWire) []tradesEvent {
	return []tradesEvent{{Type: "update", Trades: trades}}
}

func trade(id, product, px, size, side string, at time.Time) tradeWire {
	return tradeWire{TradeID: id, ProductID: product, Price: px, Size: size, Side: side, Time: at}
}

func aggRows(t *testing.T, sink *fakeSink) []db.TradesAggRow {
	t.Helper()
	var rows []db.TradesAggRow
	for _, r := range sink.collected() {
		row, ok := r.(db.TradesAggRow)
		if !ok {
			t.Fatalf("unexpected row type %T on the trades sink", r)
		}
		rows = append(rows, row)
	}
	return rows
}

// admittedHandler returns a handler whose products have started aggregating at
// the bucket beginning at `epoch`.
func admittedHandler(t *testing.T, sink *fakeSink, products ...string) *TradesHandler {
	t.Helper()
	h := NewTradesHandler(products, TradeBucketSecs*time.Second, sink)
	// The first Tick admits at the *next* whole boundary, so aggregation starts
	// at epoch when the handler is first ticked one bucket earlier.
	if err := h.Tick(context.Background(), epoch.Add(-time.Minute)); err != nil {
		t.Fatalf("admit: %v", err)
	}
	return h
}

func TestTradesAggregateOneBucket(t *testing.T) {
	sink := &fakeSink{}
	h := admittedHandler(t, sink, testPerp)
	ctx := context.Background()

	// Two aggressive buy prints 100ms apart, then two sells half a minute later:
	// two sweeps, and a VWAP that is not the average of the prices.
	err := h.Handle(ctx, message(t, channelMarketTrades, epoch, tradesFrame(
		trade("4", testPerp, "110", "3", "SELL", epoch.Add(30*time.Second+100*time.Millisecond)),
		trade("3", testPerp, "110", "1", "SELL", epoch.Add(30*time.Second)),
		trade("2", testPerp, "100", "2", "BUY", epoch.Add(time.Second+100*time.Millisecond)),
		trade("1", testPerp, "100", "2", "BUY", epoch.Add(time.Second)),
	)))
	if err != nil {
		t.Fatalf("handle: %v", err)
	}

	// Nothing is written until the bucket has closed and its grace has passed.
	if err := h.Tick(ctx, epoch.Add(time.Minute)); err != nil {
		t.Fatalf("tick at the boundary: %v", err)
	}
	if rows := sink.collected(); len(rows) != 0 {
		t.Fatalf("bucket written %d times before its late grace elapsed", len(rows))
	}

	if err := h.Tick(ctx, epoch.Add(time.Minute+lateGrace)); err != nil {
		t.Fatalf("tick after the grace: %v", err)
	}

	rows := aggRows(t, sink)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	row := rows[0]

	// ts is the bucket end (API spec section 5.1).
	if want := epoch.Add(time.Minute); !row.TS.Equal(want) {
		t.Errorf("ts = %s, want the bucket end %s", row.TS, want)
	}
	if row.BucketSecs != TradeBucketSecs {
		t.Errorf("bucket_secs = %d, want %d: it is part of the row's identity", row.BucketSecs, TradeBucketSecs)
	}
	if got := row.BuyVol.String(); got != "4" {
		t.Errorf("buy_vol = %s, want 4", got)
	}
	if got := row.SellVol.String(); got != "4" {
		t.Errorf("sell_vol = %s, want 4", got)
	}
	if row.TradeCount != 4 {
		t.Errorf("trade_count = %d, want 4", row.TradeCount)
	}
	// (100*2 + 100*2 + 110*1 + 110*3) / 8 = 840/8
	if got := row.VWAP.Decimal.String(); got != "105" {
		t.Errorf("vwap = %s, want 105 — volume weighted, not the mean of the prices", got)
	}
	if got := row.MaxSingleSz.Decimal.String(); got != "3" {
		t.Errorf("max_single_sz = %s, want 3", got)
	}
	if row.SweepCount == nil || *row.SweepCount != 2 {
		t.Errorf("sweep_count = %v, want 2 (one run per side)", row.SweepCount)
	}
}

func TestTradesWriteAnEmptyBucket(t *testing.T) {
	sink := &fakeSink{}
	h := admittedHandler(t, sink, testPerp)

	if err := h.Tick(context.Background(), epoch.Add(time.Minute+lateGrace)); err != nil {
		t.Fatalf("tick: %v", err)
	}

	rows := aggRows(t, sink)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1: a minute with no trades is an observation", len(rows))
	}
	row := rows[0]
	if !row.BuyVol.IsZero() || !row.SellVol.IsZero() || row.TradeCount != 0 {
		t.Errorf("empty bucket = %+v, want zero volumes", row)
	}
	// A VWAP of a bucket with no trades does not exist. Writing zero would put a
	// price of nothing into a column the funding estimator marks against.
	if row.VWAP.Valid {
		t.Errorf("vwap = %v, want NULL for a bucket with no volume", row.VWAP)
	}
}

func TestTradesSkipTheBucketInProgressAtStartup(t *testing.T) {
	sink := &fakeSink{}
	h := NewTradesHandler([]string{testPerp}, TradeBucketSecs*time.Second, sink)
	ctx := context.Background()

	// Startup lands mid-bucket.
	if err := h.Tick(ctx, epoch.Add(30*time.Second)); err != nil {
		t.Fatalf("admit: %v", err)
	}
	// The venue replays recent history on subscribe; those trades belong to the
	// half-elapsed bucket and to earlier ones.
	if err := h.Handle(ctx, message(t, channelMarketTrades, epoch, tradesFrame(
		trade("1", testPerp, "100", "5", "BUY", epoch.Add(10*time.Second)),
		trade("2", testPerp, "100", "5", "BUY", epoch.Add(-time.Minute)),
	))); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if err := h.Tick(ctx, epoch.Add(2*time.Minute+lateGrace)); err != nil {
		t.Fatalf("tick: %v", err)
	}

	rows := aggRows(t, sink)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1: only the first whole bucket", len(rows))
	}
	// A partial aggregate written under a whole bucket's key can never be
	// corrected — ON CONFLICT DO NOTHING would drop the correction — so the
	// bucket in progress at startup is skipped rather than half-filled.
	if want := epoch.Add(2 * time.Minute); !rows[0].TS.Equal(want) {
		t.Errorf("first bucket end = %s, want %s", rows[0].TS, want)
	}
	if !rows[0].BuyVol.IsZero() {
		t.Errorf("buy_vol = %s, want 0: the replayed history belongs to skipped buckets", rows[0].BuyVol)
	}
}

func TestARepeatedTradeIsCountedOnce(t *testing.T) {
	sink := &fakeSink{}
	h := admittedHandler(t, sink, testPerp)
	ctx := context.Background()

	same := trade("99", testPerp, "100", "2", "BUY", epoch.Add(time.Second))
	for range 3 {
		if err := h.Handle(ctx, message(t, channelMarketTrades, epoch, tradesFrame(same))); err != nil {
			t.Fatalf("handle: %v", err)
		}
	}
	if err := h.Tick(ctx, epoch.Add(time.Minute+lateGrace)); err != nil {
		t.Fatalf("tick: %v", err)
	}

	// The venue repeats recent trades across messages and replays them on every
	// subscribe. Without the trade id, a reconnect would double the volume of
	// whatever bucket was open.
	rows := aggRows(t, sink)
	if rows[0].TradeCount != 1 || rows[0].BuyVol.String() != "2" {
		t.Errorf("count=%d buy_vol=%s, want 1 and 2", rows[0].TradeCount, rows[0].BuyVol)
	}
}

func TestTradeTimeRegressionIsReported(t *testing.T) {
	sink := &fakeSink{}
	h := admittedHandler(t, sink, testPerp)
	ctx := context.Background()

	if err := h.Handle(ctx, message(t, channelMarketTrades, epoch, tradesFrame(
		trade("2", testPerp, "100", "1", "BUY", epoch.Add(30*time.Second)),
	))); err != nil {
		t.Fatalf("first batch: %v", err)
	}

	// The newest trade in the next message is older than the newest in this one:
	// the stream is being fed a stale or reordered view.
	err := h.Handle(ctx, message(t, channelMarketTrades, epoch, tradesFrame(
		trade("1", testPerp, "100", "1", "BUY", epoch.Add(10*time.Second)),
	)))
	if err == nil {
		t.Fatal("trade time went backwards between messages and no gap was reported")
	}

	// The aggregate is still written; the gap is what says not to trust it.
	if err := h.Tick(ctx, epoch.Add(time.Minute+lateGrace)); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if rows := aggRows(t, sink); len(rows) != 1 || rows[0].TradeCount != 2 {
		t.Errorf("rows = %+v, want one bucket holding both trades", rows)
	}
}

func TestTradesRejectAnUnknownSide(t *testing.T) {
	sink := &fakeSink{}
	h := admittedHandler(t, sink, testPerp)

	// The aggressor side is what separates buy volume from sell volume, and
	// therefore what the trade-imbalance feature is built on. An unrecognised
	// value must not silently land on one side.
	err := h.Handle(context.Background(), message(t, channelMarketTrades, epoch, tradesFrame(
		trade("1", testPerp, "100", "1", "buy", epoch.Add(time.Second)),
	)))
	if err == nil {
		t.Fatal("a trade with an unrecognised side was accepted")
	}
}

func TestTradesResetSkipsTheBucketSpanningAReconnect(t *testing.T) {
	sink := &fakeSink{}
	h := admittedHandler(t, sink, testPerp)
	ctx := context.Background()

	if err := h.Handle(ctx, message(t, channelMarketTrades, epoch, tradesFrame(
		trade("1", testPerp, "100", "5", "BUY", epoch.Add(time.Second)),
	))); err != nil {
		t.Fatalf("handle: %v", err)
	}

	h.Reset()

	// Re-admission starts at the next whole boundary, so the bucket that was
	// open across the drop is skipped rather than written with only the trades
	// that arrived before the socket died. The tick that re-admits reports what
	// was lost, which the stream counts as a gap
	// (TestADroppedTradeBucketIsReportedAsAGap).
	if err := h.Tick(ctx, epoch.Add(30*time.Second)); err == nil {
		t.Fatal("re-admission did not report the dropped bucket")
	}
	if err := h.Tick(ctx, epoch.Add(2*time.Minute+lateGrace)); err != nil {
		t.Fatalf("tick: %v", err)
	}

	rows := aggRows(t, sink)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if want := epoch.Add(2 * time.Minute); !rows[0].TS.Equal(want) {
		t.Errorf("bucket end = %s, want %s: the interrupted bucket must be a hole, not a wrong number",
			rows[0].TS, want)
	}
}

func TestEachProductBucketsIndependently(t *testing.T) {
	sink := &fakeSink{}
	h := admittedHandler(t, sink, testPerp, testSpot)
	ctx := context.Background()

	if err := h.Handle(ctx, message(t, channelMarketTrades, epoch, tradesFrame(
		trade("1", testPerp, "100", "2", "BUY", epoch.Add(time.Second)),
		trade("2", testSpot, "2400", "0.5", "SELL", epoch.Add(2*time.Second)),
	))); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if err := h.Tick(ctx, epoch.Add(time.Minute+lateGrace)); err != nil {
		t.Fatalf("tick: %v", err)
	}

	rows := aggRows(t, sink)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want one per product", len(rows))
	}
	byProduct := map[string]db.TradesAggRow{}
	for _, r := range rows {
		byProduct[r.ProductID] = r
	}
	if got := byProduct[testPerp].BuyVol.String(); got != "2" {
		t.Errorf("perp buy_vol = %s, want 2 (contracts)", got)
	}
	// The spot leg trades fractional ETH; the perp trades whole contracts. Both
	// go through numeric, so neither is rounded to the other's precision.
	if got := byProduct[testSpot].SellVol.String(); got != "0.5" {
		t.Errorf("spot sell_vol = %s, want 0.5", got)
	}
}

func TestSweepRunsAreCountedOncePerBurst(t *testing.T) {
	for _, tc := range []struct {
		name    string
		spacing []time.Duration // gap before each trade after the first
		sides   []aggressor
		want    int32
	}{
		{
			name:    "one burst of three is one sweep",
			spacing: []time.Duration{100 * time.Millisecond, 100 * time.Millisecond},
			sides:   []aggressor{sideBuy, sideBuy, sideBuy},
			want:    1,
		},
		{
			name:    "a gap wider than the window separates two bursts",
			spacing: []time.Duration{100 * time.Millisecond, time.Second, 100 * time.Millisecond},
			sides:   []aggressor{sideBuy, sideBuy, sideBuy, sideBuy},
			want:    2,
		},
		{
			name:    "a side change separates two bursts even when they are adjacent",
			spacing: []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond},
			sides:   []aggressor{sideBuy, sideBuy, sideSell, sideSell},
			want:    2,
		},
		{
			name:    "an unknown side cannot extend or start a run",
			spacing: []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond},
			sides:   []aggressor{sideBuy, sideUnknown, sideUnknown, sideBuy},
			want:    0,
		},
		{
			name:    "isolated prints are not sweeps",
			spacing: []time.Duration{time.Second, time.Second},
			sides:   []aggressor{sideBuy, sideBuy, sideBuy},
			want:    0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			at := epoch
			trades := make([]tradeSample, 0, len(tc.sides))
			for i, side := range tc.sides {
				if i > 0 {
					at = at.Add(tc.spacing[i-1])
				}
				trades = append(trades, tradeSample{at: at, side: side})
			}
			if got := countSweeps(trades); got != tc.want {
				t.Errorf("countSweeps = %d, want %d", got, tc.want)
			}
		})
	}
}
