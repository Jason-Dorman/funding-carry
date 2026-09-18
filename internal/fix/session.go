package fix

import (
	"context"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/quickfixgo/quickfix"

	"github.com/Jason-Dorman/funding-carry/internal/db"
)

// The operational record of the session: one fix_sessions row per logon,
// rewritten when it ends.
//
// quickfixgo's file store holds the authoritative sequence numbers — these rows
// are what a dashboard and the resend demo read, and what makes "did it resume,
// or did it start again from one?" a question answerable in SQL after the fact.

// recordTimeout bounds the write of a session row.
//
// The row is written from quickfix's callbacks, and the last one lands during
// shutdown, after the root context has been canceled. Architecture section 8 is
// explicit that shutdown I/O runs on a context the cancellation cannot reach,
// with a timeout of its own: handing the root context to this write would mean
// SIGTERM erased the record of the session it was ending.
const recordTimeout = 5 * time.Second

type sessionRecorder struct {
	sink Sink
	log  *slog.Logger
	now  func() time.Time

	mu          sync.Mutex
	startedAt   time.Time
	disconnects int32
}

func newSessionRecorder(sink Sink, log *slog.Logger, now func() time.Time) *sessionRecorder {
	return &sessionRecorder{sink: sink, log: log.With("component", "fix_session"), now: now}
}

// logon opens the record, or reopens it: a row with no end, and the sequence
// numbers left from the last time it went down.
//
// One row spans this process's whole session, not one connection. That is what
// makes `disconnects` a number worth having — a row per connection could only
// ever carry 0 or 1, and a counter that restarted on each of them would read
// 1, 2, 1 down the table with nothing saying why. Started_at is therefore the
// *first* logon of this run and stays put, which is also what lets the upsert
// find the row again. A restarted process is a new row, because it is a new run.
//
// A row that stays open is a true statement, not a missing update — either the
// session is up, or the process died without logging out. Both are worth being
// able to read later, and both are invisible if rows are only written once a
// session has ended tidily.
func (r *sessionRecorder) logon(id quickfix.SessionID) {
	r.mu.Lock()
	if r.startedAt.IsZero() {
		r.startedAt = r.now().UTC()
	}
	started := r.startedAt
	disconnects := r.disconnects
	r.mu.Unlock()

	// The sequence numbers are read here as well as at logout, and on a
	// reconnect that is the interesting half: the store has just resumed from
	// them, so this row records the position the session came back on. It is the
	// audit trail for the one thing a restart has to preserve — and writing them
	// is also what stops the reconnect from blanking what the logout recorded.
	in, out := sequenceNumbers(id)
	r.log.Info("session logged on", "session", id.String(),
		"started_at", started, "disconnects", disconnects,
		"resumed_in_seq", seqValue(in), "resumed_out_seq", seqValue(out))
	r.write(db.FIXSessionRow{
		SessionID: id.String(),
		StartedAt: started,
		// EndedAt is left zero, which writes NULL: the session is up again, so
		// the time it was last down is no longer the truth about it.
		LastInSeq:   in,
		LastOutSeq:  out,
		Disconnects: disconnects,
	})
}

// logout closes the record with the sequence numbers the store reached.
//
// The row upserts on (session_id, started_at), so this rewrites the row logon
// opened rather than adding a second one — and a reconnect rewrites it again.
func (r *sessionRecorder) logout(id quickfix.SessionID) {
	r.mu.Lock()
	// A logout with no logon before it happens when a connection is refused at
	// the handshake. Stamping it with the current time keeps it a row of its own
	// rather than an update of someone else's — and it is cleared again below,
	// because a failed handshake is not the start of the session that follows it.
	orphan := r.startedAt.IsZero()
	if orphan {
		r.startedAt = r.now().UTC()
	}
	started := r.startedAt
	r.disconnects++
	disconnects := r.disconnects
	if orphan {
		r.startedAt = time.Time{}
	}
	r.mu.Unlock()

	ended := r.now().UTC()
	in, out := sequenceNumbers(id)
	r.log.Info("session logged out", "session", id.String(),
		"started_at", started, "ended_at", ended, "disconnects", disconnects)
	r.write(db.FIXSessionRow{
		SessionID:   id.String(),
		StartedAt:   started,
		EndedAt:     ended,
		LastInSeq:   in,
		LastOutSeq:  out,
		Disconnects: disconnects,
	})
}

// sequenceNumbers reads what the store finished on.
//
// quickfix reports the *next* number it expects in each direction, so the last
// one used is one below it. A session that has never exchanged a message reports
// 1 and 1, which would be a last-used of zero — not a sequence number, so the
// columns stay NULL rather than claiming a message that never existed.
func sequenceNumbers(id quickfix.SessionID) (in, out *int32) {
	if expected, err := quickfix.GetExpectedTargetNum(id); err == nil {
		in = lastUsed(expected)
	}
	if expected, err := quickfix.GetExpectedSenderNum(id); err == nil {
		out = lastUsed(expected)
	}
	return in, out
}

// lastUsed turns quickfix's next-expected number into the last one used, or NULL.
//
// The column is an integer, and FIX sequence numbers are counted in an int: the
// bound is checked rather than converted blind, because a session that had
// somehow run past it would otherwise write a negative sequence number, which is
// a worse answer than none.
func lastUsed(expected int) *int32 {
	if expected <= 1 || expected > math.MaxInt32 {
		return nil
	}
	return db.Opt(int32(expected - 1))
}

// seqValue renders a sequence number for a log line, where a nil pointer is a
// session that has not exchanged anything yet rather than a zero.
func seqValue(n *int32) any {
	if n == nil {
		return "none"
	}
	return *n
}

// write records the row on a context the shutdown cannot cancel.
//
// A failure is logged and nothing more. The session record is operational
// history: losing a row is a hole in the dashboard, while failing the shutdown
// over it would lose the fills the writer is still flushing.
func (r *sessionRecorder) write(row db.FIXSessionRow) {
	// Background, not the root context: this is the "cancellation stops
	// producers, not writes in flight" rule from architecture section 8, and
	// context.Background is the context a cancellation cannot reach. The timeout
	// is what keeps a stuck writer from holding the shutdown open.
	ctx, cancel := context.WithTimeout(context.Background(), recordTimeout)
	defer cancel()

	if err := r.sink.Submit(ctx, row); err != nil {
		r.log.Warn("session record not written", "session", row.SessionID, "error", err)
	}
}
