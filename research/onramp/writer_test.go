package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/shopspring/decimal"
)

// fakeDB is the whole reason batchSender is declared at the consumer: the
// writer's batching, final flush and failure path are all exercised here without
// a database, a container or a migration.
type fakeDB struct {
	mu      sync.Mutex
	batches [][]queued
	ctxErrs []error // ctx.Err() as each batch arrived
	err     error
}

type queued struct {
	sql  string
	args []any
}

func (f *fakeDB) SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults {
	f.mu.Lock()
	defer f.mu.Unlock()

	sent := make([]queued, 0, b.Len())
	for _, q := range b.QueuedQueries {
		sent = append(sent, queued{sql: q.SQL, args: q.Arguments})
	}
	f.batches = append(f.batches, sent)
	f.ctxErrs = append(f.ctxErrs, ctx.Err())
	return &fakeResults{err: f.err}
}

// sizes reports the row count of each batch the writer sent, which is what every
// assertion about batching is really about.
func (f *fakeDB) sizes() []int {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]int, 0, len(f.batches))
	for _, b := range f.batches {
		out = append(out, len(b))
	}
	return out
}

type fakeResults struct{ err error }

func (r *fakeResults) Exec() (pgconn.CommandTag, error) { return pgconn.CommandTag{}, r.err }
func (r *fakeResults) Query() (pgx.Rows, error)         { return nil, r.err }
func (r *fakeResults) QueryRow() pgx.Row                { return nil }
func (r *fakeResults) Close() error                     { return r.err }

func testWriter(t *testing.T, db batchSender, batchSize int) *writer {
	t.Helper()

	m := newMetrics(prometheus.NewRegistry())
	opts := options{batchSize: batchSize, flushEvery: time.Hour}
	return newWriter(db, opts, m.writer, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func testTick(seq int64) tick {
	return tick{
		product: "ETH-USD",
		seq:     seq,
		price:   decimal.New(345000, -2),
		at:      time.Unix(1700000000+seq, 0).UTC(),
	}
}

// feedTicks sends n ticks and closes the channel, which is what the producer
// side of the binary does on shutdown.
func feedTicks(n int) chan tick {
	in := make(chan tick, n)
	for seq := int64(1); seq <= int64(n); seq++ {
		in <- testTick(seq)
	}
	close(in)
	return in
}

func TestWriterFlushesWhenBatchIsFull(t *testing.T) {
	db := &fakeDB{}
	w := testWriter(t, db, 2)

	if err := w.run(t.Context(), feedTicks(5)); err != nil {
		t.Fatalf("run: %v", err)
	}

	// Two full batches at the size threshold, then the odd row on the final
	// flush: 5 rows never sit unwritten just because they did not divide evenly.
	if got, want := db.sizes(), []int{2, 2, 1}; !equalInts(got, want) {
		t.Errorf("batch sizes = %v, want %v", got, want)
	}
}

func TestWriterFinalFlushWritesPartialBatch(t *testing.T) {
	db := &fakeDB{}
	w := testWriter(t, db, 100)

	if err := w.run(t.Context(), feedTicks(3)); err != nil {
		t.Fatalf("run: %v", err)
	}

	if got, want := db.sizes(), []int{3}; !equalInts(got, want) {
		t.Errorf("batch sizes = %v, want %v", got, want)
	}
}

// The rows a producer has already handed over must be written even though the
// context that produced them is gone. This is the SIGTERM path.
func TestWriterFinalFlushRunsOnCanceledContext(t *testing.T) {
	db := &fakeDB{}
	w := testWriter(t, db, 100)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if err := w.run(ctx, feedTicks(3)); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got, want := db.sizes(), []int{3}; !equalInts(got, want) {
		t.Errorf("batch sizes = %v, want %v", got, want)
	}
}

// Regression: a -run-for deadline landing mid-flush once failed the whole run
// with "context deadline exceeded", because only the final flush was protected
// from cancellation. Every write has to reach the database on a live context —
// the steady-state ones included — or a SIGTERM arriving mid-batch loses rows
// the producers already believe are safe.
func TestWriterNeverWritesOnACanceledContext(t *testing.T) {
	db := &fakeDB{}
	w := testWriter(t, db, 1) // flush on every tick, so these are not final flushes

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if err := w.run(ctx, feedTicks(3)); err != nil {
		t.Fatalf("run: %v", err)
	}

	db.mu.Lock()
	defer db.mu.Unlock()

	if len(db.ctxErrs) != 3 {
		t.Fatalf("sent %d batches, want 3", len(db.ctxErrs))
	}
	for i, err := range db.ctxErrs {
		if err != nil {
			t.Errorf("batch %d reached the database on a dead context: %v", i, err)
		}
	}
}

func TestWriterFlushesOnInterval(t *testing.T) {
	db := &fakeDB{}
	w := testWriter(t, db, 100)

	intervals := make(chan time.Time)
	w.ticks = intervals

	in := make(chan tick, 1)
	done := make(chan error, 1)
	go func() { done <- w.run(t.Context(), in) }()

	in <- testTick(1)
	intervals <- time.Unix(1700000000, 0)

	// The second interval tick cannot be received until the flush triggered by
	// the first has returned, so this is a synchronisation point rather than a
	// sleep.
	intervals <- time.Unix(1700000001, 0)

	close(in)
	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}

	// One row flushed on the interval, nothing left for the final flush: an
	// empty batch is never sent.
	if got, want := db.sizes(), []int{1}; !equalInts(got, want) {
		t.Errorf("batch sizes = %v, want %v", got, want)
	}
}

func TestWriterReportsInsertFailure(t *testing.T) {
	sentinel := errors.New("connection reset")
	db := &fakeDB{err: sentinel}
	w := testWriter(t, db, 1)

	err := w.run(t.Context(), feedTicks(1))
	if !errors.Is(err, sentinel) {
		t.Fatalf("run error = %v, want it to wrap %v", err, sentinel)
	}
}

// The insert is positional, and getting the order wrong would write a price into
// a sequence number without any type error to catch it.
func TestWriterInsertArgumentOrder(t *testing.T) {
	db := &fakeDB{}
	w := testWriter(t, db, 1)

	if err := w.run(t.Context(), feedTicks(1)); err != nil {
		t.Fatalf("run: %v", err)
	}

	db.mu.Lock()
	defer db.mu.Unlock()

	q := db.batches[0][0]
	if q.sql != insertSQL {
		t.Errorf("sql = %q, want %q", q.sql, insertSQL)
	}
	want := testTick(1)
	if len(q.args) != 4 {
		t.Fatalf("args = %v, want 4 of them", q.args)
	}
	if got, ok := q.args[0].(time.Time); !ok || !got.Equal(want.at) {
		t.Errorf("args[0] = %v, want %v", q.args[0], want.at)
	}
	if q.args[1] != want.product {
		t.Errorf("args[1] = %v, want %v", q.args[1], want.product)
	}
	if q.args[2] != want.seq {
		t.Errorf("args[2] = %v, want %v", q.args[2], want.seq)
	}
	// Decimals compare with Equal, never ==: the operator compares the internal
	// representation, so 3450.00 and 3450 would come out unequal.
	if got, ok := q.args[3].(decimal.Decimal); !ok || !got.Equal(want.price) {
		t.Errorf("args[3] = %v, want %v", q.args[3], want.price)
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
