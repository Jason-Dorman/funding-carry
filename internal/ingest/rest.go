package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"

	"github.com/shopspring/decimal"

	"github.com/Jason-Dorman/funding-carry/internal/db"
)

// Doer is the HTTP client as this package needs it. Declared here rather than
// taking *http.Client so the REST paths are testable against a scripted server
// without one (architecture section 12).
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Granularity is the candle width the venue serves. These are the only three
// this system asks for: one minute is what the funding backfill marks against,
// five matches the WebSocket channel, one hour is for reaching a long way back
// cheaply.
type Granularity string

// The three candle widths this system asks for.
const (
	OneMinute  Granularity = "ONE_MINUTE"
	FiveMinute Granularity = "FIVE_MINUTE"
	OneHour    Granularity = "ONE_HOUR"
)

// Interval is how much time one candle of this granularity covers.
func (g Granularity) Interval() time.Duration {
	switch g {
	case OneMinute:
		return time.Minute
	case FiveMinute:
		return 5 * time.Minute
	case OneHour:
		return time.Hour
	default:
		return 0
	}
}

// TF is the cb_bars.tf value for this granularity, which is part of a bar's
// identity (API spec section 5.1).
func (g Granularity) TF() string {
	switch g {
	case OneMinute:
		return "1m"
	case FiveMinute:
		return "5m"
	case OneHour:
		return "1h"
	default:
		return ""
	}
}

const (
	// MaxCandlesPerRequest is the venue's page size. A backfill walks backwards
	// in windows of this many candles.
	MaxCandlesPerRequest = 350

	restTimeout = 20 * time.Second
)

// RESTClient reads the public Advanced Trade market endpoints.
//
// Public is the operative word: everything here serves the perp unauthenticated
// (verified 2026-08-20, venue doc section 6), so this client holds no
// credential and mints no JWT. The authenticated account endpoints — cfm/* —
// are a separate client with a separate lifetime, because a key that can read
// balances has no business on the path that reads candles.
type RESTClient struct {
	baseURL string
	http    Doer
}

// NewRESTClient builds the client. baseURL is CB_API_URL, which already ends in
// the brokerage path.
func NewRESTClient(baseURL string, doer Doer) *RESTClient {
	if doer == nil {
		doer = &http.Client{Timeout: restTimeout}
	}
	return &RESTClient{baseURL: baseURL, http: doer}
}

// get performs one request and decodes the body into out.
//
// A non-2xx is an error carrying the status and the beginning of the body: the
// venue reports its refusals in the payload, and a bare status code turns a
// diagnosable problem into a guess.
func (c *RESTClient) get(ctx context.Context, path string, query url.Values, out any) error {
	endpoint := c.baseURL
	if endpoint != "" && endpoint[len(endpoint)-1] != '/' {
		endpoint += "/"
	}
	endpoint += path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("build request for %s: %w", path, err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("get %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		// Bounded: an error page can be arbitrarily large and none of it is
		// worth more than a line in a log.
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return &StatusError{Path: path, Status: resp.StatusCode, Body: string(snippet)}
	}

	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

// StatusError is a refusal from the venue. It is a type rather than a wrapped
// string because the poller's back-off depends on which refusal it was: a 429
// or a 5xx is worth retrying, a 400 is the request being wrong and will be
// wrong again.
type StatusError struct {
	Path   string
	Status int
	Body   string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("get %s: http %d: %s", e.Path, e.Status, e.Body)
}

// Retryable reports whether waiting could change the answer.
func (e *StatusError) Retryable() bool {
	return e.Status == http.StatusTooManyRequests || e.Status >= 500
}

// ---------------------------------------------------------------------------
// Product
// ---------------------------------------------------------------------------

// Product is the venue's description of a tradable instrument, reduced to the
// fields this system uses.
type Product struct {
	ProductID       string
	ContractSize    decimal.NullDecimal
	Tick            decimal.NullDecimal
	BaseIncrement   decimal.NullDecimal
	Status          string
	TradingDisabled bool
	OpenInterest    decimal.NullDecimal
	// FundingRate is the venue's own hourly rate, and it is populated.
	//
	// It lives at future_product_details.funding_rate — one level ABOVE the
	// perpetual_details block, where an identically named field sits and is
	// always empty for this contract. Reading the inner one and concluding the
	// venue publishes nothing is the mistake this system made for two build
	// parts (corrected 2026-08-24); the outer one is read first and the inner
	// kept only as a fallback.
	FundingRate     decimal.NullDecimal
	FundingTime     time.Time
	FundingInterval string
	MaxLeverage     decimal.NullDecimal

	// The margin rates the risk engine sizes against, also published here and
	// nowhere else public. Overnight is the one v1 uses; intraday is carried so
	// the "never opt in" rule can be checked against real numbers.
	OvernightMarginLong  decimal.NullDecimal
	OvernightMarginShort decimal.NullDecimal
	IndexPrice           decimal.NullDecimal
	SettlementPrice      decimal.NullDecimal
	// Session is the venue's own trading calendar for this contract, which is
	// more authoritative than the configured maintenance window: it names the
	// exact close and reopen rather than a weekly rule.
	Session Session
}

// Session is the FCM trading session as the venue reports it.
type Session struct {
	IsOpen    bool
	State     string
	OpenTime  time.Time
	CloseTime time.Time
}

type productWire struct {
	ProductID       string `json:"product_id"`
	Price           string `json:"price"`
	PriceIncrement  string `json:"price_increment"`
	BaseIncrement   string `json:"base_increment"`
	Status          string `json:"status"`
	TradingDisabled bool   `json:"trading_disabled"`

	FutureProductDetails struct {
		ContractSize string `json:"contract_size"`

		// The populated funding fields. perpetual_details carries the same two
		// names and leaves them empty for this contract.
		FundingRate     string     `json:"funding_rate"`
		FundingTime     *time.Time `json:"funding_time"`
		FundingInterval string     `json:"funding_interval"`
		OpenInterest    string     `json:"open_interest"`
		IndexPrice      string     `json:"index_price"`
		SettlementPrice string     `json:"settlement_price"`

		OvernightMarginRate struct {
			Long  string `json:"long_margin_rate"`
			Short string `json:"short_margin_rate"`
		} `json:"overnight_margin_rate"`

		PerpetualDetails struct {
			OpenInterest string `json:"open_interest"`
			FundingRate  string `json:"funding_rate"`
			MaxLeverage  string `json:"max_leverage"`
		} `json:"perpetual_details"`
	} `json:"future_product_details"`

	Session struct {
		IsSessionOpen bool       `json:"is_session_open"`
		SessionState  string     `json:"session_state"`
		OpenTime      *time.Time `json:"open_time"`
		CloseTime     *time.Time `json:"close_time"`
	} `json:"fcm_trading_session_details"`
}

// Product reads one product's metadata.
func (c *RESTClient) Product(ctx context.Context, productID string) (Product, error) {
	var w productWire
	if err := c.get(ctx, "market/products/"+url.PathEscape(productID), nil, &w); err != nil {
		return Product{}, err
	}

	p := Product{
		ProductID:       w.ProductID,
		Status:          w.Status,
		TradingDisabled: w.TradingDisabled,
		Session: Session{
			IsOpen: w.Session.IsSessionOpen,
			State:  w.Session.SessionState,
		},
	}
	if w.Session.OpenTime != nil {
		p.Session.OpenTime = w.Session.OpenTime.UTC()
	}
	if w.Session.CloseTime != nil {
		p.Session.CloseTime = w.Session.CloseTime.UTC()
	}

	// Every one of these is optional on the wire and several are routinely
	// empty for this contract, so each reads as absent rather than as zero.
	fpd := w.FutureProductDetails
	if fpd.FundingTime != nil {
		p.FundingTime = fpd.FundingTime.UTC()
	}
	p.FundingInterval = fpd.FundingInterval

	for _, f := range []struct {
		name string
		raw  string
		into *decimal.NullDecimal
	}{
		{"contract_size", fpd.ContractSize, &p.ContractSize},
		{"price_increment", w.PriceIncrement, &p.Tick},
		{"base_increment", w.BaseIncrement, &p.BaseIncrement},
		// Outer first, inner as fallback: see the comment on Product.FundingRate.
		{"open_interest", firstNonEmpty(fpd.OpenInterest, fpd.PerpetualDetails.OpenInterest), &p.OpenInterest},
		{"funding_rate", firstNonEmpty(fpd.FundingRate, fpd.PerpetualDetails.FundingRate), &p.FundingRate},
		{"max_leverage", fpd.PerpetualDetails.MaxLeverage, &p.MaxLeverage},
		{"overnight_margin_rate.long", fpd.OvernightMarginRate.Long, &p.OvernightMarginLong},
		{"overnight_margin_rate.short", fpd.OvernightMarginRate.Short, &p.OvernightMarginShort},
		{"index_price", fpd.IndexPrice, &p.IndexPrice},
		{"settlement_price", fpd.SettlementPrice, &p.SettlementPrice},
	} {
		v, err := parseDecimal(f.name, f.raw)
		if err != nil {
			return Product{}, fmt.Errorf("product %s: %w", productID, err)
		}
		*f.into = v
	}
	return p, nil
}

// ---------------------------------------------------------------------------
// Candles
// ---------------------------------------------------------------------------

// Candle is one OHLCV bar. Start is the open time, as the venue sends it.
type Candle struct {
	Start  time.Time
	Open   decimal.Decimal
	High   decimal.Decimal
	Low    decimal.Decimal
	Close  decimal.Decimal
	Volume decimal.Decimal
}

// Row converts a candle to the bar the schema stores, whose ts is the close.
func (c Candle) Row(productID string, g Granularity) db.BarRow {
	return db.BarRow{
		TS:        c.Start.Add(g.Interval()),
		ProductID: productID,
		TF:        g.TF(),
		Open:      c.Open,
		High:      c.High,
		Low:       c.Low,
		Close:     c.Close,
		Volume:    c.Volume,
		// TradeCount stays NULL: the candles payload does not carry one, and a
		// zero would read as a bar in which nothing traded.
	}
}

// Candles reads one page, oldest first. The venue returns at most
// MaxCandlesPerRequest and orders newest first; both are normalised here so
// callers do not have to know either.
//
// A window with no trades yields no candle at all rather than a flat one — the
// perp skips those minutes overnight — so the result can be shorter than the
// window implies, with holes in the middle. Callers must key on Start rather
// than assume position.
func (c *RESTClient) Candles(ctx context.Context, productID string, start, end time.Time, g Granularity) ([]Candle, error) {
	q := url.Values{}
	q.Set("start", strconv.FormatInt(start.Unix(), 10))
	q.Set("end", strconv.FormatInt(end.Unix(), 10))
	q.Set("granularity", string(g))
	q.Set("limit", strconv.Itoa(MaxCandlesPerRequest))

	var w struct {
		Candles []candleWire `json:"candles"`
	}
	if err := c.get(ctx, "market/products/"+url.PathEscape(productID)+"/candles", q, &w); err != nil {
		return nil, err
	}

	out := make([]Candle, 0, len(w.Candles))
	for _, raw := range w.Candles {
		parsed, err := parseCandle(raw)
		if err != nil {
			return nil, fmt.Errorf("%s candle %s: %w", productID, raw.Start, err)
		}
		out = append(out, Candle{
			Start:  parsed.start,
			Open:   parsed.row.Open,
			High:   parsed.row.High,
			Low:    parsed.row.Low,
			Close:  parsed.row.Close,
			Volume: parsed.row.Volume,
		})
	}
	sortCandles(out)
	return out, nil
}

// ---------------------------------------------------------------------------
// Book
// ---------------------------------------------------------------------------

// sortCandles orders a page oldest first.
func sortCandles(c []Candle) {
	sort.Slice(c, func(i, j int) bool { return c[i].Start.Before(c[j].Start) })
}

// BookQuote is the top of book from the REST snapshot, used when the WebSocket
// book is degraded. It is not the ticker Quote the sampler holds: that one is a
// live top-of-book from the socket, this one is a polled page of the book.
type BookQuote struct {
	ProductID string
	Bids      []level
	Asks      []level
	Time      time.Time
	Mid       decimal.NullDecimal
	SpreadBps decimal.NullDecimal
}

// Book reads a price book snapshot, deepest side first.
func (c *RESTClient) Book(ctx context.Context, productID string, depth int) (BookQuote, error) {
	q := url.Values{}
	q.Set("product_id", productID)
	q.Set("limit", strconv.Itoa(depth))

	var w struct {
		PriceBook struct {
			ProductID string                         `json:"product_id"`
			Bids      []struct{ Price, Size string } `json:"bids"`
			Asks      []struct{ Price, Size string } `json:"asks"`
			Time      time.Time                      `json:"time"`
		} `json:"pricebook"`
		MidMarket string `json:"mid_market"`
		SpreadBps string `json:"spread_bps"`
	}
	if err := c.get(ctx, "market/product_book", q, &w); err != nil {
		return BookQuote{}, err
	}

	out := BookQuote{ProductID: w.PriceBook.ProductID, Time: w.PriceBook.Time.UTC()}
	var err error
	if out.Bids, err = restLevels(w.PriceBook.Bids); err != nil {
		return BookQuote{}, fmt.Errorf("%s bids: %w", productID, err)
	}
	if out.Asks, err = restLevels(w.PriceBook.Asks); err != nil {
		return BookQuote{}, fmt.Errorf("%s asks: %w", productID, err)
	}
	if out.Mid, err = parseDecimal("mid_market", w.MidMarket); err != nil {
		return BookQuote{}, err
	}
	if out.SpreadBps, err = parseDecimal("spread_bps", w.SpreadBps); err != nil {
		return BookQuote{}, err
	}
	return out, nil
}

func restLevels(raw []struct{ Price, Size string }) ([]level, error) {
	out := make([]level, 0, len(raw))
	for _, l := range raw {
		px, err := requireDecimal("price", l.Price)
		if err != nil {
			return nil, err
		}
		sz, err := requireDecimal("size", l.Size)
		if err != nil {
			return nil, err
		}
		out = append(out, level{px: px, qty: sz})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Trades
// ---------------------------------------------------------------------------

// Trades reads the most recent trades for a product.
//
// The path is `ticker`, not `market_trades`: the WebSocket channel is called
// market_trades but the REST equivalent is not, and asking for the obvious one
// returns 404 (verified 2026-08-22).
func (c *RESTClient) Trades(ctx context.Context, productID string, limit int) ([]tradeWire, error) {
	q := url.Values{}
	q.Set("limit", strconv.Itoa(limit))

	var w struct {
		Trades []tradeWire `json:"trades"`
	}
	if err := c.get(ctx, "market/products/"+url.PathEscape(productID)+"/ticker", q, &w); err != nil {
		return nil, err
	}
	return w.Trades, nil
}

// firstNonEmpty picks the outer field over the inner one where the venue
// publishes the same name at two nesting levels and populates only one.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
