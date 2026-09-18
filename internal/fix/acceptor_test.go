package fix

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/quickfixgo/enum"
	"github.com/quickfixgo/field"
	"github.com/quickfixgo/fix44/newordersingle"
	"github.com/quickfixgo/fix44/ordercancelrequest"
	"github.com/quickfixgo/quickfix"
	"github.com/quickfixgo/tag"

	"github.com/Jason-Dorman/funding-carry/internal/config"
)

func testSession(t *testing.T) config.FIXSession {
	t.Helper()
	return config.FIXSession{
		Sender:    "CARRY",
		Target:    "SIMV",
		Host:      "sim-venue",
		Port:      5001,
		StorePath: t.TempDir(),
	}
}

// The comp ids are the mistake worth a test of its own. FIX_SENDER and
// FIX_TARGET are written from the initiator's point of view — carry sends, the
// simulator receives — so the acceptor's own SenderCompID is the configured
// *target*. Getting it backwards produces a session that never logs on, with a
// message that says only that the comp ids do not match.
func TestAcceptorSettingsInvertTheCompIDs(t *testing.T) {
	t.Parallel()

	cfg := testSession(t)
	settings, sessionID, err := acceptorSettings(cfg)
	if err != nil {
		t.Fatalf("settings: %v", err)
	}

	if sessionID.SenderCompID != cfg.Target || sessionID.TargetCompID != cfg.Sender {
		t.Errorf("session is %s->%s, want %s->%s (the acceptor sends as the configured target)",
			sessionID.SenderCompID, sessionID.TargetCompID, cfg.Target, cfg.Sender)
	}
	if sessionID.BeginString != quickfix.BeginStringFIX44 {
		t.Errorf("BeginString %s, want FIX.4.4", sessionID.BeginString)
	}

	session := settings.SessionSettings()[sessionID]
	for _, tc := range []struct{ key, want string }{
		{"ConnectionType", "acceptor"},
		{"HeartBtInt", "30"},
		// Sequence continuity is the point of running a real FIX session: a
		// session that reset on logon would renumber itself and the resend that
		// should recover a missed fill would never happen.
		{"ResetOnLogon", "N"},
		{"ResetOnLogout", "N"},
		{"ResetOnDisconnect", "N"},
	} {
		got, err := session.Setting(tc.key)
		if err != nil {
			t.Errorf("%s is unset, want %q", tc.key, tc.want)
			continue
		}
		if got != tc.want {
			t.Errorf("%s = %q, want %q", tc.key, got, tc.want)
		}
	}

	// No time range: the venue trades around the clock apart from one weekly
	// break, and that break is a trading rule rather than a reason to renumber
	// the session.
	for _, key := range []string{"StartTime", "EndTime"} {
		if session.HasSetting(key) {
			t.Errorf("%s is set; the session would roll and reset its sequence numbers", key)
		}
	}

	global := settings.GlobalSettings()
	assertSetting(t, global, "SocketAcceptPort", "5001")
	assertSetting(t, global, "FileStorePath", filepath.Join(cfg.StorePath, "store"))
	assertSetting(t, global, "FileLogPath", filepath.Join(cfg.StorePath, "log"))
}

func assertSetting(t *testing.T, s *quickfix.SessionSettings, key, want string) {
	t.Helper()
	got, err := s.Setting(key)
	if err != nil {
		t.Errorf("%s is unset, want %q", key, want)
		return
	}
	if got != want {
		t.Errorf("%s = %q, want %q", key, got, want)
	}
}

// acceptorHarness is an Acceptor with its engine running but no socket: enough
// to drive the quickfix.Application callbacks directly.
type acceptorHarness struct {
	acceptor *Acceptor
	venue    *recordingVenue
	sink     *fakeSink
	reg      *prometheus.Registry
	session  quickfix.SessionID
}

func newAcceptorHarness(t *testing.T) *acceptorHarness {
	t.Helper()

	reg := prometheus.NewRegistry()
	sink := &fakeSink{}
	books := newBooks(time.Now().UTC(), "2344.50", "2345.00")

	a, err := NewAcceptor(Options{
		Product: testPerp,
		Session: testSession(t),
		Fill:    testFill(),
	}, books, sink, NewMetrics(reg), testLogger())
	if err != nil {
		t.Fatalf("new acceptor: %v", err)
	}

	// The production venue is quickfix's session registry, which needs a socket
	// and a logged-on client. Swapping it keeps these tests about the routing.
	out := &recordingVenue{}
	a.engine.out = out

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = a.engine.run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	return &acceptorHarness{acceptor: a, venue: out, sink: sink, reg: reg, session: a.sessionID}
}

// The two application messages the simulator answers, arriving the way they
// really arrive: through FromApp on a connection goroutine.
func TestFromAppRoutesOrdersAndCancels(t *testing.T) {
	h := newAcceptorHarness(t)

	if err := h.acceptor.FromApp(testNOS(nil), h.session); err != nil {
		t.Fatalf("a well-formed order was refused: %v", err)
	}
	waitFor(t, func() bool { return len(h.venue.all()) == 1 }, "the order to be acknowledged")
	if got := h.venue.last().ordStatus; got != enum.OrdStatus_NEW {
		t.Fatalf("first report is %s, want NEW", got)
	}

	cancelReq := ordercancelrequest.New(
		field.NewOrigClOrdID("A1"),
		field.NewClOrdID("C1"),
		field.NewSide(enum.Side_BUY),
		field.NewTransactTime(time.Now().UTC()),
	)
	if err := h.acceptor.FromApp(cancelReq.ToMessage(), h.session); err != nil {
		t.Fatalf("a well-formed cancel was refused: %v", err)
	}
	waitFor(t, func() bool { return len(h.venue.all()) == 2 }, "the cancel to be acknowledged")
	if got := h.venue.last().ordStatus; got != enum.OrdStatus_CANCELED {
		t.Fatalf("second report is %s, want CANCELED", got)
	}
}

// A message this venue does not process is answered, never dropped: API spec
// section 4.2 is explicit about it, and silence on a FIX session is indis-
// tinguishable from a venue that has stopped.
func TestFromAppRefusesAMessageTypeTheVenueDoesNotHandle(t *testing.T) {
	h := newAcceptorHarness(t)

	// A NewOrderList (35=E) is well-formed FIX and not something this venue does.
	msg := quickfix.NewMessage()
	msg.Header.SetString(tag.MsgType, "E")

	err := h.acceptor.FromApp(msg, h.session)
	if err == nil {
		t.Fatal("an unsupported message type was accepted in silence")
	}
}

// A malformed order is a protocol failure, and the answer is a session-level
// Reject rather than an ExecutionReport: there is no order to report against.
func TestFromAppRejectsAMalformedOrder(t *testing.T) {
	h := newAcceptorHarness(t)

	msg := testNOS(func(_ newordersingle.NewOrderSingle, m *quickfix.Message) {
		m.Body.Remove(tag.ClOrdID)
	})
	err := h.acceptor.FromApp(msg, h.session)
	if err == nil {
		t.Fatal("an order with no ClOrdID was accepted")
	}
	if got := err.RefTagID(); got == nil || *got != tag.ClOrdID {
		t.Errorf("reject names tag %v, want ClOrdID", got)
	}
	if len(h.venue.all()) != 0 {
		t.Error("a malformed message produced an ExecutionReport; there is no order to report against")
	}
}

// Every message is counted in the direction it travelled, and a ResendRequest is
// counted again as what it operationally is: a sequence recovery in progress,
// which is the event the file-backed store exists to make possible.
func TestMessagesAreCounted(t *testing.T) {
	h := newAcceptorHarness(t)

	logon := quickfix.NewMessage()
	logon.Header.SetString(tag.MsgType, "A")
	if err := h.acceptor.FromAdmin(logon, h.session); err != nil {
		t.Fatalf("a logon was rejected: %v", err)
	}

	resend := quickfix.NewMessage()
	resend.Header.SetString(tag.MsgType, msgTypeResendRequest)
	if err := h.acceptor.FromAdmin(resend, h.session); err != nil {
		t.Fatalf("a resend request was rejected: %v", err)
	}

	heartbeat := quickfix.NewMessage()
	heartbeat.Header.SetString(tag.MsgType, "0")
	h.acceptor.ToAdmin(heartbeat, h.session)

	id := h.session.String()
	if got := testutil.ToFloat64(h.acceptorMetric(t, "fix_msgs_total", id, "A", dirIn)); got != 1 {
		t.Errorf("inbound logons counted %v, want 1", got)
	}
	if got := testutil.ToFloat64(h.acceptorMetric(t, "fix_msgs_total", id, "0", dirOut)); got != 1 {
		t.Errorf("outbound heartbeats counted %v, want 1", got)
	}
	if got := testutil.CollectAndCount(h.reg, "fix_resend_events_total"); got != 1 {
		t.Fatalf("fix_resend_events_total has %d series, want 1", got)
	}
	if got := testutil.ToFloat64(h.acceptor.counter.sm.resends); got != 1 {
		t.Errorf("resend events counted %v, want 1", got)
	}
}

// acceptorMetric reaches the one counter a test is about, by the labels the
// catalogue names.
func (h *acceptorHarness) acceptorMetric(t *testing.T, _, session, msgType, dir string) prometheus.Counter {
	t.Helper()
	return h.acceptor.counter.sm.msgs.WithLabelValues(session, msgType, dir)
}

// The store directory is where sequence numbers live, so a path that cannot be
// created is a startup failure rather than a session that silently starts again
// from one.
func TestRunFailsWhenTheStoreCannotBeCreated(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("in the way"), 0o600); err != nil {
		t.Fatal(err)
	}

	session := testSession(t)
	session.StorePath = blocker
	a, err := NewAcceptor(Options{Product: testPerp, Session: session, Fill: testFill()},
		newBooks(time.Now().UTC(), "2344.50", "2345.00"), &fakeSink{},
		NewMetrics(prometheus.NewRegistry()), testLogger())
	if err != nil {
		t.Fatalf("new acceptor: %v", err)
	}

	if err := a.Run(t.Context()); err == nil {
		t.Fatal("Run succeeded with a store path it could not create")
	}
}

// waitFor polls a condition that another goroutine satisfies. It is a poll
// rather than a sleep: the engine hands the report over on its own goroutine,
// and a fixed sleep would either be flaky or slow.
func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}
