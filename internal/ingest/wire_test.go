package ingest

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The fixtures in testdata/ are frames captured from
// wss://advanced-trade-ws.coinbase.com on 2026-08-20, one per subscribed
// channel, with no credentials involved. They are what closes the `[verify]`
// item on Part 4: the structs in wire.go are asserted against what the venue
// actually sends rather than against what its documentation says it sends. A
// fixture that changes is a venue that changed, and the diff is reviewed as
// such (testing strategy: golden files).
//
// Five of the six are the raw frame, byte for byte. **l2_data.json is not:** the
// live snapshot was 74 KB on the wire (734 price levels), and it is committed
// cut down to the top twenty levels a side. Nothing else about it was touched.
// The pretty-printed full capture ran to 122 KB of repetition that no assertion
// here reads, which is the whole of the reason.
func fixture(t *testing.T, name string) envelope {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	env, err := decodeEnvelope(raw)
	if err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
	return env
}

func fixtureMessage(t *testing.T, name string) Message {
	t.Helper()
	env := fixture(t, name)
	return Message{
		Channel: env.Channel,
		SeqNum:  env.SequenceNum,
		VenueTS: env.VenueTS(),
		RecvTS:  env.VenueTS(),
		Events:  env.Events,
	}
}

func TestLiveTickerFrameDecodes(t *testing.T) {
	sink := &fakeSink{}
	state := newSampler(t, sink)
	h := NewTickerHandler([]string{testPerp, testSpot}, state)

	msg := fixtureMessage(t, "ticker.json")
	if msg.Channel != channelTicker {
		t.Fatalf("channel = %q, want %q", msg.Channel, channelTicker)
	}
	if err := h.Handle(context.Background(), msg); err != nil {
		t.Fatalf("handle: %v", err)
	}

	if err := state.sample(context.Background(), msg.RecvTS); err != nil {
		t.Fatalf("sample: %v", err)
	}
	row := venueRows(t, sink)[testPerp]
	// best_bid 2344.5, best_ask 2345 -> mid 2344.75, spread 0.5.
	if got := row.Mid.Decimal.String(); got != "2344.75" {
		t.Errorf("mid = %s, want 2344.75", got)
	}
	if got := row.SpreadBps.Decimal.Round(6).String(); got != "2.132423" {
		t.Errorf("spread_bps = %s, want 2.132423 (0.5 on a 2344.75 mid)", got)
	}
}

func TestLiveLevel2FrameDecodes(t *testing.T) {
	sink := &fakeSink{}
	h := NewLevel2Handler(testPerp, 10*time.Second, sink)
	ctx := context.Background()

	msg := fixtureMessage(t, "l2_data.json")
	// The subscription is sent as "level2" and the data comes back as "l2_data".
	if msg.Channel != channelLevel2Data {
		t.Fatalf("channel = %q, want %q", msg.Channel, channelLevel2Data)
	}
	if err := h.Handle(ctx, msg); err != nil {
		t.Fatalf("handle: %v", err)
	}
	// Inside the book's freshness window: a boundary has passed but the book has
	// not gone stale (maxBookAge).
	if err := h.Tick(ctx, msg.RecvTS.Add(11*time.Second)); err != nil {
		t.Fatalf("tick: %v", err)
	}

	rows := bookSnapshots(t, sink)
	if len(rows) != 1 {
		t.Fatalf("snapshots = %d, want 1", len(rows))
	}
	row := rows[0]
	if got := row.BestBid.Decimal.String(); got != "2344.5" {
		t.Errorf("best_bid = %s, want 2344.5", got)
	}
	if got := row.BestAsk.Decimal.String(); got != "2345.5" {
		t.Errorf("best_ask = %s, want 2345.5", got)
	}
	if len(row.BidPx) != bookDepth || len(row.AskPx) != bookDepth {
		t.Errorf("stored depth = %d bids / %d asks, want %d a side", len(row.BidPx), len(row.AskPx), bookDepth)
	}
	// Sizes on the perp book are whole contracts, not ETH: the venue quotes
	// 1342 at the touch, which is 134.2 ETH at 0.10 ETH a contract.
	if got := row.BidDepth[0].String(); got != "1342" {
		t.Errorf("touch depth = %s, want 1342 contracts", got)
	}
	// The impact price is the depth-weighted average of the stored levels, so it
	// falls between the touch and the far end of what is stored.
	if !row.ImpactBidPx.Valid || row.ImpactBidPx.Decimal.GreaterThan(row.BestBid.Decimal) {
		t.Errorf("impact_bid_px = %v, want at or below the best bid", row.ImpactBidPx)
	}
}

func TestLiveMarketTradesFrameDecodes(t *testing.T) {
	sink := &fakeSink{}
	h := NewTradesHandler([]string{testPerp, testSpot}, TradeBucketSecs*time.Second, sink)
	ctx := context.Background()

	msg := fixtureMessage(t, "market_trades.json")
	// Admit from the bucket before the fixture's own, so its trades land in a
	// bucket this handler considers whole.
	if err := h.Tick(ctx, msg.RecvTS.Add(-2*time.Minute)); err != nil {
		t.Fatalf("admit: %v", err)
	}
	if err := h.Handle(ctx, msg); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if err := h.Tick(ctx, msg.RecvTS.Add(time.Minute+lateGrace)); err != nil {
		t.Fatalf("tick: %v", err)
	}

	rows := aggRows(t, sink)
	var traded int
	for _, r := range rows {
		if r.TradeCount > 0 {
			traded++
			if !r.VWAP.Valid {
				t.Errorf("%s bucket ending %s has %d trades and no VWAP", r.ProductID, r.TS, r.TradeCount)
			}
		}
	}
	if traded == 0 {
		t.Fatal("no bucket in the live fixture carried a trade; the aggregation is not seeing the payload")
	}
}

func TestLiveCandlesFrameIsFiveMinuteGranularity(t *testing.T) {
	var events []candlesEvent
	if err := json.Unmarshal(fixture(t, "candles.json").Events, &events); err != nil {
		t.Fatalf("decode candles: %v", err)
	}

	var starts []time.Time
	for _, e := range events {
		for _, c := range e.Candles {
			start, err := parseUnixSeconds(c.Start)
			if err != nil {
				t.Fatalf("start %q: %v", c.Start, err)
			}
			starts = append(starts, start)
		}
	}
	if len(starts) < 3 {
		t.Fatalf("fixture carries %d candles, too few to measure spacing", len(starts))
	}

	// The build plan asked for one-minute candles. The channel does not serve
	// them: subscribing with granularity "ONE_MINUTE", with 60, and with nothing
	// at all all returned candles five minutes apart. This is the assertion
	// behind that finding, and behind CandleInterval and CandleTF.
	//
	// The spacing is measured from the fixture first and compared with the
	// constant afterwards, in that order and deliberately. Asserting the gaps are
	// a multiple of CandleInterval — the obvious phrasing — is satisfied by any
	// interval that divides 300 seconds, so a regression of the constant to one
	// minute would have passed it, and the constant is what stamps every
	// cb_bars row's close time.
	smallest := time.Duration(0)
	for i := 1; i < len(starts); i++ {
		gap := starts[i-1].Sub(starts[i])
		if gap < 0 {
			gap = -gap
		}
		if gap != 0 && (smallest == 0 || gap < smallest) {
			smallest = gap
		}
	}
	if smallest != 5*time.Minute {
		t.Fatalf("the venue's candle spacing in the fixture is %s, not 5m — the granularity "+
			"finding this part is built on no longer holds", smallest)
	}
	if CandleInterval != smallest {
		t.Errorf("CandleInterval = %s but the venue sends candles %s apart; every cb_bars ts "+
			"would be stamped with the wrong close time", CandleInterval, smallest)
	}
	if CandleTF != "5m" {
		t.Errorf("CandleTF = %q, want \"5m\" to match the interval the venue actually serves", CandleTF)
	}
}

func TestLiveStatusFrameDecodes(t *testing.T) {
	sink := &fakeSink{}
	state := newSampler(t, sink)
	h := NewStatusHandler(testPerp, state, testLogger())

	if err := h.Handle(context.Background(), fixtureMessage(t, "status.json")); err != nil {
		t.Fatalf("handle: %v", err)
	}

	state.Observe(testPerp, quote(t, "99.5", "100.5", "100", epoch))
	if err := state.sample(context.Background(), epoch); err != nil {
		t.Fatalf("sample: %v", err)
	}
	// The fixture reports "online" on a Thursday, outside the weekly break.
	if venueRows(t, sink)[testPerp].MaintenanceWindow {
		t.Error("maintenance_window = true for an online product outside the window")
	}
}

func TestLiveHeartbeatFrameIsNotDataForAnyStream(t *testing.T) {
	env := fixture(t, "heartbeats.json")
	if env.Channel != channelHeartbeats {
		t.Fatalf("channel = %q, want %q", env.Channel, channelHeartbeats)
	}
	// Every handler must ignore it, or the staleness gauge on every stream would
	// advance once a second whatever the market was doing.
	for _, h := range []Handler{
		NewTickerHandler([]string{testPerp}, nil),
		NewLevel2Handler(testPerp, time.Second, nil),
		NewTradesHandler([]string{testPerp}, time.Minute, nil),
		NewCandlesHandler([]string{testPerp}, nil),
		NewStatusHandler(testPerp, nil, testLogger()),
	} {
		if env.Channel == h.DataChannel() {
			t.Errorf("%T claims the heartbeats channel as its data", h)
		}
	}
}

func TestParseDecimalTreatsAnEmptyStringAsAbsent(t *testing.T) {
	// The venue sends "" for a field it has no value for — perpetual_details
	// .funding_rate on this product is the live example — and that has to read
	// as NULL. A zero funding rate is a number the decision engine would act on.
	got, err := parseDecimal("funding_rate", "")
	if err != nil {
		t.Fatalf("empty string was an error: %v", err)
	}
	if got.Valid {
		t.Errorf("parseDecimal(\"\") = %v, want absent", got)
	}

	// Where the schema says NOT NULL, absent is an error rather than a zero.
	if _, err := requireDecimal("volume", ""); err == nil {
		t.Error("requireDecimal accepted an empty string for a NOT NULL column")
	}
}

func TestParseDecimalRejectsGarbage(t *testing.T) {
	if _, err := parseDecimal("price", "2,345.50"); err == nil {
		t.Error("a thousands separator was accepted as a price")
	}
}
