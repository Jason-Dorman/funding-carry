package fix

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/shopspring/decimal"

	"github.com/Jason-Dorman/funding-carry/internal/config"
	"github.com/Jason-Dorman/funding-carry/internal/db"
)

// Fakes for the simulator's three dependencies: the recorded market, the writer,
// and the FIX session. Each one fails where the real thing fails — a fake more
// forgiving than its subject turns its tests into decorations (testing
// strategy).

const (
	testPerp  = "ETP-20DEC30-CDE"
	testSpot  = "ETH-USD"
	testSeed  = 42
	testStore = "fix"
)

// epoch is a fixed instant every test starts from, so a report's TransactTime is
// as reproducible as its price.
var epoch = time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// testFill is the committed .env.example fill model, so the tests exercise the
// configuration a developer actually runs with.
func testFill() config.FillModel {
	return config.FillModel{
		Latency:          20 * time.Millisecond,
		SlippageBps:      decimal.RequireFromString("2"),
		PartialThreshold: decimal.RequireFromString("10"),
		PartialSlices:    3,
		BookPoll:         time.Second,
		BookMaxAge:       60 * time.Second,
	}
}

// fakeClock is the injected wall clock. Tests advance it explicitly; nothing in
// this package sleeps to make time pass. It carries no mutex because the engine
// is single-goroutine and the tests that use it drive that goroutine's methods
// directly — the loop tests use the real clock instead.
type fakeClock struct{ t time.Time }

func newClock(at time.Time) *fakeClock { return &fakeClock{t: at} }

func (c *fakeClock) now() time.Time { return c.t }

func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// fakeBooks serves one snapshot, or a failure, or nothing at all — the three
// states a real read has.
type fakeBooks struct {
	mu    sync.Mutex
	book  db.StoredBook
	have  bool
	err   error
	reads int
}

func newBooks(at time.Time, bid, ask string) *fakeBooks {
	return &fakeBooks{
		book: db.StoredBook{
			TS:      at,
			BestBid: decimal.RequireFromString(bid),
			BestAsk: decimal.RequireFromString(ask),
		},
		have: true,
	}
}

func (b *fakeBooks) LatestBook(ctx context.Context, _ string) (db.StoredBook, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.reads++
	if err := ctx.Err(); err != nil {
		return db.StoredBook{}, false, err
	}
	return b.book, b.have, b.err
}

func (b *fakeBooks) set(at time.Time, bid, ask string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.book = db.StoredBook{
		TS:      at,
		BestBid: decimal.RequireFromString(bid),
		BestAsk: decimal.RequireFromString(ask),
	}
	b.have = true
	b.err = nil
}

func (b *fakeBooks) fail(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.err = err
}

func (b *fakeBooks) readCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.reads
}

// fakeSink collects rows, and refuses them once it is told to — the writer
// answers ErrWriterStopped from the moment its shutdown begins, and a fake that
// accepted rows forever would hide the engine's obligation to stop with it.
type fakeSink struct {
	mu      sync.Mutex
	rows    []db.Row
	err     error
	rejects int
}

func (s *fakeSink) Submit(ctx context.Context, r db.Row) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.err != nil {
		s.rejects++
		return s.err
	}
	s.rows = append(s.rows, r)
	return nil
}

func (s *fakeSink) stop(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

func (s *fakeSink) fills() []db.FillRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []db.FillRow
	for _, r := range s.rows {
		if f, ok := r.(db.FillRow); ok {
			out = append(out, f)
		}
	}
	return out
}

func (s *fakeSink) sessions() []db.FIXSessionRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []db.FIXSessionRow
	for _, r := range s.rows {
		if f, ok := r.(db.FIXSessionRow); ok {
			out = append(out, f)
		}
	}
	return out
}

// recordingVenue is the FIX session: it keeps what would have gone on the wire.
type recordingVenue struct {
	mu      sync.Mutex
	reports []report
	rejects []cancelReject
	err     error
}

func (v *recordingVenue) sendReport(r report) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.err != nil {
		return v.err
	}
	v.reports = append(v.reports, r)
	return nil
}

func (v *recordingVenue) sendCancelReject(r cancelReject) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.err != nil {
		return v.err
	}
	v.rejects = append(v.rejects, r)
	return nil
}

func (v *recordingVenue) all() []report {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]report(nil), v.reports...)
}

func (v *recordingVenue) last() report {
	v.mu.Lock()
	defer v.mu.Unlock()
	if len(v.reports) == 0 {
		return report{}
	}
	return v.reports[len(v.reports)-1]
}

// harness is one engine and everything it talks to, wired for a test.
type harness struct {
	engine *engine
	books  *fakeBooks
	sink   *fakeSink
	venue  *recordingVenue
	clock  *fakeClock
	reg    *prometheus.Registry
	ctx    context.Context
}

func newHarness(t *testing.T, fill config.FillModel) *harness {
	t.Helper()
	return newSeededHarness(t, fill, testSeed)
}

// newSeededHarness is newHarness with the generator's seed under the test's
// control, which is what lets a test assert that the seed actually reaches the
// engine rather than merely that the engine is reproducible.
func newSeededHarness(t *testing.T, fill config.FillModel, seed uint64) *harness {
	t.Helper()

	clock := newClock(epoch)
	books := newBooks(epoch, "2344.50", "2345.00")
	sink := &fakeSink{}
	out := &recordingVenue{}

	reg := prometheus.NewRegistry()
	e := newEngine(engineOptions{
		Product: testPerp,
		Fill:    fill,
		Seed:    seed,
		Now:     clock.now,
	}, books, sink, out, NewMetrics(reg), testLogger())

	h := &harness{engine: e, books: books, sink: sink, venue: out, clock: clock, reg: reg, ctx: t.Context()}
	// The run loop reads the book before its first wait; a test driving the
	// engine's methods directly does the same thing here, so both paths start
	// from a book rather than from nothing.
	e.readBook(h.ctx)
	return h
}

// order submits one order and returns the ClOrdID it was given.
func (h *harness) order(clOrdID string, s side, qty, limitPx string, t tif) {
	h.submit(submission{
		clOrdID: clOrdID,
		symbol:  testPerp,
		side:    s,
		tif:     t,
		ordType: "2", // enum.OrdType_LIMIT
		qty:     decimal.RequireFromString(qty),
		limitPx: decimal.RequireFromString(limitPx),
		at:      h.clock.now(),
	})
}

func (h *harness) submit(s submission) {
	if err := h.engine.accept(h.ctx, s); err != nil {
		panic(err)
	}
}

// tick advances the clock and gives every due order its evaluation.
func (h *harness) tick(t *testing.T, d time.Duration) {
	t.Helper()
	h.clock.advance(d)
	if err := h.engine.tick(h.ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
}

func (h *harness) cancel(t *testing.T, clOrdID, origClOrdID string) {
	t.Helper()
	if err := h.engine.cancelRequested(h.ctx, cancelRequest{
		clOrdID:     clOrdID,
		origClOrdID: origClOrdID,
		at:          h.clock.now(),
	}); err != nil {
		t.Fatalf("cancel: %v", err)
	}
}
