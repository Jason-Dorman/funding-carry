package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
)

// testLogger keeps the reconnect and gap warnings out of the test output; the
// assertions are on the metrics, which is what the alerts read.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

var epoch = time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)

// frame builds one envelope as the venue sends it.
func frame(t *testing.T, channel string, seq int64, ts time.Time, events any) []byte {
	t.Helper()
	raw, err := json.Marshal(events)
	if err != nil {
		t.Fatalf("encode events: %v", err)
	}
	body, err := json.Marshal(map[string]any{
		"channel":      channel,
		"sequence_num": seq,
		"timestamp":    ts.Format(time.RFC3339Nano),
		"events":       json.RawMessage(raw),
	})
	if err != nil {
		t.Fatalf("encode envelope: %v", err)
	}
	return body
}

// heartbeat is a frame on the channel every connection subscribes to.
func heartbeat(t *testing.T, seq int64, ts time.Time) []byte {
	t.Helper()
	return frame(t, channelHeartbeats, seq, ts, []map[string]any{{"heartbeat_counter": seq}})
}

// fastBackoff removes the delay and the randomness from the reconnect schedule
// so a test observes the loop rather than the wait.
var fastBackoff = Backoff{
	Base:   time.Millisecond,
	Max:    time.Millisecond,
	Jitter: func(d time.Duration) time.Duration { return d },
}

// runStream starts a stream and returns a stop function that cancels it and
// waits for the goroutine, so goleak sees a clean package.
func runStream(t *testing.T, s *Stream) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Run(ctx)
	}()
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("stream did not stop after cancellation")
		}
	}
}

func TestStreamSubscribesToItsChannelAndToHeartbeats(t *testing.T) {
	conn := &scriptedConn{wrote: make(chan struct{}, 4)}
	dialer := &scriptedDialer{conns: []*scriptedConn{conn}, dialed: make(chan struct{}, 4)}
	h := &fakeHandler{channel: channelLevel2, data: channelLevel2Data, products: []string{"ETP-20DEC30-CDE"}}

	s := NewStream("level2", h, dialer, &fakeRecorder{}, testLogger(),
		StreamOptions{Backoff: fastBackoff, now: newClock(epoch).now})
	stop := runStream(t, s)
	waitFor(t, conn.wrote, 2, "both subscribe frames")
	stop()

	got := conn.subscriptions(t)
	if len(got) != 2 {
		t.Fatalf("subscribe frames = %d, want 2 (data channel then heartbeats): %+v", len(got), got)
	}
	if got[0].Channel != channelLevel2 || len(got[0].ProductIDs) != 1 {
		t.Errorf("data subscription = %+v, want channel %q with one product", got[0], channelLevel2)
	}
	// Heartbeats are what make the read deadline a liveness check on every
	// stream rather than only the busy ones. A stream that stopped subscribing
	// to them would still pass every other test in this file.
	if got[1].Channel != channelHeartbeats {
		t.Errorf("second subscription = %q, want %q", got[1].Channel, channelHeartbeats)
	}
	if len(got[1].ProductIDs) != 0 {
		t.Errorf("heartbeats subscribed with products %v, want none", got[1].ProductIDs)
	}
}

func TestStreamReconnectsAndResubscribesAfterADrop(t *testing.T) {
	first := &scriptedConn{
		frames: [][]byte{frame(t, channelTicker, 0, epoch, []map[string]any{})},
		after:  errors.New("connection reset by peer"),
	}
	second := &scriptedConn{
		frames: [][]byte{frame(t, channelTicker, 0, epoch.Add(time.Second), []map[string]any{})},
	}
	dialer := &scriptedDialer{conns: []*scriptedConn{first, second}, dialed: make(chan struct{}, 4)}
	rec := &fakeRecorder{}
	h := &fakeHandler{channel: channelTicker, data: channelTicker, handled: make(chan struct{}, 4)}

	s := NewStream("ticker", h, dialer, rec, testLogger(),
		StreamOptions{Backoff: fastBackoff, now: newClock(epoch).now})
	stop := runStream(t, s)
	waitFor(t, h.handled, 2, "a message on each connection")
	stop()

	reconnects, _, _ := rec.counts()
	if reconnects != 1 {
		t.Errorf("ingest_ws_reconnects_total = %d, want 1", reconnects)
	}
	if dialer.dials() != 2 {
		t.Errorf("dials = %d, want 2", dialer.dials())
	}
	if !first.closed {
		t.Error("the dropped connection was not closed; its goroutine and socket would leak")
	}

	// Every connection is a fresh subscription: the venue does not carry them
	// across, so a reconnect that skipped this would leave a stream connected and
	// permanently silent.
	if got := second.subscriptions(t); len(got) != 2 || got[0].Channel != channelTicker {
		t.Errorf("resubscription on the new connection = %+v", got)
	}

	// Reset is what drops state derived from the old connection — the order book
	// most of all.
	resets, _ := h.seen()
	if resets != 2 {
		t.Errorf("handler resets = %d, want 2 (one per connection)", resets)
	}
}

func TestStreamRetriesAFailedDial(t *testing.T) {
	good := &scriptedConn{frames: [][]byte{frame(t, channelTicker, 0, epoch, []map[string]any{})}}
	dialer := &scriptedDialer{
		conns:   []*scriptedConn{nil, nil, good},
		dialErr: errors.New("connection refused"),
		dialed:  make(chan struct{}, 8),
	}
	rec := &fakeRecorder{}
	h := &fakeHandler{channel: channelTicker, data: channelTicker, handled: make(chan struct{}, 4)}

	s := NewStream("ticker", h, dialer, rec, testLogger(),
		StreamOptions{Backoff: fastBackoff, now: newClock(epoch).now})
	stop := runStream(t, s)
	waitFor(t, h.handled, 1, "a message once the dial succeeds")
	stop()

	// A refused dial is a feed problem, not a fatal one: the stream keeps trying
	// and the binary keeps running (architecture section 8).
	if reconnects, _, _ := rec.counts(); reconnects != 2 {
		t.Errorf("ingest_ws_reconnects_total = %d, want 2", reconnects)
	}
}

func TestSequenceDiscontinuityIsCountedAsAGap(t *testing.T) {
	conn := &scriptedConn{frames: [][]byte{
		frame(t, channelTicker, 10, epoch, []map[string]any{}),
		frame(t, channelTicker, 11, epoch, []map[string]any{}),
		// 12 never arrives.
		frame(t, channelTicker, 13, epoch, []map[string]any{}),
	}}
	dialer := &scriptedDialer{conns: []*scriptedConn{conn}, dialed: make(chan struct{}, 4)}
	rec := &fakeRecorder{}
	h := &fakeHandler{channel: channelTicker, data: channelTicker, handled: make(chan struct{}, 8)}

	s := NewStream("ticker", h, dialer, rec, testLogger(),
		StreamOptions{Backoff: fastBackoff, now: newClock(epoch).now})
	stop := runStream(t, s)
	waitFor(t, h.handled, 3, "all three frames")
	stop()

	if _, gaps, _ := rec.counts(); gaps != 1 {
		t.Errorf("ingest_ws_gaps_total = %d, want 1 for the missing sequence number", gaps)
	}
}

func TestSilenceOnTheDataChannelIsCountedOnce(t *testing.T) {
	clock := newClock(epoch)
	gate := make(chan struct{}, 4)
	conn := &scriptedConn{gate: gate, frames: [][]byte{
		frame(t, channelTicker, 0, epoch, []map[string]any{}),
		// Heartbeats keep the socket alive while the data channel says nothing.
		heartbeat(t, 1, epoch),
		heartbeat(t, 2, epoch),
		heartbeat(t, 3, epoch),
	}}
	dialer := &scriptedDialer{conns: []*scriptedConn{conn}, dialed: make(chan struct{}, 4)}
	rec := &fakeRecorder{}
	h := &fakeHandler{
		channel: channelTicker, data: channelTicker,
		handled: make(chan struct{}, 8), ticked: make(chan struct{}, 8),
	}
	ticking := tickingHandler{h}

	s := NewStream("ticker", ticking, dialer, rec, testLogger(),
		StreamOptions{Silence: 30 * time.Second, Backoff: fastBackoff, now: clock.now})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); s.Run(ctx) }()

	// The data frame sets last-seen; then time passes with nothing but
	// heartbeats, and the last two are both past the threshold.
	gate <- struct{}{}
	waitFor(t, h.handled, 1, "the data frame")
	waitFor(t, h.ticked, 1, "the tick for the data frame")

	clock.advance(31 * time.Second)
	gate <- struct{}{}
	gate <- struct{}{}
	waitFor(t, h.ticked, 2, "the heartbeats after the drought began")

	cancel()
	<-done

	// One drought is one gap. Counting it on every frame would turn a single
	// thirty-second silence into thirty gaps and make the metric unreadable.
	if _, gaps, _ := rec.counts(); gaps != 1 {
		t.Errorf("ingest_ws_gaps_total = %d, want 1 for one drought", gaps)
	}
}

func TestHeartbeatsDoNotAdvanceLastSeen(t *testing.T) {
	dataAt := epoch
	conn := &scriptedConn{frames: [][]byte{
		frame(t, channelTicker, 0, dataAt, []map[string]any{}),
		heartbeat(t, 1, dataAt.Add(time.Minute)),
		heartbeat(t, 2, dataAt.Add(2*time.Minute)),
	}}
	dialer := &scriptedDialer{conns: []*scriptedConn{conn}, dialed: make(chan struct{}, 4)}
	rec := &fakeRecorder{}
	h := &fakeHandler{
		channel: channelTicker, data: channelTicker,
		handled: make(chan struct{}, 4), ticked: make(chan struct{}, 8),
	}

	s := NewStream("ticker", tickingHandler{h}, dialer, rec, testLogger(),
		StreamOptions{Backoff: fastBackoff, now: newClock(epoch).now})
	stop := runStream(t, s)
	waitFor(t, h.handled, 1, "the data frame")
	waitFor(t, h.ticked, 3, "a tick per frame")
	stop()

	// The whole point of the staleness gauge is to say when this stream's data
	// stopped. A gauge that advanced on heartbeats would report a dead feed as
	// healthy on every stream, because every connection carries heartbeats.
	_, _, lastSeen := rec.counts()
	if !lastSeen.Equal(dataAt) {
		t.Errorf("ingest_last_seen_timestamp_seconds = %s, want the data frame's %s",
			lastSeen, dataAt)
	}
}

func TestVenueErrorFrameDropsTheConnection(t *testing.T) {
	// Observed on the live socket: an unknown channel is answered with an error
	// frame and then silence, so a stream that ignored it would sit connected
	// and empty until the read deadline, over and over.
	errFrame, err := json.Marshal(map[string]any{"type": "error", "message": "authentication failure"})
	if err != nil {
		t.Fatal(err)
	}
	first := &scriptedConn{frames: [][]byte{errFrame}}
	second := &scriptedConn{frames: [][]byte{frame(t, channelTicker, 0, epoch, []map[string]any{})}}
	dialer := &scriptedDialer{conns: []*scriptedConn{first, second}, dialed: make(chan struct{}, 4)}
	h := &fakeHandler{channel: channelTicker, data: channelTicker, handled: make(chan struct{}, 4)}

	s := NewStream("ticker", h, dialer, &fakeRecorder{}, testLogger(),
		StreamOptions{Backoff: fastBackoff, now: newClock(epoch).now})
	stop := runStream(t, s)
	waitFor(t, h.handled, 1, "a message on the connection after the error frame")
	stop()

	if !first.closed {
		t.Error("the connection that sent the error frame was not closed")
	}
}

func TestAHandlerErrorIsAGapNotADisconnect(t *testing.T) {
	conn := &scriptedConn{frames: [][]byte{
		frame(t, channelTicker, 0, epoch, []map[string]any{}),
		frame(t, channelTicker, 1, epoch, []map[string]any{}),
	}}
	dialer := &scriptedDialer{conns: []*scriptedConn{conn}, dialed: make(chan struct{}, 4)}
	rec := &fakeRecorder{}
	h := &fakeHandler{
		channel: channelTicker, data: channelTicker,
		handled:   make(chan struct{}, 4),
		handleErr: errors.New("price=\"\" : missing"),
	}

	s := NewStream("ticker", h, dialer, rec, testLogger(),
		StreamOptions{Backoff: fastBackoff, now: newClock(epoch).now})
	stop := runStream(t, s)
	waitFor(t, h.handled, 2, "both frames")
	stop()

	// A message the venue sent in a shape this code did not expect costs one
	// message, not the connection: the stream reads on and the gap counter says
	// data was lost.
	if _, gaps, _ := rec.counts(); gaps != 2 {
		t.Errorf("ingest_ws_gaps_total = %d, want 2", gaps)
	}
	if dialer.dials() != 1 {
		t.Errorf("dials = %d, want 1: a bad message must not reconnect", dialer.dials())
	}
}

func TestStreamStopsWhenTheWriterIsGone(t *testing.T) {
	conn := &scriptedConn{frames: [][]byte{frame(t, channelTicker, 0, epoch, []map[string]any{})}}
	dialer := &scriptedDialer{conns: []*scriptedConn{conn}, dialed: make(chan struct{}, 4)}
	h := &fakeHandler{
		channel: channelTicker, data: channelTicker,
		handled:   make(chan struct{}, 4),
		handleErr: errWriterGone,
	}

	s := NewStream("ticker", h, dialer, &fakeRecorder{}, testLogger(),
		StreamOptions{Backoff: fastBackoff, now: newClock(epoch).now})

	// No cancellation here on purpose: the stream must return on its own. A
	// stream that treated this like any other feed error would reconnect
	// forever against a writer that has stopped accepting rows, and shutdown
	// would never complete.
	done := make(chan struct{})
	go func() { defer close(done); s.Run(context.Background()) }()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stream kept running after the writer stopped")
	}
}

func TestReadDeadlineEndsASilentConnection(t *testing.T) {
	// A socket that accepts the subscribe and then never speaks again. With
	// heartbeats subscribed, a real connection cannot do this while alive, so
	// the deadline expiring means the connection is dead.
	silent := &scriptedConn{}
	live := &scriptedConn{frames: [][]byte{frame(t, channelTicker, 0, epoch, []map[string]any{})}}
	dialer := &scriptedDialer{conns: []*scriptedConn{silent, live}, dialed: make(chan struct{}, 4)}
	rec := &fakeRecorder{}
	h := &fakeHandler{channel: channelTicker, data: channelTicker, handled: make(chan struct{}, 4)}

	s := NewStream("ticker", h, dialer, rec, testLogger(),
		StreamOptions{ReadTimeout: 20 * time.Millisecond, Backoff: fastBackoff, now: newClock(epoch).now})
	stop := runStream(t, s)
	waitFor(t, h.handled, 1, "a message on the replacement connection")
	stop()

	if !silent.closed {
		t.Error("the silent connection was not closed")
	}
	if reconnects, _, _ := rec.counts(); reconnects != 1 {
		t.Errorf("ingest_ws_reconnects_total = %d, want 1", reconnects)
	}
}
