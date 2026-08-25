package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Jason-Dorman/funding-carry/internal/config"
	"github.com/Jason-Dorman/funding-carry/internal/ingest"
)

// These tests cover the wiring in pipeline, which is where this binary's
// shutdown ordering lives and therefore the one place it can be got wrong. They
// need no database and no network: the writer's database is a fake sender, and
// the venue is a fake dialer serving real Advanced Trade envelopes.

const (
	testPerp = "ETP-20DEC30-CDE"
	testSpot = "ETH-USD"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func testConfig(t *testing.T) *config.Ingest {
	t.Helper()
	window, err := config.ParseMaintenanceWindow("Fri 17:00-18:00 America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	return &config.Ingest{
		Common: config.Common{
			PerpProductID: testPerp,
			SpotProductID: testSpot,
			// Port zero: the metrics server binds whatever is free, so these
			// tests never collide with a running stack or with each other.
			MetricsAddr: "127.0.0.1:0",
		},
		Poll: config.PollIntervals{
			REST:     50 * time.Millisecond,
			Base:     time.Second,
			BookSnap: 50 * time.Millisecond,
		},
		Maintenance: window,
	}
}

// ---------------------------------------------------------------------------
// Fake database
// ---------------------------------------------------------------------------

// failingSender rejects every batch with an error the writer knows a retry
// cannot clear, so the writer reaches its fatal path immediately rather than
// after the retry schedule. That is the "database is unreachable after retries"
// case architecture section 8 calls an invariant violation.
type failingSender struct{}

func (failingSender) SendBatch(_ context.Context, b *pgx.Batch) pgx.BatchResults {
	return &fakeResults{n: b.Len(), err: &pgconn.PgError{Code: "42703", Message: "column does not exist"}}
}

type fakeResults struct {
	n   int
	err error
}

func (r *fakeResults) Exec() (pgconn.CommandTag, error) {
	if r.err != nil {
		return pgconn.CommandTag{}, r.err
	}
	return pgconn.NewCommandTag("INSERT 0 1"), nil
}

func (r *fakeResults) Query() (pgx.Rows, error) { return nil, r.err }
func (r *fakeResults) QueryRow() pgx.Row        { return nil }
func (r *fakeResults) Close() error             { return nil }

// ---------------------------------------------------------------------------
// Fake venue
// ---------------------------------------------------------------------------

// tickingDialer serves a subscription acknowledgement and then an endless,
// paced stream of ticker frames and heartbeats. It is paced rather than a tight
// loop because it stands in for a socket: the venue-state sampler only writes
// while its quote is fresh, so a fake that delivered everything at once and then
// went quiet would stop the very producer these tests need running.
type tickingDialer struct {
	interval time.Duration

	mu   sync.Mutex
	open int
}

func (d *tickingDialer) Dial(ctx context.Context) (ingest.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	d.mu.Lock()
	d.open++
	d.mu.Unlock()
	return &tickingConn{interval: d.interval}, nil
}

type tickingConn struct {
	interval time.Duration
	seq      int64
	closed   bool
}

func (c *tickingConn) Read(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	timer := time.NewTimer(c.interval)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	c.seq++
	// Alternate a quote with a heartbeat, which is what a real connection
	// carries: this stream's data plus the liveness channel every connection
	// subscribes to.
	if c.seq%2 == 0 {
		return tickerFrame(c.seq), nil
	}
	return heartbeatFrame(c.seq), nil
}

func (c *tickingConn) Write(ctx context.Context, _ []byte) error { return ctx.Err() }

func (c *tickingConn) Close() error {
	c.closed = true
	return nil
}

func tickerFrame(seq int64) []byte {
	body, _ := json.Marshal(map[string]any{
		"channel":      "ticker",
		"sequence_num": seq,
		"timestamp":    time.Now().UTC().Format(time.RFC3339Nano),
		"events": []map[string]any{{
			"type": "update",
			"tickers": []map[string]any{
				{"product_id": testPerp, "best_bid": "2344.5", "best_ask": "2345", "price": "2344.75"},
				{"product_id": testSpot, "best_bid": "2344.2", "best_ask": "2344.4", "price": "2344.3"},
			},
		}},
	})
	return body
}

func heartbeatFrame(seq int64) []byte {
	body, _ := json.Marshal(map[string]any{
		"channel":      "heartbeats",
		"sequence_num": seq,
		"timestamp":    time.Now().UTC().Format(time.RFC3339Nano),
		"events":       []map[string]any{{"heartbeat_counter": seq}},
	})
	return body
}

// ---------------------------------------------------------------------------

// A fatal write failure must bring the binary down. architecture section 8
// treats an unreachable database as an invariant violation, and recovery is
// "process crash -> Compose restart policy" — which only works if the process
// actually exits.
//
// It did not. The writer's death reaches internal/ingest only as
// db.ErrWriterStopped from a Submit, and two of the five streams never submit:
// the ticker hands its quotes to the venue-state sampler and the status stream
// hands its flag to the same place. Neither ever called Submit, so neither ever
// learned the writer was gone, and Ingest.Run's WaitGroup never returned.
// pipeline then never reached writer.Close(), the fatal error was never
// surfaced, and the process sat alive with a dead database — still answering
// /healthz with 200, so no healthcheck-driven restart fired either.
func TestPipelineExitsWhenTheWriterDies(t *testing.T) {
	done := make(chan error, 1)
	go func() {
		// A root context that is never canceled: the only thing that can end
		// this run is the writer failing.
		done <- pipeline(context.Background(), testConfig(t), failingSender{}, nil,
			&tickingDialer{interval: 5 * time.Millisecond}, testLogger())
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("pipeline returned nil after a fatal write failure; the process would exit 0 and Compose would not restart it")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("pipeline did not return after the writer died: the binary hangs with a dead database, " +
			"discarding market data while /healthz still reports 200")
	}
}

// The shutdown order in architecture section 8 is cancel, drain, flush, close,
// and the drain's precondition is that producers have stopped. The writer must
// therefore not take the root context: if it did, SIGTERM would start its drain
// while five stream goroutines were still submitting, and a row accepted after
// the drain had passed would be stranded in the queue with its producer told nil.
//
// What this test pins is the outcome that is observable from here: cancellation
// shuts the whole binary down promptly and reports no error, with the database
// having been written to throughout. The ordering property itself is pinned
// where it is observable — internal/db's TestSubmitIsRejectedOnceShutdownBegins,
// which fails if a writer mid-shutdown ever answers a producer with nil.
func TestPipelineShutsDownCleanlyOnCancellation(t *testing.T) {
	sender := &recordingSender{}
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- pipeline(ctx, testConfig(t), sender, nil,
			&tickingDialer{interval: 5 * time.Millisecond}, testLogger())
	}()

	// Wait until the sampler has actually written something, so the shutdown
	// being tested is one with work in flight rather than an empty one.
	deadline := time.After(15 * time.Second)
	for sender.batches() == 0 {
		select {
		case <-deadline:
			t.Fatal("no batch reached the database before cancellation")
		case err := <-done:
			t.Fatalf("pipeline returned early: %v", err)
		default:
		}
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("clean shutdown returned an error: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("pipeline did not return after cancellation")
	}
}

// recordingSender accepts every batch and counts them.
type recordingSender struct {
	mu sync.Mutex
	n  int
}

func (s *recordingSender) SendBatch(_ context.Context, b *pgx.Batch) pgx.BatchResults {
	s.mu.Lock()
	s.n++
	s.mu.Unlock()
	return &fakeResults{n: b.Len()}
}

func (s *recordingSender) batches() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n
}
