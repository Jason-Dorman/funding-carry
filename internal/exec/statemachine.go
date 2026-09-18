package exec

import (
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/shopspring/decimal"

	"github.com/Jason-Dorman/funding-carry/internal/carry"
)

// The order-state machine: NEW -> PARTIAL* -> FILLED | CANCELED | REJECTED,
// enforced in one place for every venue (API spec section 3.2, ADR-0004).
//
// Reports are sequenced by CumQty, which only ever rises, rather than by the
// order they arrive in: a venue's reports can cross on the wire, and a FIX
// resend after a reconnect replays reports the tracker has already applied.
// Every report is answered with an Outcome saying what the tracker did with
// it, and the outcomes are counted, because "the duplicates were recognised"
// is the evidence that a reconnect lost nothing.

// StatePending is the tracker's own state for an order that has been handed to
// the venue and not yet acknowledged. It never leaves this package: it is not
// one of the five states the schema checks, and no report carries it.
const StatePending carry.ExecState = "PENDING"

// Outcome is what the tracker did with one report.
type Outcome string

// The outcomes. Applied and Adopted moved the order; the rest did not, and each
// says why.
const (
	// OutcomeApplied moved the order to the state the report describes.
	OutcomeApplied Outcome = "applied"
	// OutcomeAdopted applied a report for an order the tracker had not been
	// told about. After a restart every resting order is one of these: the
	// tracker is memory, the venue's book is not, and a report for an order
	// placed by a previous process is a fact about a real order.
	OutcomeAdopted Outcome = "adopted"
	// OutcomeDuplicate is a report already applied: the same ExecID, or the same
	// state at the same CumQty. A FIX resend produces these by design.
	OutcomeDuplicate Outcome = "duplicate"
	// OutcomeStale is a report describing less progress than is already known:
	// a NEW arriving after a partial, or a partial whose CumQty is below the
	// current one. The order stays where it is.
	OutcomeStale Outcome = "stale"
	// OutcomeLate is a non-duplicate report arriving after the order ended.
	OutcomeLate Outcome = "late"
	// OutcomeInvalid is a report the order cannot have produced: more filled
	// than was ordered, a partial that moved nothing, a FILLED with quantity
	// left. The order stays where it is and the report is logged in full,
	// because a venue contradicting itself is the one thing here that is not
	// a protocol artefact.
	OutcomeInvalid Outcome = "invalid"
)

var allOutcomes = []Outcome{
	OutcomeApplied, OutcomeAdopted, OutcomeDuplicate, OutcomeStale, OutcomeLate, OutcomeInvalid,
}

// Status is an order as the tracker knows it.
type Status struct {
	ClOrdID carry.OrderID
	VenueID string
	// Qty is what was ordered, or zero for an adopted order, whose size the
	// tracker was never told.
	Qty       decimal.Decimal
	State     carry.ExecState
	CumQty    decimal.Decimal
	LeavesQty decimal.Decimal
	AvgPx     decimal.Decimal
	Reason    string
	// Adopted reports that this order was never handed to this process: its
	// first report arrived for a ClOrdID the tracker did not know. Qty is then
	// unknown (zero) and SubmittedAt is a report's time, not an
	// acknowledgement's, so neither bounds anything.
	Adopted bool
	// SubmittedAt is when the order was handed to the venue, or the time on the
	// first report for an adopted order.
	SubmittedAt time.Time
	UpdatedAt   time.Time
	// Deadline is SubmittedAt plus the per-order timeout. An order past it and
	// not terminal is overdue: not dead — a resting limit order legitimately
	// outlives any timeout — but something the caller has to look at rather
	// than wait on.
	Deadline time.Time
}

// Terminal reports whether the order has ended.
func (s Status) Terminal() bool { return s.State.Terminal() }

// Overdue reports whether the order is still open past its deadline.
func (s Status) Overdue(now time.Time) bool { return !s.Terminal() && now.After(s.Deadline) }

// Options configure a Tracker.
type Options struct {
	// Venue names the venue this tracker follows, as the metrics label.
	Venue string
	// Timeout is the per-order deadline, measured from Submit.
	Timeout time.Duration
}

// The tracker deliberately has NO clock of its own, and that is what makes it
// reproducible. Every time in a Status comes from outside it: SubmittedAt from
// the caller's Ack, UpdatedAt from the venue's own report. Overdue takes the
// instant to judge against as an argument, so the same reports replayed in a
// backtest produce the same statuses they produced live. An injected clock was
// carried here for one part and read by nothing; it is removed rather than left
// as a seam nobody uses (Part 8 adversarial review).

// ErrAlreadyTracked is returned by Track for a ClOrdID the tracker knows.
var ErrAlreadyTracked = errors.New("order already tracked")

// Tracker is the state machine for one venue's orders.
//
// It is safe for concurrent use: the router submits from its own goroutine
// while reports arrive from the venue's. Everything an order is, is under one
// mutex, and the transition itself is a pure function of the current status
// and the report, so the rules are testable as a table.
type Tracker struct {
	timeout time.Duration
	m       *venueMetrics
	log     *slog.Logger

	mu     sync.Mutex
	orders map[carry.OrderID]*tracked
}

type tracked struct {
	status Status
	// adopted marks an order the tracker was never told about: its Qty is
	// unknown and its SubmittedAt is a report's time rather than an
	// acknowledgement's, so neither the quantity guards nor the round-trip
	// histogram can be applied to it.
	adopted bool
	// seen holds every ExecID applied to this order, which is what makes a
	// replayed report a recognised duplicate rather than a second fill.
	seen map[string]bool
}

// NewTracker builds a tracker for one venue.
func NewTracker(opts Options, m *Metrics, log *slog.Logger) *Tracker {
	return &Tracker{
		timeout: opts.Timeout,
		m:       m.venue(opts.Venue),
		log:     log.With("component", "exec_tracker", "venue", opts.Venue),
		orders:  make(map[carry.OrderID]*tracked),
	}
}

// Track registers an order the caller has just handed to the venue. It is
// called after Submit returned, with the acknowledgement's time as the start
// of the round trip.
//
// An order the tracker has ALREADY ADOPTED is completed rather than refused,
// and that is not a nicety: Submit has to return before Track can be called,
// and the venue's acknowledgement can cross the socket in that gap. The caller
// has no way to close the window — the Ack is what Track needs and only Submit
// can produce it — so the tracker closes it instead. Refusing would have lost
// the ordered quantity and the submit time for an order that is live at the
// venue, and returned an error for something nobody did wrong. Found by the
// Part 8 adversarial review, which reproduced the race.
func (t *Tracker) Track(o carry.Order, ack carry.Ack) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if existing, dup := t.orders[o.ClOrdID]; dup {
		if !existing.adopted {
			return fmt.Errorf("%w: %s", ErrAlreadyTracked, o.ClOrdID)
		}
		t.complete(existing, o, ack)
		return nil
	}
	t.orders[o.ClOrdID] = &tracked{
		status: Status{
			ClOrdID:     o.ClOrdID,
			VenueID:     ack.VenueID,
			Qty:         o.Qty,
			State:       StatePending,
			LeavesQty:   o.Qty,
			SubmittedAt: ack.At,
			UpdatedAt:   ack.At,
			Deadline:    ack.At.Add(t.timeout),
		},
		seen: make(map[string]bool),
	}
	return nil
}

// complete fills in what an adopted order was missing, once the caller that
// placed it catches up.
//
// Only the facts the adoption could not know are written: the ordered quantity
// and the true start of the round trip. What the reports established — state,
// filled quantity, average price — stands, because those came from the venue.
func (t *Tracker) complete(o *tracked, order carry.Order, ack carry.Ack) {
	o.adopted = false
	o.status.Adopted = false
	o.status.Qty = order.Qty
	o.status.SubmittedAt = ack.At
	o.status.Deadline = ack.At.Add(t.timeout)
	if o.status.VenueID == "" {
		o.status.VenueID = ack.VenueID
	}
	if o.status.State == StatePending {
		o.status.LeavesQty = order.Qty
	}
	// A round trip skipped at adoption because its start was unknown is
	// observable now: the order ended, and the acknowledgement it ended from
	// has just arrived.
	if o.status.Terminal() {
		t.m.observeRoundtrip(o.status.UpdatedAt.Sub(o.status.SubmittedAt))
	}
	t.log.Info("adopted order completed by its submitter",
		"cl_ord_id", o.status.ClOrdID, "qty", order.Qty.String(), "state", string(o.status.State))
}

// Apply sequences one report into the order it describes and says what it did.
func (t *Tracker) Apply(r carry.ExecReport) (Status, Outcome) {
	t.mu.Lock()
	defer t.mu.Unlock()

	o, known := t.orders[r.ClOrdID]
	if !known {
		o = t.adopt(r)
	}

	outcome := t.apply(o, r)
	if !known && outcome == OutcomeApplied {
		outcome = OutcomeAdopted
	}
	t.m.report(outcome)
	t.record(o.status, r, outcome)
	return o.status, outcome
}

// adopt opens a record for an order the tracker was never told about. The
// report's own time stands in for the submission, so the deadline is measured
// from the first thing known about the order rather than from an epoch.
func (t *Tracker) adopt(r carry.ExecReport) *tracked {
	o := &tracked{
		adopted: true,
		status: Status{
			ClOrdID:     r.ClOrdID,
			Adopted:     true,
			State:       StatePending,
			SubmittedAt: r.At,
			UpdatedAt:   r.At,
			Deadline:    r.At.Add(t.timeout),
		},
		seen: make(map[string]bool),
	}
	t.orders[r.ClOrdID] = o
	return o
}

func (t *Tracker) apply(o *tracked, r carry.ExecReport) Outcome {
	if r.ExecID != "" && o.seen[r.ExecID] {
		return OutcomeDuplicate
	}
	next, outcome := transition(o.status, r)
	if outcome != OutcomeApplied {
		return outcome
	}
	if r.ExecID != "" {
		o.seen[r.ExecID] = true
	}
	o.status = next
	// An adopted order has no acknowledgement, so it has no round trip. Its
	// SubmittedAt is the first report's own time, and when that first report is
	// the terminal one — the restart-and-resend path, which is exactly what
	// adoption is for — the subtraction below is zero. Observing it would file
	// a multi-second recovery into the fastest bucket in the histogram and make
	// an outage read as a fast venue. If the submitter catches up later, Track
	// completes the order and observes the round trip then. Found by the Part 8
	// adversarial review.
	if next.Terminal() && !o.adopted {
		t.m.observeRoundtrip(next.UpdatedAt.Sub(next.SubmittedAt))
	}
	return OutcomeApplied
}

// record logs the report at a level matching what it meant. Applied reports
// are the order's life and go at Info; a duplicate is expected after a
// reconnect and is Debug; the three that say the venue disagrees with itself
// are Warn, with the report spelled out so the disagreement can be read.
func (t *Tracker) record(s Status, r carry.ExecReport, outcome Outcome) {
	attrs := []any{
		"cl_ord_id", r.ClOrdID, "exec_id", r.ExecID, "outcome", string(outcome),
		"report_state", string(r.State), "state", string(s.State),
		"cum_qty", s.CumQty.String(), "leaves_qty", s.LeavesQty.String(),
	}
	switch outcome {
	case OutcomeApplied, OutcomeAdopted:
		if r.LastQty.IsPositive() {
			attrs = append(attrs, "last_qty", r.LastQty.String(), "last_px", r.LastPx.String())
		}
		if r.Reason != "" {
			attrs = append(attrs, "reason", r.Reason)
		}
		t.log.Info("exec report", attrs...)
	case OutcomeDuplicate:
		t.log.Debug("exec report repeated", attrs...)
	default:
		t.log.Warn("exec report not applied", append(attrs,
			"report_cum_qty", r.CumQty.String(), "report_leaves_qty", r.LeavesQty.String(),
			"order_qty", s.Qty.String())...)
	}
}

// transition is the rule table: given where an order is and what a report
// says, where does it go and why. It touches nothing but its arguments.
func transition(cur Status, r carry.ExecReport) (Status, Outcome) {
	if cur.Terminal() {
		if r.State == cur.State && r.CumQty.Equal(cur.CumQty) {
			return cur, OutcomeDuplicate
		}
		return cur, OutcomeLate
	}

	switch r.State {
	case carry.StateRejected, carry.StateCanceled:
		return closedOut(cur, r)
	case carry.StateNew:
		return acknowledged(cur, r)
	case carry.StatePartial, carry.StateFilled:
		return fill(cur, r)
	default:
		return cur, OutcomeInvalid
	}
}

// closedOut sequences the two reports that end an order without filling it: a
// CANCELED and a REJECTED. Both state the quantity filled before they took
// effect, which cannot be less than what was already reported filled, nor more
// than was ordered.
//
// A REJECTED used to skip these checks entirely, so a venue could drive an
// order terminal with any quantity at all while the identical numbers on a
// CANCELED were refused as invalid — in the one layer whose documented job is
// catching a venue whose reports disagree with themselves. Found by the Part 8
// adversarial review.
func closedOut(cur Status, r carry.ExecReport) (Status, Outcome) {
	if r.CumQty.LessThan(cur.CumQty) {
		return cur, OutcomeInvalid
	}
	if !cur.Qty.IsZero() && r.CumQty.GreaterThan(cur.Qty) {
		return cur, OutcomeInvalid
	}
	return ended(cur, r), OutcomeApplied
}

// acknowledged sequences a NEW report: it moves a pending order and nothing
// else. A NEW after a partial is the acknowledgement arriving after the fill
// it acknowledged.
func acknowledged(cur Status, r carry.ExecReport) (Status, Outcome) {
	switch cur.State {
	case StatePending:
		return progressed(cur, r), OutcomeApplied
	case carry.StateNew:
		return cur, OutcomeDuplicate
	default:
		return cur, OutcomeStale
	}
}

// fill sequences a PARTIAL or FILLED report by CumQty.
func fill(cur Status, r carry.ExecReport) (Status, Outcome) {
	switch cmp := r.CumQty.Cmp(cur.CumQty); {
	case cmp < 0:
		return cur, OutcomeStale
	case cmp == 0 && r.State == cur.State:
		return cur, OutcomeDuplicate
	case cmp == 0 && r.State == carry.StatePartial:
		// A partial that moved nothing is not a partial. (A FILLED at the same
		// CumQty is allowed: the last partial may already have reached the
		// order's quantity, and the FILLED is the venue closing it.)
		return cur, OutcomeInvalid
	}
	if !cur.Qty.IsZero() && r.CumQty.GreaterThan(cur.Qty) {
		return cur, OutcomeInvalid
	}
	if r.State == carry.StateFilled && !r.LeavesQty.IsZero() {
		return cur, OutcomeInvalid
	}
	return progressed(cur, r), OutcomeApplied
}

// progressed is the order after a report that moved it forward.
func progressed(cur Status, r carry.ExecReport) Status {
	cur.State = r.State
	cur.CumQty = r.CumQty
	cur.LeavesQty = r.LeavesQty
	cur.AvgPx = r.AvgPx
	cur.UpdatedAt = r.At
	if r.VenueID != "" {
		cur.VenueID = r.VenueID
	}
	return cur
}

// ended is the order after a report that closed it without a fill: a reject or
// a cancel. The reason travels, and nothing is left working.
//
// It carries no clamp against the quantity going backwards, and that is a
// deliberate deletion rather than an omission. One used to sit here, and once
// closedOut began bounding REJECTED as well as CANCELED — the Part 8
// adversarial review's finding — it became unreachable: every terminal report
// that would lower the filled quantity is now refused as invalid before it gets
// this far. Defensive code no test can reach is code no one can trust; the
// guarantee lives in closedOut, where a test can see it fail.
func ended(cur Status, r carry.ExecReport) Status {
	cur = progressed(cur, r)
	cur.LeavesQty = decimal.Zero
	cur.Reason = r.Reason
	return cur
}

// Status returns one order, if the tracker knows it.
func (t *Tracker) Status(id carry.OrderID) (Status, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	o, ok := t.orders[id]
	if !ok {
		return Status{}, false
	}
	return o.status, true
}

// Open returns every order that has not ended, oldest first.
func (t *Tracker) Open() []Status {
	return t.filter(func(s Status) bool { return !s.Terminal() })
}

// Overdue returns every open order past its deadline, oldest first. It is a
// question, not an action: what to do about an overdue order — cancel it,
// unwind the other leg, alert — is the router's and the risk engine's.
func (t *Tracker) Overdue(now time.Time) []Status {
	return t.filter(func(s Status) bool { return s.Overdue(now) })
}

func (t *Tracker) filter(keep func(Status) bool) []Status {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []Status
	for _, o := range t.orders {
		if keep(o.status) {
			out = append(out, o.status)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].SubmittedAt.Equal(out[j].SubmittedAt) {
			return out[i].SubmittedAt.Before(out[j].SubmittedAt)
		}
		return out[i].ClOrdID < out[j].ClOrdID
	})
	return out
}

// Forget drops an order the caller has finished with. Terminal orders are
// otherwise kept, because a resend can replay their reports long after they
// ended and a forgotten order would be adopted back as a new one.
//
// Nothing calls this yet, and the retention policy it implements one half of is
// an OPEN ITEM, not a decision: the map only grows, one Tracker lives for the
// life of the carry process, and Part 15's router submits continuously. How
// long an ended order must stay remembered is a trade between the resend window
// it protects and unbounded memory, and neither the plan nor the spec says.
// Named for the PO at Part 8 rather than answered here (build-plan changelog).
func (t *Tracker) Forget(id carry.OrderID) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.orders, id)
}
