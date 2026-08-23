package ingest

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/Jason-Dorman/funding-carry/internal/db"
)

func candlesFrame(candles ...candleWire) []candlesEvent {
	return []candlesEvent{{Type: "snapshot", Candles: candles}}
}

// candleAt builds one candle opening at `start`. The venue sends the open time
// as a decimal string of unix seconds.
func candleAt(product string, start time.Time, closePx string) candleWire {
	return candleWire{
		ProductID: product,
		Start:     strconv.FormatInt(start.Unix(), 10),
		Open:      "100",
		High:      "110",
		Low:       "90",
		Close:     closePx,
		Volume:    "1000",
	}
}

func barRows(t *testing.T, sink *fakeSink) []db.BarRow {
	t.Helper()
	var rows []db.BarRow
	for _, r := range sink.collected() {
		row, ok := r.(db.BarRow)
		if !ok {
			t.Fatalf("unexpected row type %T on the candles sink", r)
		}
		rows = append(rows, row)
	}
	return rows
}

func TestOnlyCompletedCandlesArePersisted(t *testing.T) {
	sink := &fakeSink{}
	h := NewCandlesHandler([]string{testPerp}, sink)

	// Every candles message carries a window whose newest entry is still
	// forming. Writing it would persist a bar that is about to change.
	err := h.Handle(context.Background(), message(t, channelCandles, epoch, candlesFrame(
		candleAt(testPerp, epoch, "101"),
		candleAt(testPerp, epoch.Add(CandleInterval), "102"),
		candleAt(testPerp, epoch.Add(2*CandleInterval), "103"), // in progress
	)))
	if err != nil {
		t.Fatalf("handle: %v", err)
	}

	rows := barRows(t, sink)
	if len(rows) != 2 {
		t.Fatalf("bars = %d, want 2: the newest candle in the window is still forming", len(rows))
	}
	// ts is the bar's close; the venue sends its open (API spec section 5.1).
	if want := epoch.Add(CandleInterval); !rows[0].TS.Equal(want) {
		t.Errorf("first bar ts = %s, want the close %s", rows[0].TS, want)
	}
	if rows[0].TF != CandleTF {
		t.Errorf("tf = %q, want %q", rows[0].TF, CandleTF)
	}
	// trade_count has no NULL-versus-zero ambiguity to fall into here: the
	// candles channel does not carry one, and a zero would read as a bar in
	// which nothing traded.
	if rows[0].TradeCount != nil {
		t.Errorf("trade_count = %v, want NULL", *rows[0].TradeCount)
	}
	if got := rows[0].Close.String(); got != "101" {
		t.Errorf("close = %s, want 101", got)
	}
}

func TestACandleIsWrittenOnceEvenAsTheWindowRepeatsIt(t *testing.T) {
	sink := &fakeSink{}
	h := NewCandlesHandler([]string{testPerp}, sink)
	ctx := context.Background()

	first := candlesFrame(
		candleAt(testPerp, epoch, "101"),
		candleAt(testPerp, epoch.Add(CandleInterval), "102"),
	)
	if err := h.Handle(ctx, message(t, channelCandles, epoch, first)); err != nil {
		t.Fatalf("first: %v", err)
	}

	// The next message repeats the whole window and adds one candle: the one
	// that was forming is now closed.
	second := candlesFrame(
		candleAt(testPerp, epoch, "101"),
		candleAt(testPerp, epoch.Add(CandleInterval), "102"),
		candleAt(testPerp, epoch.Add(2*CandleInterval), "103"),
	)
	if err := h.Handle(ctx, message(t, channelCandles, epoch, second)); err != nil {
		t.Fatalf("second: %v", err)
	}

	rows := barRows(t, sink)
	if len(rows) != 2 {
		t.Fatalf("bars = %d, want 2 (one per closed candle, no repeats)", len(rows))
	}
	if want := epoch.Add(2 * CandleInterval); !rows[1].TS.Equal(want) {
		t.Errorf("second bar ts = %s, want %s", rows[1].TS, want)
	}
}

func TestACandleTimeJumpIsReportedAsAGap(t *testing.T) {
	sink := &fakeSink{}
	h := NewCandlesHandler([]string{testPerp}, sink)
	ctx := context.Background()

	// The first window is the venue's own history: holes in it say nothing about
	// this process, so discontinuity is only counted from the second message on.
	if err := h.Handle(ctx, message(t, channelCandles, epoch, candlesFrame(
		candleAt(testPerp, epoch, "101"),
		candleAt(testPerp, epoch.Add(CandleInterval), "102"),
	))); err != nil {
		t.Fatalf("seed: %v", err)
	}

	err := h.Handle(ctx, message(t, channelCandles, epoch, candlesFrame(
		candleAt(testPerp, epoch.Add(3*CandleInterval), "104"), // two intervals missing
		candleAt(testPerp, epoch.Add(4*CandleInterval), "105"),
	)))
	if err == nil {
		t.Fatal("a candle-time discontinuity was not reported; it would be an invisible hole in cb_bars")
	}

	// The bars are still written. A gap says the series is incomplete, not that
	// the data that did arrive should be thrown away.
	//
	// Three, not two: the candle that was still forming in the seed message is
	// now known to have closed — a newer start has been observed — so it is
	// written as well. Under the original rule it was dropped, which is the bug
	// TestACandleIsPersistedWhenTheNextOneAppearsInALaterMessage covers.
	rows := barRows(t, sink)
	if len(rows) != 3 {
		t.Fatalf("bars = %d, want the two before the gap plus the one after", len(rows))
	}
}

func TestCandleStateSurvivesAReconnect(t *testing.T) {
	sink := &fakeSink{}
	h := NewCandlesHandler([]string{testPerp}, sink)
	ctx := context.Background()

	if err := h.Handle(ctx, message(t, channelCandles, epoch, candlesFrame(
		candleAt(testPerp, epoch, "101"),
		candleAt(testPerp, epoch.Add(CandleInterval), "102"),
	))); err != nil {
		t.Fatalf("first: %v", err)
	}

	// Reset is about connection state. What this handler remembers is which rows
	// are already in the database, which a dropped socket does not change — and
	// the fresh window the venue replays on resubscribe is what refills whatever
	// the disconnection cost.
	h.Reset()

	if err := h.Handle(ctx, message(t, channelCandles, epoch, candlesFrame(
		candleAt(testPerp, epoch, "101"),
		candleAt(testPerp, epoch.Add(CandleInterval), "102"),
		candleAt(testPerp, epoch.Add(2*CandleInterval), "103"),
	))); err != nil {
		t.Fatalf("after reset: %v", err)
	}

	if rows := barRows(t, sink); len(rows) != 2 {
		t.Fatalf("bars = %d, want 2: a reconnect must not rewrite the window it already persisted", len(rows))
	}
}

func TestAMalformedCandleDoesNotStopTheGoodOnes(t *testing.T) {
	sink := &fakeSink{}
	h := NewCandlesHandler([]string{testPerp}, sink)

	bad := candleAt(testPerp, epoch, "101")
	bad.Volume = ""

	err := h.Handle(context.Background(), message(t, channelCandles, epoch, candlesFrame(
		bad,
		candleAt(testPerp, epoch.Add(CandleInterval), "102"),
		candleAt(testPerp, epoch.Add(2*CandleInterval), "103"),
	)))
	if err == nil {
		t.Fatal("a candle with no volume was accepted into a NOT NULL column")
	}
	if rows := barRows(t, sink); len(rows) != 1 {
		t.Fatalf("bars = %d, want the one good closed candle", len(rows))
	}
}

func TestCandlesFromAnotherProductAreIgnored(t *testing.T) {
	sink := &fakeSink{}
	h := NewCandlesHandler([]string{testPerp}, sink)

	if err := h.Handle(context.Background(), message(t, channelCandles, epoch, candlesFrame(
		candleAt(testSpot, epoch, "2400"),
		candleAt(testSpot, epoch.Add(CandleInterval), "2401"),
	))); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if rows := sink.collected(); len(rows) != 0 {
		t.Fatalf("wrote %d bars for a product this handler is not subscribed to", len(rows))
	}
}
