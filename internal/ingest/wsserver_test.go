package ingest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// fakeVenue is a WebSocket server speaking the Advanced Trade envelope. Where
// the fakes in fakes_test.go stand in for the socket so the reconnect loop can
// be driven, this one is the real thing: a real handshake, real frames, and the
// real coder/websocket client underneath WSDialer. It is what proves the
// adapter in conn.go — the read limit, the close, the context plumbing — rather
// than only the logic above it.
type fakeVenue struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	sessions int
	subs     []subscribeFrame

	// script is what the server sends after the subscriptions arrive.
	script func(session int) [][]byte
}

func newFakeVenue(t *testing.T, script func(session int) [][]byte) *fakeVenue {
	v := &fakeVenue{t: t, script: script}
	v.srv = httptest.NewServer(http.HandlerFunc(v.serve))
	t.Cleanup(v.srv.Close)
	return v
}

func (v *fakeVenue) url() string { return "ws" + v.srv.URL[len("http"):] }

func (v *fakeVenue) serve(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = c.CloseNow() }()

	v.mu.Lock()
	v.sessions++
	session := v.sessions
	v.mu.Unlock()

	ctx := r.Context()
	// Every stream sends two subscribes: its data channel, then heartbeats.
	for range 2 {
		_, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		var f subscribeFrame
		if err := json.Unmarshal(data, &f); err != nil {
			return
		}
		v.mu.Lock()
		v.subs = append(v.subs, f)
		v.mu.Unlock()
	}

	for _, frame := range v.script(session) {
		if err := c.Write(ctx, websocket.MessageText, frame); err != nil {
			return
		}
	}
	// Returning closes the socket, which is what makes the client reconnect.
}

func (v *fakeVenue) subscriptions() []subscribeFrame {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]subscribeFrame(nil), v.subs...)
}

func TestStreamAgainstARealWebSocketServer(t *testing.T) {
	// Session one delivers a frame and hangs up; session two delivers another.
	// That is the whole reliability claim end to end: a dropped socket is
	// reconnected, resubscribed and resumed, with the drop counted.
	venue := newFakeVenue(t, func(session int) [][]byte {
		return [][]byte{
			heartbeat(t, 0, epoch),
			frame(t, channelTicker, 1, epoch.Add(time.Duration(session)*time.Second), []map[string]any{}),
		}
	})

	rec := &fakeRecorder{}
	h := &fakeHandler{channel: channelTicker, data: channelTicker, handled: make(chan struct{}, 8)}
	s := NewStream("ticker", h, WSDialer{URL: venue.url()}, rec, testLogger(),
		StreamOptions{Backoff: fastBackoff, now: newClock(epoch).now})

	stop := runStream(t, s)
	waitFor(t, h.handled, 2, "a ticker frame on each of two sessions")
	stop()

	if reconnects, _, lastSeen := rec.counts(); reconnects < 1 || lastSeen.IsZero() {
		t.Errorf("reconnects = %d, last_seen = %s: want at least one reconnect and a recorded timestamp",
			reconnects, lastSeen)
	}

	subs := venue.subscriptions()
	if len(subs) < 4 {
		t.Fatalf("subscribe frames = %d, want two per session", len(subs))
	}
	// The bound is checked rather than assumed: stop() waits for the client's
	// goroutine, not the server's, so a session caught between its two subscribe
	// frames leaves an odd number here. Indexing past the end would panic, and a
	// panic in a test goroutine takes down the whole package binary — turning a
	// timing artifact into every other test's result disappearing.
	//
	// Resubscription itself is not optional: the venue carries nothing across a
	// connection, so a stream that reconnected without it would sit connected
	// and permanently silent.
	for i := 0; i+1 < len(subs); i += 2 {
		if subs[i].Channel != channelTicker || subs[i+1].Channel != channelHeartbeats {
			t.Errorf("session %d subscribed %q then %q, want %q then %q",
				i/2+1, subs[i].Channel, subs[i+1].Channel, channelTicker, channelHeartbeats)
		}
	}
}

func TestARealConnectionAcceptsAFullBookSnapshot(t *testing.T) {
	// The live level2 snapshot for the perp was 74 KB. coder/websocket's default
	// read limit is 32 KB, so without conn.go raising it every snapshot would
	// fail the read, drop the connection, and the level2 stream would reconnect
	// forever without ever building a book.
	big := make([]l2Level, 0, 2000)
	for i := range 2000 {
		big = append(big, lvl("bid", decimalString(2000-i), "1"))
	}
	payload := frame(t, channelLevel2Data, 0, epoch, l2Frame(testPerp, "snapshot", big...))
	if len(payload) <= 32<<10 {
		t.Fatalf("test payload is %d bytes, too small to exceed the default read limit", len(payload))
	}

	venue := newFakeVenue(t, func(int) [][]byte { return [][]byte{payload} })

	sink := &fakeSink{}
	h := NewLevel2Handler(testPerp, time.Second, sink)
	rec := &fakeRecorder{sawData: make(chan struct{}, 4)}
	s := NewStream("level2", h, WSDialer{URL: venue.url()}, rec, testLogger(),
		StreamOptions{Backoff: fastBackoff, now: newClock(epoch).now})

	stop := runStream(t, s)
	waitFor(t, rec.sawData, 1, "the oversized snapshot to be delivered")
	stop()

	// And it was a book, not just bytes: the snapshot has to have been parsed
	// for the touch to be there.
	// The stream stamps the book with its injected clock, so the tick is
	// expressed against the same clock: one boundary on, and inside the book's
	// freshness window (maxBookAge x the 1s interval).
	if err := h.Tick(context.Background(), epoch.Add(2*time.Second)); err != nil {
		t.Fatalf("tick: %v", err)
	}
	rows := bookSnapshots(t, sink)
	if len(rows) != 1 || !rows[0].BestBid.Valid {
		t.Fatalf("snapshot rows = %+v, want one with a best bid", rows)
	}
}

// decimalString renders an integer price without importing strconv into the
// level2 helpers.
func decimalString(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
