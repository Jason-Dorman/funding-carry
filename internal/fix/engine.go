package fix

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strconv"
	"time"

	"github.com/quickfixgo/enum"
	"github.com/quickfixgo/quickfix"
	"github.com/shopspring/decimal"

	"github.com/Jason-Dorman/funding-carry/internal/config"
	"github.com/Jason-Dorman/funding-carry/internal/db"
)

// The matching engine: the simulator's book of live orders, the loop that
// evaluates them against recorded market data, and the reports that come out.
//
// One goroutine owns all of it. Orders arrive from the FIX connection's
// goroutines over channels and every piece of state below is touched only by
// run, which is what makes "what would this order have done" answerable by
// reading one function rather than by reasoning about locks.

// Sink is where rows go: the writer, named here as a one-method interface
// because this package is its consumer (architecture section 12). Submit blocks
// when the writer's queue is full, and that backpressure is deliberate — a
// simulator that kept printing fills it could not record would be inventing
// history.
type Sink interface {
	Submit(ctx context.Context, r db.Row) error
}

// BookSource is the recorded market the fill model prices against: the latest
// top-of-book ingest wrote for one product.
//
// The simulator reads the same cb_book_snapshots rows the feature engine and the
// replay read, which is the point. A fill model with its own private idea of the
// market would make a sim run unreproducible from the database, and every cost
// number derived from it unfalsifiable (ADR-0019).
type BookSource interface {
	LatestBook(ctx context.Context, product string) (db.StoredBook, bool, error)
}

// venue is where replies go: quickfix in production, a recorder in tests.
type venue interface {
	sendReport(r report) error
	sendCancelReject(r cancelReject) error
}

// waitFunc starts a wait of d and returns the channel it fires on plus a stop
// for a wait that is abandoned. The same seam the ingest pollers use, and for
// the same reason: the scheduling is the part worth testing, and a test that
// raced real timers to reach it would be testing the machine's load average.
type waitFunc func(d time.Duration) (<-chan time.Time, func() bool)

func realTimer(d time.Duration) (<-chan time.Time, func() bool) {
	t := time.NewTimer(d)
	return t.C, t.Stop
}

// Business rejections, spelled out once. They are the Text (tag 58) on the
// ExecutionReport, so they are what an operator reads in a log and what a test
// asserts on; a literal in both places would drift.
const (
	rejectDuplicate     = "duplicate ClOrdID"
	rejectUnknownSymbol = "unknown symbol"
	rejectOrdType       = "only limit orders are accepted"
	rejectTIF           = "unsupported TimeInForce"
	rejectQty           = "OrderQty must be a positive whole number of contracts"
	rejectPrice         = "Price must be positive"
	rejectNoMarket      = "no market data"
)

// Cancel reasons, likewise.
const (
	cancelRequested    = "canceled on request"
	cancelIOCRemainder = "IOC remainder canceled"
	cancelNoMarket     = "IOC canceled: no market data"
)

// submission is one NewOrderSingle, parsed but not yet judged: everything the
// engine needs to decide whether to accept it.
type submission struct {
	clOrdID string
	symbol  string
	side    side
	tif     tif
	ordType enum.OrdType
	qty     decimal.Decimal
	limitPx decimal.Decimal
	session quickfix.SessionID
	at      time.Time
}

// cancelRequest is one OrderCancelRequest.
type cancelRequest struct {
	clOrdID     string
	origClOrdID string
	session     quickfix.SessionID
	at          time.Time
}

// order is one live order on the simulator's book.
type order struct {
	clOrdID string
	orderID string
	session quickfix.SessionID
	symbol  string
	side    side
	tif     tif
	qty     decimal.Decimal
	limitPx decimal.Decimal

	// slices is the plan: the quantity of every report this order will print,
	// decided once at entry so the sequence is a property of the order rather
	// than of when the book happened to move.
	slices []decimal.Decimal
	next   int

	cumQty decimal.Decimal
	// notional is the running sum of price times quantity, which is what makes
	// AvgPx a weighted average rather than an average of prices.
	notional decimal.Decimal

	// readyAt is the earliest moment the next slice may print. It carries the
	// latency between fills, and it is also what keeps the run loop from
	// spinning: an order that could not fill is deferred to the next book read,
	// because nothing else can change its answer.
	readyAt time.Time
	execSeq int
}

func (o *order) leaves() decimal.Decimal { return o.qty.Sub(o.cumQty) }

func (o *order) done() bool { return o.next >= len(o.slices) }

// avgPx is a ratio, which is what Div is for (API spec section 3.5). It is never
// the input to anything that has to reconcile against a venue statement — that
// is cumQty and the individual fill prices, which are only added and multiplied.
func (o *order) avgPx() decimal.Decimal {
	if o.cumQty.IsZero() {
		return decimal.Zero
	}
	return o.notional.Div(o.cumQty)
}

// engineOptions is everything the engine needs that is not a dependency.
type engineOptions struct {
	Product string
	Fill    config.FillModel
	Seed    uint64
	Now     func() time.Time
	Wait    waitFunc
}

type engine struct {
	product string
	fill    config.FillModel
	books   BookSource
	sink    Sink
	out     venue
	m       *Metrics
	log     *slog.Logger
	now     func() time.Time
	wait    waitFunc
	rng     *rand.Rand

	submits chan submission
	cancels chan cancelRequest
	stopped chan struct{}

	// Everything below is owned by run and touched by nothing else.
	live         map[string]*order
	final        map[string]enum.OrdStatus
	ids          idMinter
	book         book
	haveBook     bool
	nextBookRead time.Time
}

func newEngine(opts engineOptions, books BookSource, sink Sink, out venue, m *Metrics, log *slog.Logger) *engine {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	wait := opts.Wait
	if wait == nil {
		wait = realTimer
	}
	return &engine{
		product: opts.Product,
		fill:    opts.Fill,
		books:   books,
		sink:    sink,
		out:     out,
		m:       m,
		log:     log.With("component", "fill_engine"),
		now:     now,
		wait:    wait,
		// PCG rather than the global source: the seed is the whole point, and a
		// package-global generator is shared with anything else that draws from
		// it.
		rng: rand.New(rand.NewPCG(opts.Seed, opts.Seed)), //nolint:gosec // a seeded venue-latency model, not a security decision
		// Unbuffered, deliberately. A buffer would mean a message was accepted
		// by a channel rather than by the engine, and the difference shows up
		// exactly when it matters: once the engine has stopped, a buffered send
		// is still ready, so the select below would pick at random between
		// queueing the order and reporting the shutdown — and half the time an
		// order would be answered as accepted and then sat in a buffer with no
		// reader. Unbuffered, a send can only complete when the engine has
		// actually taken it, so after shutdown the refusal is the only branch
		// that can fire. The connection goroutine blocking for the length of one
		// handoff is the backpressure, and it is correct: a venue should not
		// take orders faster than it can price them.
		submits: make(chan submission),
		cancels: make(chan cancelRequest),
		stopped: make(chan struct{}),
		live:    make(map[string]*order),
		final:   make(map[string]enum.OrdStatus),
		ids:     newIDMinter(now()),
	}
}

// submit hands a parsed order to the engine, or reports that the engine has
// stopped. It is called from a FIX connection goroutine.
func (e *engine) submit(s submission) error {
	select {
	case e.submits <- s:
		return nil
	case <-e.stopped:
		return errEngineStopped
	}
}

func (e *engine) requestCancel(c cancelRequest) error {
	select {
	case e.cancels <- c:
		return nil
	case <-e.stopped:
		return errEngineStopped
	}
}

var errEngineStopped = errors.New("fill engine stopped")

// run owns the book until ctx is canceled. It returns an error only when the
// writer has gone: a simulator that cannot record its fills has to bring the
// binary down, the same way ingest does, because the alternative is a process
// that looks healthy while discarding the only record of what it did.
func (e *engine) run(ctx context.Context) error {
	defer close(e.stopped)

	e.readBook(ctx)
	for {
		fired, stop := e.wait(e.untilNextWake())
		select {
		case <-ctx.Done():
			stop()
			return nil
		case s := <-e.submits:
			stop()
			if err := e.accept(ctx, s); err != nil {
				return err
			}
		case c := <-e.cancels:
			stop()
			if err := e.cancelRequested(ctx, c); err != nil {
				return err
			}
		case <-fired:
			if err := e.tick(ctx); err != nil {
				return err
			}
		}
	}
}

// untilNextWake is the shorter of the next book read and the next moment an
// order may print. It is never negative: a wait of zero fires immediately, which
// is correct for work that is already due and would be a spin if anything could
// stay due forever — nothing can, because every evaluation moves readyAt
// forward.
func (e *engine) untilNextWake() time.Duration {
	next := e.nextBookRead
	for _, o := range e.live {
		if o.readyAt.Before(next) {
			next = o.readyAt
		}
	}
	if d := next.Sub(e.now()); d > 0 {
		return d
	}
	return 0
}

// tick refreshes the book if it is due and evaluates every order that is.
func (e *engine) tick(ctx context.Context) error {
	now := e.now()
	if !now.Before(e.nextBookRead) {
		e.readBook(ctx)
	}
	// Evaluation order is by ClOrdID rather than map order, so a sequence of
	// reports is reproducible from the same inputs — determinism under a fixed
	// seed is worth nothing if the order of two simultaneous fills is decided by
	// Go's map iteration.
	for _, o := range e.liveInOrder() {
		if now.Before(o.readyAt) {
			continue
		}
		if err := e.evaluate(ctx, o, now); err != nil {
			return err
		}
	}
	e.m.observeResting(len(e.live))
	return nil
}

func (e *engine) liveInOrder() []*order {
	out := make([]*order, 0, len(e.live))
	for _, o := range e.live {
		out = append(out, o)
	}
	slicesSortByClOrdID(out)
	return out
}

// readBook pulls the latest snapshot and schedules the next read.
//
// A failed read degrades to staleness and never stops the simulator
// (architecture section 8): the previous book stays, its age keeps growing, and
// once it passes the configured maximum the venue refuses orders — which is a
// venue with no market, reported as one.
func (e *engine) readBook(ctx context.Context) {
	e.nextBookRead = e.now().Add(e.fill.BookPoll)

	// Bounded, because this query runs on the engine goroutine — the only
	// reader of the order and cancel channels. An unbounded read against a
	// database that has stopped answering would stop the venue answering FIX at
	// all: orders would block on the handoff, the session would heartbeat out,
	// and the process would look healthy throughout.
	readCtx, cancel := context.WithTimeout(ctx, bookReadTimeout)
	defer cancel()

	latest, ok, err := e.books.LatestBook(readCtx, e.product)
	switch {
	case err != nil:
		if ctx.Err() == nil {
			e.log.Warn("book read failed; pricing against the last one", "error", err,
				"book_age", e.bookAge().String(), "have_book", e.haveBook)
		}
	case !ok:
		if !e.haveBook {
			e.log.Warn("no book snapshot recorded for this product; orders will be refused",
				"product", e.product)
		}
	default:
		e.book = book{ts: latest.TS, bid: latest.BestBid, ask: latest.BestAsk}
		e.haveBook = true
	}
	e.m.observeBookAge(e.bookAge(), e.haveBook)
}

// bookAge is meaningful only when there is a book; haveBook is what says so.
func (e *engine) bookAge() time.Duration {
	if !e.haveBook {
		return 0
	}
	return e.book.age(e.now())
}

// bookFresh reports whether there is a snapshot new enough to price against.
func (e *engine) bookFresh(now time.Time) bool {
	return e.haveBook && e.book.age(now) <= e.fill.BookMaxAge
}

// accept judges one order and either refuses it or puts it on the book.
//
// The validations are the venue's, not the protocol's: a malformed message never
// reaches here, because quickfix answers that with a session-level Reject (35=3)
// from the parse. What is refused here is a well-formed order this venue will
// not take, and the difference is worth keeping — the first is a client that
// cannot speak FIX, the second is a client asking for something real.
func (e *engine) accept(ctx context.Context, s submission) error {
	if reason := e.refusal(s); reason != "" {
		e.m.order(resultRejected)
		e.log.Info("order rejected", "cl_ord_id", s.clOrdID, "reason", reason)
		return e.publish(ctx, report{
			orderID:   e.ids.order(),
			execID:    e.ids.exec(),
			clOrdID:   s.clOrdID,
			symbol:    s.symbol,
			side:      s.side,
			execType:  enum.ExecType_REJECTED,
			ordStatus: enum.OrdStatus_REJECTED,
			orderQty:  s.qty,
			text:      reason,
			at:        s.at,
			session:   s.session,
		})
	}

	o := &order{
		clOrdID: s.clOrdID,
		orderID: e.ids.order(),
		session: s.session,
		symbol:  s.symbol,
		side:    s.side,
		tif:     s.tif,
		qty:     s.qty,
		limitPx: s.limitPx,
		slices:  sliceSizes(s.qty, e.fill.PartialThreshold, e.fill.PartialSlices),
		// The first slice cannot print before the venue has had time to answer.
		readyAt: s.at.Add(jitterLatency(e.rng, e.fill.Latency)),
	}
	e.live[o.clOrdID] = o
	e.m.order(resultAccepted)
	e.m.observeResting(len(e.live))
	e.log.Info("order accepted", "cl_ord_id", o.clOrdID, "order_id", o.orderID,
		"side", string(o.side), "qty", o.qty.String(), "limit_px", o.limitPx.String(),
		"tif", string(o.tif), "slices", len(o.slices))

	return e.publish(ctx, report{
		orderID:   o.orderID,
		execID:    e.ids.exec(),
		clOrdID:   o.clOrdID,
		symbol:    o.symbol,
		side:      o.side,
		execType:  enum.ExecType_NEW,
		ordStatus: enum.OrdStatus_NEW,
		orderQty:  o.qty,
		leavesQty: o.qty,
		at:        s.at,
		session:   o.session,
	})
}

// refusal returns the reason this order cannot be accepted, or "".
//
// The three groups are separate because they fail for different reasons: the id
// is about this session's history, the order is about what the venue trades, and
// the market is about whether there is anything to trade against right now.
func (e *engine) refusal(s submission) string {
	if e.known(s.clOrdID) {
		return rejectDuplicate
	}
	if reason := e.unsupported(s); reason != "" {
		return reason
	}
	if !e.bookFresh(s.at) {
		return rejectNoMarket
	}
	return ""
}

// known reports whether this ClOrdID has been used on this run, live or
// finished. A ClOrdID is the client's handle on its order: reusing one would
// leave two orders it cannot tell apart and a cancel that could address either.
//
// The finished set grows for the life of the process, which is bounded by how
// many orders a simulator is ever asked to take — a handful a day from carry,
// against an id and a status each.
func (e *engine) known(clOrdID string) bool {
	if _, live := e.live[clOrdID]; live {
		return true
	}
	_, done := e.final[clOrdID]
	return done
}

// unsupported returns the reason this venue will not trade the order as asked.
func (e *engine) unsupported(s submission) string {
	switch {
	case s.symbol != e.product:
		return rejectUnknownSymbol
	case s.ordType != enum.OrdType_LIMIT:
		return rejectOrdType
	case s.tif != gtc && s.tif != ioc:
		return rejectTIF
	// Whole contracts only. The perp leg is quantized (spec section 11), and a
	// venue that accepted 1.5 contracts would let a rounding bug upstream look
	// like a working order rather than the invariant violation it is.
	case s.qty.LessThanOrEqual(decimal.Zero), !s.qty.Equal(s.qty.Truncate(0)):
		return rejectQty
	case s.limitPx.LessThanOrEqual(decimal.Zero):
		return rejectPrice
	default:
		return ""
	}
}

// evaluate gives one order its chance against the current book.
func (e *engine) evaluate(ctx context.Context, o *order, now time.Time) error {
	switch {
	case !e.bookFresh(now):
		// Nothing to price against. A resting order waits; an IOC does not, by
		// definition — it was only ever willing to trade now.
		if o.tif == ioc {
			return e.finish(ctx, o, now, "", enum.OrdStatus_CANCELED, cancelNoMarket)
		}
		o.readyAt = e.nextBookRead
		return nil

	case marketable(o.side, o.limitPx, e.book):
		if err := e.printSlice(ctx, o, now); err != nil {
			return err
		}
		if o.done() {
			return nil
		}
		o.readyAt = now.Add(jitterLatency(e.rng, e.fill.Latency))

	default:
		// Deferred to the next book read: until the market moves, the answer
		// cannot change, and re-asking sooner would be a spin.
		o.readyAt = e.nextBookRead
	}

	if o.tif == ioc {
		return e.finish(ctx, o, now, "", enum.OrdStatus_CANCELED, cancelIOCRemainder)
	}
	return nil
}

// printSlice fills the next slice of an order.
func (e *engine) printSlice(ctx context.Context, o *order, now time.Time) error {
	qty := o.slices[o.next]
	px := execPx(o.side, o.limitPx, e.book, e.fill.SlippageBps)

	o.next++
	o.cumQty = o.cumQty.Add(qty)
	o.notional = o.notional.Add(px.Mul(qty))
	o.execSeq++

	status := enum.OrdStatus_PARTIALLY_FILLED
	if o.done() {
		status = enum.OrdStatus_FILLED
		delete(e.live, o.clOrdID)
		e.final[o.clOrdID] = status
		e.m.order(resultFilled)
		e.m.observeResting(len(e.live))
	}

	e.log.Info("fill", "cl_ord_id", o.clOrdID, "order_id", o.orderID,
		"last_qty", qty.String(), "last_px", px.String(),
		"cum_qty", o.cumQty.String(), "leaves_qty", o.leaves().String(),
		"status", string(status))

	return e.publish(ctx, report{
		orderID:   o.orderID,
		execID:    o.execID(),
		clOrdID:   o.clOrdID,
		symbol:    o.symbol,
		side:      o.side,
		execType:  enum.ExecType_TRADE,
		ordStatus: status,
		lastQty:   qty,
		lastPx:    px,
		cumQty:    o.cumQty,
		leavesQty: o.leaves(),
		avgPx:     o.avgPx(),
		orderQty:  o.qty,
		at:        now,
		session:   o.session,
	})
}

// cancelRequested answers an OrderCancelRequest.
func (e *engine) cancelRequested(ctx context.Context, c cancelRequest) error {
	o, live := e.live[c.origClOrdID]
	if !live {
		// Two different refusals, and the difference is the only thing the
		// client can act on: an order this venue never had, versus one it had
		// and has already finished with.
		status, known := e.final[c.origClOrdID]
		reason := enum.CxlRejReason_UNKNOWN_ORDER
		if known {
			reason = enum.CxlRejReason_TOO_LATE_TO_CANCEL
		} else {
			status = enum.OrdStatus_REJECTED
		}
		e.log.Info("cancel rejected", "cl_ord_id", c.clOrdID, "orig_cl_ord_id", c.origClOrdID,
			"reason", string(reason))
		return e.out.sendCancelReject(cancelReject{
			orderID:     unknownOrderID,
			clOrdID:     c.clOrdID,
			origClOrdID: c.origClOrdID,
			ordStatus:   status,
			reason:      reason,
			at:          c.at,
			session:     c.session,
		})
	}
	return e.finish(ctx, o, c.at, c.clOrdID, enum.OrdStatus_CANCELED, cancelRequested)
}

// finish takes an order off the book with a terminal report. clOrdID is the
// cancel request's own id when there was one; an order the venue cancels by
// itself keeps its own.
func (e *engine) finish(ctx context.Context, o *order, at time.Time, clOrdID string,
	status enum.OrdStatus, reason string,
) error {
	delete(e.live, o.clOrdID)
	e.final[o.clOrdID] = status
	e.m.order(resultCanceled)
	e.m.observeResting(len(e.live))

	origClOrdID := ""
	if clOrdID == "" {
		clOrdID = o.clOrdID
	} else {
		origClOrdID = o.clOrdID
	}
	e.log.Info("order canceled", "cl_ord_id", o.clOrdID, "order_id", o.orderID,
		"cum_qty", o.cumQty.String(), "reason", reason)

	o.execSeq++
	return e.publish(ctx, report{
		orderID:     o.orderID,
		execID:      o.execID(),
		clOrdID:     clOrdID,
		origClOrdID: origClOrdID,
		symbol:      o.symbol,
		side:        o.side,
		execType:    enum.ExecType_CANCELED,
		ordStatus:   status,
		cumQty:      o.cumQty,
		// A canceled order leaves nothing: the remainder is gone, not resting.
		leavesQty: decimal.Zero,
		avgPx:     o.avgPx(),
		orderQty:  o.qty,
		text:      reason,
		at:        at,
		session:   o.session,
	})
}

// publish hands a report that moved quantity to the writer, and then sends it.
//
// That order is deliberate. The database is the record of what the venue did; a
// report sent but not recorded is a fill the system cannot account for, while a
// report recorded but not sent is one the FIX resend recovers. Ordering the
// handoff first means the worse of the two failures cannot happen.
//
// It is a handoff, not a durability guarantee, and the difference is worth
// stating because the comment here used to blur it. Sink.Submit enqueues onto
// the one writer goroutine, which flushes on its own batch size and interval —
// so the ExecutionReport can reach the client before the row reaches Postgres,
// and a process killed in between loses the row while the client keeps the fill.
// What this ordering buys is that the row is never *skipped*: it is in the
// queue, and the writer's drain on shutdown is what gets it out. Closing the
// remaining window would need the venue to block on a flush per fill, which is
// not what a simulator should cost; reconciling fills against orders is Part 8's
// job and is where a genuinely lost row would surface.
func (e *engine) publish(ctx context.Context, r report) error {
	if r.lastQty.IsPositive() {
		// Not on the engine's own context, and that is architecture section 8's
		// rule rather than a preference: by the time a fill is being published
		// the order's state has already moved and the report is about to go on
		// the wire, so the row is work in flight, not work being started.
		// Submitted on the root context, a SIGTERM landing mid-tick leaves
		// db.Writer.Submit with two ready cases — the queue and the cancelled
		// context — and Go picks between them at random, so a fill that really
		// happened is dropped about half the time and reported as a fatal write
		// failure. The timeout is what keeps a stuck writer from holding the
		// shutdown open. Found by the Part 7 adversarial review.
		writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), submitTimeout)
		err := e.sink.Submit(writeCtx, r.fillRow())
		cancel()
		if err != nil {
			return fmt.Errorf("record fill %s: %w", r.execID, err)
		}
	}
	if err := e.out.sendReport(r); err != nil {
		// The session is gone in a way quickfix could not queue around. It is
		// not a reason to stop simulating: the venue's own record is already
		// written, and the client's next logon is what recovers it.
		e.log.Warn("execution report not sent", "cl_ord_id", r.clOrdID,
			"exec_id", r.execID, "error", err)
	}
	return nil
}

// unknownOrderID is what tag 37 carries on a reject for an order the venue never
// had. FIX has no null, and an empty string would be a missing required field.
const unknownOrderID = "NONE"

const (
	// submitTimeout bounds the write of one fill during shutdown.
	submitTimeout = 5 * time.Second
	// bookReadTimeout bounds one snapshot read on the engine goroutine.
	bookReadTimeout = 5 * time.Second
)

// idMinter mints the venue's own identifiers.
//
// Both are prefixed with the run, and that is not decoration. fills is keyed on
// (venue, venue_exec_id) with ON CONFLICT DO NOTHING (ADR-0012), so an ExecID
// that restarted from one on every boot would make the second run's first fills
// collide with the first run's and vanish silently. The run is the process start
// in base 36, which is unique across restarts and, under an injected clock,
// identical across test runs.
type idMinter struct {
	run    string
	orders int
	execs  int
}

func newIDMinter(start time.Time) idMinter {
	return idMinter{run: strconv.FormatInt(start.UTC().UnixNano(), 36)}
}

func (m *idMinter) order() string {
	m.orders++
	return fmt.Sprintf("SIMV-%s-%d", m.run, m.orders)
}

// exec mints an id for a report that belongs to no order — the rejection of an
// order that was never accepted.
func (m *idMinter) exec() string {
	m.execs++
	return fmt.Sprintf("SIMV-%s-x%d", m.run, m.execs)
}

// execID is the id of one report of one order, which makes the sequence of a
// single order's reports readable at a glance in the fills table. The order id
// already carries the run, so this inherits its uniqueness across restarts.
func (o *order) execID() string {
	return fmt.Sprintf("%s-%d", o.orderID, o.execSeq)
}
