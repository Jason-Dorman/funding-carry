package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
)

// batchSender is the writer's view of the database: one method, the batched
// send. Declaring the interface here at the consumer rather than accepting a
// *pgxpool.Pool is the project's interface rule (architecture section 12), and
// it pays for itself immediately — writer_test.go drives batching, the final
// flush and the failure path with no database at all.
type batchSender interface {
	SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults
}

// insertSQL writes decimal.Decimal straight into a numeric column. That works
// because db.Connect registers the shopspring codec on every connection, so the
// coefficient and exponent survive the round trip without passing through a
// float (internal/db/pool.go).
const insertSQL = `INSERT INTO onramp_ticks (at, product, seq, price) VALUES ($1, $2, $3, $4)`

// flushTimeout bounds one write. It is what keeps a stuck database from turning
// shutdown into a hang, now that writes no longer inherit cancellation.
const flushTimeout = 10 * time.Second

// writer is the one goroutine that touches the database (spec section 11).
// Nothing else in the binary holds a connection, so insert order is serialized,
// batching is trivial, and there is exactly one place a write can fail.
type writer struct {
	db         batchSender
	batchSize  int
	flushEvery time.Duration
	m          *writerMetrics
	log        *slog.Logger

	// pending is owned by the run goroutine and touched by nothing else. That
	// ownership, not a mutex, is what makes the writer safe.
	pending []tick

	// ticks replaces the interval ticker in tests, so the flush-on-interval
	// case is driven rather than slept through. Unexported: production always
	// uses the real clock.
	ticks <-chan time.Time
}

func newWriter(db batchSender, opts options, m *writerMetrics, log *slog.Logger) *writer {
	return &writer{
		db:         db,
		batchSize:  opts.batchSize,
		flushEvery: opts.flushEvery,
		m:          m,
		log:        log,
		pending:    make([]tick, 0, opts.batchSize),
	}
}

// run consumes ticks until the channel closes, flushing whenever the batch fills
// or the interval elapses.
//
// There is deliberately no ctx.Done() case here. Cancellation stops the feeds,
// the feeds' completion closes the channel, and the closed channel stops the
// writer — so the writer is the last thing in the binary to stop, which is the
// shutdown order architecture section 8 requires: cancel, drain, flush, close.
// A writer that stopped on cancellation like everything else would drop the rows
// its producers believe they already handed over.
func (w *writer) run(ctx context.Context, in <-chan tick) error {
	ticks := w.ticks
	if ticks == nil {
		ticker := time.NewTicker(w.flushEvery)
		defer ticker.Stop()
		ticks = ticker.C
	}

	for {
		select {
		case t, ok := <-in:
			if !ok {
				// A receive only reports !ok once the buffer is empty, so
				// reaching here means every tick ever sent has been taken.
				return w.finalFlush(ctx)
			}
			w.pending = append(w.pending, t)
			w.m.queued(len(in))
			if len(w.pending) >= w.batchSize {
				if err := w.flush(ctx); err != nil {
					return err
				}
			}
		case <-ticks:
			if err := w.flush(ctx); err != nil {
				return err
			}
		}
	}
}

// finalFlush writes what is left when the producers are done.
func (w *writer) finalFlush(ctx context.Context) error {
	rows := len(w.pending)

	if err := w.flush(ctx); err != nil {
		return fmt.Errorf("final flush: %w", err)
	}
	w.log.Info("writer stopped", "final_flush_rows", rows)
	return nil
}

// flush writes everything pending as a single pgx batch. pgx sends a batch as one
// pipeline terminated by a single sync, so Postgres runs it inside one implicit
// transaction: a flush lands whole or not at all, for one round trip.
func (w *writer) flush(ctx context.Context) error {
	if len(w.pending) == 0 {
		return nil
	}

	batch := &pgx.Batch{}
	for _, t := range w.pending {
		batch.Queue(insertSQL, t.at, t.product, t.seq, t.price)
	}

	// Every write runs on a context that cancellation cannot reach, bounded by
	// its own timeout — not just the final one. Cancellation stops the feeds; a
	// write that has already started has to be allowed to land or to fail on its
	// own clock. Handing the root context to pgx instead means a SIGTERM that
	// arrives mid-batch kills the round trip and turns a clean shutdown into a
	// fatal error, losing rows the producers believe are already safe. That is
	// not theoretical: it is what a -run-for deadline landing mid-flush did.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), flushTimeout)
	defer cancel()

	start := time.Now()
	err := w.send(writeCtx, batch)
	w.m.batchSeconds(time.Since(start).Seconds())
	if err != nil {
		// No retry here, unlike internal/db.Writer: a failed write is fatal to
		// the toy. Backoff and bounded retries are a property of the production
		// writer, and copying half of them would teach the wrong shape.
		return fmt.Errorf("insert %d ticks: %w", len(w.pending), err)
	}

	w.m.written(len(w.pending))
	w.log.Debug("flushed", "rows", len(w.pending))
	// Keep the backing array, drop the rows: steady-state writing allocates
	// nothing per batch.
	w.pending = w.pending[:0]
	return nil
}

// send executes the batch. Every queued statement's result has to be read, in
// order, before the batch can be closed; the first failure aborts the rest, so
// that is the one worth reporting.
func (w *writer) send(ctx context.Context, batch *pgx.Batch) error {
	results := w.db.SendBatch(ctx, batch)

	var execErr error
	for range batch.Len() {
		if _, err := results.Exec(); err != nil && execErr == nil {
			execErr = err
		}
	}
	if err := results.Close(); err != nil && execErr == nil {
		execErr = err
	}
	return execErr
}
