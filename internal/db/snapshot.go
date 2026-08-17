package db

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/shopspring/decimal"
)

// Snapshot is a JSON document bound for a `jsonb` column: a decision's full
// feature input, a risk event's detail, a fill's raw venue payload. A nil
// Snapshot writes SQL NULL.
type Snapshot []byte

// EncodeSnapshot marshals v for a jsonb column.
//
// The contract this enforces is that **every decimal in the document is a JSON
// string, not a JSON number**. Postgres itself would keep a bare number exact —
// jsonb stores it as `numeric` — so nothing inside the system would ever notice
// the difference. The loss happens on the way out: `json.loads` in the
// backtester, `jq` in a triage session, and any JavaScript reading a Grafana
// panel all turn a JSON number into an IEEE-754 double, and a decision's
// recorded inputs stop matching the decision. That makes the encoding a
// contract rather than a detail (API spec section 3.5).
//
// The switch that decides this is a mutable package global in shopspring, pinned
// by internal/config. This package does not import config, so it refuses to
// encode rather than assume: a snapshot written with bare numbers is not
// recoverable after the fact, and failing here is the only place the mistake is
// still cheap.
func EncodeSnapshot(v any) (Snapshot, error) {
	return encodeSnapshot(v, decimal.MarshalJSONWithoutQuotes)
}

// errUnquotedDecimalJSON is a broken invariant, not a bad input: something in
// the process changed a global that decides how every decimal in the system
// serializes.
var errUnquotedDecimalJSON = errors.New(
	"decimal.MarshalJSONWithoutQuotes is true: decimals would be written as JSON numbers and read back as floats")

func encodeSnapshot(v any, numbersUnquoted bool) (Snapshot, error) {
	if numbersUnquoted {
		return nil, fmt.Errorf("encode snapshot: %w", errUnquotedDecimalJSON)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encode snapshot: %w", err)
	}
	return b, nil
}
