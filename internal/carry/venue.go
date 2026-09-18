package carry

import (
	"context"
	"time"

	"github.com/shopspring/decimal"
)

// The order-entry contract, exactly as API spec section 3 states it. Every
// venue — FIX to sim-venue, the paper engine, and from week 5 the live Coinbase
// and Base adapters — implements Venue and emits ExecReport into the same state
// machine (internal/exec), which is what makes paper, sim and live accounting
// identical by construction (ADR-0004).

// Namespace prefixes the series cmd/carry owns (API spec section 6).
const Namespace = "carry"

// OrderID is the client-assigned identity of an order: a ULID minted by the
// component that places the order (NewOrderID), never by the venue. It is the
// ClOrdID on the wire, the cl_ord_id on a fills row, and the key every
// ExecReport is matched back on.
type OrderID string

// Leg is which half of the carry an order belongs to.
type Leg string

// The two legs.
const (
	LegSpot Leg = "spot"
	LegPerp Leg = "perp"
)

// Side is the direction of an order.
type Side string

// The two sides.
const (
	Buy  Side = "buy"
	Sell Side = "sell"
)

// OrdType is limit or market. A market order is expressed to every venue as an
// IOC limit at a protected price, so LimitPx is required either way: the
// protection is the caller's, not the venue's.
type OrdType string

// The two order types.
const (
	Limit  OrdType = "limit"
	Market OrdType = "market"
)

// TIF is the time in force.
type TIF string

// The three times in force. ALO (add liquidity only) is the post-only flag;
// not every venue can express it, and one that cannot refuses the order rather
// than quietly working it as GTC.
const (
	GTC TIF = "GTC"
	IOC TIF = "IOC"
	ALO TIF = "ALO"
)

// Order is one instruction to a venue.
type Order struct {
	ClOrdID    OrderID // client-assigned, unique, ULID
	Asset      string  // "ETH"
	Leg        Leg
	Side       Side
	Type       OrdType
	TIF        TIF
	Qty        decimal.Decimal // spot leg: ETH. perp leg: whole contracts (integral)
	LimitPx    decimal.Decimal
	ReduceOnly bool
}

// Ack is the venue's synchronous answer to Submit: the order has been handed to
// the venue. It says nothing about acceptance — that is the first ExecReport.
type Ack struct {
	ClOrdID OrderID
	VenueID string // venue-assigned id, if synchronous
	At      time.Time
}

// ExecState is where an order is in its life. The vocabulary is closed and the
// database checks it (fills.exec_state).
type ExecState string

// The five states. The three terminal ones end the order; nothing follows them.
const (
	StateNew      ExecState = "NEW"
	StatePartial  ExecState = "PARTIAL"
	StateFilled   ExecState = "FILLED"
	StateCanceled ExecState = "CANCELED"
	StateRejected ExecState = "REJECTED"
)

// Terminal reports whether the state ends the order.
func (s ExecState) Terminal() bool {
	switch s {
	case StateFilled, StateCanceled, StateRejected:
		return true
	default:
		return false
	}
}

// ExecReport is one event in an order's life, in the venue's own terms
// translated into the shared model. Every implementation produces these; the
// state machine in internal/exec is the one place they are sequenced.
type ExecReport struct {
	ClOrdID OrderID
	VenueID string
	// ExecID is the venue's id for this report (FIX tag 17), unique per report.
	// It is what makes a replayed report — a FIX resend after a reconnect — a
	// recognisable duplicate rather than a second fill, and it is the
	// fills.venue_exec_id the schema requires of every venue.
	ExecID    string
	State     ExecState
	LastQty   decimal.Decimal // this fill
	LastPx    decimal.Decimal
	CumQty    decimal.Decimal
	LeavesQty decimal.Decimal
	AvgPx     decimal.Decimal
	Fee       decimal.Decimal // venue fee or gas, quote currency
	Reason    string          // reject/cancel reason
	At        time.Time
}

// Venue is what carry needs from an execution path, and nothing more.
//
// Submit hands an order over and returns once the venue has it; Cancel asks for
// an order to be withdrawn; ExecReports is the stream of what happened, which
// the caller drains until the venue closes it. A Submit or Cancel that could
// not reach the venue returns an error and has no effect — a caller that gets
// an error knows nothing was sent.
type Venue interface {
	Submit(ctx context.Context, o Order) (Ack, error)
	Cancel(ctx context.Context, id OrderID) error
	ExecReports() <-chan ExecReport
}
