package fix

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// The names in API spec section 6, spelled out here so a rename has to be a
// deliberate edit in two places rather than a silent break of every alert and
// dashboard that reads them.
func TestExportedSeriesMatchTheCatalogue(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	m.session("FIX.4.4:SIMV->CARRY")

	// Four outcomes, all initialized: a rejection path that has never fired is a
	// visible zero rather than an absent series.
	if n := testutil.CollectAndCount(reg, "simv_orders_total"); n != 4 {
		t.Errorf("simv_orders_total: %d series, want 4 (accepted, rejected, filled, canceled)", n)
	}

	for _, name := range []string{
		"fix_session_up",
		"fix_resend_events_total",
		"simv_book_age_seconds",
		"simv_resting_orders",
	} {
		if n := testutil.CollectAndCount(reg, name); n != 1 {
			t.Errorf("%s: %d series, want 1", name, n)
		}
	}

	// fix_msgs_total is labelled per message type, so it has no series until a
	// message has travelled. That is correct — there is no "message type that
	// has not happened" worth alerting on — but it means the name is asserted by
	// exercising it.
	m.session("FIX.4.4:SIMV->CARRY").message("D", dirIn)
	if n := testutil.CollectAndCount(reg, "fix_msgs_total"); n != 1 {
		t.Errorf("fix_msgs_total: %d series after one message, want 1", n)
	}
}

// The three simv_ series are written from the engine, and nothing asserted that
// they move until the Part 7 adversarial review pointed out that no-op versions
// of all three passed the suite. A metric nobody checks is a dashboard panel
// that silently reads zero forever.
func TestTheSimulatorsSeriesFollowWhatItDoes(t *testing.T) {
	h := newHarness(t, testFill())

	// An order accepted and resting.
	h.order("A1", buy, "3", "2300.00", gtc)
	if got := testutil.ToFloat64(h.engine.m.orders.WithLabelValues(resultAccepted)); got != 1 {
		t.Errorf("simv_orders_total{accepted} = %v, want 1", got)
	}
	h.tick(t, past)
	if got := testutil.ToFloat64(h.engine.m.resting); got != 1 {
		t.Errorf("simv_resting_orders = %v with one order resting, want 1", got)
	}

	// Filled: it leaves the book and lands on the filled counter.
	h.books.set(h.clock.now(), "2299.50", "2300.00")
	h.tick(t, h.engine.fill.BookPoll)
	if got := testutil.ToFloat64(h.engine.m.orders.WithLabelValues(resultFilled)); got != 1 {
		t.Errorf("simv_orders_total{filled} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(h.engine.m.resting); got != 0 {
		t.Errorf("simv_resting_orders = %v after the fill, want 0", got)
	}

	// Refused, and cancelled.
	h.order("A2", buy, "1.5", "2300.00", gtc)
	if got := testutil.ToFloat64(h.engine.m.orders.WithLabelValues(resultRejected)); got != 1 {
		t.Errorf("simv_orders_total{rejected} = %v, want 1", got)
	}
	h.order("A3", buy, "1", "1000.00", gtc)
	h.cancel(t, "C1", "A3")
	if got := testutil.ToFloat64(h.engine.m.orders.WithLabelValues(resultCanceled)); got != 1 {
		t.Errorf("simv_orders_total{canceled} = %v, want 1", got)
	}

	// The age gauge tracks the snapshot in hand, measured against the venue
	// timestamp on the row rather than against when it was read.
	h.clock.advance(30 * time.Second)
	h.engine.readBook(h.ctx)
	want := h.clock.now().Sub(h.engine.book.ts).Seconds()
	if got := testutil.ToFloat64(h.engine.m.bookAge); got != want {
		t.Errorf("simv_book_age_seconds = %v, want %v (the age of the snapshot on the row)", got, want)
	}
	if want < 30 {
		t.Fatalf("the test advanced the clock 30s but the age is %v: the gauge is not "+
			"measuring against the row's own timestamp", want)
	}
}

// A venue that has never read a snapshot is infinitely stale, not perfectly
// fresh. Zero here would be the healthiest possible reading in the one state
// where every order is refused, and `simv_book_age_seconds > SIM_BOOK_MAX_AGE`
// — the obvious alert — would never fire on the worst case it exists for. Three
// of the six review lenses found this independently.
func TestTheBookAgeGaugeIsInfiniteBeforeAnySnapshot(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	m.observeBookAge(0, false)

	got := testutil.ToFloat64(m.bookAge)
	if !math.IsInf(got, 1) {
		t.Fatalf("simv_book_age_seconds = %v with no snapshot ever read, want +Inf", got)
	}
	if got <= 60 {
		t.Error("an alert on age > 60s would not fire in the state where every order is refused")
	}
}

// FixSessionDown alerts on `fix_session_up == 0`, and PromQL over a series that
// does not exist yields nothing rather than firing. A venue that has never
// accepted a connection at all is exactly the case the alert is for.
func TestTheSessionGaugeExistsBeforeAnythingConnects(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	NewMetrics(reg).session("FIX.4.4:SIMV->CARRY")

	const want = `
# HELP fix_session_up 1 while the FIX session is logged on, 0 otherwise.
# TYPE fix_session_up gauge
fix_session_up{session="FIX.4.4:SIMV->CARRY"} 0
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "fix_session_up"); err != nil {
		t.Error(err)
	}
}

func TestTheSessionGaugeTracksLogonAndLogout(t *testing.T) {
	t.Parallel()

	s := NewMetrics(prometheus.NewRegistry()).session("FIX.4.4:SIMV->CARRY")
	s.loggedOn()
	if got := testutil.ToFloat64(s.up); got != 1 {
		t.Errorf("fix_session_up = %v after logon, want 1", got)
	}
	s.loggedOff()
	if got := testutil.ToFloat64(s.up); got != 0 {
		t.Errorf("fix_session_up = %v after logout, want 0", got)
	}
}

// The fix_* names carry no binary prefix on purpose: carry's initiator (Part 8)
// exports the same three, and the session label is what separates them. A
// dashboard asking "is the FIX link up" should not have to ask it twice with two
// spellings.
func TestTheSessionSeriesAreNotPrefixedPerBinary(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	NewMetrics(reg).session("FIX.4.4:SIMV->CARRY")

	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		name := f.GetName()
		if strings.HasPrefix(name, "fix_") && strings.HasPrefix(name, Namespace+"_") {
			t.Errorf("%s carries the binary prefix; the initiator would export a second spelling", name)
		}
		if !strings.HasPrefix(name, "fix_") && !strings.HasPrefix(name, Namespace+"_") {
			t.Errorf("%s is outside the catalogue: it is neither a fix_ session series nor a %s_ one",
				name, Namespace)
		}
	}
}
