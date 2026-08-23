package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// Handler turns the frames of one channel into rows. One implementation per
// subscribed channel, each owning whatever accumulation its channel needs — a
// book, a trade bucket, the set of candles already persisted.
//
// A handler is only ever called from its own stream's goroutine, so it needs no
// locking of its own. The one exception is state a handler shares with the
// venue-state sampler, which carries its own mutex (see venuestate.go).
type Handler interface {
	// Subscribe is the channel name and product ids to send in the subscribe
	// frame.
	Subscribe() (channel string, products []string)

	// DataChannel is the channel name the venue labels this handler's data with.
	// It is not always the subscription name: level2 is subscribed as "level2"
	// and arrives as "l2_data".
	DataChannel() string

	// Reset drops per-connection state. A reconnect restarts the venue's
	// sequence numbering and re-sends a fresh snapshot, so anything derived from
	// the previous connection's frames — a book built from updates, most of all
	// — is stale the moment the socket drops.
	Reset()

	// Handle processes one frame. Returning an error does not stop the stream
	// unless the writer has gone: a message the venue sent in a shape this code
	// did not expect is a data gap, not a reason to stop recording.
	Handle(ctx context.Context, msg Message) error
}

// TimeKeeper is the optional half of Handler, implemented by the handlers whose
// output is bounded by time rather than by message arrival: a book snapshot due
// every ten seconds, a trade bucket that has to close on the minute whether or
// not anything traded.
//
// Tick runs on every frame the connection carries, heartbeats included, which is
// what the heartbeat subscription is for: it gives every handler a roughly
// one-second pulse on its own goroutine, so nothing in this package needs a
// timer beside the read loop or a mutex around state the read loop owns.
type TimeKeeper interface {
	Tick(ctx context.Context, now time.Time) error
}

// recorder is the stream's slice of the metric catalogue (API spec section 6),
// defined at the consumer so the loop can be tested without a Prometheus
// registry. Metrics implements it.
type recorder interface {
	reconnect()
	gap()
	seen(at time.Time)
}

// Stream is one channel's connection: dial, subscribe, read until something
// breaks, reconnect with backoff. One goroutine each (architecture section 3).
type Stream struct {
	name    string
	handler Handler
	dialer  Dialer
	rec     recorder
	log     *slog.Logger

	readTimeout time.Duration
	silence     time.Duration
	backoff     Backoff
	now         func() time.Time

	// tick is the handler again when it keeps time, resolved once at
	// construction rather than type-asserted on every frame.
	tick TimeKeeper

	// Owned by the Run goroutine.
	lastSeq  int64
	haveSeq  bool
	lastData time.Time
	gapOpen  bool
}

// StreamOptions configures one stream. A zero ReadTimeout, Backoff or clock
// falls back to the package default; Silence of zero disables the silence check,
// which is the right setting for a channel that is legitimately quiet.
type StreamOptions struct {
	// ReadTimeout bounds one read. Every connection also subscribes to
	// heartbeats, so the venue sends something once a second on every socket no
	// matter how quiet the data channel is; a read that times out therefore means
	// the connection is dead rather than that the market is slow.
	ReadTimeout time.Duration

	// Silence is how long this stream's own channel may go without data before a
	// gap is recorded. It is per channel because "quiet" means something
	// different on each: level2 on a live book is continuous, market_trades has
	// legitimate droughts, and status speaks only when something changes.
	Silence time.Duration

	Backoff Backoff

	// now replaces the wall clock in tests.
	now func() time.Time
}

const (
	defaultReadTimeout = 30 * time.Second

	// backoffResetAfter is how long a connection must survive before the next
	// failure starts the backoff schedule over. Without it, a socket that drops
	// once an hour would keep reconnecting at the ceiling delay forever, because
	// nothing would ever clear the attempt count.
	backoffResetAfter = 2 * time.Minute
)

// NewStream builds a stream. It starts nothing; Run does that.
func NewStream(name string, h Handler, d Dialer, rec recorder, log *slog.Logger, opts StreamOptions) *Stream {
	if opts.ReadTimeout <= 0 {
		opts.ReadTimeout = defaultReadTimeout
	}
	if opts.now == nil {
		opts.now = time.Now
	}
	tick, _ := h.(TimeKeeper)
	return &Stream{
		name:        name,
		tick:        tick,
		handler:     h,
		dialer:      d,
		rec:         rec,
		log:         log.With("stream", name),
		readTimeout: opts.ReadTimeout,
		silence:     opts.Silence,
		backoff:     opts.Backoff.withDefaults(),
		now:         opts.now,
	}
}

// Name is the stream's metric label and log field.
func (s *Stream) Name() string { return s.name }

// Run connects and keeps reconnecting until ctx is canceled.
//
// It returns nothing, and that is the design rather than an omission: a feed
// error degrades to staleness and never stops the binary (CLAUDE.md engineering
// rules, architecture section 8). Every failure below — a refused dial, a
// dropped socket, an error frame, a message in an unexpected shape — is logged,
// counted, and retried. What a consumer sees instead of an error is
// ingest_last_seen_timestamp_seconds ceasing to advance, which is what the
// FeedStale alert and the risk engine's staleness check both read.
func (s *Stream) Run(ctx context.Context) {
	for attempt := 0; ctx.Err() == nil; {
		if attempt > 0 {
			s.rec.reconnect()
			delay := s.backoff.delay(attempt)
			s.log.Warn("reconnecting", "attempt", attempt, "in", delay.String())
			if !sleepCtx(ctx, delay) {
				return
			}
		}

		start := s.now()
		err := s.session(ctx)
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, errWriterGone) {
			// The one error that is not a feed problem: the writer this stream
			// hands rows to has stopped, so there is nothing left to record into.
			s.log.Info("stream stopping: writer gone")
			return
		}

		// A connection that lived long enough to be healthy starts the schedule
		// over; a flapping one keeps climbing toward the ceiling.
		if s.now().Sub(start) >= backoffResetAfter {
			attempt = 0
		}
		attempt++
		s.log.Warn("stream disconnected", "error", err)
	}
}

// errWriterGone marks the one failure a stream does not retry.
var errWriterGone = errors.New("row writer stopped")

// session runs one connection from dial to failure.
func (s *Stream) session(ctx context.Context) error {
	conn, err := s.dialer.Dial(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	if err := s.subscribe(ctx, conn); err != nil {
		return err
	}

	// Everything derived from the previous connection is dropped only once the
	// new one is subscribed, so a failed dial does not throw away a good book.
	s.handler.Reset()
	s.haveSeq = false

	for {
		// The read deadline is the liveness check. It is a child of the root
		// context, not a replacement for it, so cancellation still reaches a
		// parked read immediately.
		readCtx, cancel := context.WithTimeout(ctx, s.readTimeout)
		raw, err := conn.Read(readCtx)
		cancel()
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}
		if err := s.dispatch(ctx, raw); err != nil {
			return err
		}
	}
}

// subscribeFrame is the venue's subscribe request.
type subscribeFrame struct {
	Type       string   `json:"type"`
	Channel    string   `json:"channel"`
	ProductIDs []string `json:"product_ids,omitempty"`
}

// subscribe sends this stream's channel and then heartbeats. Heartbeats are
// second because the data subscription is the one worth failing on.
func (s *Stream) subscribe(ctx context.Context, conn Conn) error {
	channel, products := s.handler.Subscribe()
	for _, frame := range []subscribeFrame{
		{Type: "subscribe", Channel: channel, ProductIDs: products},
		{Type: "subscribe", Channel: channelHeartbeats},
	} {
		body, err := json.Marshal(frame)
		if err != nil {
			return fmt.Errorf("encode subscribe to %s: %w", frame.Channel, err)
		}
		if err := conn.Write(ctx, body); err != nil {
			return fmt.Errorf("subscribe to %s: %w", frame.Channel, err)
		}
	}
	s.log.Info("subscribed", "channel", channel, "products", products)
	return nil
}

// dispatch decodes one frame and routes it. It returns an error only for
// failures of the connection; a failure of one message is counted as a gap and
// the stream reads on.
func (s *Stream) dispatch(ctx context.Context, raw []byte) error {
	env, err := decodeEnvelope(raw)
	if err != nil {
		// Not JSON at all: the connection is not speaking the protocol.
		return err
	}
	if env.Type == errorFrameType {
		return fmt.Errorf("venue error frame: %s", env.Message)
	}

	now := s.now()
	s.checkSequence(env.SequenceNum)
	s.checkSilence(now)

	// Time-driven output runs before the frame is routed, and on every frame
	// rather than only on this stream's own data. A book snapshot or a closing
	// trade bucket is due at a wall-clock boundary; making it wait for the next
	// message on the data channel would delay it for exactly as long as the
	// market was quiet, which is when the boundary matters most.
	if s.tick != nil {
		if err := s.tick.Tick(ctx, now); err != nil {
			if errors.Is(err, errWriterGone) {
				return err
			}
			s.gap("tick", err)
		}
	}

	if env.Channel != s.handler.DataChannel() {
		// Heartbeats and the subscription acknowledgement come down the same
		// socket. They keep it alive and they advance the sequence number, but
		// they are not this stream's data and must not touch its last-seen
		// timestamp: a stream whose staleness gauge advanced on heartbeats would
		// report a dead feed as healthy.
		if env.Channel == channelSubscriptions {
			s.log.Debug("subscription acknowledged", "events", string(env.Events))
		}
		return nil
	}

	s.lastData = now
	s.gapOpen = false
	// last_seen carries the venue's timestamp rather than the receive time, so
	// the gauge measures the age of the data and not the age of the socket.
	s.rec.seen(env.VenueTS())

	if err := s.handler.Handle(ctx, Message{
		Channel: env.Channel,
		SeqNum:  env.SequenceNum,
		VenueTS: env.VenueTS(),
		RecvTS:  now,
		Events:  env.Events,
	}); err != nil {
		if errors.Is(err, errWriterGone) {
			return err
		}
		s.gap("handler", err)
	}
	return nil
}

// checkSequence looks for a hole in the connection's message counter. The venue
// numbers every frame on the socket, so a jump means this stream missed one —
// whichever channel it belonged to.
func (s *Stream) checkSequence(seq int64) {
	if s.haveSeq && seq != s.lastSeq+1 {
		s.gap("sequence", fmt.Errorf("expected %d, got %d", s.lastSeq+1, seq))
	}
	s.lastSeq = seq
	s.haveSeq = true
}

// checkSilence records a gap when this stream's own channel has gone quiet for
// longer than it should. It runs on every frame rather than on a timer of its
// own, which is only reliable because heartbeats guarantee a frame a second on
// every connection.
//
// gapOpen keeps one drought from counting once per frame for as long as it
// lasts; the count is of gaps, not of observations of a gap.
func (s *Stream) checkSilence(now time.Time) {
	if s.silence <= 0 || s.lastData.IsZero() || s.gapOpen {
		return
	}
	if quiet := now.Sub(s.lastData); quiet > s.silence {
		s.gap("silence", fmt.Errorf("no data for %s", quiet.Round(time.Second)))
		s.gapOpen = true
	}
}

// gap counts one detected gap and says why. The reason stays in the log because
// the metric's label set is fixed at `stream` (API spec section 6) — a reason
// label would multiply the series by a vocabulary that grows with the code.
func (s *Stream) gap(reason string, cause error) {
	s.rec.gap()
	s.log.Warn("gap detected", "reason", reason, "cause", cause)
}

// VenueTS is the envelope timestamp, normalized to UTC.
func (e envelope) VenueTS() time.Time { return e.Timestamp.UTC() }

// sleepCtx waits for d, reporting false if the context was canceled first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}
