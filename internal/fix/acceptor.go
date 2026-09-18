package fix

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/quickfixgo/quickfix"
	filelog "github.com/quickfixgo/quickfix/log/file"
	filestore "github.com/quickfixgo/quickfix/store/file"

	"github.com/Jason-Dorman/funding-carry/internal/config"
)

// The acceptor half of the session: sim-venue's front door.
//
// It implements quickfix.Application, which is a set of callbacks on quickfix's
// own connection goroutines. Nothing here holds state of its own beyond the
// metrics and the session record — an order arriving on this goroutine is handed
// to the engine over a channel, and the engine's goroutine owns the book.

// storeDirMode is deliberately not world-readable: the FileLog holds every
// message of every session verbatim.
const storeDirMode = 0o750

// heartBtInt is the session heartbeat from API spec section 4.1.
const heartBtInt = 30

// Options configure the simulator.
type Options struct {
	// Product is the only symbol the simulator makes a market in. An order for
	// anything else is refused by name rather than filled against a book that
	// describes a different instrument.
	Product string
	Session config.FIXSession
	Fill    config.FillModel

	// Seed fixes the latency jitter. Fills are deterministic under it (API spec
	// section 4.3), which is what makes a simulated run reproducible.
	Seed uint64

	// Now and Wait are the clock seams. Production leaves them nil.
	Now  func() time.Time
	Wait waitFunc
}

// defaultSeed makes an ordinary run reproducible too, not just a test.
//
// The alternative — seeding from the clock — would mean a demo that could never
// be replayed and a bug report that could never be reproduced, in exchange for
// randomness nothing here needs. The jitter models a venue's response time; it
// is not a source of entropy.
const defaultSeed = 0x5E1F1D

// Acceptor is the FIX 4.4 acceptor and its fill model: sim-venue.
type Acceptor struct {
	sessionID quickfix.SessionID
	settings  *quickfix.Settings
	storeRoot string
	engine    *engine
	sessions  *sessionRecorder
	counter   *msgCounter
	log       *slog.Logger
	now       func() time.Time
}

// NewAcceptor builds the simulator. It touches neither the network nor the disk;
// Run does both, so a configuration mistake and a bind failure stay distinct.
func NewAcceptor(opts Options, books BookSource, sink Sink, m *Metrics, log *slog.Logger) (*Acceptor, error) {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	if opts.Seed == 0 {
		opts.Seed = defaultSeed
	}

	settings, sessionID, err := acceptorSettings(opts.Session)
	if err != nil {
		return nil, err
	}

	log = log.With("component", "sim_venue")
	counter := newMsgCounter(m.session(sessionID.String()), log)
	return &Acceptor{
		sessionID: sessionID,
		settings:  settings,
		storeRoot: opts.Session.StorePath,
		engine: newEngine(engineOptions{
			Product: opts.Product,
			Fill:    opts.Fill,
			Seed:    opts.Seed,
			Now:     now,
			Wait:    opts.Wait,
		}, books, sink, fixSender{}, m, log),
		sessions: newSessionRecorder(sink, log, now, counter.sequenceNumbers),
		counter:  counter,
		log:      log,
		now:      now,
	}, nil
}

// Run listens until ctx is canceled, then logs the session out and stops.
//
// It returns an error only for a failure the simulator cannot continue past: it
// could not listen, or the writer died and its fills can no longer be recorded.
func (a *Acceptor) Run(ctx context.Context) error {
	for _, dir := range []string{a.storePath(), a.logPath()} {
		if err := os.MkdirAll(dir, storeDirMode); err != nil {
			return fmt.Errorf("fix store directory %s: %w", dir, err)
		}
	}

	fileLog, err := filelog.NewLogFactory(a.settings)
	if err != nil {
		return fmt.Errorf("fix log factory: %w", err)
	}
	logFactory := newTeeLogFactory(fileLog, a.log)

	acceptor, err := quickfix.NewAcceptor(a, filestore.NewStoreFactory(a.settings), a.settings, logFactory)
	if err != nil {
		return fmt.Errorf("build fix acceptor: %w", err)
	}
	if err := acceptor.Start(); err != nil {
		return fmt.Errorf("start fix acceptor: %w", err)
	}
	a.log.Info("fix acceptor listening",
		"session", a.sessionID.String(),
		"store", a.storePath(), "log", a.logPath())

	// The engine owns the book until the root context ends. Stopping the
	// acceptor afterwards is what sends the Logout, which is the last thing the
	// shutdown order in architecture section 8 does before the pool closes — and
	// it runs after the engine so an order in flight cannot be accepted onto a
	// book that has stopped evaluating.
	runErr := a.engine.run(ctx)
	acceptor.Stop()
	a.log.Info("fix acceptor stopped", "session", a.sessionID.String())
	return runErr
}

func (a *Acceptor) storePath() string { return filepath.Join(a.storeRoot, "store") }
func (a *Acceptor) logPath() string   { return filepath.Join(a.storeRoot, "log") }

// acceptorSettings builds the session configuration from API spec section 4.1 in
// code rather than from an ini file, so the one place a session is described is
// the one place it is configured.
//
// The comp ids are swapped on purpose, and it is the mistake this function
// exists to make impossible to repeat: FIX_SENDER and FIX_TARGET are written
// from the initiator's point of view (carry sends, sim-venue receives), so the
// acceptor's own SenderCompID is the configured *target*.
func acceptorSettings(s config.FIXSession) (*quickfix.Settings, quickfix.SessionID, error) {
	settings := quickfix.NewSettings()

	global := settings.GlobalSettings()
	global.Set("SocketAcceptPort", strconv.Itoa(s.Port))
	global.Set("FileStorePath", filepath.Join(s.StorePath, "store"))
	global.Set("FileLogPath", filepath.Join(s.StorePath, "log"))

	session := quickfix.NewSessionSettings()
	session.Set("ConnectionType", "acceptor")
	session.Set("BeginString", quickfix.BeginStringFIX44)
	session.Set("SenderCompID", s.Target)
	session.Set("TargetCompID", s.Sender)
	session.Set("HeartBtInt", strconv.Itoa(heartBtInt))
	// The three resets are spelled out because sequence continuity is the entire
	// reason this is a FIX session rather than a socket with JSON on it. Every
	// one of them defaults to N already; a default is not a decision, and the
	// failure they prevent — a session that quietly starts again from one, so
	// the resend that should have recovered a missed fill never happens — looks
	// exactly like a healthy session until the day it matters.
	session.Set("ResetOnLogon", "N")
	session.Set("ResetOnLogout", "N")
	session.Set("ResetOnDisconnect", "N")

	// No StartTime or EndTime: a session with no time range never rolls and
	// never resets its sequence numbers. The venue trades around the clock apart
	// from one weekly break, and that break is a trading rule the decision
	// engine applies, not a reason to renumber the session.

	sessionID, err := settings.AddSession(session)
	if err != nil {
		return nil, quickfix.SessionID{}, fmt.Errorf("fix session settings: %w", err)
	}
	return settings, sessionID, nil
}

// fixSender is the production venue: quickfix's own session registry. Messages
// handed to it while the client is disconnected are queued and delivered on the
// next logon, which is what makes a fill that printed during a client restart
// arrive rather than disappear.
type fixSender struct{}

func (fixSender) sendReport(r report) error {
	return quickfix.SendToTarget(executionReportMessage(r), r.session)
}

func (fixSender) sendCancelReject(r cancelReject) error {
	return quickfix.SendToTarget(cancelRejectMessage(r), r.session)
}

// ---------------------------------------------------------------------------
// quickfix.Application
// ---------------------------------------------------------------------------

// OnCreate fires once per configured session, before anything connects.
func (a *Acceptor) OnCreate(sessionID quickfix.SessionID) {
	a.log.Info("fix session created", "session", sessionID.String())
}

// OnLogon opens the session's record and lights fix_session_up.
func (a *Acceptor) OnLogon(sessionID quickfix.SessionID) {
	a.counter.sm.loggedOn()
	a.sessions.logon(sessionID)
}

// OnLogout closes the record with the sequence numbers the session reached. It
// fires during shutdown too, which is why the record is written on a context the
// cancellation cannot reach.
func (a *Acceptor) OnLogout(sessionID quickfix.SessionID) {
	a.counter.sm.loggedOff()
	a.sessions.logout(sessionID)
}

// ToAdmin counts an outbound session message. The simulator never edits one:
// logons, heartbeats and resends are quickfix's business.
func (a *Acceptor) ToAdmin(msg *quickfix.Message, _ quickfix.SessionID) {
	a.counter.count(msg, dirOut)
}

// ToApp counts an outbound application message — every ExecutionReport and
// OrderCancelReject the engine sends passes through here.
func (a *Acceptor) ToApp(msg *quickfix.Message, _ quickfix.SessionID) error {
	a.counter.count(msg, dirOut)
	return nil
}

// FromAdmin counts an inbound session message and notices ResendRequests, which
// are sequence recoveries in progress.
func (a *Acceptor) FromAdmin(msg *quickfix.Message, _ quickfix.SessionID) quickfix.MessageRejectError {
	a.counter.count(msg, dirIn)
	return nil
}

// FromApp routes the two application messages the simulator answers. Anything
// else is a business reject rather than silence: API spec section 4.2 is
// explicit that a message this venue cannot process is surfaced, never dropped.
func (a *Acceptor) FromApp(msg *quickfix.Message, sessionID quickfix.SessionID) quickfix.MessageRejectError {
	a.counter.count(msg, dirIn)

	at := a.now()
	switch {
	case msg.IsMsgTypeOf(msgTypeNewOrderSingle):
		s, err := parseOrder(msg, sessionID, at)
		if err != nil {
			return err
		}
		return a.deliver(a.engine.submit(s))

	case msg.IsMsgTypeOf(msgTypeOrderCancelRequest):
		c, err := parseCancel(msg, sessionID, at)
		if err != nil {
			return err
		}
		return a.deliver(a.engine.requestCancel(c))

	case msg.IsMsgTypeOf(msgTypeBusinessReject):
		// Never answered with a reject of our own, for the same reason the
		// initiator does not: a BusinessMessageReject is itself an application
		// message, and both ends refuse unrouted application messages this way.
		// Rejecting it would have the two engines rejecting each other's
		// rejects until the connection died, burning a sequence number per
		// round trip. Found by the Part 8 adversarial review, which reached it
		// through this venue's own refusal of an order that arrives after the
		// engine has stopped.
		a.log.Warn("business message rejected by the client", businessRejectDetail(msg)...)
		return nil

	default:
		return quickfix.UnsupportedMessageType()
	}
}

// deliver turns "the engine has stopped" into an answer the client can act on.
// The alternative is worse than a reject: an order accepted onto a book nothing
// is evaluating would sit silent forever.
func (a *Acceptor) deliver(err error) quickfix.MessageRejectError {
	if err == nil {
		return nil
	}
	a.log.Warn("message refused", "error", err)
	return quickfix.NewBusinessMessageRejectError(err.Error(), applicationNotAvailable, nil)
}

// applicationNotAvailable is BusinessRejectReason 4.
const applicationNotAvailable = 4
