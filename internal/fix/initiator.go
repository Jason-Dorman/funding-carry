package fix

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/quickfixgo/quickfix"
	filelog "github.com/quickfixgo/quickfix/log/file"
	filestore "github.com/quickfixgo/quickfix/store/file"
	"github.com/quickfixgo/tag"

	"github.com/Jason-Dorman/funding-carry/internal/carry"
	"github.com/Jason-Dorman/funding-carry/internal/config"
)

// The initiator half of the session: carry's FIX venue — `fixVenue` in the
// architecture's tables — the one carry.Venue implementation that speaks to
// sim-venue.
//
// Like the acceptor it implements quickfix.Application, a set of callbacks on
// quickfix's own goroutines. Submit and Cancel run on the caller's goroutine
// and put a message on the session; reports come back on quickfix's
// goroutine and are handed to the consumer over a channel. What the initiator
// owns beyond that is one map — what a cancel has to repeat about each order —
// and the logged-on flag that decides whether an order can be sent at all.

// ErrSessionDown is returned by Submit and Cancel while the session is not
// logged on. Nothing was sent.
//
// quickfix would accept the message anyway: it assigns a sequence number,
// writes it to the store, and the acceptor recovers it by resend on the next
// logon — which for an order means a price chosen now executing whenever the
// link comes back. An order-entry gateway that is down refuses orders; the
// caller's timeout and the risk engine decide what to do about the leg that
// could not be placed (architecture section 6.4). The window between this
// check and the send is real and small; the refusal is the boundary, the
// timeout is the backstop (ADR-0020).
var ErrSessionDown = errors.New("fix session not logged on")

// ErrUnknownOrder is returned by Cancel for a ClOrdID this initiator did not
// submit. A cancel has to name the order's symbol and side, which are known
// only for orders placed through this process; an order left resting by a
// previous run is the router's to reload (Part 15).
var ErrUnknownOrder = errors.New("order not submitted through this session")

// reconnectInterval is how long quickfix waits between connection attempts,
// in seconds. Five rather than the library's thirty: the acceptor restarting
// under carry is the case the store exists for, and half a minute of refused
// orders after it comes back is a long time to explain.
const reconnectInterval = 5

// reportBuffer is how many reports can wait between the session goroutine and
// the consumer, and deliverTimeout is how long one handoff may take before the
// report is abandoned.
//
// Neither is a measured value and the plan names neither, so both are recorded
// here as chosen rather than derived. The buffer absorbs a resend replaying a
// burst — the largest this venue can produce is one order's slices — not a
// consumer that has stopped; a consumer that has stopped is what the timeout is
// for, and thirty seconds is longer than any handoff to an in-process drain can
// legitimately take. The buffer also bounds what a `kill -9` can destroy:
// quickfix acknowledges a message — increments the inbound sequence number —
// only after FromApp returns, so a report still in flight is one the venue must
// resend, but a report sitting in this buffer has already been acknowledged.
const (
	reportBuffer   = 256
	deliverTimeout = 30 * time.Second
)

// InitiatorOptions configure the venue.
type InitiatorOptions struct {
	// Product is the symbol every order carries: the perp this session trades.
	Product string
	Session config.FIXSession

	// Now is the clock seam; NewID mints the ClOrdID a cancel request carries.
	// Production leaves both nil.
	Now   func() time.Time
	NewID func() carry.OrderID
}

// Initiator is the FIX 4.4 initiator as a carry.Venue.
type Initiator struct {
	sessionID quickfix.SessionID
	settings  *quickfix.Settings
	storeRoot string
	product   string
	counter   *msgCounter
	log       *slog.Logger
	now       func() time.Time
	newID     func() carry.OrderID

	reports chan carry.ExecReport
	// deliverWait is how long one handoff may block before the report is
	// abandoned. It is a field rather than the constant so a test can make the
	// giving-up path permanently available and assert that a report the
	// consumer can take is still taken.
	deliverWait time.Duration
	runOnce     sync.Once

	mu       sync.Mutex
	loggedOn bool
	orders   map[carry.OrderID]orderTerms
}

// NewInitiator builds the venue. It touches neither the network nor the disk;
// Run does both.
func NewInitiator(opts InitiatorOptions, m *SessionCatalogue, log *slog.Logger) (*Initiator, error) {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	newID := opts.NewID
	if newID == nil {
		newID = carry.NewOrderID
	}
	if opts.Product == "" {
		return nil, errors.New("fix initiator: no product configured")
	}

	settings, sessionID, err := initiatorSettings(opts.Session)
	if err != nil {
		return nil, err
	}

	log = log.With("component", "fix_venue")
	return &Initiator{
		sessionID:   sessionID,
		settings:    settings,
		storeRoot:   opts.Session.StorePath,
		product:     opts.Product,
		counter:     newMsgCounter(m.session(sessionID.String()), log),
		log:         log,
		now:         now,
		newID:       newID,
		reports:     make(chan carry.ExecReport, reportBuffer),
		deliverWait: deliverTimeout,
		orders:      make(map[carry.OrderID]orderTerms),
	}, nil
}

// Run connects and keeps the session up until ctx is canceled, then logs out
// and closes the report channel. It may be called once.
//
// It returns an error only for a failure that means there is no session: the
// store directory could not be created, or the initiator could not start. A
// venue that is not answering is not an error here — the session reconnects
// on its own and fix_session_up says so.
func (v *Initiator) Run(ctx context.Context) error {
	var err error
	ran := false
	v.runOnce.Do(func() {
		ran = true
		err = v.run(ctx)
	})
	if !ran {
		return errors.New("fix initiator: Run called twice")
	}
	return err
}

func (v *Initiator) run(ctx context.Context) error {
	// Closed on every exit, not only the clean one: a consumer ranging over
	// the reports must see the end whether the session ran or never started,
	// or the binary's drain waits forever on a venue that failed at startup.
	// On the clean path the close still happens last, after Stop has returned
	// and nothing can send.
	defer close(v.reports)

	for _, dir := range []string{v.storePath(), v.logPath()} {
		if err := os.MkdirAll(dir, storeDirMode); err != nil {
			return fmt.Errorf("fix store directory %s: %w", dir, err)
		}
	}

	fileLog, err := filelog.NewLogFactory(v.settings)
	if err != nil {
		return fmt.Errorf("fix log factory: %w", err)
	}
	logFactory := newTeeLogFactory(fileLog, v.log)
	initiator, err := quickfix.NewInitiator(v, filestore.NewStoreFactory(v.settings), v.settings, logFactory)
	if err != nil {
		return fmt.Errorf("build fix initiator: %w", err)
	}
	if err := initiator.Start(); err != nil {
		return fmt.Errorf("start fix initiator: %w", err)
	}
	v.log.Info("fix initiator connecting", "session", v.sessionID.String(),
		"store", v.storePath(), "log", v.logPath())

	<-ctx.Done()

	// Shutdown: log out, then tell the consumer there is nothing more. Stop
	// sends the Logout and waits for the session goroutines, so by the time the
	// deferred close of the report channel runs, nothing can send on it.
	//
	// Nothing is closed BEFORE Stop, and that ordering is the fix for a defect
	// this part shipped and its adversarial review found. Reports keep arriving
	// during the logout — quickfix's logout state delegates inbound application
	// messages to the in-session handler — and an earlier version raced those
	// reports against a closed "we are shutting down" channel. Both select cases
	// were then ready, Go chose between them at random, and roughly half of the
	// fills arriving in that window were discarded while the drain was still
	// running and the buffer still empty. quickfix acknowledged them anyway, so
	// they could never be resent: the one thing this part's second acceptance
	// criterion promises cannot happen.
	initiator.Stop()
	v.log.Info("fix initiator stopped", "session", v.sessionID.String())
	return nil
}

func (v *Initiator) storePath() string { return filepath.Join(v.storeRoot, "store") }
func (v *Initiator) logPath() string   { return filepath.Join(v.storeRoot, "log") }

// LoggedOn reports whether the session is up right now.
func (v *Initiator) LoggedOn() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.loggedOn
}

// SessionID names the session, as the metrics label it.
func (v *Initiator) SessionID() string { return v.sessionID.String() }

// ---------------------------------------------------------------------------
// carry.Venue
// ---------------------------------------------------------------------------

// Submit sends one order. It returns once the message is on the session, with
// the time it left; acceptance is the first ExecReport.
func (v *Initiator) Submit(ctx context.Context, o carry.Order) (carry.Ack, error) {
	if err := ctx.Err(); err != nil {
		return carry.Ack{}, err
	}
	w, err := translate(o)
	if err != nil {
		return carry.Ack{}, err
	}

	v.mu.Lock()
	if !v.loggedOn {
		v.mu.Unlock()
		return carry.Ack{}, ErrSessionDown
	}
	if _, dup := v.orders[o.ClOrdID]; dup {
		v.mu.Unlock()
		return carry.Ack{}, fmt.Errorf("%w: ClOrdID %s already submitted", ErrUnsupportedOrder, o.ClOrdID)
	}
	// Remembered before the send so a cancel racing the acknowledgement can
	// find it; forgotten again if the send fails.
	v.orders[o.ClOrdID] = orderTerms{symbol: v.product, side: w.side}
	v.mu.Unlock()

	at := v.now()
	if err := quickfix.SendToTarget(newOrderSingleMessage(o, w, v.product, at), v.sessionID); err != nil {
		v.forget(o.ClOrdID)
		return carry.Ack{}, fmt.Errorf("send NewOrderSingle %s: %w", o.ClOrdID, err)
	}
	v.log.Info("order sent", "cl_ord_id", o.ClOrdID, "side", string(o.Side),
		"qty", o.Qty.String(), "limit_px", o.LimitPx.String(), "type", string(o.Type), "tif", string(w.tif))
	return carry.Ack{ClOrdID: o.ClOrdID, At: at}, nil
}

// Cancel asks the venue to withdraw an order. The answer is asynchronous: an
// ExecReport CANCELED on the order, or an OrderCancelReject, which is logged
// and counted but does not move the order (ADR-0020).
func (v *Initiator) Cancel(ctx context.Context, id carry.OrderID) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	v.mu.Lock()
	terms, known := v.orders[id]
	loggedOn := v.loggedOn
	v.mu.Unlock()
	if !known {
		return fmt.Errorf("%w: %s", ErrUnknownOrder, id)
	}
	if !loggedOn {
		return ErrSessionDown
	}

	cancelID := v.newID()
	if err := quickfix.SendToTarget(cancelRequestMessage(cancelID, id, terms, v.now()), v.sessionID); err != nil {
		return fmt.Errorf("send OrderCancelRequest for %s: %w", id, err)
	}
	v.log.Info("cancel sent", "cl_ord_id", cancelID, "orig_cl_ord_id", id)
	return nil
}

// ExecReports is the stream of what happened, closed when Run returns.
func (v *Initiator) ExecReports() <-chan carry.ExecReport { return v.reports }

func (v *Initiator) forget(id carry.OrderID) {
	v.mu.Lock()
	defer v.mu.Unlock()
	delete(v.orders, id)
}

// deliver hands a report to the consumer.
//
// It blocks while the buffer is full, and that is the durability argument:
// quickfix acknowledges the message — increments the inbound sequence number —
// only after this returns, so a report not yet handed over is one the venue
// still has to resend if this process dies.
//
// The escape is a TIMEOUT, not a shutdown signal, and the distinction is the
// whole point. A signal that is already closed makes both cases of a select
// ready at once, and Go then chooses at random: the version of this function
// that raced a closed channel dropped about half the reports arriving during
// the logout, with the consumer still draining and the buffer still empty, and
// acknowledged every one of them. A timeout cannot fire while the handoff would
// succeed. So the fast path below is tried first and on its own, and the wait
// exists only for a consumer that has genuinely stopped draining — which is a
// broken caller (the contract is to drain until the channel closes), not an
// ordinary shutdown. Abandoning the report is the last resort it has always
// been: logged in full at Error, with the fill still in the venue's own log,
// store and fills row.
func (v *Initiator) deliver(r carry.ExecReport) {
	select {
	case v.reports <- r:
		return
	default:
	}

	// The buffer is full. Wait for room, but not forever.
	timer := time.NewTimer(v.deliverWait)
	defer timer.Stop()
	select {
	case v.reports <- r:
	case <-timer.C:
		v.log.Error("exec report abandoned: the consumer stopped draining",
			"cl_ord_id", r.ClOrdID, "exec_id", r.ExecID, "state", string(r.State),
			"cum_qty", r.CumQty.String(), "last_qty", r.LastQty.String(), "last_px", r.LastPx.String(),
			"waited", v.deliverWait.String())
	}
}

func (v *Initiator) setLoggedOn(up bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.loggedOn = up
}

// initiatorSettings builds the session configuration from API spec section
// 4.1 in code, mirroring acceptorSettings. The comp ids are used the way the
// variables are named — FIX_SENDER is this side.
func initiatorSettings(s config.FIXSession) (*quickfix.Settings, quickfix.SessionID, error) {
	settings := quickfix.NewSettings()

	global := settings.GlobalSettings()
	global.Set("FileStorePath", filepath.Join(s.StorePath, "store"))
	global.Set("FileLogPath", filepath.Join(s.StorePath, "log"))
	global.Set("ReconnectInterval", strconv.Itoa(reconnectInterval))

	session := quickfix.NewSessionSettings()
	session.Set("ConnectionType", "initiator")
	session.Set("BeginString", quickfix.BeginStringFIX44)
	session.Set("SenderCompID", s.Sender)
	session.Set("TargetCompID", s.Target)
	session.Set("SocketConnectHost", s.Host)
	session.Set("SocketConnectPort", strconv.Itoa(s.Port))
	session.Set("HeartBtInt", strconv.Itoa(heartBtInt))
	// Spelled out for the same reason the acceptor spells them out: sequence
	// continuity across a restart is the point, and a default is not a
	// decision.
	session.Set("ResetOnLogon", "N")
	session.Set("ResetOnLogout", "N")
	session.Set("ResetOnDisconnect", "N")

	sessionID, err := settings.AddSession(session)
	if err != nil {
		return nil, quickfix.SessionID{}, fmt.Errorf("fix session settings: %w", err)
	}
	return settings, sessionID, nil
}

// ---------------------------------------------------------------------------
// quickfix.Application
// ---------------------------------------------------------------------------

// OnCreate fires once, before anything connects.
func (v *Initiator) OnCreate(sessionID quickfix.SessionID) {
	v.log.Info("fix session created", "session", sessionID.String())
}

// OnLogon lights fix_session_up and opens the gate Submit checks. quickfix
// calls it once the logon exchange is complete, before any resend recovery
// the venue asks for.
func (v *Initiator) OnLogon(sessionID quickfix.SessionID) {
	v.setLoggedOn(true)
	v.counter.sm.loggedOn()
	in, out := v.counter.sequenceNumbers()
	v.log.Info("session logged on", "session", sessionID.String(),
		"resumed_in_seq", seqValue(in), "resumed_out_seq", seqValue(out))
}

// OnLogout closes the gate. It fires for a venue that went away as well as
// for a shutdown, and the reconnect loop is quickfix's.
func (v *Initiator) OnLogout(sessionID quickfix.SessionID) {
	v.setLoggedOn(false)
	v.counter.sm.loggedOff()
	in, out := v.counter.sequenceNumbers()
	v.log.Warn("session logged out", "session", sessionID.String(),
		"last_in_seq", seqValue(in), "last_out_seq", seqValue(out))
}

// ToAdmin counts an outbound session message.
func (v *Initiator) ToAdmin(msg *quickfix.Message, _ quickfix.SessionID) {
	v.counter.count(msg, dirOut)
}

// ToApp counts an outbound application message, and refuses to resend one.
//
// quickfix calls this from two places: once when a message is first sent, and
// again from `resend` when the peer asks for it back. The second call carries
// PossDupFlag, and returning ErrDoNotSend there makes quickfix gap-fill the
// message instead of replaying it.
//
// An order must never be replayed. The only messages this side sends are
// NewOrderSingle and OrderCancelRequest, and a venue that has asked for a
// resend has, by definition, lost its side of the session: sim-venue keeps its
// book of live orders in memory and has no duplicate-ClOrdID history across a
// restart, so a replayed NewOrderSingle is accepted as a brand-new order and
// filled a second time — a real position the client believes it already closed.
// The reachable trigger is resetting one end's store and not the other's, which
// Part 8 made possible by giving carry a store of its own. A gap fill tells the
// venue the sequence number was used and nothing more, which is exactly true:
// whatever that order was, it is no longer an instruction this process stands
// behind. Found by the Part 8 adversarial review.
func (v *Initiator) ToApp(msg *quickfix.Message, _ quickfix.SessionID) error {
	if isPossDup(msg) {
		msgType, _ := msg.MsgType()
		v.log.Warn("refusing to resend an application message; it will be gap-filled",
			"msg_type", msgType, "cl_ord_id", stringField(msg, tag.ClOrdID))
		return quickfix.ErrDoNotSend
	}
	v.counter.count(msg, dirOut)
	return nil
}

// isPossDup reports whether quickfix is replaying this message rather than
// sending it for the first time.
func isPossDup(msg *quickfix.Message) bool {
	var dup quickfix.FIXBoolean
	if err := msg.Header.GetField(tag.PossDupFlag, &dup); err != nil {
		return false
	}
	return bool(dup)
}

// stringField reads a tag for a log line, where absent is empty.
func stringField(msg *quickfix.Message, tg quickfix.Tag) string {
	if v, err := msg.Body.GetString(tg); err == nil {
		return v
	}
	return ""
}

// FromAdmin counts an inbound session message and surfaces the two that carry
// a reason: a Reject names a message of ours the venue could not read, and a
// Logout with text is the venue saying why — a comp-id mismatch and a sequence
// number too low both arrive this way and nowhere else.
func (v *Initiator) FromAdmin(msg *quickfix.Message, _ quickfix.SessionID) quickfix.MessageRejectError {
	v.counter.count(msg, dirIn)
	switch {
	case msg.IsMsgTypeOf(msgTypeReject):
		v.log.Warn("message rejected by the venue", rejectDetail(msg)...)
	case msg.IsMsgTypeOf(msgTypeLogout):
		if text, err := msg.Body.GetString(tag.Text); err == nil && text != "" {
			v.log.Warn("venue logout", "text", text)
		}
	}
	return nil
}

// FromApp turns the venue's two application messages into what the consumer
// sees: an ExecutionReport is delivered, an OrderCancelReject is logged.
// Anything else is a business reject, never silence.
func (v *Initiator) FromApp(msg *quickfix.Message, _ quickfix.SessionID) quickfix.MessageRejectError {
	v.counter.count(msg, dirIn)

	switch {
	case msg.IsMsgTypeOf(msgTypeExecutionReport):
		r, err := parseExecReport(msg, v.now())
		if err != nil {
			v.log.Error("execution report unreadable; rejected back to the venue", "error", err.Error())
			return err
		}
		v.deliver(r)
		return nil

	case msg.IsMsgTypeOf(msgTypeOrderCancelReject):
		v.log.Warn("cancel rejected", cancelRejectDetail(msg)...)
		return nil

	case msg.IsMsgTypeOf(msgTypeBusinessReject):
		// Never answered with a reject of our own. A BusinessMessageReject is
		// itself an application message, and the other end refuses unrouted
		// application messages the same way this one does — so rejecting it
		// would have each engine rejecting the other's reject until the
		// connection died, burning a sequence number per round trip. Found by
		// the Part 8 adversarial review.
		v.log.Warn("business message rejected by the venue", businessRejectDetail(msg)...)
		return nil

	default:
		return quickfix.UnsupportedMessageType()
	}
}
