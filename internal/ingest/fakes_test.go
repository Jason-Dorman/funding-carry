package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Jason-Dorman/funding-carry/internal/db"
)

// ---------------------------------------------------------------------------
// Fake socket
//
// These fakes enforce every constraint the real connection enforces, which is
// the rule that internal/db's fake broke: a fake that ignores the context it is
// handed makes a cancellation test pass over a real bug (testing strategy,
// principle 3). Read here fails on a canceled or expired context before it looks
// at its script, and blocks until the context ends when the script runs out,
// exactly as a quiet socket does.
// ---------------------------------------------------------------------------

// scriptedConn replays a fixed sequence of frames and then does whatever
// `after` says: return an error, or go quiet.
type scriptedConn struct {
	mu     sync.Mutex
	frames [][]byte
	next   int
	writes [][]byte
	closed bool

	// after is returned once the frames are exhausted. A nil value means the
	// socket goes quiet and the read parks until the context ends, which is what
	// makes the read deadline observable.
	after error

	// gate, when set, holds each frame back until the test releases it. That is
	// how a test that needs to move the clock between two frames does it: the
	// read loop is faster than any clock a test could advance, so without a gate
	// every frame would be consumed before the first assertion.
	gate chan struct{}

	// wrote reports each subscribe frame.
	wrote chan struct{}
}

func (c *scriptedConn) Read(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	c.mu.Lock()
	if c.next < len(c.frames) {
		frame := c.frames[c.next]
		c.next++
		gate := c.gate
		c.mu.Unlock()
		if gate != nil {
			select {
			case <-gate:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return frame, nil
	}
	after := c.after
	c.mu.Unlock()

	if after != nil {
		return nil, after
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func (c *scriptedConn) Write(ctx context.Context, msg []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writes = append(c.writes, msg)
	signal(c.wrote)
	return nil
}

func (c *scriptedConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}

func (c *scriptedConn) subscriptions(t *testing.T) []subscribeFrame {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()

	frames := make([]subscribeFrame, 0, len(c.writes))
	for _, w := range c.writes {
		var f subscribeFrame
		if err := json.Unmarshal(w, &f); err != nil {
			t.Fatalf("subscribe frame is not JSON: %v", err)
		}
		frames = append(frames, f)
	}
	return frames
}

// scriptedDialer hands out one connection per dial, in order, and reports how
// many dials it saw. A nil entry is a dial that fails.
type scriptedDialer struct {
	mu    sync.Mutex
	conns []*scriptedConn
	next  int
	// dialErr is returned for a nil connection.
	dialErr error
	// exhausted is returned once the script runs out, so a runaway reconnect
	// loop stops rather than panicking on an index.
	exhausted error

	// dialed reports each dial. Tests wait on it rather than sleeping until a
	// reconnect has plausibly happened (testing strategy: synchronize with
	// channels, never with the clock).
	dialed chan struct{}
}

var errDialScriptEnded = errors.New("dial script exhausted")

func (d *scriptedDialer) Dial(ctx context.Context) (Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	signal(d.dialed)
	if d.next >= len(d.conns) {
		if d.exhausted != nil {
			return nil, d.exhausted
		}
		return nil, errDialScriptEnded
	}
	c := d.conns[d.next]
	d.next++
	if c == nil {
		return nil, d.dialErr
	}
	return c, nil
}

func (d *scriptedDialer) dials() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.next
}

// ---------------------------------------------------------------------------
// Fake recorder and sink
// ---------------------------------------------------------------------------

type fakeRecorder struct {
	mu         sync.Mutex
	reconnects int
	gaps       int
	lastSeen   time.Time

	// sawData reports each data message, so a test can wait for one instead of
	// spinning on the counters.
	sawData chan struct{}
}

func (r *fakeRecorder) reconnect() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reconnects++
}

func (r *fakeRecorder) gap() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gaps++
}

func (r *fakeRecorder) seen(at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastSeen = at
	signal(r.sawData)
}

func (r *fakeRecorder) counts() (reconnects, gaps int, lastSeen time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reconnects, r.gaps, r.lastSeen
}

// fakeSink collects rows. It fails where the writer fails: a canceled context is
// an error, and once stopped it returns db.ErrWriterStopped like the real one.
type fakeSink struct {
	mu      sync.Mutex
	rows    []db.Row
	stopped bool
}

func (s *fakeSink) Submit(ctx context.Context, r db.Row) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return db.ErrWriterStopped
	}
	s.rows = append(s.rows, r)
	return nil
}

func (s *fakeSink) stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopped = true
}

func (s *fakeSink) collected() []db.Row {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]db.Row(nil), s.rows...)
}

// ---------------------------------------------------------------------------
// Fake clock
// ---------------------------------------------------------------------------

// fakeClock is the injected wall clock. Tests advance it explicitly; nothing in
// this package sleeps to make time pass (testing strategy: no wall clock in
// logic).
type fakeClock struct {
	mu sync.Mutex
	at time.Time
}

func newClock(at time.Time) *fakeClock { return &fakeClock{at: at} }

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// ---------------------------------------------------------------------------
// Fake handler
// ---------------------------------------------------------------------------

type fakeHandler struct {
	channel  string
	data     string
	products []string

	mu        sync.Mutex
	resets    int
	messages  []Message
	ticks     []time.Time
	handleErr error
	tickErr   error

	// handled and ticked report each call, so a test can wait for work to have
	// happened instead of guessing how long it takes.
	handled chan struct{}
	ticked  chan struct{}
}

func (h *fakeHandler) Subscribe() (string, []string) { return h.channel, h.products }

func (h *fakeHandler) DataChannel() string { return h.data }

func (h *fakeHandler) Reset() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.resets++
}

func (h *fakeHandler) Handle(_ context.Context, msg Message) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.messages = append(h.messages, msg)
	signal(h.handled)
	return h.handleErr
}

func (h *fakeHandler) seen() (resets int, messages []Message) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.resets, append([]Message(nil), h.messages...)
}

// tickingHandler is a fakeHandler that also keeps time, so the dispatch path
// that drives Tick can be exercised without a real book or trade bucket.
type tickingHandler struct{ *fakeHandler }

func (h tickingHandler) Tick(_ context.Context, now time.Time) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ticks = append(h.ticks, now)
	signal(h.ticked)
	return h.tickErr
}

// ---------------------------------------------------------------------------
// Synchronization helpers
// ---------------------------------------------------------------------------

// signal reports an event without ever blocking the goroutine under test. A
// fake that blocked on an unread channel would change the timing of the code it
// is supposed to be observing.
func signal(ch chan struct{}) {
	if ch == nil {
		return
	}
	select {
	case ch <- struct{}{}:
	default:
	}
}

// waitFor receives n events or fails the test. The deadline is generous because
// it is only there to turn a hang into a readable failure, never to pace
// anything.
func waitFor(t *testing.T, ch chan struct{}, n int, what string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for i := range n {
		select {
		case <-ch:
		case <-deadline:
			t.Fatalf("timed out waiting for %s: got %d of %d", what, i, n)
		}
	}
}
