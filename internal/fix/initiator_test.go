package fix

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/quickfixgo/enum"
	"github.com/quickfixgo/field"
	"github.com/quickfixgo/fix44/executionreport"
	"github.com/quickfixgo/quickfix"
	"github.com/quickfixgo/tag"
	"github.com/shopspring/decimal"

	"github.com/Jason-Dorman/funding-carry/internal/carry"
	"github.com/Jason-Dorman/funding-carry/internal/config"
	"github.com/Jason-Dorman/funding-carry/internal/exec"
)

// Part 8's acceptance criteria, over a real socket against the Part 7
// acceptor: the initiator sends a NewOrderSingle, the reports arrive on the
// channel, the state machine reaches a terminal state — and, the one that
// matters, a fill that prints while the initiator is down is delivered by
// resend recovery when it comes back.

// venueUnderTest is a running Initiator with its Run goroutine and registry.
type venueUnderTest struct {
	t   *testing.T
	in  *Initiator
	reg *prometheus.Registry

	stopOnce sync.Once
	cancel   context.CancelFunc
	done     chan error
}

// startInitiator starts an Initiator against a session and waits for logon
// unless told not to.
func startInitiator(t *testing.T, session config.FIXSession, store string, waitLogon bool) *venueUnderTest {
	t.Helper()

	session.StorePath = store
	reg := prometheus.NewRegistry()
	in, err := NewInitiator(InitiatorOptions{Product: testPerp, Session: session},
		NewSessionCatalogue(reg), testLogger())
	if err != nil {
		t.Fatalf("new initiator: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- in.Run(ctx) }()

	v := &venueUnderTest{t: t, in: in, reg: reg, cancel: cancel, done: done}
	t.Cleanup(v.stop)
	if waitLogon {
		waitFor(t, in.LoggedOn, "the initiator to log on")
	}
	return v
}

// stop cancels Run and waits for it, which is the initiator's logout.
func (v *venueUnderTest) stop() {
	v.stopOnce.Do(func() {
		v.cancel()
		if err := <-v.done; err != nil {
			v.t.Errorf("initiator: %v", err)
		}
	})
}

// next is the next report on the channel, or a failure.
func (v *venueUnderTest) next(what string) carry.ExecReport {
	v.t.Helper()
	select {
	case r, ok := <-v.in.ExecReports():
		if !ok {
			v.t.Fatalf("the report channel closed while waiting for %s", what)
		}
		return r
	case <-time.After(10 * time.Second):
		v.t.Fatalf("timed out waiting for %s", what)
	}
	return carry.ExecReport{}
}

// none asserts nothing arrives within a window.
func (v *venueUnderTest) none(window time.Duration) {
	v.t.Helper()
	select {
	case r := <-v.in.ExecReports():
		v.t.Fatalf("unexpected report: %+v", r)
	case <-time.After(window):
	}
}

func (v *venueUnderTest) msgs(msgType, dir string) float64 { return countedMsgs(v.in, msgType, dir) }

// countedMsgs reads one cell of fix_msgs_total for an initiator.
func countedMsgs(in *Initiator, msgType, dir string) float64 {
	return testutil.ToFloat64(in.counter.sm.msgs.WithLabelValues(in.counter.sm.id, msgType, dir))
}

func fastFill() config.FillModel {
	fill := testFill()
	fill.Latency = time.Millisecond
	fill.BookPoll = 10 * time.Millisecond
	return fill
}

func perpOrder(id carry.OrderID, side carry.Side, qty, px string, t carry.TIF) carry.Order {
	return carry.Order{
		ClOrdID: id, Asset: "ETH", Leg: carry.LegPerp, Side: side, Type: carry.Limit, TIF: t,
		Qty: dec(qty), LimitPx: dec(px),
	}
}

func newTracker(t *testing.T) *exec.Tracker {
	t.Helper()
	return exec.NewTracker(exec.Options{Venue: "sim", Timeout: time.Minute},
		exec.NewMetrics(prometheus.NewRegistry()), testLogger())
}

// The first acceptance item: NOS -> sim-venue -> ExecReports on the channel,
// and the state machine terminal.
func TestInitiatorRunsAnOrderToFilled(t *testing.T) {
	sim := startSimVenue(t, fastFill())
	v := startInitiator(t, sim.session, t.TempDir(), true)
	tracker := newTracker(t)

	order := perpOrder("A1", carry.Buy, "3", "2350.00", carry.GTC)
	ack, err := v.in.Submit(t.Context(), order)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if ack.ClOrdID != "A1" || ack.At.IsZero() {
		t.Fatalf("ack %+v, want the ClOrdID and a time", ack)
	}
	if err := tracker.Track(order, ack); err != nil {
		t.Fatal(err)
	}

	acked := v.next("NEW")
	if acked.State != carry.StateNew || acked.ClOrdID != "A1" {
		t.Fatalf("first report %+v, want NEW for A1", acked)
	}
	if acked.VenueID == "" || acked.ExecID == "" {
		t.Errorf("report carries venue id %q exec id %q, want both", acked.VenueID, acked.ExecID)
	}
	if _, outcome := tracker.Apply(acked); outcome != exec.OutcomeApplied {
		t.Fatalf("NEW: outcome %s", outcome)
	}

	filled := v.next("FILLED")
	if filled.State != carry.StateFilled || !filled.CumQty.Equal(dec("3")) || !filled.LeavesQty.IsZero() {
		t.Fatalf("second report %+v, want FILLED cum 3 leaves 0", filled)
	}
	// Two basis points of adverse slippage on a 2345.00 ask, which is the sim's
	// fill model doing what API spec section 4.3 says and the initiator
	// carrying the price through untouched.
	if !filled.LastQty.Equal(dec("3")) || !filled.LastPx.Equal(dec("2345.469")) {
		t.Errorf("fill %s @ %s, want 3 @ 2345.469", filled.LastQty, filled.LastPx)
	}
	if filled.At.IsZero() || filled.At.Location() != time.UTC {
		t.Errorf("report time %v, want the venue's TransactTime in UTC", filled.At)
	}

	status, outcome := tracker.Apply(filled)
	if outcome != exec.OutcomeApplied || !status.Terminal() {
		t.Fatalf("FILLED: outcome %s, status %+v; want applied and terminal", outcome, status)
	}
	if !status.AvgPx.Equal(dec("2345.469")) {
		t.Errorf("avg px %s, want 2345.469", status.AvgPx)
	}
}

// Every refusal in translate is a fact about what the venue speaks, and each
// happens before any I/O: the counter of outbound orders stays at zero.
func TestInitiatorRefusesWhatTheVenueCannotExpress(t *testing.T) {
	sim := startSimVenue(t, fastFill())
	v := startInitiator(t, sim.session, t.TempDir(), true)

	good := perpOrder("A1", carry.Buy, "3", "2350.00", carry.GTC)
	cases := []struct {
		name   string
		mutate func(*carry.Order)
	}{
		{"no ClOrdID", func(o *carry.Order) { o.ClOrdID = "" }},
		{"spot leg", func(o *carry.Order) { o.Leg = carry.LegSpot }},
		{"unknown side", func(o *carry.Order) { o.Side = "short" }},
		{"fractional contracts", func(o *carry.Order) { o.Qty = dec("1.5") }},
		{"zero quantity", func(o *carry.Order) { o.Qty = dec("0") }},
		{"negative quantity", func(o *carry.Order) { o.Qty = dec("-1") }},
		{"no limit price", func(o *carry.Order) { o.LimitPx = decimal.Zero }},
		{"post-only", func(o *carry.Order) { o.TIF = carry.ALO }},
		{"blank TIF on a limit", func(o *carry.Order) { o.TIF = "" }},
		{"market that is not IOC", func(o *carry.Order) { o.Type = carry.Market; o.TIF = carry.GTC }},
		{"unknown order type", func(o *carry.Order) { o.Type = "stop" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := good
			c.mutate(&o)
			_, err := v.in.Submit(t.Context(), o)
			if !errors.Is(err, ErrUnsupportedOrder) {
				t.Fatalf("Submit: %v, want ErrUnsupportedOrder", err)
			}
		})
	}
	if got := v.msgs(msgTypeNewOrderSingle, dirOut); got != 0 {
		t.Errorf("%v NewOrderSingles sent by refused orders, want 0", got)
	}

	// A ClOrdID is used once per session.
	if _, err := v.in.Submit(t.Context(), good); err != nil {
		t.Fatalf("first submit: %v", err)
	}
	if _, err := v.in.Submit(t.Context(), good); !errors.Is(err, ErrUnsupportedOrder) {
		t.Fatalf("second submit of the same ClOrdID: %v, want ErrUnsupportedOrder", err)
	}
}

// A market order goes on the wire as an IOC limit at the protected price, and
// the simulator — which accepts only limits — fills it.
func TestInitiatorSendsAMarketOrderAsAnIOCLimit(t *testing.T) {
	sim := startSimVenue(t, fastFill())
	v := startInitiator(t, sim.session, t.TempDir(), true)

	o := perpOrder("M1", carry.Sell, "2", "2340.00", "")
	o.Type = carry.Market
	if _, err := v.in.Submit(t.Context(), o); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if r := v.next("NEW"); r.State != carry.StateNew {
		t.Fatalf("first report %+v, want NEW", r)
	}
	if r := v.next("FILLED"); r.State != carry.StateFilled || !r.CumQty.Equal(dec("2")) {
		t.Fatalf("second report %+v, want FILLED cum 2", r)
	}
}

// Submit and Cancel refuse while the session is down, and send nothing: an
// order queued into quickfix's store would execute whenever the link came
// back, at a price chosen now (ADR-0020).
func TestInitiatorRefusesOrdersWhileTheSessionIsDown(t *testing.T) {
	session := config.FIXSession{Sender: "CARRY", Target: "SIMV", Host: "127.0.0.1", Port: freePort(t)}
	v := startInitiator(t, session, t.TempDir(), false)

	if _, err := v.in.Submit(t.Context(), perpOrder("A1", carry.Buy, "1", "2350.00", carry.GTC)); !errors.Is(err, ErrSessionDown) {
		t.Fatalf("Submit with nothing listening: %v, want ErrSessionDown", err)
	}
	if err := v.in.Cancel(t.Context(), "A1"); !errors.Is(err, ErrUnknownOrder) {
		t.Fatalf("Cancel of an order never submitted: %v, want ErrUnknownOrder", err)
	}
	if got := v.msgs(msgTypeNewOrderSingle, dirOut); got != 0 {
		t.Errorf("%v NewOrderSingles sent while down, want 0", got)
	}
	// Nothing was assigned a sequence number: the counter saw no outbound
	// message, and the store — safe to read here, with no session up to write
	// it — still expects to send its first.
	if _, out := v.in.counter.sequenceNumbers(); out != nil {
		t.Errorf("last outbound seq %v, want none: a refused order must not consume a sequence number", *out)
	}
	waitFor(t, func() bool { _, err := quickfix.GetExpectedSenderNum(v.in.sessionID); return err == nil },
		"the session to be registered")
	if n, err := quickfix.GetExpectedSenderNum(v.in.sessionID); err != nil || n != 1 {
		t.Errorf("next outbound seq %d (%v), want 1", n, err)
	}
}

// A cancel is answered on the order it withdrew: the CANCELED report is filed
// under the original ClOrdID, not the cancel request's own.
func TestInitiatorCancelsARestingOrder(t *testing.T) {
	sim := startSimVenue(t, fastFill())
	v := startInitiator(t, sim.session, t.TempDir(), true)
	tracker := newTracker(t)

	order := perpOrder("A1", carry.Buy, "3", "2000.00", carry.GTC) // rests: far below the ask
	ack, err := v.in.Submit(t.Context(), order)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if err := tracker.Track(order, ack); err != nil {
		t.Fatal(err)
	}
	tracker.Apply(v.next("NEW"))

	if err := v.in.Cancel(t.Context(), "A1"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	canceled := v.next("CANCELED")
	if canceled.ClOrdID != "A1" {
		t.Fatalf("cancel acknowledged under %q, want the order's own id A1", canceled.ClOrdID)
	}
	if canceled.State != carry.StateCanceled || !canceled.CumQty.IsZero() || canceled.Reason == "" {
		t.Fatalf("report %+v, want CANCELED, nothing filled, with a reason", canceled)
	}
	status, outcome := tracker.Apply(canceled)
	if outcome != exec.OutcomeApplied || status.State != carry.StateCanceled {
		t.Fatalf("outcome %s, status %+v", outcome, status)
	}
	if err := v.in.Cancel(t.Context(), "nope"); !errors.Is(err, ErrUnknownOrder) {
		t.Errorf("cancel of an unknown order: %v, want ErrUnknownOrder", err)
	}
}

// An OrderCancelReject is counted and logged, and does not move the order: a
// cancel that arrived too late is not a state the order can be in, and the
// FILLED that beat it is the truth.
func TestInitiatorLogsACancelRejectWithoutMovingTheOrder(t *testing.T) {
	sim := startSimVenue(t, fastFill())
	v := startInitiator(t, sim.session, t.TempDir(), true)

	if _, err := v.in.Submit(t.Context(), perpOrder("A1", carry.Buy, "3", "2350.00", carry.GTC)); err != nil {
		t.Fatalf("submit: %v", err)
	}
	v.next("NEW")
	v.next("FILLED")

	if err := v.in.Cancel(t.Context(), "A1"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	waitFor(t, func() bool { return v.msgs(msgTypeOrderCancelReject, dirIn) == 1 }, "the cancel reject")
	v.none(200 * time.Millisecond)
}

// The second acceptance item, and the reason there is a file-backed store at
// all: kill the initiator with an order resting, let the venue fill it while
// nobody is listening, bring the initiator back — and the fill arrives.
//
// quickfix on the venue side persists the FILLED under its next sequence
// number and drops its send queue when the client is gone, so the report can
// reach the client only one way: the client logs back on with the sequence
// numbers it had, notices the gap, and asks for it. Everything asserted here
// is a piece of that path — the venue recorded the fill with the client down,
// the client asked for a resend, the venue saw the client come back on the
// next number rather than on one, and the report is on the channel.
func TestReportsSurviveARestartOfTheInitiator(t *testing.T) {
	sim := startSimVenue(t, fastFill())
	store := t.TempDir()
	first := startInitiator(t, sim.session, store, true)
	tracker := newTracker(t)

	order := perpOrder("A1", carry.Buy, "3", "2300.00", carry.GTC) // rests: below the ask
	ack, err := first.in.Submit(t.Context(), order)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if err := tracker.Track(order, ack); err != nil {
		t.Fatal(err)
	}
	tracker.Apply(first.next("NEW"))

	// Down. The venue's own record of the session closes with the sequence
	// number the initiator's Logout carried.
	first.stop()
	waitFor(t, func() bool {
		rows := sim.sink.sessions()
		return len(rows) > 0 && !rows[len(rows)-1].EndedAt.IsZero()
	}, "the venue to record the logout")
	rows := sim.sink.sessions()
	atLogout := rows[len(rows)-1]
	if atLogout.LastInSeq == nil {
		t.Fatal("the venue recorded no inbound sequence number at logout")
	}
	if _, err := first.in.Submit(t.Context(), perpOrder("A2", carry.Buy, "1", "2300.00", carry.GTC)); !errors.Is(err, ErrSessionDown) {
		t.Fatalf("Submit on the stopped initiator: %v, want ErrSessionDown", err)
	}

	// The market comes to the order while nobody is listening. The venue fills
	// it and records the fill: this is the report that must not be lost.
	sim.books.set(time.Now().UTC(), "2299.50", "2300.00")
	waitFor(t, func() bool { return len(sim.sink.fills()) == 1 }, "the venue to fill while the initiator is down")

	// Back, on the same store.
	second := startInitiator(t, sim.session, store, true)

	filled := second.next("the FILLED report recovered by resend")
	if filled.ClOrdID != "A1" || filled.State != carry.StateFilled || !filled.CumQty.Equal(dec("3")) {
		t.Fatalf("recovered report %+v, want FILLED cum 3 for A1", filled)
	}
	status, outcome := tracker.Apply(filled)
	if outcome != exec.OutcomeApplied || !status.Terminal() {
		t.Fatalf("outcome %s, status %+v; want applied and terminal", outcome, status)
	}

	// The mechanism, not just the outcome. The initiator asked for the resend:
	// fix_resend_events_total on its session moved.
	if got := testutil.ToFloat64(second.in.counter.sm.resends); got < 1 {
		t.Errorf("fix_resend_events_total = %v on the restarted initiator, want at least 1: "+
			"the report arrived without a resend, so this test is not proving recovery", got)
	}
	// And the venue saw the initiator come back on the next sequence number
	// after the one its Logout carried — contiguity across the restart, which
	// only a preserved store produces. A reset store would have logged on at
	// one and been refused as too low.
	waitFor(t, func() bool {
		latest := sim.sink.sessions()
		return len(latest) > 0 && latest[len(latest)-1].EndedAt.IsZero()
	}, "the venue to record the re-logon")
	rows = sim.sink.sessions()
	atLogon := rows[len(rows)-1]
	if atLogon.LastInSeq == nil || *atLogon.LastInSeq != *atLogout.LastInSeq+1 {
		t.Errorf("the venue saw the initiator log back on at seq %v after a logout at %d, want %d: "+
			"the initiator's store did not survive the restart",
			seqValue(atLogon.LastInSeq), *atLogout.LastInSeq, *atLogout.LastInSeq+1)
	}
	// The recovered session still trades.
	if _, err := second.in.Submit(t.Context(), perpOrder("A3", carry.Buy, "1", "2350.00", carry.GTC)); err != nil {
		t.Fatalf("submit after the restart: %v", err)
	}
	if r := second.next("NEW for A3"); r.ClOrdID != "A3" || r.State != carry.StateNew {
		t.Fatalf("report %+v, want NEW for A3", r)
	}
}

// The session settings from API spec section 4.1, from the initiator's point
// of view — the comp ids as configured, no swap.
func TestInitiatorSettings(t *testing.T) {
	t.Parallel()

	s := testSession(t)
	settings, sessionID, err := initiatorSettings(s)
	if err != nil {
		t.Fatal(err)
	}
	if sessionID.SenderCompID != "CARRY" || sessionID.TargetCompID != "SIMV" {
		t.Errorf("session %s, want CARRY->SIMV", sessionID)
	}
	session := settings.SessionSettings()[sessionID]
	for _, tc := range []struct{ key, want string }{
		{"ConnectionType", "initiator"},
		{"SocketConnectHost", "sim-venue"},
		{"SocketConnectPort", "5001"},
		{"HeartBtInt", "30"},
		{"ResetOnLogon", "N"},
		{"ResetOnLogout", "N"},
		{"ResetOnDisconnect", "N"},
	} {
		assertSetting(t, session, tc.key, tc.want)
	}
	if got, err := settings.GlobalSettings().Setting("ReconnectInterval"); err != nil || got != "5" {
		t.Errorf("ReconnectInterval = %q (%v), want 5", got, err)
	}
}

// ---------------------------------------------------------------------------
// The wire mapping, without a socket.
// ---------------------------------------------------------------------------

func TestNewOrderSingleTags(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	o := perpOrder("01K5", carry.Sell, "7", "2400.50", carry.IOC)
	w, err := translate(o)
	if err != nil {
		t.Fatal(err)
	}
	msg := newOrderSingleMessage(o, w, testPerp, at)

	assertField(t, msg, tag.MsgType, msgTypeNewOrderSingle)
	assertField(t, msg, tag.ClOrdID, "01K5")
	assertField(t, msg, tag.Symbol, testPerp)
	assertField(t, msg, tag.Side, string(enum.Side_SELL))
	assertField(t, msg, tag.OrderQty, "7")
	assertField(t, msg, tag.OrdType, string(enum.OrdType_LIMIT))
	assertField(t, msg, tag.Price, "2400.5")
	assertField(t, msg, tag.TimeInForce, string(enum.TimeInForce_IMMEDIATE_OR_CANCEL))
	assertField(t, msg, tag.TransactTime, "20260917-12:00:00.000")
}

func TestCancelRequestTags(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	msg := cancelRequestMessage("C1", "A1", orderTerms{symbol: testPerp, side: enum.Side_BUY}, at)

	assertField(t, msg, tag.MsgType, msgTypeOrderCancelRequest)
	assertField(t, msg, tag.ClOrdID, "C1")
	assertField(t, msg, tag.OrigClOrdID, "A1")
	assertField(t, msg, tag.Symbol, testPerp)
	assertField(t, msg, tag.Side, string(enum.Side_BUY))
	assertField(t, msg, tag.TransactTime, "20260917-12:00:00.000")
}

// testER builds an ExecutionReport the way sim-venue sends one, then lets a
// case adjust it.
func testER(mutate func(executionreport.ExecutionReport, *quickfix.Message)) *quickfix.Message {
	er := executionreport.New(
		field.NewOrderID("O-1"),
		field.NewExecID("E-1"),
		field.NewExecType(enum.ExecType_TRADE),
		field.NewOrdStatus(enum.OrdStatus_PARTIALLY_FILLED),
		field.NewSide(enum.Side_BUY),
		field.NewLeavesQty(dec("2"), 0),
		field.NewCumQty(dec("1"), 0),
		field.NewAvgPx(dec("2345.47"), 2),
	)
	er.SetClOrdID("A1")
	er.SetSymbol(testPerp)
	er.SetOrderQty(dec("3"), 0)
	er.SetLastQty(dec("1"), 0)
	er.SetLastPx(dec("2345.47"), 2)
	er.SetTransactTime(time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC))
	msg := er.ToMessage()
	if mutate != nil {
		mutate(er, msg)
	}
	return msg
}

func TestParseExecReportReadsTheDictionary(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 17, 13, 0, 0, 0, time.UTC)
	r, err := parseExecReport(testER(nil), now)
	if err != nil {
		t.Fatal(err)
	}
	want := carry.ExecReport{
		ClOrdID: "A1", VenueID: "O-1", ExecID: "E-1", State: carry.StatePartial,
		LastQty: dec("1"), LastPx: dec("2345.47"), CumQty: dec("1"), LeavesQty: dec("2"),
		AvgPx: dec("2345.47"), At: time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC),
	}
	assertReport(t, r, want)
}

func TestParseExecReportFilesACancelUnderTheOriginalOrder(t *testing.T) {
	t.Parallel()

	msg := testER(func(er executionreport.ExecutionReport, _ *quickfix.Message) {
		er.SetClOrdID("C1")
		er.SetOrigClOrdID("A1")
		er.SetOrdStatus(enum.OrdStatus_CANCELED)
		er.SetText(cancelRequested)
	})
	r, err := parseExecReport(msg, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if r.ClOrdID != "A1" || r.State != carry.StateCanceled || r.Reason != cancelRequested {
		t.Errorf("%+v, want CANCELED on A1 with the reason", r)
	}
}

// What a NEW carries and what it does not: no fill fields, and the time is the
// venue's when it says, the clock's when it does not.
func TestParseExecReportTreatsFillFieldsAsOptional(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 17, 13, 0, 0, 0, time.UTC)
	msg := testER(func(er executionreport.ExecutionReport, m *quickfix.Message) {
		er.SetOrdStatus(enum.OrdStatus_NEW)
		er.SetCumQty(dec("0"), 0)
		er.SetLeavesQty(dec("3"), 0)
		m.Body.Remove(tag.LastQty)
		m.Body.Remove(tag.LastPx)
		m.Body.Remove(tag.TransactTime)
	})
	r, err := parseExecReport(msg, now)
	if err != nil {
		t.Fatal(err)
	}
	if r.State != carry.StateNew || !r.LastQty.IsZero() || !r.LastPx.IsZero() {
		t.Errorf("%+v, want NEW with no fill", r)
	}
	if !r.At.Equal(now) {
		t.Errorf("at %v, want the clock's %v when the venue sent no TransactTime", r.At, now)
	}
}

// A report the initiator cannot read is refused back to the venue by tag, and
// an OrdStatus outside the five states is refused rather than folded into the
// nearest one.
func TestParseExecReportRejectsWhatItCannotRead(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		mutate func(executionreport.ExecutionReport, *quickfix.Message)
		tag    quickfix.Tag
	}{
		{"no ClOrdID", func(_ executionreport.ExecutionReport, m *quickfix.Message) { m.Body.Remove(tag.ClOrdID) }, tag.ClOrdID},
		{"no ExecID", func(_ executionreport.ExecutionReport, m *quickfix.Message) { m.Body.Remove(tag.ExecID) }, tag.ExecID},
		{"no OrdStatus", func(_ executionreport.ExecutionReport, m *quickfix.Message) { m.Body.Remove(tag.OrdStatus) }, tag.OrdStatus},
		{"no CumQty", func(_ executionreport.ExecutionReport, m *quickfix.Message) { m.Body.Remove(tag.CumQty) }, tag.CumQty},
		{"expired", func(er executionreport.ExecutionReport, _ *quickfix.Message) { er.SetOrdStatus(enum.OrdStatus_EXPIRED) }, tag.OrdStatus},
		{"pending cancel", func(er executionreport.ExecutionReport, _ *quickfix.Message) {
			er.SetOrdStatus(enum.OrdStatus_PENDING_CANCEL)
		}, tag.OrdStatus},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, err := parseExecReport(testER(c.mutate), time.Now())
			if err == nil {
				t.Fatal("parsed, want a reject")
			}
			if err.RefTagID() == nil || *err.RefTagID() != c.tag {
				t.Errorf("reject names tag %v, want %d", err.RefTagID(), c.tag)
			}
		})
	}
}

func assertReport(t *testing.T, got, want carry.ExecReport) {
	t.Helper()
	if got.ClOrdID != want.ClOrdID || got.VenueID != want.VenueID || got.ExecID != want.ExecID ||
		got.State != want.State || got.Reason != want.Reason || !got.At.Equal(want.At) {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
	for _, f := range []struct {
		name      string
		got, want decimal.Decimal
	}{
		{"LastQty", got.LastQty, want.LastQty}, {"LastPx", got.LastPx, want.LastPx},
		{"CumQty", got.CumQty, want.CumQty}, {"LeavesQty", got.LeavesQty, want.LeavesQty},
		{"AvgPx", got.AvgPx, want.AvgPx}, {"Fee", got.Fee, want.Fee},
	} {
		if !f.got.Equal(f.want) {
			t.Errorf("%s = %s, want %s", f.name, f.got, f.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Regressions from the Part 8 adversarial review.
// ---------------------------------------------------------------------------

// The defect: run() closed a "we are shutting down" channel BEFORE
// initiator.Stop(), and deliver selected over that channel and the report
// handoff. Once the channel is closed both cases are ready whenever the buffer
// has room, and Go chooses between them at random — so roughly half the reports
// arriving during the logout were logged and thrown away while the consumer was
// still draining. quickfix acknowledges a report as soon as FromApp returns, so
// every one of those was acknowledged too and could never be resent: the exact
// opposite of this part's second acceptance criterion.
//
// The fix is that giving up is a TIMEOUT, which cannot fire while the handoff
// would succeed. This test makes giving up permanently available — a zero wait,
// so the timer has already expired — and asserts that every report still
// reaches the consumer. Against the old design it fails about half the time per
// report, which over a thousand reports is never.
func TestDeliverPrefersTheConsumerOverGivingUp(t *testing.T) {
	t.Parallel()

	const reports = 1000
	// A buffer that cannot fill, so the handoff can ALWAYS succeed, and a wait
	// of zero, so the giving-up path is permanently available. Every report
	// must still be taken. That is the invariant the old design broke: it chose
	// between an always-ready abandon signal and a handoff that would have
	// succeeded immediately, and Go chooses uniformly at random.
	v := &Initiator{
		reports:     make(chan carry.ExecReport, reports),
		deliverWait: 0,
		log:         testLogger(),
	}

	for i := range reports {
		v.deliver(carry.ExecReport{ClOrdID: carry.OrderID(strconv.Itoa(i)), State: carry.StateFilled})
	}

	if n := len(v.reports); n != reports {
		t.Fatalf("%d of %d reports were taken with room for every one of them; %d were "+
			"acknowledged to the venue and thrown away, and the venue will never resend them",
			n, reports, reports-n)
	}
}

// The other half of the contract: a consumer that has genuinely stopped
// draining does not hang the session forever. The report is abandoned, loudly,
// once the buffer is full and the wait has run out.
func TestDeliverGivesUpWhenTheConsumerStopsDraining(t *testing.T) {
	t.Parallel()

	v := &Initiator{
		reports:     make(chan carry.ExecReport, 2),
		deliverWait: 20 * time.Millisecond,
		log:         testLogger(),
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 4 { // two fit in the buffer; two must be abandoned
			v.deliver(carry.ExecReport{State: carry.StateFilled})
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("deliver blocked forever on a consumer that stopped draining")
	}
	if n := len(v.reports); n != 2 {
		t.Errorf("%d reports buffered, want the 2 that fit", n)
	}
}

// ADR-0020 decision 1 covers Cancel as well as Submit, and nothing tested the
// Cancel half: the test that named it could only ever reach the ErrUnknownOrder
// branch, because the Submit before it had already been refused. This submits
// while the session is UP, then takes the venue away.
func TestCancelIsRefusedWhileTheSessionIsDown(t *testing.T) {
	sim := startSimVenue(t, fastFill())
	v := startInitiator(t, sim.session, t.TempDir(), true)

	// A resting order, so it is still live when the venue goes away.
	if _, err := v.in.Submit(t.Context(), perpOrder("A1", carry.Buy, "1", "1000.00", carry.GTC)); err != nil {
		t.Fatalf("submit: %v", err)
	}
	v.next("NEW")

	sim.stop()
	waitFor(t, func() bool { return !v.in.LoggedOn() }, "the session to go down")

	before, _ := quickfix.GetExpectedSenderNum(v.in.sessionID)
	if err := v.in.Cancel(t.Context(), "A1"); !errors.Is(err, ErrSessionDown) {
		t.Fatalf("Cancel with the venue gone: %v, want ErrSessionDown", err)
	}
	// Nothing was sent: quickfix would otherwise have numbered and stored the
	// cancel for the venue to recover by resend.
	after, _ := quickfix.GetExpectedSenderNum(v.in.sessionID)
	if after != before {
		t.Errorf("the refused cancel consumed a sequence number (%d -> %d)", before, after)
	}
	if got := v.msgs(msgTypeOrderCancelRequest, dirOut); got != 0 {
		t.Errorf("%v OrderCancelRequests sent while down, want 0", got)
	}
}

// An order is never replayed. quickfix answers a ResendRequest by calling ToApp
// again with PossDupFlag set; returning ErrDoNotSend there makes it gap-fill
// the message instead.
//
// The reachable trigger is one end's store being reset and not the other's,
// which Part 8 made possible by giving carry a store of its own. sim-venue
// keeps its book in memory and has no ClOrdID history across a restart, so a
// replayed NewOrderSingle is accepted as a brand-new order and filled a second
// time — a real position the client believes it already closed.
func TestAnOrderIsNeverResent(t *testing.T) {
	t.Parallel()

	v := &Initiator{counter: newMsgCounter(
		NewSessionCatalogue(prometheus.NewRegistry()).session("s"), testLogger()), log: testLogger()}

	o := perpOrder("A1", carry.Buy, "1", "2350.00", carry.GTC)
	w, err := translate(o)
	if err != nil {
		t.Fatal(err)
	}
	msg := newOrderSingleMessage(o, w, testPerp, epoch)

	// First send: ordinary, counted.
	if err := v.ToApp(msg, v.sessionID); err != nil {
		t.Fatalf("first send: %v, want it to go out", err)
	}
	if got := countedMsgs(v, msgTypeNewOrderSingle, dirOut); got != 1 {
		t.Errorf("%v NewOrderSingles counted on the first send, want 1", got)
	}

	// The replay quickfix would perform for a ResendRequest.
	msg.Header.SetField(tag.PossDupFlag, quickfix.FIXBoolean(true))
	if err := v.ToApp(msg, v.sessionID); !errors.Is(err, quickfix.ErrDoNotSend) {
		t.Fatalf("replay of a NewOrderSingle: %v, want ErrDoNotSend so quickfix gap-fills it", err)
	}
	if got := countedMsgs(v, msgTypeNewOrderSingle, dirOut); got != 1 {
		t.Errorf("%v NewOrderSingles counted after the replay, want still 1", got)
	}
}

// A BusinessMessageReject is itself an application message, and both ends of
// this session refuse unrouted application messages with one. Answering a 35=j
// with another 35=j has the two engines rejecting each other's rejects until
// the connection dies, burning a sequence number per round trip.
func TestABusinessRejectIsNotAnsweredWithAnother(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	v := &Initiator{counter: newMsgCounter(NewSessionCatalogue(reg).session("s"), testLogger()), log: testLogger()}
	a := &Acceptor{
		counter: newMsgCounter(NewMetrics(prometheus.NewRegistry()).session("s"), testLogger()),
		log:     testLogger(),
		now:     func() time.Time { return epoch },
	}

	msg := quickfix.NewMessage()
	msg.Header.SetField(tag.MsgType, quickfix.FIXString(msgTypeBusinessReject))
	msg.Body.SetField(tag.RefMsgType, quickfix.FIXString(msgTypeBusinessReject))
	msg.Body.SetField(tag.BusinessRejectReason, quickfix.FIXInt(applicationNotAvailable))
	msg.Body.SetField(tag.Text, quickfix.FIXString("unsupported message type"))

	if err := v.FromApp(msg, v.sessionID); err != nil {
		t.Errorf("initiator answered a 35=j with %v; the venue answers that with another 35=j, forever", err)
	}
	if err := a.FromApp(msg, a.sessionID); err != nil {
		t.Errorf("acceptor answered a 35=j with %v; the client answers that with another 35=j, forever", err)
	}
}
