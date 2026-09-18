package fix

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/quickfixgo/enum"
	"github.com/shopspring/decimal"

	"github.com/Jason-Dorman/funding-carry/internal/db"
)

// The engine's behaviour, driven through its own methods under a clock the test
// owns. The run loop is tested separately, at the bottom: what matters here is
// what the simulator decides, not which goroutine it decided it on.

// past is comfortably longer than the jittered latency can ever be, so "the
// order has had its chance" is not itself a timing question.
const past = 50 * time.Millisecond

// A straight round trip: the acceptance criterion D -> 8(NEW) -> 8(FILLED).
func TestAnOrderRunsFromNewToFilled(t *testing.T) {
	h := newHarness(t, testFill())

	h.order("A1", buy, "3", "2350.00", gtc)

	first := h.venue.last()
	if first.ordStatus != enum.OrdStatus_NEW || first.execType != enum.ExecType_NEW {
		t.Fatalf("first report is %s/%s, want NEW/NEW", first.execType, first.ordStatus)
	}
	if !first.leavesQty.Equal(dec("3")) {
		t.Errorf("NEW leaves %s, want the whole order", first.leavesQty)
	}
	if len(h.sink.fills()) != 0 {
		t.Error("an acknowledgement was recorded as a fill; fills is a table of executions")
	}

	h.tick(t, past)

	fill := h.venue.last()
	if fill.execType != enum.ExecType_TRADE || fill.ordStatus != enum.OrdStatus_FILLED {
		t.Fatalf("second report is %s/%s, want TRADE/FILLED", fill.execType, fill.ordStatus)
	}
	if !fill.cumQty.Equal(dec("3")) || !fill.leavesQty.IsZero() {
		t.Errorf("cum %s leaves %s, want 3 and 0", fill.cumQty, fill.leavesQty)
	}
	// 2 bps of the 2345.00 ask, paid by the buyer.
	if !fill.lastPx.Equal(dec("2345.469")) {
		t.Errorf("fill price %s, want the ask plus slippage", fill.lastPx)
	}
	if !fill.avgPx.Equal(fill.lastPx) {
		t.Errorf("avg px %s, want the single fill price %s", fill.avgPx, fill.lastPx)
	}
	if len(h.engine.live) != 0 {
		t.Error("a filled order is still on the book")
	}

	rows := h.sink.fills()
	if len(rows) != 1 {
		t.Fatalf("%d fills recorded, want 1", len(rows))
	}
	row := rows[0]
	if row.Venue != db.VenueSim || row.Leg != db.LegPerp || row.Side != db.SideBuy {
		t.Errorf("row is %s/%s/%s, want sim/perp/buy", row.Venue, row.Leg, row.Side)
	}
	if row.ExecState != "FILLED" || row.VenueExecID != fill.execID || row.ClOrdID != "A1" {
		t.Errorf("row exec_state %s exec_id %s cl_ord_id %s, want FILLED/%s/A1",
			row.ExecState, row.VenueExecID, row.ClOrdID, fill.execID)
	}
	if len(row.Raw) == 0 {
		t.Error("the fill row carries no raw payload")
	}
}

// The second acceptance criterion: an oversized order produces partials summing
// to the full quantity, with CumQty monotonic across them.
func TestAnOversizedOrderFillsInSlicesThatSumToTheOrder(t *testing.T) {
	h := newHarness(t, testFill())

	h.order("A1", sell, "35", "2340.00", gtc)
	for range 3 {
		h.tick(t, past)
	}

	var trades []report
	for _, r := range h.venue.all() {
		if r.execType == enum.ExecType_TRADE {
			trades = append(trades, r)
		}
	}
	if len(trades) != 3 {
		t.Fatalf("%d trade reports, want 3 slices", len(trades))
	}

	wantQty := []string{"11", "11", "13"}
	wantStatus := []enum.OrdStatus{
		enum.OrdStatus_PARTIALLY_FILLED, enum.OrdStatus_PARTIALLY_FILLED, enum.OrdStatus_FILLED,
	}
	total, prev := decimal.Zero, decimal.Zero
	for i, r := range trades {
		if !r.lastQty.Equal(dec(wantQty[i])) {
			t.Errorf("slice %d qty %s, want %s", i, r.lastQty, wantQty[i])
		}
		if r.ordStatus != wantStatus[i] {
			t.Errorf("slice %d status %s, want %s", i, r.ordStatus, wantStatus[i])
		}
		if r.cumQty.LessThanOrEqual(prev) {
			t.Errorf("slice %d cum %s did not advance past %s: CumQty must be monotonic",
				i, r.cumQty, prev)
		}
		if !r.cumQty.Add(r.leavesQty).Equal(dec("35")) {
			t.Errorf("slice %d cum %s + leaves %s is not the order", i, r.cumQty, r.leavesQty)
		}
		prev = r.cumQty
		total = total.Add(r.lastQty)
	}
	if !total.Equal(dec("35")) {
		t.Errorf("the partials sum to %s, want the whole order of 35", total)
	}

	if rows := h.sink.fills(); len(rows) != 3 {
		t.Errorf("%d fill rows, want one per partial", len(rows))
	}
}

// Cancel before anything has printed: the first half of the third acceptance
// criterion.
func TestCancelBeforeAnyFill(t *testing.T) {
	h := newHarness(t, testFill())

	h.order("A1", buy, "3", "2350.00", gtc)
	h.cancel(t, "C1", "A1")

	r := h.venue.last()
	if r.execType != enum.ExecType_CANCELED || r.ordStatus != enum.OrdStatus_CANCELED {
		t.Fatalf("report is %s/%s, want CANCELED/CANCELED", r.execType, r.ordStatus)
	}
	if r.clOrdID != "C1" || r.origClOrdID != "A1" {
		t.Errorf("report is for %s/%s, want the cancel's own id against the original",
			r.clOrdID, r.origClOrdID)
	}
	if !r.cumQty.IsZero() || !r.leavesQty.IsZero() {
		t.Errorf("cum %s leaves %s, want nothing filled and nothing resting", r.cumQty, r.leavesQty)
	}

	// And nothing fills afterwards, however long the market stays crossed.
	h.tick(t, past)
	h.tick(t, past)
	if len(h.sink.fills()) != 0 {
		t.Error("a canceled order still filled")
	}
}

// Cancel mid-partial: the other half. What is filled stays filled, and the
// remainder is gone rather than resting.
func TestCancelMidPartial(t *testing.T) {
	h := newHarness(t, testFill())

	h.order("A1", buy, "35", "2350.00", gtc)
	h.tick(t, past)
	if got := h.venue.last().cumQty; !got.Equal(dec("11")) {
		t.Fatalf("cum after one slice is %s, want 11", got)
	}

	h.cancel(t, "C1", "A1")

	r := h.venue.last()
	if r.ordStatus != enum.OrdStatus_CANCELED {
		t.Fatalf("report is %s, want CANCELED", r.ordStatus)
	}
	if !r.cumQty.Equal(dec("11")) {
		t.Errorf("canceled with cum %s, want the 11 already filled", r.cumQty)
	}
	if !r.leavesQty.IsZero() {
		t.Errorf("canceled with leaves %s, want 0: the remainder is gone, not resting", r.leavesQty)
	}
	if !r.avgPx.Equal(dec("2345.469")) {
		t.Errorf("avg px %s, want the price the 11 printed at", r.avgPx)
	}

	h.tick(t, past)
	if n := len(h.sink.fills()); n != 1 {
		t.Errorf("%d fills after the cancel, want the single partial", n)
	}
}

// A cancel the venue cannot honour is answered with an OrderCancelReject, and
// the reason distinguishes an order it never had from one it has finished with.
func TestCancelRejects(t *testing.T) {
	h := newHarness(t, testFill())

	h.cancel(t, "C1", "never-existed")
	h.order("A1", buy, "3", "2350.00", gtc)
	h.tick(t, past)
	h.cancel(t, "C2", "A1")

	if len(h.venue.rejects) != 2 {
		t.Fatalf("%d cancel rejects, want 2", len(h.venue.rejects))
	}
	unknown, late := h.venue.rejects[0], h.venue.rejects[1]

	if unknown.reason != enum.CxlRejReason_UNKNOWN_ORDER {
		t.Errorf("unknown order rejected with %s, want UNKNOWN_ORDER", unknown.reason)
	}
	if unknown.orderID != unknownOrderID {
		t.Errorf("OrderID %q on an unknown order, want %q", unknown.orderID, unknownOrderID)
	}
	if late.reason != enum.CxlRejReason_TOO_LATE_TO_CANCEL {
		t.Errorf("finished order rejected with %s, want TOO_LATE_TO_CANCEL", late.reason)
	}
	if late.ordStatus != enum.OrdStatus_FILLED {
		t.Errorf("reject carries status %s, want the order's own FILLED", late.ordStatus)
	}
}

// A non-crossing order rests and fills when the market comes to it. This is the
// PO decision of 2026-09-15: the simulator models a resting order rather than
// deciding an order's fate once at entry.
func TestARestingOrderFillsWhenTheMarketReachesIt(t *testing.T) {
	h := newHarness(t, testFill())

	// A buy well under the ask: marketable to nobody.
	h.order("A1", buy, "3", "2300.00", gtc)
	h.tick(t, past)
	if len(h.sink.fills()) != 0 {
		t.Fatal("an order below the ask filled")
	}
	if len(h.engine.live) != 1 {
		t.Fatal("a non-crossing order was not left resting")
	}

	// The market comes down to it. The new snapshot is stamped at the clock's
	// current reading, which is what a fresh row from ingest looks like.
	h.clock.advance(h.engine.fill.BookPoll)
	h.books.set(h.clock.now(), "2299.50", "2300.00")
	h.tick(t, 0)

	fill := h.venue.last()
	if fill.execType != enum.ExecType_TRADE {
		t.Fatalf("last report is %s, want a trade once the market crossed", fill.execType)
	}
	// The slipped price would be 2300.46, which is through the limit, so the
	// fill prints at the limit itself.
	if !fill.lastPx.Equal(dec("2300.00")) {
		t.Errorf("fill price %s, want the limit", fill.lastPx)
	}
}

// An order that could not fill is deferred to the next book read rather than
// re-asked immediately: without that, the run loop would wake on an order that
// is permanently due and spin.
func TestAnUnfillableOrderIsDeferredToTheNextBookRead(t *testing.T) {
	h := newHarness(t, testFill())

	h.order("A1", buy, "3", "2300.00", gtc)
	h.tick(t, past)

	o := h.engine.live["A1"]
	if !o.readyAt.Equal(h.engine.nextBookRead) {
		t.Fatalf("readyAt %s, want the next book read at %s", o.readyAt, h.engine.nextBookRead)
	}
	if d := h.engine.untilNextWake(); d <= 0 {
		t.Fatalf("next wake is %s: the loop would spin on an order that cannot fill", d)
	}
}

// The book is re-read on its own interval rather than on every evaluation: the
// snapshot is what the venue prices against, and re-querying it per order would
// make the database the simulator's hot path.
func TestTheBookIsRereadOnItsOwnInterval(t *testing.T) {
	h := newHarness(t, testFill())
	after := h.books.readCount()

	// Inside the interval: orders are priced against the snapshot in hand.
	h.order("A1", buy, "1", "2350.00", gtc)
	h.tick(t, past)
	if got := h.books.readCount(); got != after {
		t.Errorf("%d reads inside one poll interval, want none beyond the %d already made", got, after)
	}

	// Past it: one read, and the gauge follows.
	h.tick(t, h.engine.fill.BookPoll)
	if got := h.books.readCount(); got != after+1 {
		t.Errorf("%d reads after the interval elapsed, want %d", got, after+1)
	}
}

// IOC means "trade now or not at all". With nothing to trade against it is
// canceled rather than left resting, and an oversized one takes the slice it can
// and cancels the rest.
func TestIOCDoesNotRest(t *testing.T) {
	t.Run("nothing to trade against", func(t *testing.T) {
		h := newHarness(t, testFill())
		h.order("A1", buy, "3", "2300.00", ioc)
		h.tick(t, past)

		r := h.venue.last()
		if r.ordStatus != enum.OrdStatus_CANCELED {
			t.Fatalf("report is %s, want CANCELED", r.ordStatus)
		}
		if r.text != cancelIOCRemainder {
			t.Errorf("reason %q, want %q", r.text, cancelIOCRemainder)
		}
		if len(h.engine.live) != 0 {
			t.Error("an IOC is resting on the book")
		}
	})

	t.Run("oversized fills one slice", func(t *testing.T) {
		h := newHarness(t, testFill())
		h.order("A1", buy, "35", "2350.00", ioc)
		h.tick(t, past)

		reports := h.venue.all()
		trade, cancel := reports[len(reports)-2], reports[len(reports)-1]
		if trade.execType != enum.ExecType_TRADE || !trade.lastQty.Equal(dec("11")) {
			t.Fatalf("expected one slice of 11, got %s of %s", trade.execType, trade.lastQty)
		}
		if cancel.ordStatus != enum.OrdStatus_CANCELED || !cancel.cumQty.Equal(dec("11")) {
			t.Fatalf("remainder report is %s with cum %s, want CANCELED with 11",
				cancel.ordStatus, cancel.cumQty)
		}
	})

	t.Run("no market at all", func(t *testing.T) {
		h := newHarness(t, testFill())
		h.order("A1", buy, "3", "2350.00", ioc)
		// The book ages past the maximum between acceptance and evaluation.
		h.tick(t, h.engine.fill.BookMaxAge+time.Second)

		r := h.venue.last()
		if r.ordStatus != enum.OrdStatus_CANCELED || r.text != cancelNoMarket {
			t.Fatalf("report is %s %q, want CANCELED %q", r.ordStatus, r.text, cancelNoMarket)
		}
	})
}

// Entry validation: a well-formed order this venue will not take is refused by
// name, against the client's own order id.
func TestOrdersRefusedAtEntry(t *testing.T) {
	base := func() submission {
		return submission{
			clOrdID: "A1",
			symbol:  testPerp,
			side:    buy,
			tif:     gtc,
			ordType: enum.OrdType_LIMIT,
			qty:     dec("3"),
			limitPx: dec("2350.00"),
			at:      epoch,
		}
	}

	for _, tc := range []struct {
		name   string
		mutate func(*submission)
		want   string
	}{
		{"a symbol this venue does not make a market in",
			func(s *submission) { s.symbol = testSpot }, rejectUnknownSymbol},
		{"a market order",
			func(s *submission) { s.ordType = enum.OrdType_MARKET }, rejectOrdType},
		{"a time in force the simulator does not honour",
			func(s *submission) { s.tif = "" }, rejectTIF},
		{"a fractional contract count",
			func(s *submission) { s.qty = dec("1.5") }, rejectQty},
		{"a quantity of nothing",
			func(s *submission) { s.qty = decimal.Zero }, rejectQty},
		{"a negative quantity",
			func(s *submission) { s.qty = dec("-3") }, rejectQty},
		{"a price of nothing",
			func(s *submission) { s.limitPx = decimal.Zero }, rejectPrice},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, testFill())
			s := base()
			tc.mutate(&s)
			h.submit(s)

			r := h.venue.last()
			if r.ordStatus != enum.OrdStatus_REJECTED || r.execType != enum.ExecType_REJECTED {
				t.Fatalf("report is %s/%s, want REJECTED/REJECTED", r.execType, r.ordStatus)
			}
			if r.text != tc.want {
				t.Errorf("reason %q, want %q", r.text, tc.want)
			}
			if r.clOrdID != s.clOrdID {
				t.Errorf("rejection carries %q, want the client's own id", r.clOrdID)
			}
			if len(h.engine.live) != 0 {
				t.Error("a rejected order was put on the book")
			}
		})
	}
}

// A ClOrdID is the client's handle on its order. Reusing one — live or
// finished — is refused, because the alternative is two orders the client cannot
// tell apart and a cancel that could address either.
func TestADuplicateClOrdIDIsRefused(t *testing.T) {
	h := newHarness(t, testFill())

	h.order("A1", buy, "3", "2350.00", gtc)
	h.order("A1", buy, "3", "2350.00", gtc)
	if r := h.venue.last(); r.text != rejectDuplicate {
		t.Fatalf("resubmitting a live id was answered %q, want %q", r.text, rejectDuplicate)
	}

	h.tick(t, past)
	h.order("A1", buy, "3", "2350.00", gtc)
	if r := h.venue.last(); r.text != rejectDuplicate {
		t.Fatalf("reusing a finished id was answered %q, want %q", r.text, rejectDuplicate)
	}
}

// No market data, no venue. The PO's decision of 2026-09-15: an order arriving
// with nothing fresh to price against is refused rather than accepted onto a
// book that cannot move.
func TestOrdersAreRefusedWithoutAFreshBook(t *testing.T) {
	t.Run("no snapshot at all", func(t *testing.T) {
		h := newHarness(t, testFill())
		h.books.have = false
		h.engine.haveBook = false
		h.order("A1", buy, "3", "2350.00", gtc)

		if r := h.venue.last(); r.text != rejectNoMarket {
			t.Fatalf("answered %q, want %q", r.text, rejectNoMarket)
		}
	})

	t.Run("a snapshot older than the maximum", func(t *testing.T) {
		h := newHarness(t, testFill())
		h.clock.advance(h.engine.fill.BookMaxAge + time.Second)
		h.order("A1", buy, "3", "2350.00", gtc)

		if r := h.venue.last(); r.text != rejectNoMarket {
			t.Fatalf("answered %q, want %q", r.text, rejectNoMarket)
		}
	})
}

// A failed read is a feed problem: it degrades to staleness and never stops the
// simulator (architecture section 8). What it must not do is keep filling
// forever against a book that is no longer being refreshed.
func TestABookReadFailureDegradesToStaleness(t *testing.T) {
	h := newHarness(t, testFill())
	h.books.fail(errors.New("connection refused"))

	// Still filling against the last good snapshot, which is what "degrade"
	// means: the order was placed into a market that was real a moment ago.
	h.order("A1", buy, "3", "2350.00", gtc)
	h.tick(t, past)
	if len(h.sink.fills()) != 1 {
		t.Fatal("the last good book was discarded on the first read failure")
	}

	// Once it is older than the maximum, the venue says so instead of pricing
	// against it.
	h.clock.advance(h.engine.fill.BookMaxAge + time.Second)
	h.order("A2", buy, "3", "2350.00", gtc)
	if r := h.venue.last(); r.text != rejectNoMarket {
		t.Fatalf("answered %q, want %q once the book aged out", r.text, rejectNoMarket)
	}
}

// A simulator that cannot record its fills has to stop, the same way ingest
// does: a process that looks healthy while discarding the only record of what it
// did is the failure the Compose restart policy exists to catch.
func TestTheEngineStopsWhenTheWriterDies(t *testing.T) {
	h := newHarness(t, testFill())

	h.order("A1", buy, "3", "2350.00", gtc)
	h.sink.stop(db.ErrWriterStopped)

	h.clock.advance(past)
	err := h.engine.tick(h.ctx)
	if !errors.Is(err, db.ErrWriterStopped) {
		t.Fatalf("tick returned %v, want the writer's ErrWriterStopped", err)
	}
}

// The record-before-send guarantee, which was a comment and nothing else until
// the Part 7 adversarial review: reversing the two statements in publish passed
// the entire suite.
//
// The ordering is not a style preference. A report sent but not recorded is an
// execution the venue has told a client about and has no record of — the client
// believes it holds a position the database cannot account for. A row written
// and not sent is recovered by the FIX resend. Recording first is what makes the
// unrecoverable direction unreachable.
func TestAFillIsRecordedBeforeItIsReported(t *testing.T) {
	h := newHarness(t, testFill())

	h.order("A1", buy, "3", "2350.00", gtc)
	acked := len(h.venue.all()) // the NEW acknowledgement, which moves no quantity

	// The writer dies between the acknowledgement and the fill.
	h.sink.stop(db.ErrWriterStopped)
	h.clock.advance(past)

	err := h.engine.tick(h.ctx)
	if !errors.Is(err, db.ErrWriterStopped) {
		t.Fatalf("tick returned %v, want the writer's ErrWriterStopped", err)
	}
	if got := len(h.venue.all()); got != acked {
		t.Fatalf("%d reports on the wire, want the %d from before the writer died: a fill was "+
			"reported to the client after its row failed to record, so the client believes in "+
			"an execution the database has no record of", got, acked)
	}
	if len(h.sink.fills()) != 0 {
		t.Fatal("a fill row was recorded by a sink that was refusing writes")
	}
}

// Determinism under a fixed seed (API spec section 4.3), asserted on the wire
// rather than on the struct: the same seed produces byte-identical messages, and
// a different seed does not.
//
// The second half is the part that matters, and this test did not have it until
// the Part 7 adversarial review. The tick step is now deliberately SHORTER than
// the jitter spread. With a step longer than the widest possible draw — 50ms
// against a [10ms, 30ms) jitter, which is what this test used — every slice is
// due on the first tick whatever the generator returned, the jitter never
// reaches the wire, and the comparison holds for any seed at all. Two lenses
// demonstrated the consequence independently: replacing the seeded generator
// with a wall-clock one passed the entire suite.
func TestFillsAreDeterministicUnderASeed(t *testing.T) {
	// Short enough that which tick a slice lands on is decided by the draw.
	const step = 5 * time.Millisecond
	// Enough ticks for three slices at up to 30ms of latency each, plus slack.
	const ticks = 30

	run := func(seed uint64) []string {
		h := newSeededHarness(t, testFill(), seed)
		h.order("A1", buy, "35", "2350.00", gtc)
		h.order("A2", sell, "4", "2340.00", gtc)
		for range ticks {
			h.tick(t, step)
		}
		var wire []string
		for _, r := range h.venue.all() {
			wire = append(wire, executionReportMessage(r).String())
		}
		return wire
	}

	first, second := run(testSeed), run(testSeed)
	if len(first) != len(second) {
		t.Fatalf("%d reports then %d: the same seed produced different sequences",
			len(first), len(second))
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("report %d differs between two runs of the same seed:\n%s\n%s",
				i, first[i], second[i])
		}
	}
	if len(first) == 0 {
		t.Fatal("no reports: the comparison proved nothing")
	}

	// And the seed has to be what produced it. Without this, any regression that
	// stopped the seed reaching rand.NewPCG — including seeding from the clock —
	// would leave every assertion above still passing.
	if other := run(testSeed + 1); equalStrings(first, other) {
		t.Fatal("a different seed produced byte-identical fills: the seed is not reaching " +
			"the engine's generator, so nothing here constrains determinism")
	}
}

// Two orders that become fillable on the same tick report in ClOrdID order.
// Without that, Go's map iteration would decide the sequence and the run above
// would be reproducible only by luck.
func TestSimultaneousFillsReportInAStableOrder(t *testing.T) {
	h := newHarness(t, testFill())

	h.order("B1", buy, "1", "2350.00", gtc)
	h.order("A1", buy, "1", "2350.00", gtc)
	h.tick(t, past)

	var order []string
	for _, r := range h.venue.all() {
		if r.execType == enum.ExecType_TRADE {
			order = append(order, r.clOrdID)
		}
	}
	if len(order) != 2 || order[0] != "A1" || order[1] != "B1" {
		t.Fatalf("fills reported in %v, want A1 before B1", order)
	}
}

// ExecIDs must be unique across restarts. fills is keyed on
// (venue, venue_exec_id) with ON CONFLICT DO NOTHING, so a counter that started
// again from one on every boot would make the second run's fills vanish into the
// first run's rows without a single error anywhere.
func TestExecIDsDoNotRepeatAcrossRuns(t *testing.T) {
	ids := func(start time.Time) []string {
		m := newIDMinter(start)
		return []string{m.order(), m.order(), m.exec()}
	}

	first := ids(epoch)
	if same := ids(epoch); !equalStrings(first, same) {
		t.Fatal("the same start produced different ids; a replay would not be reproducible")
	}
	second := ids(epoch.Add(time.Second))
	for i := range first {
		if first[i] == second[i] {
			t.Fatalf("id %d is %q in both runs: a restart would collide in fills", i, first[i])
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// The run loop
// ---------------------------------------------------------------------------

// The loop is the part the method-level tests above do not reach: it has to wake
// on its own timer, fill, and return when the context ends without leaking the
// goroutine. It runs on the real clock with millisecond latencies, because what
// is being tested is the scheduling itself.
func TestTheRunLoopFillsAndStopsOnCancellation(t *testing.T) {
	fill := testFill()
	fill.Latency = time.Millisecond
	fill.BookPoll = 5 * time.Millisecond

	books := newBooks(time.Now().UTC(), "2344.50", "2345.00")
	sink := &fakeSink{}
	out := &recordingVenue{}
	e := newEngine(engineOptions{Product: testPerp, Fill: fill, Seed: testSeed},
		books, sink, out, NewMetrics(prometheus.NewRegistry()), testLogger())

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- e.run(ctx) }()

	if err := e.submit(submission{
		clOrdID: "A1", symbol: testPerp, side: buy, tif: gtc,
		ordType: enum.OrdType_LIMIT, qty: dec("3"), limitPx: dec("2350.00"),
		at: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("submit: %v", err)
	}

	deadline := time.After(5 * time.Second)
	for len(sink.fills()) == 0 {
		select {
		case err := <-done:
			t.Fatalf("the loop returned before filling: %v", err)
		case <-deadline:
			t.Fatal("no fill after five seconds: the loop is not waking on its own timer")
		case <-time.After(time.Millisecond):
		}
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("cancellation returned an error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the loop did not return after cancellation")
	}

	// And once it has stopped, a message arriving on a connection goroutine is
	// answered rather than blocking forever on a channel nothing is reading.
	if err := e.submit(submission{clOrdID: "A2"}); !errors.Is(err, errEngineStopped) {
		t.Errorf("submit after shutdown returned %v, want errEngineStopped", err)
	}
	if err := e.requestCancel(cancelRequest{clOrdID: "C1"}); !errors.Is(err, errEngineStopped) {
		t.Errorf("cancel after shutdown returned %v, want errEngineStopped", err)
	}
}
