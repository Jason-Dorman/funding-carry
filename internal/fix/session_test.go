package fix

import (
	"errors"
	"testing"
	"time"

	"github.com/quickfixgo/quickfix"
)

// errWriterGone stands in for the writer having stopped.
var errWriterGone = errors.New("writer stopped")

// The fix_sessions record: one row per logon, rewritten when the session ends.

func testSessionID() quickfix.SessionID {
	return quickfix.SessionID{BeginString: quickfix.BeginStringFIX44, SenderCompID: "SIMV", TargetCompID: "CARRY"}
}

// A session that is up has a row with no end. That is a true statement rather
// than a missing update, and it is invisible if rows are only written once a
// session has ended tidily — which is exactly the case a crash does not produce.
func TestALiveSessionIsRecordedWithNoEnd(t *testing.T) {
	t.Parallel()

	sink := &fakeSink{}
	clock := newClock(epoch)
	r := newSessionRecorder(sink, testLogger(), clock.now)

	r.logon(testSessionID())

	rows := sink.sessions()
	if len(rows) != 1 {
		t.Fatalf("%d session rows, want 1", len(rows))
	}
	row := rows[0]
	if row.SessionID != testSessionID().String() {
		t.Errorf("session_id %q, want %q", row.SessionID, testSessionID().String())
	}
	if !row.StartedAt.Equal(epoch) {
		t.Errorf("started_at %s, want %s", row.StartedAt, epoch)
	}
	if !row.EndedAt.IsZero() {
		t.Errorf("ended_at %s on a live session, want the zero time, which writes NULL", row.EndedAt)
	}
	if row.LastInSeq != nil || row.LastOutSeq != nil {
		t.Error("sequence numbers recorded before anything was exchanged")
	}
}

// The logout rewrites the row the logon opened, which is what the upsert on
// (session_id, started_at) is for: the same key, carrying the facts that only
// exist at the end.
func TestLogoutRewritesTheSameRow(t *testing.T) {
	t.Parallel()

	sink := &fakeSink{}
	clock := newClock(epoch)
	r := newSessionRecorder(sink, testLogger(), clock.now)

	r.logon(testSessionID())
	clock.advance(90 * time.Minute)
	r.logout(testSessionID())

	rows := sink.sessions()
	if len(rows) != 2 {
		t.Fatalf("%d session rows, want 2 writes of one row", len(rows))
	}
	opened, closed := rows[0], rows[1]
	if !closed.StartedAt.Equal(opened.StartedAt) {
		t.Fatalf("the close is stamped %s against an open of %s: a different key would add a "+
			"second row rather than completing the first", closed.StartedAt, opened.StartedAt)
	}
	if closed.EndedAt.IsZero() {
		t.Error("ended_at is unset on a session that logged out")
	}
	if closed.Disconnects != 1 {
		t.Errorf("disconnects %d, want 1", closed.Disconnects)
	}
}

// Sequence numbers come from quickfix's own store. For a session that was never
// registered — or one that never exchanged a message — there is no last-used
// number, and the columns stay NULL rather than claiming a message that never
// existed.
func TestSequenceNumbersAreNullWhenThereAreNone(t *testing.T) {
	t.Parallel()

	in, out := sequenceNumbers(testSessionID())
	if in != nil || out != nil {
		t.Errorf("last_in_seq %v last_out_seq %v, want NULL on both", in, out)
	}
}

// A logout with no logon before it happens when a connection is refused at the
// handshake. It is stamped with its own time, so it becomes a row of its own
// rather than an update of a session someone else is running.
func TestALogoutWithoutALogonIsItsOwnRow(t *testing.T) {
	t.Parallel()

	sink := &fakeSink{}
	clock := newClock(epoch)
	r := newSessionRecorder(sink, testLogger(), clock.now)

	r.logout(testSessionID())

	rows := sink.sessions()
	if len(rows) != 1 {
		t.Fatalf("%d rows, want 1", len(rows))
	}
	if rows[0].StartedAt.IsZero() {
		t.Error("started_at is the zero time, which the schema refuses as NOT NULL")
	}
	if rows[0].Disconnects != 1 {
		t.Errorf("disconnects %d, want 1", rows[0].Disconnects)
	}
}

// A flapping session is one row with a rising count, not a row per connection.
//
// This is what the first live run got wrong: every logon opened a new row, so
// three rows carried disconnect counts of 1, 2 and 1 — a running total split
// across rows that reset whenever the process did, which is unreadable in either
// direction. One row per run, started_at fixed at the first logon, makes the
// count mean what its name says.
func TestAFlappingSessionIsOneRowWithARisingCount(t *testing.T) {
	t.Parallel()

	sink := &fakeSink{}
	clock := newClock(epoch)
	r := newSessionRecorder(sink, testLogger(), clock.now)

	for range 3 {
		r.logon(testSessionID())
		clock.advance(time.Minute)
		r.logout(testSessionID())
		clock.advance(time.Minute)
	}

	rows := sink.sessions()
	for i, row := range rows {
		if !row.StartedAt.Equal(epoch) {
			t.Fatalf("row %d is stamped %s, want the first logon at %s: every write must "+
				"address the same row", i, row.StartedAt, epoch)
		}
	}
	last := rows[len(rows)-1]
	if last.Disconnects != 3 {
		t.Errorf("disconnects %d after three cycles, want 3", last.Disconnects)
	}
}

// Coming back up clears the end: ended_at is when the session was last down, and
// a session that has reconnected is not down.
func TestAReconnectReopensTheRow(t *testing.T) {
	t.Parallel()

	sink := &fakeSink{}
	clock := newClock(epoch)
	r := newSessionRecorder(sink, testLogger(), clock.now)

	r.logon(testSessionID())
	clock.advance(time.Minute)
	r.logout(testSessionID())
	clock.advance(time.Minute)
	r.logon(testSessionID())

	rows := sink.sessions()
	reopened := rows[len(rows)-1]
	if !reopened.EndedAt.IsZero() {
		t.Errorf("ended_at is %s after reconnecting, want the zero time, which writes NULL",
			reopened.EndedAt)
	}
	if reopened.Disconnects != 1 {
		t.Errorf("disconnects %d, want the one that already happened", reopened.Disconnects)
	}
}

// A failed write is logged and nothing more. The session record is operational
// history: losing a row is a hole in a dashboard, while failing the shutdown
// over it would lose the fills the writer is still flushing.
func TestASessionRecordFailureIsNotFatal(t *testing.T) {
	t.Parallel()

	sink := &fakeSink{}
	sink.stop(errWriterGone)
	r := newSessionRecorder(sink, testLogger(), newClock(epoch).now)

	r.logon(testSessionID())
	r.logout(testSessionID())
}
