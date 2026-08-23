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
	"github.com/jackc/pgx/v5/pgconn"
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
	// shutdown closes stop before it drains, so the orderly paths have already
	// done this. It is repeated here for the one exit that does not go through
	// shutdown — a fatal flush, from either trigger — after which Submit must
	// stop returning nil for rows queued into a channel whose only reader has
	// gone. stopOnce makes it idempotent with both.
	w.stopOnce.Do(func() { close(w.stop) })
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
	// Closing stop first is what makes Submit's contract true. It used to be
	// closed only after this function returned, which left the drain and the
	// whole width of the final round trip as a window in which Submit still
	// accepted rows — and every one of them landed in a channel whose only
	// reader had already passed the drain below. The producer was told nil,
	// nothing was logged, no metric moved, and the row was gone.
	//
	// That window is reachable on every shutdown where a producer is still
	// running, which is every shutdown: a WebSocket frame already read when
	// SIGTERM lands is dispatched on the canceled root context and submits from
	// there. A producer must be told "stopped" rather than "accepted", so that
	// the row it holds is a visible refusal instead of a silent loss.
	// TestSubmitIsRejectedOnceShutdownBegins is the regression.
	w.stopOnce.Do(func() { close(w.stop) })

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
	// tables runs parallel to the queued statements so each command tag can be
	// attributed back to the table it wrote.
	tables := make([]string, 0, w.pending)
	queued := make(map[string]int64, len(w.batches))
	for _, key := range w.order {
		b := w.batches[key]
		for _, values := range b.values {
			batch.Queue(b.stmt, values...)
			tables = append(tables, b.table)
		}
		queued[b.table] += int64(len(b.values))
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
	applied, err := w.send(writeCtx, batch, tables)
	w.m.observeBatch(time.Since(start).Seconds())
	if err != nil {
		return err
	}

	for table, n := range queued {
		// Two counters, because the difference is the interesting number: rows
		// the database took, and rows it already had. A flush that is all
		// conflicts is a retry landing on work already done — worth seeing.
		w.m.addRows(table, applied[table])
		if skipped := n - applied[table]; skipped > 0 {
			w.m.addConflicts(table, skipped)
		}
	}
	for _, key := range w.order {
		// Keep the statement and the backing array, drop the rows.
		w.batches[key].values = w.batches[key].values[:0]
	}
	w.pending = 0
	w.m.observeQueue(len(w.rows))
	return nil
}

// send executes the batch, retrying a failure that another attempt might clear.
//
// Re-sending is safe because every insert carries ON CONFLICT ... DO NOTHING
// against the natural key of its table, so a batch that turns out to have
// committed already lands the second time as a no-op. That is the whole reason
// the keys exist: without them the writer had to abandon any batch whose commit
// status it could not determine, which bought correctness with availability at
// the worst moment — a dropped connection became a restart, a restart became a
// gap, and a gap in the self-recorded series (venue state, features, book
// snapshots, trade aggregates) cannot be backfilled from anywhere, because those
// series are computed here and exist nowhere else.
//
// TestEveryRowTypeIsIdempotent is what keeps that assumption true as tables are
// added; without it this retry silently becomes the duplicate bug again.
func (w *Writer) send(ctx context.Context, batch *pgx.Batch, tables []string) (map[string]int64, error) {
	var (
		applied map[string]int64
		err     error
	)
	for attempt := 1; attempt <= w.opts.MaxAttempts; attempt++ {
		if applied, err = w.sendOnce(ctx, batch, tables); err == nil {
			return applied, nil
		}
		if !worthRetrying(err) {
			return nil, fmt.Errorf("batch insert of %d rows will not succeed on a retry: %w",
				batch.Len(), err)
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
			return nil, fmt.Errorf("batch insert of %d rows abandoned after %d attempts: %w",
				batch.Len(), attempt, err)
		}
	}
	return nil, fmt.Errorf("batch insert of %d rows failed after %d attempts: %w",
		batch.Len(), w.opts.MaxAttempts, err)
}

// sendOnce runs the batch once and reports how many rows each table actually
// took. tables names the destination of every queued statement, in queue order,
// so the command tags can be attributed as they are read.
func (w *Writer) sendOnce(ctx context.Context, batch *pgx.Batch, tables []string) (map[string]int64, error) {
	results := w.db.SendBatch(ctx, batch)

	// Every queued statement's result must be read, in order, before the batch
	// can be closed: the first failure aborts the rest, so it is the one worth
	// reporting.
	applied := make(map[string]int64, len(w.batches))
	var execErr error
	for i := range batch.Len() {
		tag, e := results.Exec()
		if e != nil {
			execErr = fmt.Errorf("statement %d of %d: %w", i+1, batch.Len(), e)
			break
		}
		// RowsAffected, not one-per-statement: an insert that hit its natural key
		// reports zero, and counting it as a write would hide the retry storms
		// idempotency makes possible behind a healthy-looking throughput line.
		applied[tables[i]] += tag.RowsAffected()
	}

	closeErr := results.Close()

	switch {
	case execErr != nil:
		return nil, execErr
	case closeErr != nil:
		// Every statement reported success, so the batch reached the point where
		// only the commit was left. A failure here — the connection dropping
		// between the last CommandComplete and the ReadyForQuery that follows the
		// commit — leaves the rows possibly in the database and possibly not.
		// Re-sending settles it, because a repeat is a no-op.
		return nil, fmt.Errorf("commit status unknown after close: %w", closeErr)
	}
	return applied, nil
}

// worthRetrying reports whether another attempt could succeed. Safety is not in
// question — idempotent inserts make any repeat harmless — so the only thing
// left to decide is whether the failure is one that a moment's wait can clear.
func worthRetrying(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return transientServerError(pgErr.Code)
	}
	// Not an answer from the server: a connection lost mid-results, a pool that
	// could not hand one over, a commit whose acknowledgement never arrived. Any
	// of those can clear. (pgconn.SafeToRetry would say the same for the subset
	// it recognises; it is subsumed here.)
	return true
}

// transientServerError reports whether a SQLSTATE describes a condition a moment
// of waiting can clear. Everything else — a constraint violation, a type
// mismatch, a column that does not exist — is a property of the rows or of the
// schema, and would fail identically on all three attempts while the flush
// timeout ran down. A unique violation is in that list deliberately: with
// ON CONFLICT everywhere it should be unreachable, and if it is ever raised the
// conflict target and the index have diverged, which retrying cannot fix.
func transientServerError(sqlstate string) bool {
	switch sqlstate {
	case "40001", // serialization_failure
		"40P01", // deadlock_detected
		"53300", // too_many_connections
		"53400", // configuration_limit_exceeded
		"55P03", // lock_not_available
		"57P03": // cannot_connect_now
		return true
	}
	return false
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
