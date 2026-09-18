package fix

import (
	"sort"
	"strings"
	"time"

	"github.com/quickfixgo/enum"
	"github.com/quickfixgo/field"
	"github.com/quickfixgo/fix44/executionreport"
	"github.com/quickfixgo/fix44/newordersingle"
	"github.com/quickfixgo/fix44/ordercancelreject"
	"github.com/quickfixgo/fix44/ordercancelrequest"
	"github.com/quickfixgo/quickfix"
	"github.com/quickfixgo/tag"
	"github.com/shopspring/decimal"

	"github.com/Jason-Dorman/funding-carry/internal/db"
)

// The wire boundary: FIX 4.4 messages in, FIX 4.4 messages out, and the one
// place a report becomes a fills row.
//
// Every tag here is in the dictionary in API spec section 4.2, and the mapping
// runs one way only — nothing downstream of this file knows a tag number, and
// nothing upstream of it knows what a fills row looks like.

// report is one ExecutionReport in the simulator's own terms. It is rendered
// twice, to FIX and to the database, and both renderings live in this file so
// the two accounts of the same event cannot drift apart.
type report struct {
	orderID     string
	execID      string
	clOrdID     string
	origClOrdID string
	symbol      string
	side        side
	execType    enum.ExecType
	ordStatus   enum.OrdStatus
	lastQty     decimal.Decimal
	lastPx      decimal.Decimal
	cumQty      decimal.Decimal
	leavesQty   decimal.Decimal
	avgPx       decimal.Decimal
	orderQty    decimal.Decimal
	text        string
	at          time.Time
	session     quickfix.SessionID
}

// cancelReject is an OrderCancelReject (35=9).
type cancelReject struct {
	orderID     string
	clOrdID     string
	origClOrdID string
	ordStatus   enum.OrdStatus
	reason      enum.CxlRejReason
	at          time.Time
	session     quickfix.SessionID
}

// parseOrder reads a NewOrderSingle.
//
// What it refuses is protocol, not policy: a missing required tag or a value
// outside the FIX enumeration is answered with a session-level Reject (35=3),
// which is what tag 373 exists for. Whether this venue will *take* a well-formed
// order — the symbol, the size, the time in force — is the engine's judgement,
// and it is answered with an ExecutionReport so the client sees it against its
// own order id.
func parseOrder(msg *quickfix.Message, sessionID quickfix.SessionID, at time.Time) (submission, quickfix.MessageRejectError) {
	nos := newordersingle.FromMessage(msg)

	clOrdID, err := nos.GetClOrdID()
	if err != nil {
		return submission{}, err
	}
	symbol, err := nos.GetSymbol()
	if err != nil {
		return submission{}, err
	}
	rawSide, err := nos.GetSide()
	if err != nil {
		return submission{}, err
	}
	s, ok := sideFromFIX(rawSide)
	if !ok {
		return submission{}, quickfix.ValueIsIncorrect(tag.Side)
	}
	qty, err := nos.GetOrderQty()
	if err != nil {
		return submission{}, err
	}
	ordType, err := nos.GetOrdType()
	if err != nil {
		return submission{}, err
	}

	px, rawTIF, err := optionalTerms(nos, msg)
	if err != nil {
		return submission{}, err
	}

	return submission{
		clOrdID: clOrdID,
		symbol:  symbol,
		side:    s,
		tif:     tifFromFIX(rawTIF),
		ordType: ordType,
		qty:     qty,
		limitPx: px,
		session: sessionID,
		at:      at,
	}, nil
}

// parseCancel reads an OrderCancelRequest.
func parseCancel(msg *quickfix.Message, sessionID quickfix.SessionID, at time.Time) (cancelRequest, quickfix.MessageRejectError) {
	req := ordercancelrequest.FromMessage(msg)

	clOrdID, err := req.GetClOrdID()
	if err != nil {
		return cancelRequest{}, err
	}
	origClOrdID, err := req.GetOrigClOrdID()
	if err != nil {
		return cancelRequest{}, err
	}
	return cancelRequest{
		clOrdID:     clOrdID,
		origClOrdID: origClOrdID,
		session:     sessionID,
		at:          at,
	}, nil
}

// optionalTerms reads the two fields FIX 4.4 makes conditionally required: a
// market order carries no price, and an absent TimeInForce means Day.
//
// Reading them as optional and letting the engine refuse what this venue does
// not support keeps "you did not say" and "you asked for something we do not do"
// as two different answers — the first is a protocol reject naming a tag, the
// second an ExecutionReport the client can match to its own order.
func optionalTerms(nos newordersingle.NewOrderSingle, msg *quickfix.Message) (
	decimal.Decimal, enum.TimeInForce, quickfix.MessageRejectError,
) {
	px := decimal.Zero
	if msg.Body.Has(tag.Price) {
		var err quickfix.MessageRejectError
		if px, err = nos.GetPrice(); err != nil {
			return decimal.Zero, "", err
		}
	}
	tif := enum.TimeInForce_DAY
	if msg.Body.Has(tag.TimeInForce) {
		var err quickfix.MessageRejectError
		if tif, err = nos.GetTimeInForce(); err != nil {
			return decimal.Zero, "", err
		}
	}
	return px, tif, nil
}

func sideFromFIX(s enum.Side) (side, bool) {
	switch s {
	case enum.Side_BUY:
		return buy, true
	case enum.Side_SELL:
		return sell, true
	default:
		return "", false
	}
}

func sideToFIX(s side) enum.Side {
	if s == buy {
		return enum.Side_BUY
	}
	return enum.Side_SELL
}

// tifFromFIX maps only what the simulator honours. Anything else comes back
// empty and is refused by name at entry, rather than being quietly treated as
// the one the venue happens to implement.
func tifFromFIX(t enum.TimeInForce) tif {
	switch t {
	case enum.TimeInForce_GOOD_TILL_CANCEL:
		return gtc
	case enum.TimeInForce_IMMEDIATE_OR_CANCEL:
		return ioc
	default:
		return ""
	}
}

// executionReport renders a report as FIX 4.4 (API spec section 4.2).
func executionReportMessage(r report) *quickfix.Message {
	m := executionreport.New(
		field.NewOrderID(r.orderID),
		field.NewExecID(r.execID),
		field.NewExecType(r.execType),
		field.NewOrdStatus(r.ordStatus),
		field.NewSide(sideToFIX(r.side)),
		field.NewLeavesQty(r.leavesQty, wireScale(r.leavesQty)),
		field.NewCumQty(r.cumQty, wireScale(r.cumQty)),
		field.NewAvgPx(r.avgPx, wireScale(r.avgPx)),
	)
	m.SetClOrdID(r.clOrdID)
	m.SetSymbol(r.symbol)
	m.SetOrderQty(r.orderQty, wireScale(r.orderQty))
	m.SetTransactTime(r.at.UTC())

	if r.origClOrdID != "" {
		m.SetOrigClOrdID(r.origClOrdID)
	}
	// LastQty and LastPx describe this fill, so they appear only on a report
	// that was one. A zero on a NEW would be a fill of nothing at a price of
	// nothing, which is a different statement from silence.
	if r.lastQty.IsPositive() {
		m.SetLastQty(r.lastQty, wireScale(r.lastQty))
		m.SetLastPx(r.lastPx, wireScale(r.lastPx))
	}
	if r.text != "" {
		m.SetText(r.text)
	}
	return m.ToMessage()
}

// cancelRejectMessage renders an OrderCancelReject (35=9).
func cancelRejectMessage(r cancelReject) *quickfix.Message {
	m := ordercancelreject.New(
		field.NewOrderID(r.orderID),
		field.NewClOrdID(r.clOrdID),
		field.NewOrigClOrdID(r.origClOrdID),
		field.NewOrdStatus(r.ordStatus),
		field.NewCxlRejResponseTo(enum.CxlRejResponseTo_ORDER_CANCEL_REQUEST),
	)
	m.SetCxlRejReason(r.reason)
	m.SetTransactTime(r.at.UTC())
	return m.ToMessage()
}

// wireScale is how many digits after the point a value goes out with.
//
// It is counted from String rather than taken from Exponent, and the difference
// is not cosmetic. quickfixgo writes a decimal field with StringFixed(scale), and
// shopspring returns every quotient at DivisionPrecision whatever the value is —
// so an average price of exactly 2345.47 comes back from Div with exponent -16
// and would go on the wire as 2345.4700000000000000. String is what drops the
// trailing zeros, so the scale is read off it.
func wireScale(d decimal.Decimal) int32 {
	s := d.String()
	i := strings.IndexByte(s, '.')
	if i < 0 {
		return 0
	}
	// Clamped rather than converted blind. A scale is a small number for any
	// value that came from a market, but the input is a decimal of arbitrary
	// precision and a conversion that could wrap would put a negative scale into
	// StringFixed.
	digits := len(s) - i - 1
	if digits > maxWireScale {
		return maxWireScale
	}
	return int32(digits) //nolint:gosec // bounded by maxWireScale on the line above, and non-negative by construction
}

// maxWireScale is well past anything a price or a size carries — the widest here
// is a quotient at DivisionPrecision, which is sixteen — and short of where the
// conversion above could misbehave.
const maxWireScale = 32

// fillRow is the report as the fills table records it (API spec section 5.2).
//
// Only a report that moved quantity becomes a row — the table is fills, not
// events — and venue_exec_id is the ExecID, which is what makes replaying a
// report after a resend insert nothing new.
func (r report) fillRow() db.FillRow {
	return db.FillRow{
		TS:      r.at.UTC(),
		ClOrdID: r.clOrdID,
		Venue:   db.VenueSim,
		// The simulator makes a market in the perp and nothing else: the symbol
		// is checked against the configured product at entry, so a row here can
		// only be the perp leg.
		Leg:       db.LegPerp,
		Side:      string(r.side),
		Qty:       r.lastQty,
		Px:        r.lastPx,
		ExecState: execState(r.ordStatus),
		// Fee is left NULL rather than modelled. The venue's fee tier is not
		// something this simulator observes (cb_products carries NULLs for it
		// until the authenticated endpoint is read), and a fabricated fee would
		// flow straight into the P&L decomposition as though it had been
		// charged. NULL is "not observed", which is the truth.
		VenueExecID: r.execID,
		Raw:         r.raw(),
	}
}

// execState maps FIX OrdStatus onto the fills.exec_state vocabulary the schema
// checks (NEW | PARTIAL | FILLED | CANCELED | REJECTED).
func execState(s enum.OrdStatus) string {
	switch s {
	case enum.OrdStatus_PARTIALLY_FILLED:
		return "PARTIAL"
	case enum.OrdStatus_FILLED:
		return "FILLED"
	case enum.OrdStatus_CANCELED:
		return "CANCELED"
	case enum.OrdStatus_REJECTED:
		return "REJECTED"
	default:
		return "NEW"
	}
}

// raw is the venue payload the fills row keeps, as the decimals-are-strings
// document jsonb requires (ADR-0011). It is the report's own fields rather than
// the FIX message text: the message is already in the FileLog verbatim, and what
// is useful in a query is the numbers.
func (r report) raw() db.Snapshot {
	snap, err := db.EncodeSnapshot(map[string]any{
		"order_id":   r.orderID,
		"exec_id":    r.execID,
		"cl_ord_id":  r.clOrdID,
		"symbol":     r.symbol,
		"exec_type":  string(r.execType),
		"ord_status": string(r.ordStatus),
		"last_qty":   r.lastQty,
		"last_px":    r.lastPx,
		"cum_qty":    r.cumQty,
		"leaves_qty": r.leavesQty,
		"avg_px":     r.avgPx,
		"order_qty":  r.orderQty,
	})
	if err != nil {
		// EncodeSnapshot fails only when the decimal JSON global has been moved,
		// which is a broken invariant rather than a bad input. The fill itself
		// still has to be recorded, so the row keeps its columns and loses only
		// the payload.
		return nil
	}
	return snap
}

// slicesSortByClOrdID puts the live orders in a stable order for evaluation, so
// two orders that become fillable on the same tick always report in the same
// sequence.
func slicesSortByClOrdID(o []*order) {
	sort.Slice(o, func(i, j int) bool { return o[i].clOrdID < o[j].clOrdID })
}
