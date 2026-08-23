package ingest

import (
	"context"
	"fmt"
	"time"

	"github.com/coder/websocket"
)

// Conn is the stream loop's view of a WebSocket: read a message, write a
// message, close. Declaring it here rather than taking a *websocket.Conn is the
// consumer-defined-interface rule (architecture section 12), and it is what lets
// the reconnect loop, the backoff schedule, gap detection and resubscription be
// tested against a scripted socket with no network involved.
//
// Every method takes a context because cancellation has to reach the read that
// is currently parked (architecture section 8). A client whose reads could only
// be interrupted by a deadline would need a second cancellation mechanism
// running beside the root context; that is the reason for the library choice in
// ADR-0013.
type Conn interface {
	Read(ctx context.Context) ([]byte, error)
	Write(ctx context.Context, msg []byte) error
	Close() error
}

// Dialer opens one connection. The stream calls it again on every reconnect, so
// an implementation must be safe to call repeatedly.
type Dialer interface {
	Dial(ctx context.Context) (Conn, error)
}

// readLimit bounds one message. The level2 snapshot for the perp is the largest
// thing the venue sends — 74 KB observed on 2026-08-20 for a full book — and
// coder/websocket's default limit is 32 KB, which would fail every snapshot.
// 4 MB leaves two orders of magnitude of headroom and still caps a malformed or
// hostile frame well below anything that could exhaust memory.
const readLimit = 4 << 20

// WSDialer dials the Coinbase Advanced Trade market-data socket.
//
// No credentials are involved: every market-data channel this binary subscribes
// to accepts the perp product unauthenticated (verified 2026-08-20, venue doc
// section 6). JWT minting arrives with the account endpoints in Part 5 and order
// entry in Part 16, where it is first actually needed.
type WSDialer struct {
	URL string
}

// Dial opens the socket. The handshake is bounded by dialTimeout rather than by
// the root context alone, so a venue that accepts the TCP connection and then
// stalls becomes a reconnect with backoff instead of a stream parked forever.
func (d WSDialer) Dial(ctx context.Context) (Conn, error) {
	dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()

	// bodyclose flags the discarded handshake response. The library's contract is
	// explicit that it owns it — "You never need to close resp.Body yourself" —
	// and closing it here would close the hijacked connection.
	c, _, err := websocket.Dial(dialCtx, d.URL, nil) //nolint:bodyclose // the library closes the handshake response
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", d.URL, err)
	}
	c.SetReadLimit(readLimit)
	return &wsConn{c: c}, nil
}

const dialTimeout = 15 * time.Second

// wsConn adapts coder/websocket to Conn. It exists so that nothing above this
// file imports the WebSocket library: the whole of the dependency's surface is
// three methods wide.
type wsConn struct {
	c *websocket.Conn
}

func (w *wsConn) Read(ctx context.Context) ([]byte, error) {
	_, data, err := w.c.Read(ctx)
	if err != nil {
		return nil, err
	}
	return data, nil
}

func (w *wsConn) Write(ctx context.Context, msg []byte) error {
	return w.c.Write(ctx, websocket.MessageText, msg)
}

// Close tears the connection down without waiting for a close handshake. The
// stream calls it on a connection it has already decided to abandon — a read
// error, a venue error frame, or shutdown — and in every one of those cases the
// peer is either gone or about to be ignored, so waiting for it to answer would
// only delay the reconnect.
func (w *wsConn) Close() error {
	return w.c.CloseNow()
}
