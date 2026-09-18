package fix

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/quickfixgo/enum"
	"github.com/quickfixgo/field"
	"github.com/quickfixgo/fix44/newordersingle"
	"github.com/quickfixgo/fix44/ordercancelrequest"
	"github.com/quickfixgo/quickfix"
	"github.com/quickfixgo/tag"
	"github.com/shopspring/decimal"

	"github.com/Jason-Dorman/funding-carry/internal/db"
)

// The wire boundary, both directions: what arrives becomes a submission, and a
// report becomes exactly the tags API spec section 4.2 lists.

// testNOS builds a well-formed NewOrderSingle and lets a test break one thing
// about it.
func testNOS(mutate func(newordersingle.NewOrderSingle, *quickfix.Message)) *quickfix.Message {
	nos := newordersingle.New(
		field.NewClOrdID("A1"),
		field.NewSide(enum.Side_BUY),
		field.NewTransactTime(epoch),
		field.NewOrdType(enum.OrdType_LIMIT),
	)
	nos.SetSymbol(testPerp)
	nos.SetOrderQty(dec("3"), 0)
	nos.SetPrice(dec("2345.50"), 2)
	nos.SetTimeInForce(enum.TimeInForce_GOOD_TILL_CANCEL)

	msg := nos.ToMessage()
	if mutate != nil {
		mutate(nos, msg)
	}
	return msg
}

func TestParseOrderReadsTheDictionary(t *testing.T) {
	t.Parallel()

	got, err := parseOrder(testNOS(nil), quickfix.SessionID{}, epoch)
	if err != nil {
		t.Fatalf("a well-formed order was rejected: %v", err)
	}

	if got.clOrdID != "A1" || got.symbol != testPerp {
		t.Errorf("cl_ord_id %q symbol %q", got.clOrdID, got.symbol)
	}
	if got.side != buy || got.tif != gtc || got.ordType != enum.OrdType_LIMIT {
		t.Errorf("side %q tif %q ordType %q", got.side, got.tif, got.ordType)
	}
	if !got.qty.Equal(dec("3")) || !got.limitPx.Equal(dec("2345.50")) {
		t.Errorf("qty %s px %s", got.qty, got.limitPx)
	}
	if !got.at.Equal(epoch) {
		t.Errorf("at %s, want the acceptor's own reading of the clock", got.at)
	}
}

// What the parse refuses is protocol, not policy: a message that cannot be read
// is a session-level Reject (35=3), which is what tag 373 exists for. Everything
// this venue merely declines to trade is the engine's business, and is answered
// with an ExecutionReport instead.
func TestParseOrderRejectsWhatIsNotFIX(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		mutate  func(newordersingle.NewOrderSingle, *quickfix.Message)
		wantTag quickfix.Tag
	}{
		{"no ClOrdID", func(_ newordersingle.NewOrderSingle, m *quickfix.Message) {
			m.Body.Remove(tag.ClOrdID)
		}, tag.ClOrdID},
		{"no Symbol", func(_ newordersingle.NewOrderSingle, m *quickfix.Message) {
			m.Body.Remove(tag.Symbol)
		}, tag.Symbol},
		{"no OrderQty", func(_ newordersingle.NewOrderSingle, m *quickfix.Message) {
			m.Body.Remove(tag.OrderQty)
		}, tag.OrderQty},
		{"a Side outside the enumeration", func(_ newordersingle.NewOrderSingle, m *quickfix.Message) {
			m.Body.SetString(tag.Side, "9")
		}, tag.Side},
		{"a price that is not a number", func(_ newordersingle.NewOrderSingle, m *quickfix.Message) {
			m.Body.SetString(tag.Price, "not-a-price")
		}, tag.Price},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := parseOrder(testNOS(tc.mutate), quickfix.SessionID{}, epoch)
			if err == nil {
				t.Fatal("accepted a message that cannot be read as FIX")
			}
			if got := err.RefTagID(); got == nil || *got != tc.wantTag {
				t.Errorf("reject names tag %v, want %d", got, tc.wantTag)
			}
		})
	}
}

// Price and TimeInForce are conditionally required in FIX 4.4: a market order
// carries no price, and an absent TIF means Day. Reading them as optional keeps
// "you did not say" separate from "we do not do that" — the second is a
// rejection with a reason on it.
func TestParseOrderTreatsPriceAndTIFAsOptional(t *testing.T) {
	t.Parallel()

	msg := testNOS(func(_ newordersingle.NewOrderSingle, m *quickfix.Message) {
		m.Body.Remove(tag.Price)
		m.Body.Remove(tag.TimeInForce)
	})
	got, err := parseOrder(msg, quickfix.SessionID{}, epoch)
	if err != nil {
		t.Fatalf("an order without a price or a TIF was rejected at the parse: %v", err)
	}
	if !got.limitPx.IsZero() {
		t.Errorf("limit px %s, want zero so the engine refuses it by name", got.limitPx)
	}
	if got.tif != "" {
		t.Errorf("tif %q, want empty: Day is not a TIF this venue honours", got.tif)
	}
}

func TestParseCancel(t *testing.T) {
	t.Parallel()

	req := ordercancelrequest.New(
		field.NewOrigClOrdID("A1"),
		field.NewClOrdID("C1"),
		field.NewSide(enum.Side_BUY),
		field.NewTransactTime(epoch),
	)
	req.SetSymbol(testPerp)

	got, err := parseCancel(req.ToMessage(), quickfix.SessionID{}, epoch)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.clOrdID != "C1" || got.origClOrdID != "A1" {
		t.Errorf("cancel is %q against %q, want C1 against A1", got.clOrdID, got.origClOrdID)
	}
}

// The ExecutionReport carries the tags in API spec section 4.2 and, just as
// importantly, does not carry the ones that would be a statement of their own: a
// LastQty of zero on an acknowledgement is a fill of nothing at a price of
// nothing, which is not the same as silence.
func TestExecutionReportTags(t *testing.T) {
	t.Parallel()

	ack := executionReportMessage(report{
		orderID: "SIMV-1", execID: "SIMV-1-0", clOrdID: "A1", symbol: testPerp,
		side: buy, execType: enum.ExecType_NEW, ordStatus: enum.OrdStatus_NEW,
		orderQty: dec("3"), leavesQty: dec("3"), at: epoch,
	})
	assertField(t, ack, tag.MsgType, "8")
	assertField(t, ack, tag.OrderID, "SIMV-1")
	assertField(t, ack, tag.ExecID, "SIMV-1-0")
	assertField(t, ack, tag.ClOrdID, "A1")
	assertField(t, ack, tag.Symbol, testPerp)
	assertField(t, ack, tag.Side, "1")
	assertField(t, ack, tag.ExecType, "0")
	assertField(t, ack, tag.OrdStatus, "0")
	assertField(t, ack, tag.OrderQty, "3")
	assertField(t, ack, tag.LeavesQty, "3")
	assertField(t, ack, tag.CumQty, "0")
	assertField(t, ack, tag.TransactTime, "20260915-12:00:00.000")
	assertAbsent(t, ack, tag.LastQty)
	assertAbsent(t, ack, tag.LastPx)
	assertAbsent(t, ack, tag.Text)
	assertAbsent(t, ack, tag.OrigClOrdID)

	fill := executionReportMessage(report{
		orderID: "SIMV-1", execID: "SIMV-1-1", clOrdID: "A1", symbol: testPerp,
		side: sell, execType: enum.ExecType_TRADE, ordStatus: enum.OrdStatus_PARTIALLY_FILLED,
		lastQty: dec("11"), lastPx: dec("2344.031"), cumQty: dec("11"), leavesQty: dec("24"),
		avgPx: dec("2344.031"), orderQty: dec("35"), at: epoch,
	})
	assertField(t, fill, tag.ExecType, "F")
	assertField(t, fill, tag.OrdStatus, "1")
	assertField(t, fill, tag.LastQty, "11")
	assertField(t, fill, tag.LastPx, "2344.031")
	assertField(t, fill, tag.AvgPx, "2344.031")

	canceled := executionReportMessage(report{
		orderID: "SIMV-1", execID: "SIMV-1-2", clOrdID: "C1", origClOrdID: "A1",
		symbol: testPerp, side: buy, execType: enum.ExecType_CANCELED,
		ordStatus: enum.OrdStatus_CANCELED, cumQty: dec("11"), leavesQty: decimal.Zero,
		orderQty: dec("35"), text: cancelRequested, at: epoch,
	})
	assertField(t, canceled, tag.ExecType, "4")
	assertField(t, canceled, tag.OrigClOrdID, "A1")
	assertField(t, canceled, tag.Text, cancelRequested)
}

func TestCancelRejectTags(t *testing.T) {
	t.Parallel()

	msg := cancelRejectMessage(cancelReject{
		orderID: unknownOrderID, clOrdID: "C1", origClOrdID: "A1",
		ordStatus: enum.OrdStatus_REJECTED, reason: enum.CxlRejReason_UNKNOWN_ORDER, at: epoch,
	})
	assertField(t, msg, tag.MsgType, "9")
	assertField(t, msg, tag.OrderID, unknownOrderID)
	assertField(t, msg, tag.ClOrdID, "C1")
	assertField(t, msg, tag.OrigClOrdID, "A1")
	assertField(t, msg, tag.CxlRejReason, "1")
	assertField(t, msg, tag.CxlRejResponseTo, "1")
}

// quickfixgo writes a decimal field with StringFixed(scale), and shopspring
// returns every quotient at DivisionPrecision whatever the value is. Taking the
// scale from Exponent would put an average price of exactly 2345.47 on the wire
// as 2345.4700000000000000.
func TestWireScaleComesFromTheValueNotTheExponent(t *testing.T) {
	t.Parallel()

	avg := dec("7036.41").Div(dec("3"))
	if avg.Exponent() != -16 {
		t.Fatalf("this test is about a quotient carrying DivisionPrecision; exponent is %d", avg.Exponent())
	}
	if got := wireScale(avg); got != 2 {
		t.Errorf("wireScale = %d, want 2", got)
	}

	msg := executionReportMessage(report{avgPx: avg, leavesQty: decimal.Zero, cumQty: dec("3")})
	assertField(t, msg, tag.AvgPx, "2345.47")

	for _, tc := range []struct {
		in   string
		want int32
	}{{"3", 0}, {"2345.5", 1}, {"0.0001", 4}} {
		if got := wireScale(dec(tc.in)); got != tc.want {
			t.Errorf("wireScale(%s) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// One report, two renderings. The fills row is the venue's own record of what it
// did, and venue_exec_id is the ExecID — which is what makes replaying a report
// after a resend insert nothing new (ADR-0012).
func TestFillRowMapping(t *testing.T) {
	t.Parallel()

	row := report{
		orderID: "SIMV-1", execID: "SIMV-1-1", clOrdID: "A1", symbol: testPerp,
		side: sell, execType: enum.ExecType_TRADE, ordStatus: enum.OrdStatus_PARTIALLY_FILLED,
		lastQty: dec("11"), lastPx: dec("2344.031"), cumQty: dec("11"), leavesQty: dec("24"),
		avgPx: dec("2344.031"), orderQty: dec("35"), at: epoch,
	}.fillRow()

	if row.Venue != db.VenueSim || row.Leg != db.LegPerp || row.Side != db.SideSell {
		t.Errorf("row is %s/%s/%s", row.Venue, row.Leg, row.Side)
	}
	if row.ExecState != "PARTIAL" || row.VenueExecID != "SIMV-1-1" {
		t.Errorf("exec_state %s exec_id %s", row.ExecState, row.VenueExecID)
	}
	if !row.Qty.Equal(dec("11")) || !row.Px.Equal(dec("2344.031")) {
		t.Errorf("qty %s px %s", row.Qty, row.Px)
	}
	if row.Fee.Valid {
		t.Error("a fee was recorded: the simulator does not observe one, and NULL is the truth")
	}
	if row.PositionID != nil {
		t.Error("a position id was recorded: the venue does not know about positions")
	}

	// Every decimal in the raw payload is a JSON string, never a bare number
	// (ADR-0011): a jsonb number is exact in Postgres and a float everywhere
	// outside Go.
	var raw map[string]any
	if err := json.Unmarshal(row.Raw, &raw); err != nil {
		t.Fatalf("raw payload is not JSON: %v", err)
	}
	for _, key := range []string{"last_qty", "last_px", "cum_qty", "leaves_qty", "avg_px", "order_qty"} {
		if _, ok := raw[key].(string); !ok {
			t.Errorf("raw %s is %T, want a JSON string", key, raw[key])
		}
	}
}

// The schema checks fills.exec_state against a closed vocabulary, so a status
// that mapped to something outside it would be a write that fails at three in
// the morning rather than a test that fails now.
func TestExecStateMapsOntoTheSchemaVocabulary(t *testing.T) {
	t.Parallel()

	allowed := map[string]bool{"NEW": true, "PARTIAL": true, "FILLED": true, "CANCELED": true, "REJECTED": true}
	for _, s := range []enum.OrdStatus{
		enum.OrdStatus_NEW, enum.OrdStatus_PARTIALLY_FILLED, enum.OrdStatus_FILLED,
		enum.OrdStatus_CANCELED, enum.OrdStatus_REJECTED, enum.OrdStatus_PENDING_CANCEL,
	} {
		if got := execState(s); !allowed[got] {
			t.Errorf("execState(%s) = %q, which the fills CHECK constraint refuses", s, got)
		}
	}
}

func assertField(t *testing.T, msg *quickfix.Message, tg quickfix.Tag, want string) {
	t.Helper()

	var got quickfix.FIXString
	source := msg.Body
	if tg == tag.MsgType {
		source = quickfix.Body{FieldMap: msg.Header.FieldMap}
	}
	if err := source.GetField(tg, &got); err != nil {
		t.Errorf("tag %d is absent, want %q", tg, want)
		return
	}
	if string(got) != want {
		t.Errorf("tag %d = %q, want %q", tg, string(got), want)
	}
}

func assertAbsent(t *testing.T, msg *quickfix.Message, tg quickfix.Tag) {
	t.Helper()
	if msg.Body.Has(tg) {
		var got quickfix.FIXString
		_ = msg.Body.GetField(tg, &got)
		t.Errorf("tag %d is present as %q, want absent", tg, string(got))
	}
}

// A report's TransactTime goes out in UTC whatever the process timezone is: a
// venue timestamp in local time is a timestamp nobody can compare.
func TestTransactTimeIsUTC(t *testing.T) {
	t.Parallel()

	local := time.Date(2026, 9, 15, 8, 0, 0, 0, time.FixedZone("EDT", -4*60*60))
	msg := executionReportMessage(report{leavesQty: decimal.Zero, cumQty: decimal.Zero, at: local})
	assertField(t, msg, tag.TransactTime, "20260915-12:00:00.000")
}

func TestRawPayloadKeysAreStable(t *testing.T) {
	t.Parallel()

	raw := string(report{lastQty: dec("1"), lastPx: dec("2"), at: epoch}.raw())
	for _, key := range []string{"order_id", "exec_id", "cl_ord_id", "symbol", "exec_type", "ord_status"} {
		if !strings.Contains(raw, `"`+key+`"`) {
			t.Errorf("raw payload has no %q; a query written against it would silently return nothing", key)
		}
	}
}
