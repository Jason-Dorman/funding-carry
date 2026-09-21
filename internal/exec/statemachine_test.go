package exec

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/shopspring/decimal"

	"github.com/Jason-Dorman/funding-carry/internal/carry"
)

// The rule table for API spec section 3.2: NEW -> PARTIAL* -> FILLED |
// CANCELED | REJECTED, sequenced by CumQty, duplicates idempotent. A reviewer
// should be able to reconstruct the state machine from these cases (testing
// strategy).

var epoch = time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)

func dec(s string) decimal.Decimal { return decimal.RequireFromString(s) }

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

const testVenue = "sim"

// at builds a Status in a given state with a given progress, for an order of
// three contracts.
func at(state carry.ExecState, cum string) Status {
	s := Status{
		ClOrdID:   "A1",
		Qty:       dec("3"),
		State:     state,
		CumQty:    dec(cum),
		LeavesQty: dec("3").Sub(dec(cum)),
	}
	if s.Terminal() {
		s.LeavesQty = decimal.Zero
	}
	return s
}

// rep builds a report for the same order.
func rep(state carry.ExecState, cum, leaves string) carry.ExecReport {
	return carry.ExecReport{
		ClOrdID:   "A1",
		VenueID:   "V1",
		State:     state,
		CumQty:    dec(cum),
		LeavesQty: dec(leaves),
		At:        epoch,
	}
}

func TestTransitions(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		cur     Status
		report  carry.ExecReport
		want    Outcome
		state   carry.ExecState // expected state afterwards
		cumQty  string
		reason  string
		venueID string
	}{
		// The happy path.
		{"pending acknowledged", at(StatePending, "0"), rep(carry.StateNew, "0", "3"),
			OutcomeApplied, carry.StateNew, "0", "", "V1"},
		{"new partially filled", at(carry.StateNew, "0"), rep(carry.StatePartial, "1", "2"),
			OutcomeApplied, carry.StatePartial, "1", "", "V1"},
		{"partial fills more", at(carry.StatePartial, "1"), rep(carry.StatePartial, "2", "1"),
			OutcomeApplied, carry.StatePartial, "2", "", "V1"},
		{"partial completes", at(carry.StatePartial, "2"), rep(carry.StateFilled, "3", "0"),
			OutcomeApplied, carry.StateFilled, "3", "", "V1"},
		{"new fills at once", at(carry.StateNew, "0"), rep(carry.StateFilled, "3", "0"),
			OutcomeApplied, carry.StateFilled, "3", "", "V1"},
		{"new canceled", at(carry.StateNew, "0"), withReason(rep(carry.StateCanceled, "0", "0"), "on request"),
			OutcomeApplied, carry.StateCanceled, "0", "on request", "V1"},
		{"partial canceled keeps the fill", at(carry.StatePartial, "1"), rep(carry.StateCanceled, "1", "0"),
			OutcomeApplied, carry.StateCanceled, "1", "", "V1"},
		{"rejected at entry", at(StatePending, "0"), withReason(rep(carry.StateRejected, "0", "0"), "no market data"),
			OutcomeApplied, carry.StateRejected, "0", "no market data", "V1"},
		{"rejected after ack", at(carry.StateNew, "0"), rep(carry.StateRejected, "0", "0"),
			OutcomeApplied, carry.StateRejected, "0", "", "V1"},

		// Fills straight from pending: the acknowledgement crossed the fill on
		// the wire, and the fill is the more advanced fact.
		{"pending partially filled", at(StatePending, "0"), rep(carry.StatePartial, "1", "2"),
			OutcomeApplied, carry.StatePartial, "1", "", "V1"},

		// Out of order: sequenced by CumQty, never by arrival.
		{"partial after a later partial is stale", at(carry.StatePartial, "2"), rep(carry.StatePartial, "1", "2"),
			OutcomeStale, carry.StatePartial, "2", "", ""},
		{"new after a partial is stale", at(carry.StatePartial, "1"), rep(carry.StateNew, "0", "3"),
			OutcomeStale, carry.StatePartial, "1", "", ""},
		{"partial after filled is late", at(carry.StateFilled, "3"), rep(carry.StatePartial, "2", "1"),
			OutcomeLate, carry.StateFilled, "3", "", ""},
		{"new after canceled is late", at(carry.StateCanceled, "0"), rep(carry.StateNew, "0", "3"),
			OutcomeLate, carry.StateCanceled, "0", "", ""},
		{"cancel after filled is late", at(carry.StateFilled, "3"), rep(carry.StateCanceled, "3", "0"),
			OutcomeLate, carry.StateFilled, "3", "", ""},

		// Duplicates: a resend replays what was already applied.
		{"repeated new", at(carry.StateNew, "0"), rep(carry.StateNew, "0", "3"),
			OutcomeDuplicate, carry.StateNew, "0", "", ""},
		{"repeated partial", at(carry.StatePartial, "2"), rep(carry.StatePartial, "2", "1"),
			OutcomeDuplicate, carry.StatePartial, "2", "", ""},
		{"repeated filled", at(carry.StateFilled, "3"), rep(carry.StateFilled, "3", "0"),
			OutcomeDuplicate, carry.StateFilled, "3", "", ""},
		{"repeated canceled", at(carry.StateCanceled, "1"), rep(carry.StateCanceled, "1", "0"),
			OutcomeDuplicate, carry.StateCanceled, "1", "", ""},

		// Reports the order cannot have produced.
		{"filled beyond the order", at(carry.StatePartial, "2"), rep(carry.StateFilled, "4", "0"),
			OutcomeInvalid, carry.StatePartial, "2", "", ""},
		{"partial that moved nothing", at(carry.StateNew, "0"), rep(carry.StatePartial, "0", "3"),
			OutcomeInvalid, carry.StateNew, "0", "", ""},
		{"filled with quantity left", at(carry.StatePartial, "2"), rep(carry.StateFilled, "3", "1"),
			OutcomeInvalid, carry.StatePartial, "2", "", ""},
		{"cancel losing a fill", at(carry.StatePartial, "2"), rep(carry.StateCanceled, "1", "0"),
			OutcomeInvalid, carry.StatePartial, "2", "", ""},
		{"cancel claiming more than was ordered", at(carry.StatePartial, "2"), rep(carry.StateCanceled, "4", "0"),
			OutcomeInvalid, carry.StatePartial, "2", "", ""},
		// A REJECTED used to skip every quantity check, so a venue could drive
		// an order terminal with any number at all while the identical numbers
		// on a CANCELED were refused.
		{"reject claiming more than was ordered", at(carry.StateNew, "0"), rep(carry.StateRejected, "1000000", "0"),
			OutcomeInvalid, carry.StateNew, "0", "", ""},
		{"reject losing a fill", at(carry.StatePartial, "2"), rep(carry.StateRejected, "1", "0"),
			OutcomeInvalid, carry.StatePartial, "2", "", ""},
		{"reject after a partial keeps the fill", at(carry.StatePartial, "2"), rep(carry.StateRejected, "2", "0"),
			OutcomeApplied, carry.StateRejected, "2", "", "V1"},
		{"unknown state", at(carry.StateNew, "0"), rep(carry.ExecState("EXPIRED"), "0", "0"),
			OutcomeInvalid, carry.StateNew, "0", "", ""},

		// An adopted order has no known quantity, so nothing bounds its fills.
		{"adopted order fills without a bound", Status{ClOrdID: "A1", State: StatePending},
			rep(carry.StateFilled, "35", "0"), OutcomeApplied, carry.StateFilled, "35", "", "V1"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, outcome := transition(c.cur, c.report)
			if outcome != c.want {
				t.Fatalf("outcome %s, want %s", outcome, c.want)
			}
			if got.State != c.state {
				t.Errorf("state %s, want %s", got.State, c.state)
			}
			if !got.CumQty.Equal(dec(c.cumQty)) {
				t.Errorf("cum_qty %s, want %s", got.CumQty, c.cumQty)
			}
			if got.Reason != c.reason {
				t.Errorf("reason %q, want %q", got.Reason, c.reason)
			}
			if c.venueID != "" && got.VenueID != c.venueID {
				t.Errorf("venue id %q, want %q", got.VenueID, c.venueID)
			}
			if got.Terminal() && !got.LeavesQty.IsZero() {
				t.Errorf("terminal with leaves %s, want 0", got.LeavesQty)
			}
			if outcome != OutcomeApplied && (got.State != c.cur.State || !got.CumQty.Equal(c.cur.CumQty)) {
				t.Error("a report that was not applied still moved the order")
			}
		})
	}
}

// The guarantee the table's individual cases add up to: no applied transition
// ever moves an order's filled quantity backwards, in any state, for any
// report. A clamp inside ended() used to assert this defensively and no test
// could reach it; the guarantee is asserted here instead, where it is the
// property that actually matters.
func TestAnAppliedReportNeverLowersTheFilledQuantity(t *testing.T) {
	t.Parallel()

	states := []carry.ExecState{StatePending, carry.StateNew, carry.StatePartial}
	reports := []carry.ExecState{
		carry.StateNew, carry.StatePartial, carry.StateFilled, carry.StateCanceled, carry.StateRejected,
	}
	for _, from := range states {
		for _, cum := range []string{"0", "1", "2", "3"} {
			for _, rs := range reports {
				for _, rc := range []string{"0", "1", "2", "3", "4"} {
					cur := at(from, cum)
					if from == StatePending && cum != "0" {
						continue
					}
					got, outcome := transition(cur, rep(rs, rc, "0"))
					if outcome != OutcomeApplied {
						continue
					}
					if got.CumQty.LessThan(cur.CumQty) {
						t.Errorf("%s(cum %s) + %s(cum %s) applied and lowered the filled quantity to %s",
							from, cum, rs, rc, got.CumQty)
					}
					if got.Terminal() && !got.LeavesQty.IsZero() {
						t.Errorf("%s(cum %s) + %s(cum %s) is terminal with leaves %s",
							from, cum, rs, rc, got.LeavesQty)
					}
				}
			}
		}
	}
}

func withReason(r carry.ExecReport, reason string) carry.ExecReport {
	r.Reason = reason
	return r
}

// The full sequence, through the Tracker: an order tracked, acknowledged,
// filled in two partials, and every one of those reports replayed by a resend
// afterwards, all recognised by ExecID.
func TestTrackerAppliesOnceAndRecognisesAResend(t *testing.T) {
	t.Parallel()

	tr, reg := newTracker(t, time.Minute)
	order := carry.Order{ClOrdID: "A1", Leg: carry.LegPerp, Side: carry.Sell, Qty: dec("3")}
	if err := tr.Track(order, carry.Ack{ClOrdID: "A1", At: epoch}); err != nil {
		t.Fatal(err)
	}

	sequence := []carry.ExecReport{
		withExec(rep(carry.StateNew, "0", "3"), "E1", epoch.Add(10*time.Millisecond)),
		withExec(rep(carry.StatePartial, "1", "2"), "E2", epoch.Add(20*time.Millisecond)),
		withExec(rep(carry.StateFilled, "3", "0"), "E3", epoch.Add(30*time.Millisecond)),
	}
	for _, r := range sequence {
		if _, outcome := tr.Apply(r); outcome != OutcomeApplied {
			t.Fatalf("%s: outcome %s, want applied", r.ExecID, outcome)
		}
	}
	// The resend: every report again, same ExecIDs.
	for _, r := range sequence {
		if _, outcome := tr.Apply(r); outcome != OutcomeDuplicate {
			t.Fatalf("replayed %s: outcome %s, want duplicate", r.ExecID, outcome)
		}
	}

	s, ok := tr.Status("A1")
	if !ok || s.State != carry.StateFilled || !s.CumQty.Equal(dec("3")) {
		t.Fatalf("status %+v, want FILLED with cum 3", s)
	}
	if got := testutil.ToFloat64(tr.m.reports.WithLabelValues(testVenue, string(OutcomeDuplicate))); got != 3 {
		t.Errorf("carry_exec_reports_total{duplicate} = %v, want 3", got)
	}
	// The round trip is observed once, on the terminal report, and measured
	// from the acknowledgement to the venue's own time on that report.
	if n := testutil.CollectAndCount(reg, "carry_order_roundtrip_seconds"); n != 1 {
		t.Errorf("carry_order_roundtrip_seconds: %d series, want 1", n)
	}
	if got := sampleCount(t, reg, "carry_order_roundtrip_seconds"); got != 1 {
		t.Errorf("round trip observed %d times, want once", got)
	}
}

// The round trip is measured from the acknowledgement to the terminal report's
// venue time, and NOTHING asserted the value until the Part 8 adversarial
// review pointed out that replacing the observation with a constant zero left
// the whole suite green.
func TestTheRoundTripMeasuresAcknowledgementToTerminalReport(t *testing.T) {
	t.Parallel()

	tr, reg := newTracker(t, time.Minute)
	if err := tr.Track(carry.Order{ClOrdID: "A1", Qty: dec("3")}, carry.Ack{At: epoch}); err != nil {
		t.Fatal(err)
	}
	tr.Apply(withExec(rep(carry.StateNew, "0", "3"), "E1", epoch.Add(10*time.Millisecond)))
	tr.Apply(withExec(rep(carry.StateFilled, "3", "0"), "E2", epoch.Add(1500*time.Millisecond)))

	if got, want := sampleSum(t, reg, "carry_order_roundtrip_seconds"), 1.5; got != want {
		t.Errorf("observed %v seconds, want %v (the ack at T+0 to the terminal report at T+1.5s)", got, want)
	}
}

// An adopted order has no acknowledgement, so it has no round trip. Observing
// the zero its two identical timestamps produce would file every restart
// recovery into the fastest bucket in the histogram, and an outage would read
// as a fast venue.
func TestAnAdoptedOrderContributesNoRoundTrip(t *testing.T) {
	t.Parallel()

	tr, reg := newTracker(t, time.Minute)
	_, outcome := tr.Apply(withExec(rep(carry.StateFilled, "3", "0"), "E1", epoch))
	if outcome != OutcomeAdopted {
		t.Fatalf("outcome %s, want adopted", outcome)
	}
	if n := sampleCount(t, reg, "carry_order_roundtrip_seconds"); n != 0 {
		t.Fatalf("%d round trips observed for an adopted order, want none: its SubmittedAt is "+
			"the report's own time, so the only duration it can produce is a fabricated zero", n)
	}
	if got := sampleSum(t, reg, "carry_order_roundtrip_seconds"); got != 0 {
		t.Errorf("round-trip sum %v, want 0", got)
	}
}

// The race the probe hit live: the venue's acknowledgement crosses the socket
// before the submitter can register the order. Track completes what adoption
// could not know rather than refusing an order that is live at the venue.
func TestTrackCompletesAnOrderAlreadyAdopted(t *testing.T) {
	t.Parallel()

	tr, reg := newTracker(t, time.Minute)
	// The NEW arrives first, for an order the tracker has not been told about.
	adopted, outcome := tr.Apply(withExec(rep(carry.StateNew, "0", "3"), "E1", epoch.Add(5*time.Millisecond)))
	if outcome != OutcomeAdopted || !adopted.Adopted {
		t.Fatalf("outcome %s adopted=%v, want an adopted order", outcome, adopted.Adopted)
	}
	if !adopted.Qty.IsZero() {
		t.Fatalf("an adopted order knows the ordered quantity (%s); it cannot", adopted.Qty)
	}

	// The submitter catches up.
	order := carry.Order{ClOrdID: "A1", Qty: dec("3")}
	if err := tr.Track(order, carry.Ack{ClOrdID: "A1", VenueID: "V1", At: epoch}); err != nil {
		t.Fatalf("Track after adoption: %v, want the order to be completed", err)
	}
	s, _ := tr.Status("A1")
	if s.Adopted {
		t.Error("still marked adopted after its submitter registered it")
	}
	if !s.Qty.Equal(dec("3")) {
		t.Errorf("qty %s after completion, want the 3 that was ordered", s.Qty)
	}
	if !s.SubmittedAt.Equal(epoch) {
		t.Errorf("submitted_at %v, want the acknowledgement's %v, not the report's", s.SubmittedAt, epoch)
	}
	if !s.Deadline.Equal(epoch.Add(time.Minute)) {
		t.Errorf("deadline %v, want it measured from the acknowledgement", s.Deadline)
	}

	// The quantity guard is live again: 4 filled on an order of 3 is refused,
	// which it could not be while the order was adopted.
	if _, outcome := tr.Apply(withExec(rep(carry.StateFilled, "4", "0"), "E2", epoch)); outcome != OutcomeInvalid {
		t.Errorf("filled beyond the completed order: %s, want invalid", outcome)
	}

	// And the round trip, skipped at adoption, is observed once the order ends.
	tr.Apply(withExec(rep(carry.StateFilled, "3", "0"), "E3", epoch.Add(2*time.Second)))
	if got, want := sampleSum(t, reg, "carry_order_roundtrip_seconds"), 2.0; got != want {
		t.Errorf("round trip %v seconds, want %v", got, want)
	}
}

// Tracking a live (non-adopted) order twice is still a caller error.
// A duplicate ExecID is a duplicate even when its contents would otherwise
// read as progress: the venue said this already, and a venue does not say the
// same execution twice with different numbers.
func TestTrackerTrustsExecIDOverContents(t *testing.T) {
	t.Parallel()

	tr, _ := newTracker(t, time.Minute)
	if err := tr.Track(carry.Order{ClOrdID: "A1", Qty: dec("3")}, carry.Ack{At: epoch}); err != nil {
		t.Fatal(err)
	}
	tr.Apply(withExec(rep(carry.StatePartial, "1", "2"), "E1", epoch))
	if _, outcome := tr.Apply(withExec(rep(carry.StatePartial, "2", "1"), "E1", epoch)); outcome != OutcomeDuplicate {
		t.Fatalf("outcome %s, want duplicate", outcome)
	}
}

// After a restart the tracker is empty and the venue's book is not. A report
// for an order it was never told about is adopted, not dropped: the fill
// happened whether or not this process placed the order.
func TestTrackerAdoptsAnUnknownOrder(t *testing.T) {
	t.Parallel()

	tr, _ := newTracker(t, time.Minute)
	s, outcome := tr.Apply(withExec(rep(carry.StateFilled, "3", "0"), "E1", epoch))
	if outcome != OutcomeAdopted {
		t.Fatalf("outcome %s, want adopted", outcome)
	}
	if s.State != carry.StateFilled || !s.CumQty.Equal(dec("3")) {
		t.Errorf("adopted as %+v, want FILLED cum 3", s)
	}
	if !s.SubmittedAt.Equal(epoch) {
		t.Errorf("submitted_at %v, want the report's time %v", s.SubmittedAt, epoch)
	}
	// Adopted once; a second report for the same order is ordinary.
	if _, outcome := tr.Apply(withExec(rep(carry.StateFilled, "3", "0"), "E1", epoch)); outcome != OutcomeDuplicate {
		t.Errorf("replay after adoption: %s, want duplicate", outcome)
	}
}

func TestTrackerRefusesToTrackTheSameOrderTwice(t *testing.T) {
	t.Parallel()

	tr, _ := newTracker(t, time.Minute)
	o := carry.Order{ClOrdID: "A1", Qty: dec("1")}
	if err := tr.Track(o, carry.Ack{At: epoch}); err != nil {
		t.Fatal(err)
	}
	if err := tr.Track(o, carry.Ack{At: epoch}); !errors.Is(err, ErrAlreadyTracked) {
		t.Fatalf("second Track: %v, want ErrAlreadyTracked", err)
	}
}

// The per-order timeout bounds the ACKNOWLEDGEMENT, not the fill (a PO
// decision after the Part 8 review): an order the venue has not answered by
// its deadline is overdue; an acknowledged order resting past it is not. The
// tracker answers the question and takes no action; the list comes back
// oldest first.
func TestTrackerReportsOverdueOrders(t *testing.T) {
	t.Parallel()

	tr, reg := newTracker(t, 30*time.Second)
	for i, id := range []carry.OrderID{"A1", "A2", "A3", "A4"} {
		submitted := epoch.Add(time.Duration(i) * time.Second)
		if err := tr.Track(carry.Order{ClOrdID: id, Qty: dec("1")}, carry.Ack{At: submitted}); err != nil {
			t.Fatal(err)
		}
	}
	// A3 is acknowledged and rests; A4 fills. A1 and A2 never hear back.
	ack := withExec(rep(carry.StateNew, "0", "1"), "E1", epoch.Add(3*time.Second))
	ack.ClOrdID = "A3"
	tr.Apply(ack)
	fill := withExec(rep(carry.StateFilled, "1", "0"), "E2", epoch.Add(4*time.Second))
	fill.ClOrdID = "A4"
	tr.Apply(fill)

	if got := tr.Overdue(epoch.Add(29 * time.Second)); len(got) != 0 {
		t.Fatalf("overdue before any deadline: %d orders", len(got))
	}
	got := tr.Overdue(epoch.Add(31 * time.Second))
	if len(got) != 1 || got[0].ClOrdID != "A1" {
		t.Fatalf("overdue at +31s: %+v, want only A1", got)
	}
	got = tr.Overdue(epoch.Add(time.Hour))
	if len(got) != 2 || got[0].ClOrdID != "A1" || got[1].ClOrdID != "A2" {
		t.Fatalf("overdue at +1h: %+v, want A1 then A2 — A3 was acknowledged and rests, "+
			"which is the order working, not the venue failing to answer", got)
	}
	if open := tr.Open(); len(open) != 3 {
		t.Errorf("%d open orders, want 3 (A1, A2 unacknowledged; A3 resting)", len(open))
	}

	// The gauges are what makes the timeout measurable rather than guessed,
	// and they are read here by their EXPORTED names out of the registry. The
	// first version read tr.m.open and tr.m.overdue, which is the code
	// comparing itself to itself: swapping the two Name fields in NewMetrics
	// published each count under the other's name and left the suite green.
	// Found by the Part 8 follow-up adversarial review.
	tr.Observe(epoch.Add(time.Hour))
	if got := gauge(t, reg, "carry_orders_open"); got != 3 {
		t.Errorf("carry_orders_open = %v, want 3", got)
	}
	if got := gauge(t, reg, "carry_orders_overdue"); got != 2 {
		t.Errorf("carry_orders_overdue = %v, want 2", got)
	}
	if n := testutil.CollectAndCount(reg, "carry_orders_overdue"); n != 1 {
		t.Errorf("carry_orders_overdue: %d series, want 1", n)
	}
}

// An adopted order arrived WITH its acknowledgement, so it can never be
// overdue, whatever its deadline — for EVERY shape of first report, which is
// the half the first version of this test missed. It only tried a report that
// applies; a report that is refused also creates a record, and that record
// stayed PENDING and therefore overdue forever.
func TestAnAdoptedOrderIsNeverOverdue(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name  string
		first carry.ExecReport
	}{
		{"acknowledged", rep(carry.StateNew, "0", "3")},
		{"already filled, the restart-and-resend case", rep(carry.StateFilled, "3", "0")},
		{"refused: a partial that filled nothing", rep(carry.StatePartial, "0", "3")},
		{"refused: a state the machine has no rule for", rep(carry.ExecState("PENDING_CANCEL"), "0", "3")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tr, _ := newTracker(t, time.Second)
			tr.Apply(withExec(tt.first, "E1", epoch))
			if got := tr.Overdue(epoch.Add(time.Hour)); len(got) != 0 {
				t.Fatalf("an adopted order reported overdue: %+v", got)
			}
		})
	}
}

// A report the tracker refuses leaves NOTHING behind. Adoption has to create
// the record before the rule table can judge the report, so a refused first
// report would otherwise leave that record PENDING for the life of the
// process: permanently open, permanently overdue under the acknowledgement
// deadline, and unremovable now that retention is process lifetime and Forget
// is gone. One malformed wire message would have pinned carry_orders_overdue
// at 1 and handed Part 15's router a standing instruction to unwind the other
// leg of an entry for an order that exists at no venue. Found by the Part 8
// follow-up adversarial review.
func TestARefusedFirstReportLeavesNoOrderBehind(t *testing.T) {
	t.Parallel()

	tr, reg := newTracker(t, time.Second)
	// A PARTIAL that moved nothing: the venue disagreeing with itself, which
	// is the outcome this part promoted to an invariant violation.
	if _, outcome := tr.Apply(withExec(rep(carry.StatePartial, "0", "3"), "E1", epoch)); outcome != OutcomeInvalid {
		t.Fatalf("outcome %q, want invalid", outcome)
	}
	if _, known := tr.Status("A1"); known {
		t.Error("the refused report left a phantom order behind")
	}
	if open := tr.Open(); len(open) != 0 {
		t.Errorf("%d open orders after a refused report: %+v", len(open), open)
	}
	tr.Observe(epoch.Add(time.Hour))
	for name, want := range map[string]float64{"carry_orders_open": 0, "carry_orders_overdue": 0} {
		if got := gauge(t, reg, name); got != want {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
	// The report is still counted and still logged — refusing it is not
	// forgetting it happened.
	if got, _ := counterValue(t, reg, "carry_exec_reports_total", "outcome", "invalid"); got != 1 {
		t.Errorf("carry_exec_reports_total{outcome=\"invalid\"} = %v, want 1", got)
	}
}

// api-spec section 6 documents carry_orders_open as refreshed "on every report
// and every second". This is the every-report half, and nothing else asserts
// it: every other gauge assertion calls Observe first, so deleting both
// per-report refreshes left the suite green. Found by the Part 8 follow-up
// adversarial review.
func TestTheOpenGaugeIsRefreshedOnEveryReport(t *testing.T) {
	t.Parallel()

	tr, reg := newTracker(t, 30*time.Second)
	if err := tr.Track(carry.Order{ClOrdID: "A1", Qty: dec("3")}, carry.Ack{At: epoch}); err != nil {
		t.Fatal(err)
	}
	if got := gauge(t, reg, "carry_orders_open"); got != 1 {
		t.Fatalf("carry_orders_open after Track = %v, want 1 — no Observe is called in this test", got)
	}
	tr.Apply(withExec(rep(carry.StatePartial, "1", "2"), "E1", epoch.Add(time.Second)))
	if got := gauge(t, reg, "carry_orders_open"); got != 1 {
		t.Errorf("carry_orders_open while partially filled = %v, want 1", got)
	}
	tr.Apply(withExec(rep(carry.StateFilled, "3", "0"), "E2", epoch.Add(2*time.Second)))
	if got := gauge(t, reg, "carry_orders_open"); got != 0 {
		t.Errorf("carry_orders_open after the fill = %v, want 0", got)
	}
}

// PO decision 3: an invalid report is an invariant violation, not a warning.
// Until Part 13 gives it a risk_events row the log level IS the decision, so
// the level is what this asserts — reverting Error to Warn, or deleting the
// case so invalid falls back to the Warn default, left the suite green. Found
// by the Part 8 follow-up adversarial review.
func TestAnInvalidReportIsLoggedAsAnInvariantViolation(t *testing.T) {
	t.Parallel()

	var logged bytes.Buffer
	reg := prometheus.NewRegistry()
	tr := NewTracker(Options{Venue: testVenue, Timeout: time.Second}, NewMetrics(reg),
		slog.New(slog.NewTextHandler(&logged, nil)))
	if err := tr.Track(carry.Order{ClOrdID: "A1", Qty: dec("3")}, carry.Ack{At: epoch}); err != nil {
		t.Fatal(err)
	}
	tr.Apply(withExec(rep(carry.StatePartial, "2", "1"), "E1", epoch))

	// More filled than was ordered: the venue's reports disagree with
	// themselves, which is the one thing a delta-neutral book cannot carry.
	logged.Reset()
	if _, outcome := tr.Apply(withExec(rep(carry.StateFilled, "4", "0"), "E2", epoch)); outcome != OutcomeInvalid {
		t.Fatalf("outcome %q, want invalid", outcome)
	}
	if got := logged.String(); !strings.Contains(got, "level=ERROR") {
		t.Errorf("an invalid report logged as:\n%s\nwant level=ERROR", got)
	}

	// A stale report, by contrast, is a protocol artefact and stays a warning:
	// the level distinguishes the two, so both halves are asserted.
	logged.Reset()
	if _, outcome := tr.Apply(withExec(rep(carry.StatePartial, "1", "2"), "E3", epoch)); outcome != OutcomeStale {
		t.Fatalf("outcome %q, want stale", outcome)
	}
	if got := logged.String(); !strings.Contains(got, "level=WARN") {
		t.Errorf("a stale report logged as:\n%s\nwant level=WARN", got)
	}
}

// The buckets are part of the catalogue (api-spec section 6), not an
// implementation detail: they are what decides whether the distribution can
// answer "is 30 s the right deadline". Default buckets would stop at 10 s,
// one side of the value being defended.
func TestAcknowledgementBucketsMatchTheCatalogue(t *testing.T) {
	t.Parallel()

	tr, reg := newTracker(t, 30*time.Second)
	if err := tr.Track(carry.Order{ClOrdID: "A1", Qty: dec("3"), Leg: carry.LegPerp},
		carry.Ack{At: epoch}); err != nil {
		t.Fatal(err)
	}
	tr.Apply(withExec(rep(carry.StateNew, "0", "3"), "E1", epoch.Add(time.Second)))

	want := []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30, 60}
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var got []float64
	for _, f := range families {
		if f.GetName() != "carry_order_ack_seconds" {
			continue
		}
		for _, b := range f.GetMetric()[0].GetHistogram().GetBucket() {
			got = append(got, b.GetUpperBound())
		}
	}
	if len(got) != len(want) {
		t.Fatalf("carry_order_ack_seconds has %d buckets %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("bucket %d is le=%v, want le=%v", i, got[i], want[i])
		}
	}
}

// The two instants are both OUR clock: the Ack we returned from Submit, and
// the moment the report reached this process. The venue's own TransactTime is
// the wrong far end and this is the case that shows it — a report RESENT after
// a restart carries the original event's time, which here is the instant of
// submission itself, so measuring to r.At would call a forty-five-second wait
// an instant answer. (PO revision to CHANGE-002.)
func TestAcknowledgementLatencyIsMeasuredOnOurClock(t *testing.T) {
	t.Parallel()

	tr, reg := newTracker(t, 30*time.Second)
	if err := tr.Track(carry.Order{ClOrdID: "A1", Qty: dec("3"), Leg: carry.LegPerp},
		carry.Ack{At: epoch}); err != nil {
		t.Fatal(err)
	}
	answer := withExec(rep(carry.StateNew, "0", "3"), "E1", epoch)
	answer.ReceivedAt = epoch.Add(45 * time.Second)
	tr.Apply(answer)

	n, sum, found := histogramFor(t, reg, "carry_order_ack_seconds", "leg", "perp")
	if !found || n != 1 || sum != 45 {
		t.Errorf("carry_order_ack_seconds: %d observations summing to %v, want 1 at 45 — "+
			"the venue's TransactTime says the event happened at submission, but the report "+
			"did not reach this process for forty-five seconds, and that is the wait the "+
			"deadline races", n, sum)
	}
}

// A venue that stamps no receipt time contributes nothing rather than a wrong
// number. Falling back to the venue's clock would put a silent second
// measurement into the one distribution the timeout is set from.
func TestAReportWithNoReceiptTimeContributesNoAcknowledgementLatency(t *testing.T) {
	t.Parallel()

	tr, reg := newTracker(t, 30*time.Second)
	if err := tr.Track(carry.Order{ClOrdID: "A1", Qty: dec("3"), Leg: carry.LegPerp},
		carry.Ack{At: epoch}); err != nil {
		t.Fatal(err)
	}
	unstamped := rep(carry.StateNew, "0", "3")
	unstamped.ExecID, unstamped.At = "E1", epoch.Add(250*time.Millisecond)
	if _, outcome := tr.Apply(unstamped); outcome != OutcomeApplied {
		t.Fatalf("outcome %q, want applied — the report is still a good report", outcome)
	}
	if n, _, found := histogramFor(t, reg, "carry_order_ack_seconds", "leg", "perp"); found {
		t.Errorf("%d acknowledgement latencies observed from a report with no receipt time", n)
	}
}

// CHANGE-002. The acknowledgement latency is the distribution ORDER_TIMEOUT
// is meant to be set from, so it is measured over exactly the edge the
// deadline judges: the report that takes an order out of PENDING. Every shape
// of first answer counts, because measuring only a NEW would leave out the
// orders the venue answered fastest — the ones that came back already filled.
func TestAcknowledgementLatencyIsObserved(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name  string
		first carry.ExecReport
	}{
		{"a NEW is the venue's answer", rep(carry.StateNew, "0", "3")},
		{"so is a first report that already filled", rep(carry.StateFilled, "3", "0")},
		{"so is a rejection", rep(carry.StateRejected, "0", "0")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tr, reg := newTracker(t, 30*time.Second)
			if err := tr.Track(carry.Order{ClOrdID: "A1", Qty: dec("3"), Leg: carry.LegPerp},
				carry.Ack{At: epoch}); err != nil {
				t.Fatal(err)
			}
			tr.Apply(withExec(tt.first, "E1", epoch.Add(250*time.Millisecond)))

			n, sum, found := histogramFor(t, reg, "carry_order_ack_seconds", "leg", "perp")
			if !found {
				t.Fatal("carry_order_ack_seconds{leg=\"perp\"}: not in the registry")
			}
			if n != 1 || sum != 0.25 {
				t.Errorf("carry_order_ack_seconds: %d observations summing to %v, want 1 at 0.25", n, sum)
			}
			if _, ok := counterValue(t, reg, "carry_order_ack_timeouts_total", "leg", "perp"); ok {
				t.Error("an acknowledged order was also counted as a timeout")
			}
		})
	}
}

// Only the FIRST answer is the acknowledgement. The reports that follow it are
// the order working, and folding them in would turn a latency distribution
// into a fill-progress one.
func TestOnlyTheFirstAnswerIsAnAcknowledgement(t *testing.T) {
	t.Parallel()

	tr, reg := newTracker(t, 30*time.Second)
	if err := tr.Track(carry.Order{ClOrdID: "A1", Qty: dec("3"), Leg: carry.LegPerp},
		carry.Ack{At: epoch}); err != nil {
		t.Fatal(err)
	}
	tr.Apply(withExec(rep(carry.StateNew, "0", "3"), "E1", epoch.Add(250*time.Millisecond)))
	tr.Apply(withExec(rep(carry.StatePartial, "1", "2"), "E2", epoch.Add(time.Second)))
	tr.Apply(withExec(rep(carry.StateFilled, "3", "0"), "E3", epoch.Add(2*time.Second)))

	if n, sum, _ := histogramFor(t, reg, "carry_order_ack_seconds", "leg", "perp"); n != 1 || sum != 0.25 {
		t.Errorf("carry_order_ack_seconds: %d observations summing to %v, want 1 at 0.25", n, sum)
	}
}

// An adopted order has no acknowledgement to measure: its SubmittedAt is its
// own first report's time, so the interval would be a fabricated zero in the
// fastest bucket — the defect the Part 8 review found in the round-trip
// histogram, kept out of this one by construction.
func TestAnAdoptedOrderContributesNoAcknowledgementLatency(t *testing.T) {
	t.Parallel()

	tr, reg := newTracker(t, 30*time.Second)
	tr.Apply(withExec(rep(carry.StateNew, "0", "3"), "E1", epoch))

	for _, leg := range []string{"perp", "spot", ""} {
		if n, _, found := histogramFor(t, reg, "carry_order_ack_seconds", "leg", leg); found {
			t.Errorf("an adopted order observed %d acknowledgement latencies under leg=%q", n, leg)
		}
	}
}

// A deadline that passes unanswered is counted in the companion counter and
// never observed into the histogram: a synthetic value would corrupt the one
// distribution the timeout is meant to be set from. It is counted once per
// order rather than once per tick, and a response that turns up late is still
// measured at its true latency — so the two series overlap on exactly the
// late answers, which is what this test pins down.
func TestAnUnacknowledgedOrderIsCountedOnceAndStillMeasuredIfItArrives(t *testing.T) {
	t.Parallel()

	tr, reg := newTracker(t, 30*time.Second)
	if err := tr.Track(carry.Order{ClOrdID: "A1", Qty: dec("3"), Leg: carry.LegPerp},
		carry.Ack{At: epoch}); err != nil {
		t.Fatal(err)
	}

	tr.Observe(epoch.Add(29 * time.Second))
	if _, ok := counterValue(t, reg, "carry_order_ack_timeouts_total", "leg", "perp"); ok {
		t.Fatal("an order inside its deadline was counted as a timeout")
	}
	for _, at := range []time.Duration{31, 32, 33} {
		tr.Observe(epoch.Add(at * time.Second))
	}
	if got, _ := counterValue(t, reg, "carry_order_ack_timeouts_total", "leg", "perp"); got != 1 {
		t.Errorf("carry_order_ack_timeouts_total = %v after three observations, want 1 — "+
			"the count is per order that missed its deadline, not per tick", got)
	}
	if _, _, found := histogramFor(t, reg, "carry_order_ack_seconds", "leg", "perp"); found {
		t.Error("an unanswered order was observed into the latency histogram")
	}

	// The venue answers, late.
	tr.Apply(withExec(rep(carry.StateNew, "0", "3"), "E1", epoch.Add(45*time.Second)))
	n, sum, _ := histogramFor(t, reg, "carry_order_ack_seconds", "leg", "perp")
	if n != 1 || sum != 45 {
		t.Errorf("carry_order_ack_seconds after the late answer: %d observations summing to %v, want 1 at 45", n, sum)
	}
	if got, _ := counterValue(t, reg, "carry_order_ack_timeouts_total", "leg", "perp"); got != 1 {
		t.Errorf("carry_order_ack_timeouts_total = %v after the late answer, want 1", got)
	}
}

// The two series in API spec section 6, present from the start: every outcome
// at zero, and the histogram registered under the carry prefix.
func TestExportedSeriesMatchTheCatalogue(t *testing.T) {
	t.Parallel()

	_, reg := newTracker(t, time.Minute)

	for _, name := range []string{"carry_orders_open", "carry_orders_overdue"} {
		if n := testutil.CollectAndCount(reg, name); n != 1 {
			t.Errorf("%s: %d series before any order, want 1 at zero", name, n)
		}
	}
	// The two leg-labelled series are the deliberate exception (CHANGE-002):
	// a zero would advertise a leg=spot path v1 does not have, so they appear
	// with the first order of a leg instead.
	for _, name := range []string{"carry_order_ack_seconds", "carry_order_ack_timeouts_total"} {
		if n := testutil.CollectAndCount(reg, name); n != 0 {
			t.Errorf("%s: %d series before any order, want none until a leg is seen", name, n)
		}
	}

	// The outcome vocabulary is spelled out here, as API spec section 6 spells
	// it out, rather than taken from allOutcomes — which is the slice the
	// production code iterates to create the series. Comparing the code to
	// itself let the vocabulary drift from the catalogue with both sides moving
	// together, which the Part 8 adversarial review pointed out.
	const want = `
# HELP carry_exec_reports_total ExecReports by what the state machine did with them. duplicate rising after a reconnect is the resend recovery doing its job; stale, late and invalid are a venue whose reports disagree with themselves.
# TYPE carry_exec_reports_total counter
carry_exec_reports_total{outcome="adopted",venue="sim"} 0
carry_exec_reports_total{outcome="applied",venue="sim"} 0
carry_exec_reports_total{outcome="duplicate",venue="sim"} 0
carry_exec_reports_total{outcome="invalid",venue="sim"} 0
carry_exec_reports_total{outcome="late",venue="sim"} 0
carry_exec_reports_total{outcome="stale",venue="sim"} 0
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "carry_exec_reports_total"); err != nil {
		t.Error(err)
	}
	// The histogram exists at zero observations before any order, so a panel
	// over it shows an empty distribution rather than no data.
	if n := testutil.CollectAndCount(reg, "carry_order_roundtrip_seconds"); n != 1 {
		t.Errorf("carry_order_roundtrip_seconds: %d series before any order, want 1", n)
	}
	if got := sampleCount(t, reg, "carry_order_roundtrip_seconds"); got != 0 {
		t.Errorf("round trip observed %d times before any order, want 0", got)
	}
}

func newTracker(t *testing.T, timeout time.Duration) (*Tracker, *prometheus.Registry) {
	t.Helper()
	reg := prometheus.NewRegistry()
	tr := NewTracker(Options{Venue: testVenue, Timeout: timeout}, NewMetrics(reg), testLogger())
	return tr, reg
}

// withExec stamps a report with its id and its time. The venue's time and our
// receipt of it are set together, which is the ordinary case — they differ by
// a network hop. Where the difference is the point, a test sets ReceivedAt
// itself.
func withExec(r carry.ExecReport, execID string, at time.Time) carry.ExecReport {
	r.ExecID = execID
	r.At = at
	r.ReceivedAt = at
	return r
}

// gauge reads a gauge out of the registry BY ITS EXPORTED NAME, and insists
// there is exactly one series under it. Reading the struct field instead lets
// a test pass while the series is published under the wrong name.
func gauge(t *testing.T, reg *prometheus.Registry, name string) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		if n := len(f.GetMetric()); n != 1 {
			t.Fatalf("%s: %d series, want 1", name, n)
		}
		return f.GetMetric()[0].GetGauge().GetValue()
	}
	t.Fatalf("%s: no such series in the registry", name)
	return 0
}

// counterValue reads one labelled counter out of the registry. Absence is
// reported rather than fatal, because "this series does not exist yet" is
// itself something the tests assert.
func counterValue(t *testing.T, reg *prometheus.Registry, name, label, value string) (float64, bool) {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == label && l.GetValue() == value {
					return m.GetCounter().GetValue(), true
				}
			}
		}
	}
	return 0, false
}

// histogramFor reads one labelled histogram's count and sum out of the
// registry, and says whether the series exists at all.
func histogramFor(t *testing.T, reg *prometheus.Registry, name, label, value string) (uint64, float64, bool) {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == label && l.GetValue() == value {
					return m.GetHistogram().GetSampleCount(), m.GetHistogram().GetSampleSum(), true
				}
			}
		}
	}
	return 0, 0, false
}

// sampleSum reads a histogram's total observed value out of the registry.
func sampleSum(t *testing.T, reg *prometheus.Registry, name string) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		var sum float64
		for _, m := range f.GetMetric() {
			sum += m.GetHistogram().GetSampleSum()
		}
		return sum
	}
	return 0
}

// sampleCount reads a histogram's observation count out of the registry.
func sampleCount(t *testing.T, reg *prometheus.Registry, name string) uint64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		var n uint64
		for _, m := range f.GetMetric() {
			n += m.GetHistogram().GetSampleCount()
		}
		return n
	}
	return 0
}
