package db

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/shopspring/decimal"
)

// ---------------------------------------------------------------------------
// A fake database, per the testing strategy: a hand-written stand-in for the one
// method the writer actually uses, not a mocking framework.
// ---------------------------------------------------------------------------

type sentBatch struct {
	statements []string
	arguments  [][]any
}

type fakeDB struct {
	mu       sync.Mutex
	batches  []sentBatch
	ctxErrs  []error // ctx.Err() of the context each batch was written on
	sent     chan struct{}
	failures int   // number of leading sends whose statements fail
	failErr  error // the failure they report

	// closeFailures fails the batch at Close instead of at Exec: every statement
	// reports success and only the commit's acknowledgement is lost. That is the
	// in-doubt case, and it is the one a fake has to be able to produce, because
	// a test cannot ask a real database to drop a connection at that instant.
	closeFailures int
	closeErr      error

	// noopAfter makes every batch from this one onwards report zero rows
	// inserted, standing in for a re-sent batch landing on rows already written.
	noopAfter int

	// hold, when set, parks the first batch inside the fake until the test
	// closes it, and entered reports that the writer has arrived there. Together
	// they let a test land a cancellation in the middle of a write rather than
	// hoping to hit the window by timing.
	hold    chan struct{}
	held    bool // the hold applies to the first batch only
	entered chan struct{}
}

func newFakeDB() *fakeDB {
	return &fakeDB{sent: make(chan struct{}, 64)}
}

// blocking returns a fake whose first batch stalls until the test releases it.
func blockingFakeDB() *fakeDB {
	f := newFakeDB()
	f.hold = make(chan struct{})
	f.entered = make(chan struct{}, 1)
	return f
}

func (f *fakeDB) SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults {
	f.mu.Lock()
	var hold chan struct{}
	if !f.held {
		// The field itself is left alone so the test still owns the channel and
		// can close it to release the write.
		f.held, hold = true, f.hold
	}
	f.mu.Unlock()

	if hold != nil {
		f.entered <- struct{}{}
		<-hold
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	// Recorded here, after the hold releases, so a test that cancels while the
	// batch is parked is asking the question that matters: is the context this
	// write is running on still alive?
	f.ctxErrs = append(f.ctxErrs, ctx.Err())

	batch := sentBatch{}
	for _, q := range b.QueuedQueries {
		batch.statements = append(batch.statements, q.SQL)
		batch.arguments = append(batch.arguments, q.Arguments)
	}
	f.batches = append(f.batches, batch)

	var closeErr error
	if f.closeFailures > 0 {
		f.closeFailures--
		closeErr = f.closeErr
	}

	var err error
	switch {
	case ctx.Err() != nil:
		// A real driver fails the round trip when the context it was handed is
		// dead. A fake that ignored that would let every cancellation bug in
		// this file pass unnoticed — which is how the flush defect survived.
		err = ctx.Err()
	case f.failures > 0:
		f.failures--
		err = f.failErr
	}

	// Non-blocking so a test that does not watch the channel cannot deadlock the
	// writer.
	select {
	case f.sent <- struct{}{}:
	default:
	}

	noop := f.noopAfter > 0 && len(f.batches) >= f.noopAfter
	return &fakeResults{n: len(batch.statements), err: err, closeErr: closeErr, noop: noop}
}

// contexts reports the liveness of every context the writer handed the database.
func (f *fakeDB) contexts() []error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]error(nil), f.ctxErrs...)
}

// distinctRowsFor counts the distinct rows the fake was asked to insert into a
// table. Retries re-send the same row, so a raw statement count can make a lost
// row and a retried one look identical.
func (f *fakeDB) distinctRowsFor(table string) int {
	seen := map[string]struct{}{}
	for _, b := range f.snapshot() {
		for i, stmt := range b.statements {
			if strings.HasPrefix(stmt, "INSERT INTO "+table+" ") {
				seen[fmt.Sprint(b.arguments[i])] = struct{}{}
			}
		}
	}
	return len(seen)
}

func (f *fakeDB) snapshot() []sentBatch {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sentBatch(nil), f.batches...)
}

// rowsFor counts the rows the fake was asked to insert into one table, across
// every batch it received.
func (f *fakeDB) rowsFor(table string) int {
	var n int
	for _, b := range f.snapshot() {
		for _, stmt := range b.statements {
			if strings.HasPrefix(stmt, "INSERT INTO "+table+" ") {
				n++
			}
		}
	}
	return n
}

type fakeResults struct {
	n        int
	err      error
	closeErr error
	// noop makes every statement report that it inserted nothing, which is what
	// an ON CONFLICT ... DO NOTHING insert does when the row is already there.
	noop bool
}

func (r *fakeResults) Exec() (pgconn.CommandTag, error) {
	if r.err != nil {
		return pgconn.CommandTag{}, r.err
	}
	if r.noop {
		return pgconn.NewCommandTag("INSERT 0 0"), nil
	}
	return pgconn.NewCommandTag("INSERT 0 1"), nil
}
func (r *fakeResults) Query() (pgx.Rows, error) { return nil, errors.New("not used") }
func (r *fakeResults) QueryRow() pgx.Row        { return nil }
func (r *fakeResults) Close() error             { return r.closeErr }

// ---------------------------------------------------------------------------

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testMetrics() *WriterMetrics {
	return NewWriterMetrics(prometheus.NewRegistry(), "test")
}

// startWriter runs the writer and returns a stop function that closes it and
// reports its error. Tests never sleep to synchronize; they either drive the
// flush ticker or wait on the fake's send channel.
func startWriter(t *testing.T, f *fakeDB, opts WriterOptions) (*Writer, func() error) {
	t.Helper()

	w := NewWriter(f, opts, discardLogger(), testMetrics())
	done := make(chan error, 1)
	go func() { done <- w.Run(context.Background()) }()

	var (
		once   sync.Once
		runErr error
	)
	stop := func() error {
		once.Do(func() {
			_ = w.Close()
			runErr = <-done
		})
		return runErr
	}
	t.Cleanup(func() { _ = stop() })
	return w, stop
}

func sampleBar(product string, ts time.Time) BarRow {
	return BarRow{
		TS: ts, ProductID: product, TF: "1m",
		Open:  decimal.RequireFromString("4000.10"),
		High:  decimal.RequireFromString("4001.20"),
		Low:   decimal.RequireFromString("3999.80"),
		Close: decimal.RequireFromString("4000.90"),
		// A candle with no trades is a real candle; the count is zero, not absent.
		Volume:     decimal.Zero,
		TradeCount: Opt[int32](0),
	}
}

// Batching is size-triggered: the writer holds rows until the pending count
// reaches BatchSize, then sends them as one batch.
func TestWriterFlushesOnBatchSize(t *testing.T) {
	t.Parallel()

	f := newFakeDB()
	// A long interval takes the ticker out of the picture entirely.
	w, _ := startWriter(t, f, WriterOptions{BatchSize: 3, FlushInterval: time.Hour})

	ctx := context.Background()
	base := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	for i := range 3 {
		if err := w.Submit(ctx, sampleBar("ETP-20DEC30-CDE", base.Add(time.Duration(i)*time.Minute))); err != nil {
			t.Fatalf("submit: %v", err)
		}
	}

	<-f.sent

	batches := f.snapshot()
	if len(batches) != 1 {
		t.Fatalf("expected exactly one batch, got %d", len(batches))
	}
	if got := len(batches[0].statements); got != 3 {
		t.Fatalf("expected 3 statements in the batch, got %d", got)
	}
}

// The interval is the other trigger: a row that never reaches the batch size
// must still be written promptly.
func TestWriterFlushesOnInterval(t *testing.T) {
	t.Parallel()

	f := newFakeDB()
	// Unbuffered, so a send here lands only when the writer's select takes it.
	ticks := make(chan time.Time)
	w, _ := startWriter(t, f, WriterOptions{BatchSize: 1000, ticks: ticks})

	ts := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	if err := w.Submit(context.Background(), sampleBar("ETP-20DEC30-CDE", ts)); err != nil {
		t.Fatalf("submit: %v", err)
	}

	// The queued row and the tick are both ready, and select picks between them
	// at random: a tick taken first finds nothing pending and flushes nothing.
	// That is fine in production — the next tick writes the row — but a test
	// cannot wait for a ticker it owns, so it keeps offering ticks until a batch
	// actually goes out.
	for flushed := false; !flushed; {
		select {
		case ticks <- ts:
		case <-f.sent:
			flushed = true
		}
	}

	if got := f.rowsFor("cb_bars"); got != 1 {
		t.Fatalf("expected 1 bar written on the interval tick, got %d", got)
	}
}

// Rows are grouped per statement and every value is a bind parameter: nothing a
// producer supplies reaches the SQL text.
func TestWriterGroupsRowsPerTable(t *testing.T) {
	t.Parallel()

	f := newFakeDB()
	w, stop := startWriter(t, f, WriterOptions{BatchSize: 100, FlushInterval: time.Hour})

	ctx := context.Background()
	ts := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	rows := []Row{
		sampleBar("ETP-20DEC30-CDE", ts),
		BaseStateRow{TS: ts, SpotPx: Num(decimal.RequireFromString("4000.25"))},
		sampleBar("ETH-USD", ts),
		VenueStateRow{TS: ts, ProductID: "ETP-20DEC30-CDE", FundingSource: FundingSourceComputed},
	}
	for _, r := range rows {
		if err := w.Submit(ctx, r); err != nil {
			t.Fatalf("submit: %v", err)
		}
	}
	if err := stop(); err != nil {
		t.Fatalf("close: %v", err)
	}

	batches := f.snapshot()
	if len(batches) != 1 {
		t.Fatalf("expected one batch, got %d", len(batches))
	}

	// Same table, same statement — and the two bars are adjacent, because rows
	// accumulate per statement rather than in arrival order.
	want := []string{"cb_bars", "cb_bars", "base_state", "cb_venue_state"}
	for i, table := range want {
		prefix := "INSERT INTO " + table + " ("
		if !strings.HasPrefix(batches[0].statements[i], prefix) {
			t.Errorf("statement %d: expected an insert into %s, got %q", i, table, batches[0].statements[i])
		}
	}
	for _, stmt := range batches[0].statements {
		if strings.Contains(stmt, "4000") {
			t.Errorf("value interpolated into SQL text: %q", stmt)
		}
	}
}

// Close is a flush: rows still pending when the producers stop are written
// before Run returns. This is the "one flush on close" acceptance item.
func TestWriterFlushesPendingRowsOnClose(t *testing.T) {
	t.Parallel()

	f := newFakeDB()
	w, stop := startWriter(t, f, WriterOptions{BatchSize: 1000, FlushInterval: time.Hour})

	ctx := context.Background()
	base := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	for i := range 7 {
		if err := w.Submit(ctx, sampleBar("ETP-20DEC30-CDE", base.Add(time.Duration(i)*time.Minute))); err != nil {
			t.Fatalf("submit: %v", err)
		}
	}

	// Nothing has been written yet: neither trigger has fired.
	if got := len(f.snapshot()); got != 0 {
		t.Fatalf("expected no batch before close, got %d", got)
	}

	if err := stop(); err != nil {
		t.Fatalf("close: %v", err)
	}

	batches := f.snapshot()
	if len(batches) != 1 {
		t.Fatalf("expected exactly one flush on close, got %d", len(batches))
	}
	if got := len(batches[0].statements); got != 7 {
		t.Fatalf("expected the 7 pending rows in the final flush, got %d", got)
	}
}

// Cancelling the root context is the SIGTERM path: it must still flush, on a
// context the cancellation cannot reach.
func TestWriterFlushesOnContextCancel(t *testing.T) {
	t.Parallel()

	f := newFakeDB()
	w := NewWriter(f, WriterOptions{BatchSize: 1000, FlushInterval: time.Hour}, discardLogger(), testMetrics())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	ts := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	if err := w.Submit(context.Background(), sampleBar("ETP-20DEC30-CDE", ts)); err != nil {
		t.Fatalf("submit: %v", err)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := f.rowsFor("cb_bars"); got != 1 {
		t.Fatalf("expected the pending row to survive cancellation, got %d", got)
	}
}

// Regression for a defect the Part 3 onramp toy hit first: only the final flush
// was detached from cancellation, so a shutdown landing while a size- or
// interval-triggered batch was in flight aborted the round trip. A write in
// progress must reach the database on a live context whatever the parent is
// doing.
func TestWriterNeverWritesOnACanceledContext(t *testing.T) {
	t.Parallel()

	f := blockingFakeDB()
	w := NewWriter(f, WriterOptions{BatchSize: 1, FlushInterval: time.Hour}, discardLogger(), testMetrics())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	ts := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	if err := w.Submit(context.Background(), sampleBar("ETP-20DEC30-CDE", ts)); err != nil {
		t.Fatalf("submit: %v", err)
	}

	<-f.entered // the batch is now inside the database call
	cancel()    // ... and the parent dies underneath it
	close(f.hold)

	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}

	got := f.contexts()
	if len(got) == 0 {
		t.Fatal("no batches reached the database")
	}
	for i, err := range got {
		if err != nil {
			t.Errorf("batch %d was written on a dead context: %v", i, err)
		}
	}
}

// The other half of the same defect: once the in-flight batch survives, the rows
// queued behind it must still be drained and flushed. This is the promise in
// architecture section 8 — cancel, drain, flush, close — and it is the path that
// runs on every SIGTERM while a position is open.
func TestWriterDrainsAndFinalFlushesAfterCancellationMidBatch(t *testing.T) {
	t.Parallel()

	f := blockingFakeDB()
	w := NewWriter(f, WriterOptions{BatchSize: 1, FlushInterval: time.Hour}, discardLogger(), testMetrics())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	ts := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	if err := w.Submit(context.Background(), sampleBar("ETP-20DEC30-CDE", ts)); err != nil {
		t.Fatalf("submit: %v", err)
	}
	<-f.entered

	// Two more rows accepted while the writer is stuck in the first batch. The
	// producers have been told these are safe.
	for i := 1; i <= 2; i++ {
		if err := w.Submit(context.Background(), sampleBar("ETP-20DEC30-CDE", ts.Add(time.Duration(i)*time.Minute))); err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
	}

	cancel()
	close(f.hold)

	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := f.distinctRowsFor("cb_bars"); got != 3 {
		t.Fatalf("distinct rows written = %d, want all 3: the queued rows were dropped on shutdown", got)
	}
}

// A transient failure the server itself reported is retried: the server answered,
// which means it rolled the implicit transaction back, so re-sending cannot
// duplicate anything.
func TestWriterRetriesFailedBatch(t *testing.T) {
	t.Parallel()

	f := newFakeDB()
	f.failures = 2
	f.failErr = &pgconn.PgError{Code: "40001", Message: "serialization failure"}

	w, stop := startWriter(t, f, WriterOptions{
		BatchSize:    1,
		MaxAttempts:  3,
		RetryBackoff: time.Millisecond,
	})

	ts := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	if err := w.Submit(context.Background(), sampleBar("ETP-20DEC30-CDE", ts)); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if err := stop(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if got := len(f.snapshot()); got != 3 {
		t.Fatalf("expected 3 send attempts, got %d", got)
	}
}

// Exhausting the retries is fatal. A feed error degrades to staleness; a
// database that will not take writes is an invariant violation, and Run says so
// rather than dropping rows quietly (architecture section 8).
func TestWriterFailsFatallyWhenRetriesAreExhausted(t *testing.T) {
	t.Parallel()

	f := newFakeDB()
	f.failures = 100
	// A failure that is safe to repeat, so the batch really does get all of its
	// attempts. A bare error would be abandoned after the first, which is a
	// different path — TestWriterDoesNotResendABatchWhoseCommitIsUnknown covers it.
	f.failErr = &pgconn.PgError{Code: "40001", Message: "serialization failure"}

	w := NewWriter(f, WriterOptions{
		BatchSize:    1,
		MaxAttempts:  2,
		RetryBackoff: time.Millisecond,
	}, discardLogger(), testMetrics())

	done := make(chan error, 1)
	go func() { done <- w.Run(context.Background()) }()

	ts := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	if err := w.Submit(context.Background(), sampleBar("ETP-20DEC30-CDE", ts)); err != nil {
		t.Fatalf("submit: %v", err)
	}

	err := <-done
	if err == nil {
		t.Fatal("expected a fatal error after the retries were exhausted")
	}
	if !strings.Contains(err.Error(), "failed after 2 attempts") {
		t.Fatalf("expected the error to name the attempts, got %v", err)
	}
	// Close reports the same failure to a caller that only holds the writer.
	if closeErr := w.Close(); !errors.Is(closeErr, err) {
		t.Fatalf("Close returned %v, want the run error %v", closeErr, err)
	}
}

// Submitting after Close is refused rather than silently dropped or panicking on
// a closed channel.
func TestSubmitAfterCloseIsRefused(t *testing.T) {
	t.Parallel()

	f := newFakeDB()
	w, stop := startWriter(t, f, WriterOptions{})
	if err := stop(); err != nil {
		t.Fatalf("close: %v", err)
	}

	ts := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	if err := w.Submit(context.Background(), sampleBar("ETP-20DEC30-CDE", ts)); !errors.Is(err, ErrWriterStopped) {
		t.Fatalf("Submit after Close returned %v, want ErrWriterStopped", err)
	}
}

// The generated SQL is parameterized, names its columns, and carries the
// conflict clause the idempotent tables need.
func TestInsertStatement(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		row  Row
		want string
	}{
		{
			name: "insert keyed on the sample instant",
			row:  BaseStateRow{},
			want: "INSERT INTO base_state (ts, spot_px, wallet_eth, wallet_usdc, gas_gwei) " +
				"VALUES ($1, $2, $3, $4, $5) ON CONFLICT (ts) DO NOTHING",
		},
		{
			name: "idempotent insert",
			row:  sampleBar("ETP-20DEC30-CDE", time.Now()),
			want: "INSERT INTO cb_bars (ts, product_id, tf, open, high, low, close, volume, trade_count) " +
				"VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) " +
				"ON CONFLICT (product_id, tf, ts) DO NOTHING",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := insertStatement(tc.row.row()); got != tc.want {
				t.Errorf("insertStatement:\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}

// Submit's contract has to hold on every path Run can exit by, not just the one
// Close causes. A writer that has stopped must say so; returning nil for a row
// queued into a channel with no reader reports a durable write that will never
// happen.
func TestSubmitIsRefusedAfterRunExitsOnItsOwn(t *testing.T) {
	t.Parallel()

	tests := map[string]func(t *testing.T) *Writer{
		"fatal flush": func(t *testing.T) *Writer {
			f := newFakeDB()
			f.failures = 100
			f.failErr = errors.New("connection refused")
			w := NewWriter(f, WriterOptions{BatchSize: 1, MaxAttempts: 1}, discardLogger(), testMetrics())

			done := make(chan error, 1)
			go func() { done <- w.Run(context.Background()) }()
			if err := w.Submit(context.Background(), sampleBar("ETP-20DEC30-CDE", time.Now())); err != nil {
				t.Fatalf("submit: %v", err)
			}
			if err := <-done; err == nil {
				t.Fatal("expected the writer to fail fatally")
			}
			return w
		},
		"root context canceled": func(t *testing.T) *Writer {
			f := newFakeDB()
			w := NewWriter(f, WriterOptions{FlushInterval: time.Hour}, discardLogger(), testMetrics())

			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- w.Run(ctx) }()
			cancel()
			if err := <-done; err != nil {
				t.Fatalf("run: %v", err)
			}
			return w
		},
	}

	for name, exit := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			w := exit(t)

			// context.Background, deliberately: a producer whose own context is
			// still alive is the case that would otherwise get nil back.
			err := w.Submit(context.Background(), sampleBar("ETP-20DEC30-CDE", time.Now()))
			if !errors.Is(err, ErrWriterStopped) {
				t.Fatalf("Submit after Run returned %v, want ErrWriterStopped", err)
			}
			// And Close still works afterwards, reporting whatever Run reported.
			_ = w.Close()
		})
	}
}

// The in-doubt case: every statement reported success and only the close failed,
// so the batch may or may not have committed. Before the natural keys existed
// this had to be abandoned, because re-sending a batch that had committed would
// duplicate every row in it. Now it is retried, and the retry is a no-op if the
// first attempt did land — which is the availability the keys were bought for.
func TestWriterRetriesABatchWhoseCommitIsUnknown(t *testing.T) {
	t.Parallel()

	f := newFakeDB()
	f.closeFailures = 1 // the first attempt is in doubt; the second settles it
	f.closeErr = errors.New("unexpected EOF")
	f.noopAfter = 2 // and the second attempt finds the rows already there

	registry := prometheus.NewRegistry()
	w := NewWriter(f, WriterOptions{
		BatchSize:    1,
		MaxAttempts:  3,
		RetryBackoff: time.Millisecond,
	}, discardLogger(), NewWriterMetrics(registry, "test"))

	done := make(chan error, 1)
	go func() { done <- w.Run(context.Background()) }()

	if err := w.Submit(context.Background(), sampleBar("ETP-20DEC30-CDE", time.Now())); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("an in-doubt batch should now be recoverable, got %v", err)
	}

	if got := len(f.snapshot()); got != 2 {
		t.Fatalf("batch was sent %d times, want 2 (the in-doubt attempt and the retry)", got)
	}
	// The retry inserted nothing, and the metrics say so rather than reporting a
	// write that never happened.
	if got := counterValue(t, registry, "test_rows_written_total", "cb_bars"); got != 0 {
		t.Errorf("rows_written_total = %v, want 0: the retry landed on a row that was already there", got)
	}
	if got := counterValue(t, registry, "test_rows_conflicted_total", "cb_bars"); got != 1 {
		t.Errorf("rows_conflicted_total = %v, want 1", got)
	}
}

// A rejection the server will repeat is not worth three attempts. A constraint
// violation is a property of the rows or the schema; retrying it only spends the
// flush timeout before failing with the same error.
func TestWriterDoesNotRetryAServerRejection(t *testing.T) {
	t.Parallel()

	f := newFakeDB()
	f.failures = 100
	f.failErr = &pgconn.PgError{
		Code:    "23514", // check_violation, e.g. an unknown funding_source
		Message: `new row violates check constraint "cb_venue_state_funding_source_check"`,
	}

	w := NewWriter(f, WriterOptions{
		BatchSize:   1,
		MaxAttempts: 3,
		// Short, so a regression fails on the attempt count below rather than
		// hanging until the package timeout.
		RetryBackoff: time.Millisecond,
	}, discardLogger(), testMetrics())

	done := make(chan error, 1)
	go func() { done <- w.Run(context.Background()) }()

	if err := w.Submit(context.Background(), sampleBar("ETP-20DEC30-CDE", time.Now())); err != nil {
		t.Fatalf("submit: %v", err)
	}

	err := <-done
	if err == nil {
		t.Fatal("expected a rejected batch to be fatal")
	}
	if !strings.Contains(err.Error(), "check constraint") {
		t.Errorf("the server's reason should survive to the top, got %v", err)
	}
	if got := len(f.snapshot()); got != 1 {
		t.Fatalf("batch was sent %d times, want 1", got)
	}
	_ = w.Close()
}

// The classification in one table, so the policy can be read rather than
// inferred from three tests. Safety is no longer part of it — idempotent inserts
// make any repeat harmless — so the only question left is whether another
// attempt could succeed.
func TestResendPolicy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"serialization failure clears on its own", &pgconn.PgError{Code: "40001"}, true},
		{"deadlock clears on its own", &pgconn.PgError{Code: "40P01"}, true},
		{"connection limit clears on its own", &pgconn.PgError{Code: "53300"}, true},
		{"check violation will repeat", &pgconn.PgError{Code: "23514"}, false},
		{"unique violation means the conflict target is wrong", &pgconn.PgError{Code: "23505"}, false},
		{"undefined column will repeat", &pgconn.PgError{Code: "42703"}, false},
		{"wrapped server error is still classified", fmt.Errorf("statement 3 of 9: %w", &pgconn.PgError{Code: "40001"}), true},
		{"a lost connection may come back", errors.New("unexpected EOF"), true},
		{"an unknown commit is settled by repeating it", errors.New("commit status unknown after close"), true},
		{"a context error is handled by the retry loop, not the policy", context.Canceled, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := worthRetrying(tc.err); got != tc.want {
				t.Errorf("worthRetrying(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func counterValue(t *testing.T, g prometheus.Gatherer, name, label string) float64 {
	t.Helper()

	families, err := g.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			for _, pair := range metric.GetLabel() {
				if pair.GetValue() == label {
					return metric.GetCounter().GetValue()
				}
			}
		}
	}
	t.Fatalf("no metric %s with label %s", name, label)
	return 0
}
