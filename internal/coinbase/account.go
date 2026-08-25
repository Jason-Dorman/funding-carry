package coinbase

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/shopspring/decimal"
)

// The authenticated account surface: the three read-only endpoints the risk
// engine and the treasury depend on (API spec section 1.2).
//
// Read-only is a property of this file, not just of the key. Nothing here
// issues anything but a GET, so a key that was accidentally over-scoped still
// cannot be used by this code to move anything.

const (
	defaultHost    = "api.coinbase.com"
	accountTimeout = 20 * time.Second

	pathBalanceSummary = "/api/v3/brokerage/cfm/balance_summary"
	pathPositions      = "/api/v3/brokerage/cfm/positions"
	pathMarginSetting  = "/api/v3/brokerage/cfm/intraday/margin_setting"
)

// Client reads the authenticated account endpoints.
type Client struct {
	host   string
	signer *Signer
	http   *http.Client
}

// NewClient builds the authenticated client. A nil signer is a programming
// error rather than a configuration one — callers decide whether the account
// half runs at all by whether they construct this.
func NewClient(signer *Signer) *Client {
	return &Client{
		host:   defaultHost,
		signer: signer,
		http:   &http.Client{Timeout: accountTimeout},
	}
}

// get performs one authenticated GET. A fresh token is minted per request
// because each is bound to its own path.
func (c *Client) get(ctx context.Context, path string, out any) error {
	tok, err := c.signer.Token(http.MethodGet, c.host, path)
	if err != nil {
		return fmt.Errorf("mint token for %s: %w", path, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+c.host+path, nil)
	if err != nil {
		return fmt.Errorf("build request for %s: %w", path, err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("get %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return &APIError{Path: path, Status: resp.StatusCode, Body: string(body)}
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

// APIError is a refusal from the venue.
type APIError struct {
	Path   string
	Status int
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("get %s: http %d: %s", e.Path, e.Status, e.Body)
}

// Retryable reports whether waiting could change the answer. A 401 cannot: the
// credential is wrong and will be wrong on the next tick too.
func (e *APIError) Retryable() bool {
	return e.Status == http.StatusTooManyRequests || e.Status >= 500
}

// Account is one polled snapshot of the futures account.
//
// Every field is optional. The venue omits what does not apply, and an absent
// margin figure has to stay absent: the risk engine reads margin_ratio from
// these rows and a zero would read as "no margin left" — the most dangerous
// possible misreading of "not reported".
type Account struct {
	AvailableMargin      decimal.NullDecimal
	LiquidationThreshold decimal.NullDecimal
	InitialMargin        decimal.NullDecimal
	CFMBalance           decimal.NullDecimal
	CBIBalance           decimal.NullDecimal
	FuturesBuyingPower   decimal.NullDecimal
	UnrealizedPnL        decimal.NullDecimal
	DailyRealizedPnL     decimal.NullDecimal
}

// MarginRatio is available_margin / liquidation_threshold, the figure the risk
// engine's floor is expressed against.
//
// It is a ratio, so Div is legitimate (API spec section 3.5). A zero or absent
// threshold yields NULL rather than an infinity: an account with no liquidation
// threshold has no ratio, and inventing one would trip or suppress a hard stop.
func (a Account) MarginRatio() decimal.NullDecimal {
	if !a.AvailableMargin.Valid || !a.LiquidationThreshold.Valid || a.LiquidationThreshold.Decimal.IsZero() {
		return decimal.NullDecimal{}
	}
	return decimal.NullDecimal{
		Decimal: a.AvailableMargin.Decimal.Div(a.LiquidationThreshold.Decimal),
		Valid:   true,
	}
}

type balanceWire struct {
	BalanceSummary struct {
		FuturesBuyingPower   wireAmount `json:"futures_buying_power"`
		TotalUSDBalance      wireAmount `json:"total_usd_balance"`
		CBIUSDBalance        wireAmount `json:"cbi_usd_balance"`
		CFMUSDBalance        wireAmount `json:"cfm_usd_balance"`
		TotalOpenOrders      wireAmount `json:"total_open_orders_hold_amount"`
		UnrealizedPnL        wireAmount `json:"unrealized_pnl"`
		DailyRealizedPnL     wireAmount `json:"daily_realized_pnl"`
		InitialMargin        wireAmount `json:"initial_margin"`
		AvailableMargin      wireAmount `json:"available_margin"`
		LiquidationThreshold wireAmount `json:"liquidation_threshold"`
	} `json:"balance_summary"`
}

// wireAmount is the venue's money shape. The value is always a string, which is
// what keeps it out of a float on the way in.
//
// The venue uses two shapes for it, and which one depends on the endpoint:
// balance_summary sends {"value":"1.23","currency":"USD"}, while positions
// sends a bare "1.23" for avg_entry_price and unrealized_pnl. Accepting only
// the object made the positions call fail outright the moment a position
// existed — invisible while the account was flat, and therefore invisible right
// up until the first live carry, when contracts_held would have silently
// stopped being recorded.
type wireAmount struct {
	Value    string
	Currency string
}

// UnmarshalJSON accepts either shape.
func (w *wireAmount) UnmarshalJSON(b []byte) error {
	var asString string
	if err := json.Unmarshal(b, &asString); err == nil {
		w.Value = asString
		return nil
	}
	var asObject struct {
		Value    string `json:"value"`
		Currency string `json:"currency"`
	}
	if err := json.Unmarshal(b, &asObject); err != nil {
		return fmt.Errorf("amount is neither a string nor a {value,currency} object: %w", err)
	}
	w.Value, w.Currency = asObject.Value, asObject.Currency
	return nil
}

func (w wireAmount) decimal(field string) (decimal.NullDecimal, error) {
	if w.Value == "" {
		return decimal.NullDecimal{}, nil
	}
	d, err := decimal.NewFromString(w.Value)
	if err != nil {
		return decimal.NullDecimal{}, fmt.Errorf("%s=%q: %w", field, w.Value, err)
	}
	return decimal.NullDecimal{Decimal: d, Valid: true}, nil
}

// BalanceSummary reads the futures account's cash and margin.
func (c *Client) BalanceSummary(ctx context.Context) (Account, error) {
	var w balanceWire
	if err := c.get(ctx, pathBalanceSummary, &w); err != nil {
		return Account{}, err
	}
	b := w.BalanceSummary

	var a Account
	for _, f := range []struct {
		name string
		raw  wireAmount
		into *decimal.NullDecimal
	}{
		{"available_margin", b.AvailableMargin, &a.AvailableMargin},
		{"liquidation_threshold", b.LiquidationThreshold, &a.LiquidationThreshold},
		{"initial_margin", b.InitialMargin, &a.InitialMargin},
		{"cfm_usd_balance", b.CFMUSDBalance, &a.CFMBalance},
		{"cbi_usd_balance", b.CBIUSDBalance, &a.CBIBalance},
		{"futures_buying_power", b.FuturesBuyingPower, &a.FuturesBuyingPower},
		{"unrealized_pnl", b.UnrealizedPnL, &a.UnrealizedPnL},
		{"daily_realized_pnl", b.DailyRealizedPnL, &a.DailyRealizedPnL},
	} {
		v, err := f.raw.decimal(f.name)
		if err != nil {
			return Account{}, err
		}
		*f.into = v
	}
	return a, nil
}

// Position is one open futures position.
type Position struct {
	ProductID string
	// Contracts is signed: negative is short, which is the side this system
	// holds. The venue reports magnitude and side separately, and they are
	// combined here so nothing downstream can forget to apply the sign.
	Contracts     int64
	AvgEntryPrice decimal.NullDecimal
	UnrealizedPnL decimal.NullDecimal
}

type positionsWire struct {
	Positions []struct {
		ProductID         string     `json:"product_id"`
		Side              string     `json:"side"`
		NumberOfContracts string     `json:"number_of_contracts"`
		AvgEntryPrice     wireAmount `json:"avg_entry_price"`
		UnrealizedPnL     wireAmount `json:"unrealized_pnl"`
	} `json:"positions"`
}

// Positions reads the open futures positions.
func (c *Client) Positions(ctx context.Context) ([]Position, error) {
	var w positionsWire
	if err := c.get(ctx, pathPositions, &w); err != nil {
		return nil, err
	}

	out := make([]Position, 0, len(w.Positions))
	for _, p := range w.Positions {
		contracts, err := signedContracts(p.NumberOfContracts, p.Side)
		if err != nil {
			return nil, fmt.Errorf("position %s: %w", p.ProductID, err)
		}
		avg, err := p.AvgEntryPrice.decimal("avg_entry_price")
		if err != nil {
			return nil, err
		}
		pnl, err := p.UnrealizedPnL.decimal("unrealized_pnl")
		if err != nil {
			return nil, err
		}
		out = append(out, Position{
			ProductID: p.ProductID, Contracts: contracts,
			AvgEntryPrice: avg, UnrealizedPnL: pnl,
		})
	}
	return out, nil
}

// signedContracts turns the venue's magnitude-plus-side into one signed count.
//
// An unrecognised side is an error rather than a guess: getting this sign wrong
// inverts the delta the whole system is built to keep at zero.
func signedContracts(raw, side string) (int64, error) {
	if raw == "" {
		return 0, nil
	}
	n, err := decimal.NewFromString(raw)
	if err != nil {
		return 0, fmt.Errorf("number_of_contracts=%q: %w", raw, err)
	}
	switch side {
	case "FUTURES_POSITION_SIDE_SHORT", "SHORT", "SELL":
		return -n.IntPart(), nil
	case "FUTURES_POSITION_SIDE_LONG", "LONG", "BUY":
		return n.IntPart(), nil
	default:
		return 0, fmt.Errorf("side=%q: outside the venue's vocabulary", side)
	}
}

// IntradayMarginEnabled reports whether the account has opted into intraday
// margin.
//
// v1 requires this to be false (spec section 9, architecture section 12): with
// intraday margin off, the intraday-to-overnight transition can never trigger a
// call. It is polled rather than assumed because it is a setting on the account
// that a human can change from a phone.
func (c *Client) IntradayMarginEnabled(ctx context.Context) (bool, error) {
	var w struct {
		// The venue sends `{"setting": "INTRADAY_MARGIN_SETTING_..."}` — a bare
		// string, not the nested object the field name suggests (verified
		// 2026-08-24; decoding it as an object failed on every poll). Both
		// shapes are accepted here because a field that has been one thing and
		// could be the other is exactly where a silent NULL creeps back in, and
		// this particular NULL would mean nobody was checking that intraday
		// margin is off.
		Setting json.RawMessage `json:"setting"`
	}
	if err := c.get(ctx, pathMarginSetting, &w); err != nil {
		return false, err
	}

	setting, err := decodeMarginSetting(w.Setting)
	if err != nil {
		return false, err
	}

	return classifyIntradayMargin(setting)
}

// classifyIntradayMargin maps the venue's setting to the assertion v1 depends on.
//
// Only an explicit STANDARD means off. An absent field, a JSON null, an
// UNSPECIFIED and an unrecognised value are all *unknown*, and unknown must not
// read as off — this is the flag asserting that the intraday-to-overnight
// transition can never trigger a margin call. An error leaves the column NULL,
// which says "not observed"; false would say "checked, and it is safe".
func classifyIntradayMargin(setting string) (bool, error) {
	switch setting {
	case "INTRADAY_MARGIN_SETTING_STANDARD":
		return false, nil
	case "INTRADAY_MARGIN_SETTING_INTRADAY":
		return true, nil
	case "":
		return false, errors.New("intraday_margin_setting is absent; cannot assert intraday margin is off")
	default:
		return false, fmt.Errorf("intraday_margin_setting=%q: outside the venue's vocabulary", setting)
	}
}

// decodeMarginSetting reads either shape the venue has used for this field.
func decodeMarginSetting(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return asString, nil
	}
	var asObject struct {
		IntradayMarginSetting string `json:"intraday_margin_setting"`
	}
	if err := json.Unmarshal(raw, &asObject); err != nil {
		return "", fmt.Errorf("setting is neither a string nor an object: %w", err)
	}
	return asObject.IntradayMarginSetting, nil
}
