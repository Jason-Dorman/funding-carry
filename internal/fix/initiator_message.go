package fix

import (
	"errors"
	"fmt"
	"time"

	"github.com/quickfixgo/enum"
	"github.com/quickfixgo/field"
	"github.com/quickfixgo/fix44/executionreport"
	"github.com/quickfixgo/fix44/newordersingle"
	"github.com/quickfixgo/fix44/ordercancelrequest"
	"github.com/quickfixgo/quickfix"
	"github.com/quickfixgo/tag"

	"github.com/Jason-Dorman/funding-carry/internal/carry"
)

// The initiator's side of the wire boundary: an Order becomes a
// NewOrderSingle, a cancel becomes an OrderCancelRequest, and an
// ExecutionReport becomes the ExecReport every venue emits (API spec section
// 4.2). Nothing outside this file knows a tag number.

// ErrUnsupportedOrder is returned by Submit for an order this venue cannot
// express, with the reason wrapped in. It is a refusal before any I/O: the
// order was not sent.
var ErrUnsupportedOrder = errors.New("order not expressible over FIX to this venue")

// orderTerms is what a cancel has to repeat about the order it withdraws: FIX
// requires Symbol and Side on an OrderCancelRequest, so the initiator keeps
// them from the submission.
type orderTerms struct {
	symbol string
	side   enum.Side
}

// wireOrder is an Order translated into FIX vocabulary, or the reason it
// cannot be.
type wireOrder struct {
	side    enum.Side
	ordType enum.OrdType
	tif     enum.TimeInForce
}

// translate checks an Order against what this venue speaks and maps its
// enumerations. Every refusal here is a fact about the venue, not about the
// order's merit: a fractional contract count, a leg this session does not
// trade, a time in force FIX 4.4 has no tag for.
//
// Refused rather than adjusted, every time. Rounding a fractional quantity
// would be sizing into more risk than was asked for; working an ALO as a GTC
// would take liquidity the caller said not to take.
func translate(o carry.Order) (wireOrder, error) {
	if o.ClOrdID == "" {
		return wireOrder{}, fmt.Errorf("%w: no ClOrdID", ErrUnsupportedOrder)
	}
	if o.Leg != carry.LegPerp {
		return wireOrder{}, fmt.Errorf("%w: leg %q (this session trades the perp only)", ErrUnsupportedOrder, o.Leg)
	}
	side, err := sideToWire(o.Side)
	if err != nil {
		return wireOrder{}, err
	}
	if err = checkSize(o); err != nil {
		return wireOrder{}, err
	}
	tif, err := timeInForceToWire(o)
	if err != nil {
		return wireOrder{}, err
	}
	// Both order types go on the wire as OrdType=2. A market order is an IOC
	// limit at the caller's protected price (API spec section 3.2): the venue
	// never sees an unpriced order from this system.
	return wireOrder{side: side, ordType: enum.OrdType_LIMIT, tif: tif}, nil
}

func sideToWire(s carry.Side) (enum.Side, error) {
	switch s {
	case carry.Buy:
		return enum.Side_BUY, nil
	case carry.Sell:
		return enum.Side_SELL, nil
	default:
		return "", fmt.Errorf("%w: side %q", ErrUnsupportedOrder, s)
	}
}

// checkSize refuses a size or a price the perp leg cannot carry. The perp leg
// is whole contracts (spec section 11); the venue refuses a fraction too, but
// refusing here means no message, no sequence number and no reject to
// reconcile.
func checkSize(o carry.Order) error {
	if !o.Qty.IsPositive() || !o.Qty.Equal(o.Qty.Truncate(0)) {
		return fmt.Errorf("%w: qty %s is not a positive whole number of contracts", ErrUnsupportedOrder, o.Qty)
	}
	if !o.LimitPx.IsPositive() {
		return fmt.Errorf("%w: limit price %s (a market order is an IOC limit at a protected price, "+
			"so a price is required either way)", ErrUnsupportedOrder, o.LimitPx)
	}
	return nil
}

// timeInForceToWire maps the order type and time in force onto tag 59.
func timeInForceToWire(o carry.Order) (enum.TimeInForce, error) {
	switch o.Type {
	case carry.Limit:
		switch o.TIF {
		case carry.GTC:
			return enum.TimeInForce_GOOD_TILL_CANCEL, nil
		case carry.IOC:
			return enum.TimeInForce_IMMEDIATE_OR_CANCEL, nil
		default:
			// ALO included: FIX 4.4 carries post-only as an ExecInst the
			// simulator does not honour, and an ALO worked as a GTC would take
			// the liquidity it was told not to.
			return "", fmt.Errorf("%w: time in force %q", ErrUnsupportedOrder, o.TIF)
		}
	case carry.Market:
		if o.TIF != "" && o.TIF != carry.IOC {
			return "", fmt.Errorf("%w: a market order is IOC, not %q", ErrUnsupportedOrder, o.TIF)
		}
		return enum.TimeInForce_IMMEDIATE_OR_CANCEL, nil
	default:
		return "", fmt.Errorf("%w: order type %q", ErrUnsupportedOrder, o.Type)
	}
}

// newOrderSingleMessage renders an Order as a NewOrderSingle (35=D).
//
// ReduceOnly is not carried: FIX 4.4 has no standard tag for it and the
// simulator has no position to reduce. The router enforces reduce-only by
// what it asks for, not by a flag the venue might ignore.
func newOrderSingleMessage(o carry.Order, w wireOrder, symbol string, at time.Time) *quickfix.Message {
	nos := newordersingle.New(
		field.NewClOrdID(string(o.ClOrdID)),
		field.NewSide(w.side),
		field.NewTransactTime(at.UTC()),
		field.NewOrdType(w.ordType),
	)
	nos.SetSymbol(symbol)
	nos.SetOrderQty(o.Qty, 0)
	nos.SetPrice(o.LimitPx, wireScale(o.LimitPx))
	nos.SetTimeInForce(w.tif)
	return nos.ToMessage()
}

// cancelRequestMessage renders an OrderCancelRequest (35=F). The request has
// a ClOrdID of its own — a cancel is a message the venue answers by id — and
// names the order it withdraws in OrigClOrdID.
func cancelRequestMessage(clOrdID, orig carry.OrderID, terms orderTerms, at time.Time) *quickfix.Message {
	req := ordercancelrequest.New(
		field.NewOrigClOrdID(string(orig)),
		field.NewClOrdID(string(clOrdID)),
		field.NewSide(terms.side),
		field.NewTransactTime(at.UTC()),
	)
	req.SetSymbol(terms.symbol)
	return req.ToMessage()
}

// parseExecReport reads an ExecutionReport (35=8) into the shared model.
//
// A report the initiator cannot read is answered with a session-level Reject
// naming the tag, the same rule the acceptor applies to orders: a missing
// OrdStatus is a venue that cannot speak FIX, and quickfix's reject is the
// protocol's answer to that. The ClOrdID the report is filed under is the
// order's own — OrigClOrdID when the report answers a cancel — so a cancel's
// acknowledgement lands on the order it withdrew.
func parseExecReport(msg *quickfix.Message, now time.Time) (carry.ExecReport, quickfix.MessageRejectError) {
	er := executionreport.FromMessage(msg)

	r, err := requiredReportFields(er, msg)
	if err != nil {
		return carry.ExecReport{}, err
	}
	r.At = now
	if err := optionalReportFields(er, msg, &r); err != nil {
		return carry.ExecReport{}, err
	}
	return r, nil
}

// requiredReportFields reads the identity, state and totals every report
// carries.
func requiredReportFields(er executionreport.ExecutionReport, msg *quickfix.Message) (carry.ExecReport, quickfix.MessageRejectError) {
	r, err := reportIdentity(er, msg)
	if err != nil {
		return carry.ExecReport{}, err
	}
	ordStatus, err := er.GetOrdStatus()
	if err != nil {
		return carry.ExecReport{}, err
	}
	state, ok := stateFromFIX(ordStatus)
	if !ok {
		return carry.ExecReport{}, quickfix.ValueIsIncorrect(tag.OrdStatus)
	}
	r.State = state
	if r.CumQty, err = er.GetCumQty(); err != nil {
		return carry.ExecReport{}, err
	}
	if r.LeavesQty, err = er.GetLeavesQty(); err != nil {
		return carry.ExecReport{}, err
	}
	if r.AvgPx, err = er.GetAvgPx(); err != nil {
		return carry.ExecReport{}, err
	}
	return r, nil
}

// reportIdentity reads the three ids. The ClOrdID the report is filed under
// is the order's own — OrigClOrdID when the report answers a cancel — so a
// cancel's acknowledgement lands on the order it withdrew.
func reportIdentity(er executionreport.ExecutionReport, msg *quickfix.Message) (carry.ExecReport, quickfix.MessageRejectError) {
	clOrdID, err := er.GetClOrdID()
	if err != nil {
		return carry.ExecReport{}, err
	}
	if msg.Body.Has(tag.OrigClOrdID) {
		if clOrdID, err = er.GetOrigClOrdID(); err != nil {
			return carry.ExecReport{}, err
		}
	}
	venueID, err := er.GetOrderID()
	if err != nil {
		return carry.ExecReport{}, err
	}
	execID, err := er.GetExecID()
	if err != nil {
		return carry.ExecReport{}, err
	}
	return carry.ExecReport{ClOrdID: carry.OrderID(clOrdID), VenueID: venueID, ExecID: execID}, nil
}

// optionalReportFields reads what a report carries only sometimes: a fill's
// quantity and price, a reason, and the venue's own time for the event.
//
// Fee stays zero. The simulator observes no fee tier and sends no Commission
// tag, and a live FIX venue's fee reporting is a Part 21 question; zero is
// "none reported", which the P&L decomposition can tell apart from a fee of
// nothing because the sim's fills rows carry NULL for the same reason.
func optionalReportFields(er executionreport.ExecutionReport, msg *quickfix.Message, r *carry.ExecReport) quickfix.MessageRejectError {
	if msg.Body.Has(tag.LastQty) {
		lastQty, err := er.GetLastQty()
		if err != nil {
			return err
		}
		r.LastQty = lastQty
	}
	if msg.Body.Has(tag.LastPx) {
		lastPx, err := er.GetLastPx()
		if err != nil {
			return err
		}
		r.LastPx = lastPx
	}
	if msg.Body.Has(tag.Text) {
		text, err := er.GetText()
		if err != nil {
			return err
		}
		r.Reason = text
	}
	// The venue's time, when it says: on a resent report it is the time of the
	// original event, which is the one the round trip and the fills row want.
	if msg.Body.Has(tag.TransactTime) {
		at, err := er.GetTransactTime()
		if err != nil {
			return err
		}
		r.At = at.UTC()
	}
	return nil
}

// stateFromFIX maps OrdStatus onto the five states. Anything else — pending
// cancel, expired, suspended — is a status this system has no state for, and
// a report carrying one is refused by tag rather than folded into the nearest
// state it does have.
func stateFromFIX(s enum.OrdStatus) (carry.ExecState, bool) {
	switch s {
	case enum.OrdStatus_NEW:
		return carry.StateNew, true
	case enum.OrdStatus_PARTIALLY_FILLED:
		return carry.StatePartial, true
	case enum.OrdStatus_FILLED:
		return carry.StateFilled, true
	case enum.OrdStatus_CANCELED:
		return carry.StateCanceled, true
	case enum.OrdStatus_REJECTED:
		return carry.StateRejected, true
	default:
		return "", false
	}
}

// cancelRejectDetail reads what an OrderCancelReject (35=9) says, for the log.
// Every field is optional here: the message is being described, not acted
// on, and a reject that cannot be fully read is still worth reporting.
func cancelRejectDetail(msg *quickfix.Message) []any {
	return fields(msg, []taggedField{
		{"cl_ord_id", tag.ClOrdID}, {"orig_cl_ord_id", tag.OrigClOrdID},
		{"order_id", tag.OrderID}, {"ord_status", tag.OrdStatus},
		{"reason", tag.CxlRejReason}, {"text", tag.Text},
	})
}

// taggedField names a tag for a log line.
type taggedField struct {
	name string
	tg   quickfix.Tag
}

// fields reads the tags that are present, as slog attributes.
func fields(msg *quickfix.Message, want []taggedField) []any {
	var attrs []any
	for _, f := range want {
		if v, err := msg.Body.GetString(f.tg); err == nil {
			attrs = append(attrs, f.name, v)
		}
	}
	return attrs
}

// businessRejectDetail reads what a BusinessMessageReject (35=j) says, for the
// log. Like the others here, every field is optional: the message is being
// described, not acted on.
func businessRejectDetail(msg *quickfix.Message) []any {
	return fields(msg, []taggedField{
		{"ref_seq_num", tag.RefSeqNum}, {"ref_msg_type", tag.RefMsgType},
		{"ref_id", tag.BusinessRejectRefID}, {"reason", tag.BusinessRejectReason},
		{"text", tag.Text},
	})
}

// rejectDetail reads what a session-level Reject (35=3) says, for the log:
// which message it refused, which tag, and why. API spec section 4.2 requires
// a Reject to surface as a metric and a log, never to be dropped.
func rejectDetail(msg *quickfix.Message) []any {
	return fields(msg, []taggedField{
		{"ref_seq_num", tag.RefSeqNum}, {"ref_tag_id", tag.RefTagID},
		{"ref_msg_type", tag.RefMsgType}, {"session_reject_reason", tag.SessionRejectReason},
		{"text", tag.Text},
	})
}
