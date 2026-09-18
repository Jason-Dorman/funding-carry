package fix

import (
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/quickfixgo/enum"
	"github.com/quickfixgo/field"
	"github.com/quickfixgo/fix44/newordersingle"
	"github.com/quickfixgo/fix44/ordercancelrequest"
	"github.com/quickfixgo/quickfix"
	filelog "github.com/quickfixgo/quickfix/log/file"
	filestore "github.com/quickfixgo/quickfix/store/file"
	"github.com/quickfixgo/tag"
	"github.com/shopspring/decimal"

	"github.com/Jason-Dorman/funding-carry/internal/config"
)

// Part 7's acceptance criteria, over a real socket: a scripted FIX client logs
// on, sends orders, and reads what comes back.
//
// The client here is quickfixgo's initiator with a recording application — not
// carry's fixVenue, which is Part 8. What is under test is the venue: that a
// NewOrderSingle produces the reports API spec section 4.2 describes, that an
// oversized order arrives as partials, that a cancel works both before a fill
// and in the middle of one, and that sequence numbers survive the venue being
// restarted underneath the client.

// clientReport is one message the scripted client received, with the sequence
// number it arrived under — which is the whole point of the restart case.
type clientReport struct {
	msgType      string
	seqNum       int
	clOrdID      string
	execType     enum.ExecType
	ordStatus    enum.OrdStatus
	lastQty      decimal.Decimal
	cumQty       decimal.Decimal
	leavesQty    decimal.Decimal
	text         string
	orderID      string
	cxlRejReason string
}

// scriptedClient is the FIX 4.4 initiator the tests drive.
type scriptedClient struct {
	t *testing.T

	mu        sync.Mutex
	reports   []clientReport
	admin     []clientReport
	arrivals  []clientReport
	logons    int
	loggedOn  bool
	sessionID quickfix.SessionID
}

func (c *scriptedClient) OnCreate(id quickfix.SessionID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sessionID = id
}

func (c *scriptedClient) OnLogon(quickfix.SessionID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.loggedOn = true
	// Counted, not just flagged. quickfix calls this once the session has
	// finished logging on AND completed any resend recovery, so a count that has
	// moved is the only reliable signal that a NEW session is ready to carry an
	// application message. The flag alone is stale-true straight after a restart,
	// because the client has not yet processed the logout.
	c.logons++
}

func (c *scriptedClient) OnLogout(quickfix.SessionID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.loggedOn = false
}

func (c *scriptedClient) ToAdmin(*quickfix.Message, quickfix.SessionID)     {}
func (c *scriptedClient) ToApp(*quickfix.Message, quickfix.SessionID) error { return nil }

// FromAdmin records the session-level traffic, which is the only place a resumed
// session and a reset one look different. An ExecutionReport carries no evidence
// either way.
func (c *scriptedClient) FromAdmin(msg *quickfix.Message, _ quickfix.SessionID) quickfix.MessageRejectError {
	r := clientReport{}
	r.msgType, _ = msg.MsgType()
	r.seqNum, _ = msg.Header.GetInt(tag.MsgSeqNum)

	c.mu.Lock()
	defer c.mu.Unlock()
	c.admin = append(c.admin, r)
	c.arrivals = append(c.arrivals, r)
	return nil
}

func (c *scriptedClient) FromApp(msg *quickfix.Message, _ quickfix.SessionID) quickfix.MessageRejectError {
	r := clientReport{}
	r.msgType, _ = msg.MsgType()
	r.seqNum, _ = msg.Header.GetInt(tag.MsgSeqNum)
	r.clOrdID, _ = msg.Body.GetString(tag.ClOrdID)
	r.text, _ = msg.Body.GetString(tag.Text)
	r.orderID, _ = msg.Body.GetString(tag.OrderID)
	r.cxlRejReason, _ = msg.Body.GetString(tag.CxlRejReason)

	if v, err := msg.Body.GetString(tag.ExecType); err == nil {
		r.execType = enum.ExecType(v)
	}
	if v, err := msg.Body.GetString(tag.OrdStatus); err == nil {
		r.ordStatus = enum.OrdStatus(v)
	}
	for _, f := range []struct {
		tg  quickfix.Tag
		out *decimal.Decimal
	}{{tag.LastQty, &r.lastQty}, {tag.CumQty, &r.cumQty}, {tag.LeavesQty, &r.leavesQty}} {
		var v quickfix.FIXDecimal
		if err := msg.Body.GetField(f.tg, &v); err == nil {
			*f.out = v.Decimal
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.reports = append(c.reports, r)
	c.arrivals = append(c.arrivals, r)
	return nil
}

func (c *scriptedClient) received() []clientReport {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]clientReport(nil), c.reports...)
}

// arrivalsSince returns every message — session level and application level —
// the client received after a mark, in the order it received them.
func (c *scriptedClient) arrivalsSince(mark int) []clientReport {
	c.mu.Lock()
	defer c.mu.Unlock()
	if mark > len(c.arrivals) {
		mark = len(c.arrivals)
	}
	return append([]clientReport(nil), c.arrivals[mark:]...)
}

func (c *scriptedClient) arrivalCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.arrivals)
}

func (c *scriptedClient) logonCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.logons
}

func (c *scriptedClient) up() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.loggedOn
}

func (c *scriptedClient) session() quickfix.SessionID {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sessionID
}

// send puts a message on the session, failing the test if it cannot.
func (c *scriptedClient) send(m quickfix.Messagable) {
	c.t.Helper()
	if err := quickfix.SendToTarget(m, c.session()); err != nil {
		c.t.Fatalf("send: %v", err)
	}
}

func (c *scriptedClient) order(clOrdID string, s enum.Side, qty, px string, t enum.TimeInForce) {
	nos := newordersingle.New(
		field.NewClOrdID(clOrdID),
		field.NewSide(s),
		field.NewTransactTime(time.Now().UTC()),
		field.NewOrdType(enum.OrdType_LIMIT),
	)
	nos.SetSymbol(testPerp)
	nos.SetOrderQty(dec(qty), 0)
	nos.SetPrice(dec(px), 2)
	nos.SetTimeInForce(t)
	c.send(nos)
}

func (c *scriptedClient) cancel(clOrdID, origClOrdID string) {
	req := ordercancelrequest.New(
		field.NewOrigClOrdID(origClOrdID),
		field.NewClOrdID(clOrdID),
		field.NewSide(enum.Side_BUY),
		field.NewTransactTime(time.Now().UTC()),
	)
	req.SetSymbol(testPerp)
	c.send(req)
}

// wireUp starts a simulator and a scripted client connected to it, and returns
// the client plus a function that restarts the venue on the same port and store.
type wiring struct {
	client  *scriptedClient
	sink    *fakeSink
	books   *fakeBooks
	session config.FIXSession
	fill    config.FillModel
	restart func()
	stop    func()
}

func wireUp(t *testing.T) *wiring {
	t.Helper()
	fill := testFill()
	// Milliseconds rather than the configured twenty: what is being tested here
	// is the protocol, and a test that waited on the production latency would be
	// slower without proving anything more.
	fill.Latency = time.Millisecond
	fill.BookPoll = 10 * time.Millisecond
	return wireUpWith(t, fill)
}

func wireUpWith(t *testing.T, fill config.FillModel) *wiring {
	t.Helper()

	sim := startSimVenue(t, fill)
	client := &scriptedClient{t: t}
	initiator := startClient(t, client, sim.session, t.TempDir())

	w := &wiring{
		client:  client,
		sink:    sim.sink,
		books:   sim.books,
		session: sim.session,
		fill:    fill,
		restart: sim.restart,
	}
	w.stop = func() {
		initiator.Stop()
		sim.stop()
	}
	t.Cleanup(w.stop)

	waitFor(t, client.up, "the client to log on")
	return w
}

func startClient(t *testing.T, app quickfix.Application, s config.FIXSession, store string) *quickfix.Initiator {
	t.Helper()

	settings := quickfix.NewSettings()
	global := settings.GlobalSettings()
	global.Set("FileStorePath", store)
	global.Set("FileLogPath", store)
	// One second, not the thirty-second default: the restart case needs the
	// client back on the socket while the test is still running.
	global.Set("ReconnectInterval", "1")

	session := quickfix.NewSessionSettings()
	session.Set("ConnectionType", "initiator")
	session.Set("BeginString", quickfix.BeginStringFIX44)
	session.Set("SenderCompID", s.Sender)
	session.Set("TargetCompID", s.Target)
	session.Set("SocketConnectHost", s.Host)
	session.Set("SocketConnectPort", strconv.Itoa(s.Port))
	session.Set("HeartBtInt", strconv.Itoa(heartBtInt))
	session.Set("ResetOnLogon", "N")
	if _, err := settings.AddSession(session); err != nil {
		t.Fatalf("client settings: %v", err)
	}

	logFactory, err := filelog.NewLogFactory(settings)
	if err != nil {
		t.Fatalf("client log factory: %v", err)
	}
	initiator, err := quickfix.NewInitiator(app, filestore.NewStoreFactory(settings), settings, logFactory)
	if err != nil {
		t.Fatalf("new initiator: %v", err)
	}
	if err := initiator.Start(); err != nil {
		t.Fatalf("start initiator: %v", err)
	}
	return initiator
}

// waitQuiet blocks until the client has received nothing new for the given
// window, which is how this file says "the session has settled" without
// reaching for a bare sleep whose length means nothing.
func waitQuiet(t *testing.T, c *scriptedClient, quiet time.Duration) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	last := c.arrivalCount()
	settledAt := time.Now()
	for {
		time.Sleep(10 * time.Millisecond)
		if n := c.arrivalCount(); n != last {
			last, settledAt = n, time.Now()
		}
		if time.Since(settledAt) >= quiet {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the session never went quiet")
		}
	}
}

// seenMsgType reports whether a message of this type appears in the span.
func seenMsgType(span []clientReport, msgType string) bool {
	for _, m := range span {
		if m.msgType == msgType {
			return true
		}
	}
	return false
}

// reportsFor returns everything the client received for one ClOrdID.
func reportsFor(c *scriptedClient, clOrdID string) []clientReport {
	var out []clientReport
	for _, r := range c.received() {
		if r.clOrdID == clOrdID {
			out = append(out, r)
		}
	}
	return out
}

// D -> 8(NEW) -> 8(FILLED), over a socket, from a client that speaks only FIX.
func TestScriptedClientRunsAnOrderToFilled(t *testing.T) {
	w := wireUp(t)

	w.client.order("A1", enum.Side_BUY, "3", "2350.00", enum.TimeInForce_GOOD_TILL_CANCEL)
	waitFor(t, func() bool { return len(reportsFor(w.client, "A1")) == 2 }, "NEW then FILLED")

	got := reportsFor(w.client, "A1")
	if got[0].execType != enum.ExecType_NEW || got[0].ordStatus != enum.OrdStatus_NEW {
		t.Fatalf("first report %s/%s, want NEW", got[0].execType, got[0].ordStatus)
	}
	if got[1].execType != enum.ExecType_TRADE || got[1].ordStatus != enum.OrdStatus_FILLED {
		t.Fatalf("second report %s/%s, want TRADE/FILLED", got[1].execType, got[1].ordStatus)
	}
	if !got[1].cumQty.Equal(dec("3")) || !got[1].leavesQty.IsZero() {
		t.Errorf("filled with cum %s leaves %s", got[1].cumQty, got[1].leavesQty)
	}
	if n := len(w.sink.fills()); n != 1 {
		t.Errorf("%d rows recorded, want the one fill", n)
	}
}

// An oversized order arrives as partials that sum to the order.
func TestScriptedClientSeesPartialsSummingToTheOrder(t *testing.T) {
	w := wireUp(t)

	w.client.order("A1", enum.Side_BUY, "35", "2350.00", enum.TimeInForce_GOOD_TILL_CANCEL)
	waitFor(t, func() bool { return len(reportsFor(w.client, "A1")) == 4 }, "NEW and three partials")

	got := reportsFor(w.client, "A1")
	total := decimal.Zero
	for _, r := range got[1:] {
		total = total.Add(r.lastQty)
	}
	if !total.Equal(dec("35")) {
		t.Errorf("the partials sum to %s, want 35", total)
	}
	if last := got[len(got)-1]; last.ordStatus != enum.OrdStatus_FILLED {
		t.Errorf("last report is %s, want FILLED", last.ordStatus)
	}
}

// Cancel works before a fill and in the middle of one.
func TestScriptedClientCancels(t *testing.T) {
	t.Run("before any fill", func(t *testing.T) {
		w := wireUp(t)

		// Priced where it cannot trade, so it is still resting when the cancel
		// arrives.
		w.client.order("A1", enum.Side_BUY, "3", "2000.00", enum.TimeInForce_GOOD_TILL_CANCEL)
		waitFor(t, func() bool { return len(reportsFor(w.client, "A1")) == 1 }, "the acknowledgement")

		w.client.cancel("C1", "A1")
		waitFor(t, func() bool { return len(reportsFor(w.client, "C1")) == 1 }, "the cancel")

		got := reportsFor(w.client, "C1")[0]
		if got.ordStatus != enum.OrdStatus_CANCELED {
			t.Fatalf("report is %s, want CANCELED", got.ordStatus)
		}
		if !got.cumQty.IsZero() {
			t.Errorf("canceled with cum %s, want nothing filled", got.cumQty)
		}
	})

	t.Run("in the middle of a partial", func(t *testing.T) {
		// A slow venue on purpose. "Mid-partial" is a window, and at the
		// millisecond latency the other cases run at the whole order fills
		// before a cancel can cross the socket — the test would then be
		// asserting against a filled order and failing for a reason that has
		// nothing to do with cancelling.
		fill := testFill()
		fill.Latency = 500 * time.Millisecond
		fill.BookPoll = 10 * time.Millisecond
		w := wireUpWith(t, fill)

		w.client.order("A1", enum.Side_BUY, "35", "2350.00", enum.TimeInForce_GOOD_TILL_CANCEL)
		waitFor(t, func() bool { return len(reportsFor(w.client, "A1")) >= 2 }, "the first partial")

		w.client.cancel("C1", "A1")
		waitFor(t, func() bool { return len(reportsFor(w.client, "C1")) == 1 }, "the cancel")

		got := reportsFor(w.client, "C1")[0]
		if got.ordStatus != enum.OrdStatus_CANCELED {
			t.Fatalf("report is %s, want CANCELED", got.ordStatus)
		}
		if !got.cumQty.IsPositive() {
			t.Error("canceled with nothing filled, but a partial had already printed")
		}
		if !got.leavesQty.IsZero() {
			t.Errorf("canceled with leaves %s, want 0", got.leavesQty)
		}
	})
}

// An OrderCancelReject over a real socket. Until the Part 7 adversarial review
// this whole path — fixSender.sendCancelReject and cancelRejectMessage — was
// never executed by any test: the engine tests substitute a recording venue, so
// a no-op implementation of the production sender passed the suite.
func TestScriptedClientIsRejectedForCancellingAnOrderTheVenueNeverHad(t *testing.T) {
	w := wireUp(t)

	w.client.cancel("C1", "no-such-order")
	waitFor(t, func() bool { return len(reportsFor(w.client, "C1")) == 1 }, "the cancel reject")

	got := reportsFor(w.client, "C1")[0]
	if got.msgType != msgTypeOrderCancelReject {
		t.Fatalf("answered with 35=%s, want an OrderCancelReject (35=9)", got.msgType)
	}
	if got.cxlRejReason != string(enum.CxlRejReason_UNKNOWN_ORDER) {
		t.Errorf("CxlRejReason %q, want %q (unknown order)",
			got.cxlRejReason, enum.CxlRejReason_UNKNOWN_ORDER)
	}
	if got.orderID != unknownOrderID {
		t.Errorf("OrderID %q, want %q: FIX has no null and an empty tag 37 is a missing "+
			"required field", got.orderID, unknownOrderID)
	}
}

// The second acceptance criterion: restart the venue and the session resumes on
// the sequence numbers it left off at, rather than starting again from one.
//
// A reset would be invisible in every other signal — the session logs on, orders
// work, the metrics look healthy — right up until a resend was needed and the
// numbers on the two sides no longer described the same messages.
func TestSequenceNumbersSurviveARestart(t *testing.T) {
	w := wireUp(t)

	w.client.order("A1", enum.Side_BUY, "3", "2350.00", enum.TimeInForce_GOOD_TILL_CANCEL)
	waitFor(t, func() bool { return len(reportsFor(w.client, "A1")) == 2 }, "the first order to fill")
	before := reportsFor(w.client, "A1")[1].seqNum
	if before < 2 {
		t.Fatalf("the venue's second report came under seq %d; the test proves nothing", before)
	}

	mark := w.client.arrivalCount()
	logons := w.client.logonCount()
	w.restart()
	// Waiting on client.up returns immediately — it is still true from the
	// session that just went away — and waiting on the Logon *message* is too
	// early, because the venue follows it with a ResendRequest and an
	// application message sent into that recovery is gap-filled away rather than
	// executed. A completed logon is the condition that means "ready to trade".
	waitFor(t, func() bool { return w.client.logonCount() > logons }, "the client to log back on")

	// The session-level span is checked FIRST, before anything depends on an
	// order filling. A venue that came back with an empty store cannot resume,
	// so the order would never fill either — but "timed out waiting for a fill"
	// is a symptom five steps from its cause, and this test exists to name the
	// cause.
	//
	// "The number went up" is NOT the assertion, and this test made exactly that
	// mistake until the Part 7 adversarial review: with the store discarded on
	// restart it passed. A venue that lost its store renumbers from one, the
	// client's resend recovery pulls it forward, and the reports still arrive
	// under numbers higher than before.
	//
	// What only a preserved store satisfies is CONTIGUITY ACROSS THE RESTART:
	// the venue's parting Logout and the new process's Logon carry exactly the
	// next two numbers, with no gap and no re-synchronisation between them.
	span := w.client.arrivalsSince(mark)
	if len(span) < 2 {
		t.Fatalf("only %d messages across the restart; expected at least a Logout and a Logon",
			len(span))
	}
	expect := before + 1
	for i, m := range span {
		if m.msgType == msgTypeSequenceReset {
			t.Fatalf("message %d across the restart is a SequenceReset at seq %d: the session "+
				"re-synchronised rather than resumed", i, m.seqNum)
		}
		if m.seqNum != expect {
			t.Fatalf("message %d across the restart (35=%s) carries seq %d, want %d. "+
				"The venue reached %d before the restart, so a resumed session continues "+
				"from there; a renumbering or a gap means the store did not survive.",
				i, m.msgType, m.seqNum, expect, before)
		}
		expect++
	}
	if !seenMsgType(span, msgTypeLogon) {
		t.Fatal("no Logon in the span across the restart; the venue never came back")
	}

	// And the resumed session still trades. The wait for quiet is not padding:
	// the venue answers the reconnect with a ResendRequest, and an application
	// message sent into that recovery is gap-filled away rather than executed —
	// a SequenceReset-GapFill legitimately skips application messages, which is
	// the protocol working as designed. The client's own OnLogon fires before
	// that exchange has finished, so "logged on" is not yet "ready to trade".
	waitQuiet(t, w.client, 500*time.Millisecond)

	w.client.order("A2", enum.Side_BUY, "3", "2350.00", enum.TimeInForce_GOOD_TILL_CANCEL)
	waitFor(t, func() bool { return len(reportsFor(w.client, "A2")) == 2 }, "the second order to fill")

	after := reportsFor(w.client, "A2")[0].seqNum
	if after <= before {
		t.Fatalf("after the restart the venue sent seq %d, having previously reached %d", after, before)
	}
}
