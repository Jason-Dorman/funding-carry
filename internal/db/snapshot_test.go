package db

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/shopspring/decimal"
)

// featureSnapshot stands in for the FeatureSnapshot Part 12 will persist: the
// shape does not matter here, only that it carries decimals.
type featureSnapshot struct {
	FundingRate decimal.Decimal `json:"funding_rate"`
	SpotMark    decimal.Decimal `json:"spot_mark"`
	Contracts   int64           `json:"contracts"`
}

// The contract: decimals are JSON strings. The value chosen has more significant
// digits than a float64 can hold, so if it were ever written as a bare JSON
// number, every consumer outside Go would read back a different number.
func TestEncodeSnapshotWritesDecimalsAsStrings(t *testing.T) {
	t.Parallel()

	const rate = "0.0000123456789012345678"
	snap := featureSnapshot{
		FundingRate: decimal.RequireFromString(rate),
		SpotMark:    decimal.RequireFromString("4137.285714285714285714"),
		Contracts:   -7,
	}

	encoded, err := EncodeSnapshot(snap)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	if !strings.Contains(string(encoded), `"funding_rate":"`+rate+`"`) {
		t.Fatalf("funding rate is not a quoted string in %s", encoded)
	}

	// Decoding into json.Number would succeed for a bare number too; decoding
	// into a decimal is the check that the value survived intact.
	var back featureSnapshot
	if err := json.Unmarshal(encoded, &back); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !back.FundingRate.Equal(snap.FundingRate) {
		t.Errorf("funding rate round trip: got %s, want %s", back.FundingRate, snap.FundingRate)
	}
	if back.FundingRate.String() != rate {
		t.Errorf("funding rate lost digits: got %s, want %s", back.FundingRate, rate)
	}
	if !back.SpotMark.Equal(snap.SpotMark) {
		t.Errorf("spot mark round trip: got %s, want %s", back.SpotMark, snap.SpotMark)
	}
}

// If the global that controls decimal marshalling is ever flipped, encoding
// fails loudly instead of writing snapshots that cannot be trusted later.
func TestEncodeSnapshotRefusesUnquotedDecimals(t *testing.T) {
	t.Parallel()

	_, err := encodeSnapshot(featureSnapshot{}, true)
	if !errors.Is(err, errUnquotedDecimalJSON) {
		t.Fatalf("got %v, want errUnquotedDecimalJSON", err)
	}
}
