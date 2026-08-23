package ingest

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Jason-Dorman/funding-carry/internal/db"
)

const testPerp = "ETP-20DEC30-CDE"

// message wraps events in the envelope a handler sees.
func message(t *testing.T, channel string, recv time.Time, events any) Message {
	t.Helper()
	raw, err := json.Marshal(events)
	if err != nil {
		t.Fatalf("encode events: %v", err)
	}
	return Message{Channel: channel, VenueTS: recv, RecvTS: recv, Events: raw}
}

func l2Frame(product, kind string, levels ...l2Level) []l2Event {
	return []l2Event{{ProductID: product, Type: kind, Updates: levels}}
}

func lvl(side, px, qty string) l2Level {
	return l2Level{Side: side, PriceLevel: px, NewQuantity: qty}
}

// bookSnapshots picks the snapshot rows out of what the sink collected.
func bookSnapshots(t *testing.T, sink *fakeSink) []db.BookSnapshotRow {
	t.Helper()
	var rows []db.BookSnapshotRow
	for _, r := range sink.collected() {
		row, ok := r.(db.BookSnapshotRow)
		if !ok {
			t.Fatalf("unexpected row type %T on the level2 sink", r)
		}
		rows = append(rows, row)
	}
	return rows
}

func TestLevel2SnapshotThenUpdatesBecomeOneRowPerBoundary(t *testing.T) {
	sink := &fakeSink{}
	h := NewLevel2Handler(testPerp, 10*time.Second, sink)
	ctx := context.Background()

	at := epoch
	if err := h.Handle(ctx, message(t, channelLevel2Data, at, l2Frame(testPerp, "snapshot",
		lvl("bid", "100", "2"),
		lvl("bid", "99", "3"),
		lvl("offer", "101", "1"),
		lvl("offer", "102", "4"),
	))); err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	// An update moves the touch and removes a level.
	if err := h.Handle(ctx, message(t, channelLevel2Data, at, l2Frame(testPerp, "update",
		lvl("bid", "100", "0"),
	))); err != nil {
		t.Fatalf("update: %v", err)
	}

	// Two ticks inside the same window; only the first crosses a boundary.
	if err := h.Tick(ctx, at.Add(11*time.Second)); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if err := h.Tick(ctx, at.Add(12*time.Second)); err != nil {
		t.Fatalf("tick: %v", err)
	}

	rows := bookSnapshots(t, sink)
	if len(rows) != 1 {
		t.Fatalf("snapshots = %d, want 1 per boundary", len(rows))
	}
	row := rows[0]

	// The row is stamped with the boundary, not with the clock: (product_id, ts)
	// is its identity, and a clock reading would make every row unique and the
	// idempotency constraint decorative (API spec section 5.3).
	if want := at.Add(10 * time.Second); !row.TS.Equal(want) {
		t.Errorf("ts = %s, want the boundary %s", row.TS, want)
	}
	if got := row.BestBid.Decimal.String(); got != "99" {
		t.Errorf("best_bid = %s, want 99 after the 100 level was removed", got)
	}
	if got := row.BestAsk.Decimal.String(); got != "101" {
		t.Errorf("best_ask = %s, want 101", got)
	}
	if len(row.BidPx) != 1 || len(row.BidDepth) != 1 {
		t.Errorf("bid arrays = %v / %v, want one level each", row.BidPx, row.BidDepth)
	}
	// The stored arrays and the derived columns must agree, because that is what
	// makes impact price reproducible from the row: (101*1 + 102*4)/5.
	if got := row.ImpactAskPx.Decimal.String(); got != "101.8" {
		t.Errorf("impact_ask_px = %s, want 101.8", got)
	}
}

func TestLevel2IgnoresUpdatesBeforeASnapshot(t *testing.T) {
	sink := &fakeSink{}
	h := NewLevel2Handler(testPerp, 10*time.Second, sink)
	ctx := context.Background()

	// A book built from updates alone would contain only the levels that
	// happened to change, which reads as a thin market rather than as missing
	// data — and nothing downstream could tell the difference.
	if err := h.Handle(ctx, message(t, channelLevel2Data, epoch, l2Frame(testPerp, "update",
		lvl("bid", "100", "2"),
	))); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := h.Tick(ctx, epoch.Add(11*time.Second)); err != nil {
		t.Fatalf("tick: %v", err)
	}

	if rows := sink.collected(); len(rows) != 0 {
		t.Fatalf("wrote %d snapshots before any venue snapshot arrived", len(rows))
	}
}

func TestLevel2ResetRequiresAFreshSnapshot(t *testing.T) {
	sink := &fakeSink{}
	h := NewLevel2Handler(testPerp, 10*time.Second, sink)
	ctx := context.Background()

	if err := h.Handle(ctx, message(t, channelLevel2Data, epoch, l2Frame(testPerp, "snapshot",
		lvl("bid", "100", "2"),
	))); err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	h.Reset()

	// Updates arriving after a reconnect but before the new snapshot must not
	// rebuild a book on top of the old one: every update that happened while the
	// socket was down is missing, and the book would be wrong in a way the row
	// itself could not reveal.
	if err := h.Handle(ctx, message(t, channelLevel2Data, epoch, l2Frame(testPerp, "update",
		lvl("bid", "99", "1"),
	))); err != nil {
		t.Fatalf("update after reset: %v", err)
	}
	if err := h.Tick(ctx, epoch.Add(11*time.Second)); err != nil {
		t.Fatalf("tick: %v", err)
	}

	if rows := sink.collected(); len(rows) != 0 {
		t.Fatalf("wrote %d snapshots from a book carried across a reset", len(rows))
	}
}

func TestLevel2RejectsAMalformedLevel(t *testing.T) {
	sink := &fakeSink{}
	h := NewLevel2Handler(testPerp, 10*time.Second, sink)

	err := h.Handle(context.Background(), message(t, channelLevel2Data, epoch, l2Frame(testPerp, "snapshot",
		lvl("bid", "not-a-price", "2"),
	)))
	if err == nil {
		t.Fatal("a level with an unparseable price was accepted; it would be counted as data, not as a gap")
	}
}

func TestLevel2SubscribesAndListensOnDifferentChannelNames(t *testing.T) {
	h := NewLevel2Handler(testPerp, 10*time.Second, &fakeSink{})
	sub, products := h.Subscribe()

	// The venue names this channel "level2" on the way in and "l2_data" on the
	// way out. A handler that matched on the subscription name would connect,
	// subscribe, and then see nothing at all.
	if sub != channelLevel2 || h.DataChannel() != channelLevel2Data {
		t.Errorf("subscribe=%q data=%q, want %q and %q", sub, h.DataChannel(), channelLevel2, channelLevel2Data)
	}
	if len(products) != 1 || products[0] != testPerp {
		t.Errorf("products = %v, want the perp alone: the spot leg executes on Base", products)
	}
}
