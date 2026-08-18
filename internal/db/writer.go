package db

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrWriterStopped is returned by Submit once the writer has begun shutting
// down. It is not a failure: it means the row arrived after the system stopped
// accepting work, and the producer should stop too.
var ErrWriterStopped = errors.New("writer stopped")

// Writer defaults. The flush interval bounds how long a row can sit unwritten,
// the batch size bounds how large one round trip gets, and the queue size is the
// backpressure point: once it fills, producers block instead of growing memory
// while the database is unreachable.
const (
	defaultQueueSize     = 4096
	defaultBatchSize     = 500
	defaultFlushInterval = time.Second
	defaultFlushTimeout  = 10 * time.Second
	defaultMaxAttempts   = 3
	defaultRetryBackoff  = 250 * time.Millisecond
)

// batchSender is the writer's view of the database: one method, the batched send.
// Declaring it here rather than accepting a *pgxpool.Pool follows the
// consumer-defined-interface rule (architecture section 12) and has a concrete
// payoff — batching, flush triggers, retries and the final flush on close are
// all testable without a database.
type batchSender interface {
	SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults
}

// WriterOptions tunes the writer. A zero value is valid: every field falls back
// to the package default.
type WriterOptions struct {
	QueueSize     int           // rows buffered before Submit blocks
	BatchSize     int           // pending rows that trigger an immediate flush
	FlushInterval time.Duration // longest a row waits before being written
	FlushTimeout  time.Duration // bound on one flush, including retries; every flush, not just the last
	MaxAttempts   int           // flush attempts before the failure becomes fatal
	RetryBackoff  time.Duration // base delay between attempts, multiplied by attempt

	// ticks replaces the interval ticker in tests, so a flush-on-interval test
	// is driven rather than slept through. Unexported: production always uses
	// the real clock.
	ticks <-chan time.Time
}

func (o WriterOptions) withDefaults() WriterOptions {
	if o.QueueSize <= 0 {
		o.QueueSize = defaultQueueSize
	}
	if o.BatchSize <= 0 {
		o.BatchSize = defaultBatchSize
	}
	if o.FlushInterval <= 0 {
		o.FlushInterval = defaultFlushInterval
	}
	if o.FlushTimeout <= 0 {
		o.FlushTimeout = defaultFlushTimeout
	}
	if o.MaxAttempts <= 0 {
		o.MaxAttempts = defaultMaxAttempts
	}
	if o.RetryBackoff <= 0 {
		o.RetryBackoff = defaultRetryBackoff
	}
	return o
}

// Writer is the one goroutine per binary that touches the database (spec section
// 11). Producers hand it typed rows on a channel and never hold a connection, so
// insert order is serialized, batching is trivial, and there is exactly one
// place where a write can fail.
//
// Lifecycle: construct with NewWriter, run Run in its own goroutine exactly
// once, Submit from anywhere, and Close when the producers are done. Close waits
// for the final flush and returns whatever Run returned, so a caller that only
// holds the Writer still sees a fatal write failure.
//
// The Writer is append-only and does not read generated ids back. Rows that must
// know their own id before other rows can reference them — positions, whose id
// fills and funding events carry — are an open question recorded against Part 13
// in the build plan, not something this layer guesses at.
type Writer struct {
	db   batchSender
	opts WriterOptions
	log  *slog.Logger
	m    *WriterMetrics

	rows chan Row

	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
	err      error

	// Everything below is owned by the Run goroutine and touched by nothing else.
	batches map[string]*tableBatch
	order   []string
	pending int
}

// tableBatch accumulates the rows of one insert statement. Rows of the same
// table with the same column list share a statement, which is what "batched per
// table" means in practice.
type tableBatch struct {
	table  string
	stmt   string
	values [][]any
}

// NewWriter builds the writer. It starts nothing; Run does that.
func NewWriter(db batchSender, opts WriterOptions, log *slog.Logger, m *WriterMetrics) *Writer {
	opts = opts.withDefaults()
	return &Writer{
		db:      db,
		opts:    opts,
		log:     log,
		m:       m,
		rows:    make(chan Row, opts.QueueSize),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
		batches: make(map[string]*tableBatch),
	}
}

// Submit queues a row. It blocks when the queue is full — that is the intended
// backpressure: a database that cannot keep up slows its producers down rather
// than being hidden by an unbounded buffer.
func (w *Writer) Submit(ctx context.Context, r Row) error {
	// Shutdown is checked on its own first. In a single select, a queue with
	// room to spare and a closed stop channel are both ready, and the choice
	// between them is random — so a writer that has stopped would still accept
	// rows about half the time.
	select {
	case <-w.stop:
		return ErrWriterStopped
	default:
	}

	select {
	case w.rows <- r:
		w.m.observeQueue(len(w.rows))
		return nil
	case <-w.stop:
		return ErrWriterStopped
	case <-ctx.Done():
		return fmt.Errorf("submit row to %s: %w", r.row().table, ctx.Err())
	}
}

// Run is the writer goroutine. It returns when Close is called or ctx is
// canceled, after one last flush, or earlier with a fatal error if a batch could
// not be written after MaxAttempts — an unreachable database is an invariant
// violation, not a degraded feed (architecture section 8).
//
// Run must be called exactly once.
func (w *Writer) Run(ctx context.Context) error {
	err := w.run(ctx)
	w.err = err
	close(w.done)
	return err
}

func (w *Writer) run(ctx context.Context) error {
	ticks := w.opts.ticks
	if ticks == nil {
		ticker := time.NewTicker(w.opts.FlushInterval)
		defer ticker.Stop()
		ticks = ticker.C
	}

	for {
		select {
		case r := <-w.rows:
			w.enqueue(r)
			if w.pending >= w.opts.BatchSize {
				if err := w.flush(ctx); err != nil {
					return err
				}
			}
		case <-ticks:
			if err := w.flush(ctx); err != nil {
				return err
			}
		case <-w.stop:
			return w.shutdown(ctx)
		case <-ctx.Done():
			return w.shutdown(ctx)
		}
	}
}

// Close stops the writer and waits for its final flush. It is safe to call more
// than once and returns the same error Run returned. Calling it without a
// running Run would block forever.
func (w *Writer) Close() error {
	w.stopOnce.Do(func() { close(w.stop) })
	<-w.done
	return w.err
}

// shutdown takes the rows producers already handed over — they are in the
// channel buffer, and dropping them would lose writes the producer believes
// succeeded — and flushes once more. flush is what detaches the write from
// cancellation, so this path needs no context of its own.
func (w *Writer) shutdown(ctx context.Context) error {
	for draining := true; draining; {
		select {
		case r := <-w.rows:
			w.enqueue(r)
		default:
			draining = false
		}
	}

	final := w.pending
	if err := w.flush(ctx); err != nil {
		return fmt.Errorf("final flush: %w", err)
	}
	w.log.Info("writer stopped", "final_flush_rows", final)
	return nil
}

// enqueue files a row under its statement. Statements are built once and the
// slices are reused across flushes, so steady-state writing allocates nothing
// per row beyond the values themselves.
func (w *Writer) enqueue(r Row) {
	d := r.row()
	key := d.table + "(" + strings.Join(d.columns, ",") + ")"

	b, ok := w.batches[key]
	if !ok {
		b = &tableBatch{table: d.table, stmt: insertStatement(d)}
		w.batches[key] = b
		w.order = append(w.order, key)
	}
	b.values = append(b.values, d.values)
	w.pending++
}

// flush writes everything pending as a single pgx batch. pgx sends a batch as
// one pipeline terminated by a single sync, so Postgres runs it inside one
// implicit transaction: a flush lands whole or not at all, and costs one round
// trip regardless of how many tables it spans.
func (w *Writer) flush(ctx context.Context) error {
	if w.pending == 0 {
		return nil
	}

	batch := &pgx.Batch{}
	for _, key := range w.order {
		b := w.batches[key]
		for _, values := range b.values {
			batch.Queue(b.stmt, values...)
		}
	}

	// Every flush runs on a context that cancellation cannot reach, bounded by
	// FlushTimeout — the final one is not a special case. Cancellation is meant
	// to stop producers; a batch already in flight has to be allowed to land or
	// to fail on its own clock.
	//
	// Passing ctx straight to pgx here was a defect: a SIGTERM arriving while a
	// size- or interval-triggered batch was in flight aborted the round trip,
	// send gave up at its first ctx.Done() check, Run returned fatal, and
	// shutdown — the drain and the final flush architecture section 8 promises —
	// never ran. Rows producers had been told were accepted were lost, on the
	// one path that exists to prevent exactly that. Found by the Part 3 onramp
	// toy hitting it, not by reading this code.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), w.opts.FlushTimeout)
	defer cancel()

	start := time.Now()
	err := w.send(writeCtx, batch)
	w.m.observeBatch(time.Since(start).Seconds())
	if err != nil {
		return err
	}

	for _, key := range w.order {
		b := w.batches[key]
		if len(b.values) == 0 {
			continue
		}
		w.m.addRows(b.table, len(b.values))
		// Keep the statement and the backing array, drop the rows.
		b.values = b.values[:0]
	}
	w.pending = 0
	w.m.observeQueue(len(w.rows))
	return nil
}

// send executes the batch, retrying a failure a bounded number of times before
// giving up. The retry exists because a brief connection loss is recoverable and
// a dropped batch is not; exhausting the attempts is fatal by design.
func (w *Writer) send(ctx context.Context, batch *pgx.Batch) error {
	var err error
	for attempt := 1; attempt <= w.opts.MaxAttempts; attempt++ {
		if err = w.sendOnce(ctx, batch); err == nil {
			return nil
		}
		if attempt == w.opts.MaxAttempts {
			break
		}
		w.log.Warn("batch insert failed, retrying",
			"attempt", attempt, "rows", batch.Len(), "error", err)

		timer := time.NewTimer(w.opts.RetryBackoff * time.Duration(attempt))
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("batch insert of %d rows abandoned after %d attempts: %w",
				batch.Len(), attempt, err)
		}
	}
	return fmt.Errorf("batch insert of %d rows failed after %d attempts: %w",
		batch.Len(), w.opts.MaxAttempts, err)
}

func (w *Writer) sendOnce(ctx context.Context, batch *pgx.Batch) error {
	results := w.db.SendBatch(ctx, batch)

	// Every queued statement's result must be read, in order, before the batch
	// can be closed: the first failure aborts the rest, so it is the one worth
	// reporting.
	var execErr error
	for i := range batch.Len() {
		if _, err := results.Exec(); err != nil {
			execErr = fmt.Errorf("statement %d of %d: %w", i+1, batch.Len(), err)
			break
		}
	}

	closeErr := results.Close()
	if execErr != nil {
		return execErr
	}
	if closeErr != nil {
		return fmt.Errorf("close batch: %w", closeErr)
	}
	return nil
}

// insertStatement builds the parameterized INSERT for one row shape. Table and
// column names come from the unexported row methods in this package — never from
// a caller — and every value is a bind parameter, so no data reaches the
// statement text.
func insertStatement(d rowData) string {
	var sb strings.Builder
	sb.WriteString("INSERT INTO ")
	sb.WriteString(d.table)
	sb.WriteString(" (")
	sb.WriteString(strings.Join(d.columns, ", "))
	sb.WriteString(") VALUES (")
	for i := range d.columns {
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString("$")
		sb.WriteString(strconv.Itoa(i + 1))
	}
	sb.WriteString(")")
	if d.conflict != "" {
		sb.WriteString(" ")
		sb.WriteString(d.conflict)
	}
	return sb.String()
}
