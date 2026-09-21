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
	// Deadline is SubmittedAt plus the per-order timeout, and it bounds the
	// ACKNOWLEDGEMENT, not the fill: an order the venue has not answered by
	// then is overdue. An order the venue has acknowledged is never overdue,
	// however long it rests — a resting limit order outliving a timeout is the
	// order working, and whether to keep it working is the router's call, not
	// a deadline's. Decided by the PO after the Part 8 review.
	Deadline time.Time
}

// Terminal reports whether the order has ended.
func (s Status) Terminal() bool { return s.State.Terminal() }

// Overdue reports whether the venue has failed to acknowledge the order by its
// deadline. Only a PENDING order can be overdue, and a pending order is always
// one this process submitted: adoption starts a record pending, but Apply
// either moves it on with the report that caused it or removes it again, so an
// adopted order is never left pending to go overdue.
func (s Status) Overdue(now time.Time) bool { return s.State == StatePending && now.After(s.Deadline) }

// Options configure a Tracker.
type Options struct {
	// Venue names the venue this tracker follows, as the metrics label.
	Venue string
	// Timeout is how long the venue has to acknowledge an order, measured from
	// Submit (ORDER_TIMEOUT).
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
	// leg labels this order's two acknowledgement series. An adopted order
	// has none — nothing on the wire carries the leg — and an adopted order
	// is neither acknowledged nor overdue, so neither series needs one.
	leg carry.Leg
	// ackTimedOut records that this order has already been counted as having
	// missed its acknowledgement deadline, so the count is one per order
	// rather than one per tick of the observer.
	ackTimedOut bool
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
	defer func() { t.m.observeOpen(t.countOpen()) }()
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
		leg:  o.Leg,
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
	o.leg = order.Leg
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
	if !known {
		if outcome == OutcomeApplied {
			outcome = OutcomeAdopted
		} else {
			// The adoption is undone when the report that caused it was not
			// applied. adopt has to create the record before the rule table
			// can judge the report, and a record left behind by a refused
			// report is a PENDING order that no report will ever move and
			// nothing can remove: permanently open, permanently overdue
			// against the acknowledgement deadline, and — once Part 15 reads
			// the gauge — a standing instruction to unwind the other leg of
			// an entry for an order that exists at no venue. A PARTIAL that
			// filled nothing reaches this, and so does an OrdStatus the
			// machine has no rule for. Found by the Part 8 follow-up
			// adversarial review; the report is still counted and logged,
			// because refusing it is not forgetting that it arrived.
			delete(t.orders, r.ClOrdID)
		}
	}
	t.m.report(outcome)
	t.record(o.status, r, outcome)
	t.m.observeOpen(t.countOpen())
	return o.status, outcome
}

// Observe refreshes the gauges that need a clock: how many orders are open and
// how many the venue has failed to acknowledge. The instant is an argument for
// the same reason Overdue's is — the tracker has no clock of its own — so the
// binary calls this on a ticker, and once more as it stops so the last scrape
// sees a coherent pair.
//
// What the pair is good for is narrower than it first looks: a rising overdue
// count says acknowledgements are missing the deadline, and a count that rises
// and stays says the venue has stopped answering. It cannot say by how much
// the deadline was missed — a gauge carries no latency, and at a 15 s scrape a
// marginally late acknowledgement is usually invisible — so it can falsify
// ORDER_TIMEOUT but not produce the value to replace it. The Part 8 follow-up
// adversarial review measured that arithmetic against the claim this comment
// used to make. The value itself comes from carry_order_ack_seconds, which
// measures the interval rather than counting threshold crossings (CHANGE-002).
func (t *Tracker) Observe(now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	open, overdue := 0, 0
	for _, o := range t.orders {
		if o.status.Terminal() {
			continue
		}
		open++
		if o.status.Overdue(now) {
			overdue++
			// Nothing in this system "fires" a timeout — the tracker answers
			// a question and the binary asks it on a ticker — so the edge is
			// detected here and counted once. A response that turns up later
			// is still observed at its true latency, so the two series
			// OVERLAP: a late answer is in both, one that never comes is
			// here only, and dividing this into the histogram's count would
			// double-count the late answers (CHANGE-002).
			if !o.ackTimedOut {
				o.ackTimedOut = true
				t.m.ackTimedOut(o.leg)
			}
		}
	}
	t.m.observeOpen(open)
	t.m.observeOverdue(overdue)
}

// countOpen is Observe's open half, for callers already holding the lock.
func (t *Tracker) countOpen() int {
	n := 0
	for _, o := range t.orders {
		if !o.status.Terminal() {
			n++
		}
	}
	return n
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
	// Whether this report is the venue's first answer, read before the
	// transition overwrites the state it is read from.
	wasPending := o.status.State == StatePending
	next, outcome := transition(o.status, r)
	if outcome != OutcomeApplied {
		return outcome
	}
	if r.ExecID != "" {
		o.seen[r.ExecID] = true
	}
	o.status = next
	t.observeTimings(o, wasPending, next, r)
	return OutcomeApplied
}

// observeTimings records what an applied report was worth to the two
// histograms: the round trip if it ended the order, the venue's first response
// if it answered one.
//
// Neither is observed for an ADOPTED order, and that is the same lesson twice.
// Such an order has no acknowledgement — its SubmittedAt is its own first
// report's time — so both intervals would be a fabricated zero, filing a
// multi-second restart recovery into the fastest bucket and making an outage
// read as the fastest venue the system has ever seen. Found by the Part 8
// adversarial review on the round trip; kept out of the acknowledgement
// series by construction. If the submitter catches up later, Track completes
// the order and observes the round trip then.
func (t *Tracker) observeTimings(o *tracked, wasPending bool, next Status, r carry.ExecReport) {
	if o.adopted {
		return
	}
	if next.Terminal() {
		t.m.observeRoundtrip(next.UpdatedAt.Sub(next.SubmittedAt))
	}
	// The venue's first response, measured over exactly the interval
	// ORDER_TIMEOUT races and on exactly the clock it races it on.
	//
	// The edge is the report that takes an order out of PENDING, whether it
	// says NEW, or arrives already filled, or rejects: all three are the venue
	// answering, all three stop the order being overdue, and measuring only a
	// NEW would leave the orders answered fastest out of the distribution.
	//
	// The instants are the Ack we returned from Submit and the moment the
	// report reached this process — both our clock. NOT r.At, which is the
	// venue's TransactTime when it sends one: that excludes inbound network
	// and parse time, which is the tail the deadline exists to catch, and on a
	// resent report it is the original event's time, which can be hours stale.
	// A venue that stamps no ReceivedAt contributes nothing rather than a
	// wrong number. (PO revision to CHANGE-002.)
	if wasPending && next.State != StatePending && !r.ReceivedAt.IsZero() {
		t.m.observeAck(o.leg, r.ReceivedAt.Sub(next.SubmittedAt))
	}
}

// record logs the report at a level matching what it meant. Applied reports
// are the order's life and go at Info; a duplicate is expected after a
// reconnect and is Debug; a stale or late report is a protocol artefact and is
// Warn; an invalid one is an invariant violation and is Error (PO decision
// after the Part 8 review). The three that say the venue disagrees with itself
// all spell the report out, so the disagreement can be read.
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
	case OutcomeInvalid:
		// The one outcome that is not a protocol artefact: the venue's reports
		// disagree with themselves, so the system's belief about its own
		// position is in doubt. The PO decided (after the Part 8 review) that
		// this is an invariant violation — Part 13 wires it to a risk_event
		// and halts submission on the venue until a manual reset. Until then it
		// is the loudest thing this package can say.
		t.log.Error("exec report invalid: the venue's reports disagree with themselves", append(attrs,
			"report_cum_qty", r.CumQty.String(), "report_leaves_qty", r.LeavesQty.String(),
			"order_qty", s.Qty.String())...)
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

// Overdue returns every order the venue has not acknowledged by its deadline,
// oldest first. It is a question, not an action: what to do about one — cancel
// it, unwind the other leg, alert — is the router's and the risk engine's.
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

// Orders are kept for the life of the process, and there is deliberately no way
// to drop one. A resend can replay an order's reports long after it ended, and
// a forgotten order would be adopted back as a new one — the duplicate this
// tracker exists to recognise. The cost is a few hundred bytes per order for a
// system that places a handful a day, which is not memory under pressure; the
// PO decided it that way after the Part 8 review, to be revisited only if the
// order rate changes by orders of magnitude.
