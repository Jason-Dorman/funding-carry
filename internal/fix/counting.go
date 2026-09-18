package fix

import (
	"log/slog"
	"math"
	"sync"

	"github.com/quickfixgo/quickfix"
	"github.com/quickfixgo/tag"

	"github.com/Jason-Dorman/funding-carry/internal/db"
)

// Message counting: the label vocabulary of fix_msgs_total and the counter
// both halves of the session feed.

// FIX MsgTypes this package names.
const (
	msgTypeNewOrderSingle     = "D"
	msgTypeOrderCancelRequest = "F"
	msgTypeResendRequest      = "2"
	msgTypeSequenceReset      = "4"
	msgTypeOrderCancelReject  = "9"
	msgTypeExecutionReport    = "8"
	msgTypeBusinessReject     = "j"
	msgTypeReject             = "3"
	msgTypeLogout             = "5"
	msgTypeLogon              = "A"
	msgTypeUnknown            = "unknown"
	msgTypeOther              = "other"
)

// countedMsgTypes is the label vocabulary of fix_msgs_total.
//
// The MsgType comes off the wire, and API spec section 6 requires these labels
// stay low-cardinality. No data dictionary is configured, so tag 35 reaches this
// code as whatever the client put there: an unfiltered label would let one
// buggy — or hostile — client on the FIX port mint a new Prometheus time series
// per message and take the metrics endpoint down with it. Anything outside the
// set this venue actually speaks is counted as "other", which preserves the
// signal an operator needs (something is arriving that we do not handle) without
// the cardinality. Found by the Part 7 adversarial review.
var countedMsgTypes = map[string]bool{
	"0": true, // Heartbeat
	"1": true, // TestRequest
	"2": true, // ResendRequest
	"3": true, // Reject
	"4": true, // SequenceReset
	"5": true, // Logout
	"A": true, // Logon
	"8": true, // ExecutionReport
	"9": true, // OrderCancelReject
	"D": true, // NewOrderSingle
	"F": true, // OrderCancelRequest
	"j": true, // BusinessMessageReject
}

// msgCounter is the one place a FIX message becomes a metric, shared by the
// acceptor and the initiator so the two ends of the session count the same
// way. It also keeps the highest sequence number seen in each direction,
// which is what the session record and the logon log lines report.
//
// The numbers are read off the message headers as they pass through the
// application callbacks — quickfix assigns the outbound number before ToAdmin
// and ToApp see the message, and an inbound message reaches FromAdmin or
// FromApp only after its number has been checked. They are deliberately not
// read from quickfix's store: GetExpectedSenderNum reads the store without the
// session's send lock, so a logout being recorded on the session goroutine
// while a fill is being sent on the engine's is a data race, and the race
// detector found exactly that once the initiator's tests gave the acceptor a
// client that disconnects while orders are working.
type msgCounter struct {
	sm  *SessionMetrics
	log *slog.Logger

	mu      sync.Mutex
	lastIn  int
	lastOut int
}

func newMsgCounter(sm *SessionMetrics, log *slog.Logger) *msgCounter {
	return &msgCounter{sm: sm, log: log}
}

// count records one message in one direction, and notices the one admin message
// that means something operationally: a ResendRequest is a sequence recovery in
// progress, which is the event the file-backed store exists to make possible.
func (c *msgCounter) count(msg *quickfix.Message, dir string) {
	c.noteSeqNum(msg, dir)
	msgType, err := msg.MsgType()
	switch {
	case err != nil:
		// A message whose own type could not be read still happened, and a
		// counter that skipped it would make the gap invisible.
		msgType = msgTypeUnknown
	case !countedMsgTypes[msgType]:
		msgType = msgTypeOther
	}
	c.sm.message(msgType, dir)
	if msgType == msgTypeResendRequest {
		c.sm.resend()
		c.log.Info("sequence recovery", "dir", dir, "session", c.sm.id)
	}
}

// noteSeqNum keeps the highest number seen in a direction. Highest rather than
// latest because a resend replays earlier numbers with PossDupFlag set, and
// "the last number this session used" should not go backwards while it does.
func (c *msgCounter) noteSeqNum(msg *quickfix.Message, dir string) {
	n, err := msg.Header.GetInt(tag.MsgSeqNum)
	if err != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if dir == dirIn && n > c.lastIn {
		c.lastIn = n
	}
	if dir == dirOut && n > c.lastOut {
		c.lastOut = n
	}
}

// sequenceNumbers is the last number used in each direction, or NULL for a
// direction nothing has travelled in yet.
func (c *msgCounter) sequenceNumbers() (in, out *int32) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return seqColumn(c.lastIn), seqColumn(c.lastOut)
}

// seqColumn turns a sequence number into the nullable column value. Zero is
// "nothing yet", not a sequence number, so it stays NULL. The column is an
// integer, and FIX sequence numbers are counted in an int: the bound is
// checked rather than converted blind, because a session that had somehow run
// past it would otherwise write a negative sequence number, which is a worse
// answer than none.
func seqColumn(n int) *int32 {
	if n <= 0 || n > math.MaxInt32 {
		return nil
	}
	return db.Opt(int32(n))
}
