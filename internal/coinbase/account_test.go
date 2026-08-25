package coinbase

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/shopspring/decimal"
)

func num(t *testing.T, s string) decimal.NullDecimal {
	t.Helper()
	d, err := decimal.NewFromString(s)
	if err != nil {
		t.Fatalf("decimal %q: %v", s, err)
	}
	return decimal.NullDecimal{Decimal: d, Valid: true}
}

func TestMarginRatioIsNullWithoutALiquidationThreshold(t *testing.T) {
	// What the venue actually returns for an account holding nothing: a real
	// available margin and a liquidation threshold of zero (verified
	// 2026-08-24). There is no ratio to compute.
	a := Account{AvailableMargin: num(t, "1000"), LiquidationThreshold: num(t, "0")}

	// NULL, not zero and not infinity. The risk engine's floor is a lower bound
	// on this number, so a zero would read as "liquidation is imminent" for an
	// account with no position at all.
	if got := a.MarginRatio(); got.Valid {
		t.Fatalf("margin_ratio = %v, want NULL when the threshold is zero", got)
	}
}

func TestMarginRatioDivides(t *testing.T) {
	a := Account{AvailableMargin: num(t, "1500"), LiquidationThreshold: num(t, "1000")}
	got := a.MarginRatio()
	if !got.Valid || got.Decimal.String() != "1.5" {
		t.Errorf("margin_ratio = %v, want 1.5", got)
	}
}

func TestMarginRatioNeedsBothFields(t *testing.T) {
	for _, tc := range []struct {
		name string
		a    Account
	}{
		{"no available margin", Account{LiquidationThreshold: num(t, "1000")}},
		{"no threshold", Account{AvailableMargin: num(t, "1000")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.a.MarginRatio(); got.Valid {
				t.Errorf("margin_ratio = %v, want NULL", got)
			}
		})
	}
}

func TestIntradayMarginSettingDecodesEitherShape(t *testing.T) {
	// The venue sends a bare string. Decoding it as the nested object the field
	// name implies failed on every poll until 2026-08-24.
	for _, tc := range []struct {
		name, raw, want string
	}{
		{"bare string", `"INTRADAY_MARGIN_SETTING_STANDARD"`, "INTRADAY_MARGIN_SETTING_STANDARD"},
		{"nested object", `{"intraday_margin_setting":"INTRADAY_MARGIN_SETTING_INTRADAY"}`, "INTRADAY_MARGIN_SETTING_INTRADAY"},
		{"absent", ``, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decodeMarginSetting(json.RawMessage(tc.raw))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSignedContracts(t *testing.T) {
	for _, tc := range []struct {
		name, raw, side string
		want            int64
		wantErr         bool
	}{
		{name: "short is negative", raw: "5", side: "FUTURES_POSITION_SIDE_SHORT", want: -5},
		{name: "long is positive", raw: "5", side: "FUTURES_POSITION_SIDE_LONG", want: 5},
		{name: "short alias", raw: "3", side: "SHORT", want: -3},
		{name: "no contracts", raw: "", side: "", want: 0},
		// Getting this sign wrong inverts the delta the entire system exists to
		// hold at zero, so an unknown side is an error rather than a default.
		{name: "unknown side", raw: "5", side: "SIDEWAYS", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := signedContracts(tc.raw, tc.side)
			if tc.wantErr {
				if err == nil {
					t.Fatal("an unrecognised side was accepted")
				}
				return
			}
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if got != tc.want {
				t.Errorf("contracts = %d, want %d", got, tc.want)
			}
		})
	}
}

// Positions carry prices as bare strings, not {value,currency} objects.
//
// balance_summary uses the object shape; positions does not. Accepting only the
// object made the positions call fail outright the moment a position existed —
// which is invisible while the account is flat, and therefore invisible right
// up until the first live carry.
func TestPositionAmountsDecodeFromEitherShape(t *testing.T) {
	body := `{"positions":[{"product_id":"ETP-20DEC30-CDE","side":"FUTURES_POSITION_SIDE_SHORT",` +
		`"number_of_contracts":"3","avg_entry_price":"3141.20","unrealized_pnl":"6.195"}]}`

	var w positionsWire
	if err := json.Unmarshal([]byte(body), &w); err != nil {
		t.Fatalf("the documented positions shape did not decode: %v", err)
	}
	if len(w.Positions) != 1 {
		t.Fatalf("positions = %d, want 1", len(w.Positions))
	}
	p := w.Positions[0]
	if p.AvgEntryPrice.Value != "3141.20" {
		t.Errorf("avg_entry_price = %q, want 3141.20", p.AvgEntryPrice.Value)
	}
	if p.UnrealizedPnL.Value != "6.195" {
		t.Errorf("unrealized_pnl = %q, want 6.195", p.UnrealizedPnL.Value)
	}

	// And the object shape balance_summary uses still works.
	var obj wireAmount
	if err := json.Unmarshal([]byte(`{"value":"12.34","currency":"USD"}`), &obj); err != nil {
		t.Fatalf("the object shape stopped decoding: %v", err)
	}
	if obj.Value != "12.34" || obj.Currency != "USD" {
		t.Errorf("object shape = %+v, want value 12.34 currency USD", obj)
	}
}

// An unknown intraday margin setting must not read as "off".
//
// This is the flag asserting that the intraday-to-overnight transition can never
// trigger a margin call. An error leaves the column NULL, which says "not
// observed"; false would say "checked, and it is safe".
func TestAnUnknownIntradayMarginSettingIsAnError(t *testing.T) {
	for _, tc := range []struct {
		name, setting string
		wantEnabled   bool
		wantErr       bool
	}{
		{name: "standard is off", setting: "INTRADAY_MARGIN_SETTING_STANDARD"},
		{name: "intraday is on", setting: "INTRADAY_MARGIN_SETTING_INTRADAY", wantEnabled: true},
		{name: "absent is unknown", setting: "", wantErr: true},
		{name: "unspecified is unknown", setting: "INTRADAY_MARGIN_SETTING_UNSPECIFIED", wantErr: true},
		{name: "a new value is unknown", setting: "INTRADAY_MARGIN_SETTING_PORTFOLIO", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := classifyIntradayMargin(tc.setting)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("setting %q was accepted as %v; unknown must not read as off", tc.setting, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if got != tc.wantEnabled {
				t.Errorf("enabled = %v, want %v", got, tc.wantEnabled)
			}
		})
	}
}

func TestAMalformedSixtyFourByteKeyIsRefused(t *testing.T) {
	// A 64-byte blob that is not an Ed25519 key: the right length, the wrong
	// contents. Go's ed25519.Sign only checks the length, so without a
	// consistency check this would mint tokens the venue rejects, five seconds
	// apart, hours after a successful-looking startup.
	blob := make([]byte, 64)
	for i := range blob {
		blob[i] = byte(i)
	}
	_, err := NewSigner("abc", base64.StdEncoding.EncodeToString(blob))
	if err == nil {
		t.Fatal("a 64-byte non-key was accepted; the binary would start and then fail every poll")
	}
}
