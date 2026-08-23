package ingest

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/shopspring/decimal"
)

// Channel names. The name a channel is subscribed under is not always the name
// its data arrives under — level2 is the one that differs, and a handler that
// matched on the subscription name would silently see no data — so both are
// spelled out here rather than assumed equal.
const (
	channelTicker       = "ticker"
	channelLevel2       = "level2"
	channelLevel2Data   = "l2_data"
	channelMarketTrades = "market_trades"
	channelCandles      = "candles"
	channelStatus       = "status"

	// channelHeartbeats is subscribed on every connection. It is not persisted;
	// it exists so that every socket carries traffic once a second regardless of
	// how quiet its data channel is. That turns two otherwise awkward problems
	// into simple ones: a read deadline becomes a reliable liveness check for
	// every stream rather than only the busy ones, and the silence check that
	// detects a data gap gets run at least once a second without a timer of its
	// own. Verified against the venue on 2026-08-20.
	channelHeartbeats = "heartbeats"

	// channelSubscriptions is the venue's acknowledgement of a subscribe.
	channelSubscriptions = "subscriptions"
)

// envelope is the frame every Advanced Trade message arrives in.
//
// sequence_num counts messages on the connection, not on the channel — the
// subscription acknowledgement and the heartbeats share the same counter as the
// data — which is exactly the property gap detection wants: one connection
// carries one data channel, so a jump in this number means this stream missed
// something.
type envelope struct {
	Channel     string          `json:"channel"`
	Timestamp   time.Time       `json:"timestamp"`
	SequenceNum int64           `json:"sequence_num"`
	Events      json.RawMessage `json:"events"`

	// An error frame carries these two instead of a channel. The venue answers
	// an unknown channel name with "authentication failure" and then goes quiet
	// (observed 2026-08-20), so an error frame is treated as a dead connection
	// rather than as something to wait out.
	Type    string `json:"type"`
	Message string `json:"message"`
}

const errorFrameType = "error"

// Message is one decoded frame handed to a handler. It carries both timestamps
// API spec section 3.6 asks for: the venue's, which is what the data is about,
// and the receive time, which is what staleness and latency are measured from.
type Message struct {
	Channel string
	SeqNum  int64
	VenueTS time.Time
	RecvTS  time.Time
	Events  json.RawMessage
}

// decodeEnvelope parses one raw frame. A frame that is not JSON is a protocol
// failure of the connection, not of one message, so the caller reconnects.
func decodeEnvelope(raw []byte) (envelope, error) {
	var e envelope
	if err := json.Unmarshal(raw, &e); err != nil {
		return envelope{}, fmt.Errorf("decode envelope: %w", err)
	}
	return e, nil
}

// ---------------------------------------------------------------------------
// Per-channel event payloads, as observed on the live socket 2026-08-20.
// Every number arrives as a JSON string and is parsed into a decimal; there is
// no float on this path (spec section 11).
// ---------------------------------------------------------------------------

type tickerEvent struct {
	Type    string       `json:"type"` // snapshot | update
	Tickers []tickerWire `json:"tickers"`
}

type tickerWire struct {
	ProductID       string `json:"product_id"`
	Price           string `json:"price"`
	BestBid         string `json:"best_bid"`
	BestAsk         string `json:"best_ask"`
	BestBidQuantity string `json:"best_bid_quantity"`
	BestAskQuantity string `json:"best_ask_quantity"`
}

type l2Event struct {
	ProductID string    `json:"product_id"`
	Type      string    `json:"type"` // snapshot | update
	Updates   []l2Level `json:"updates"`
}

type l2Level struct {
	Side        string `json:"side"` // bid | offer  (the snapshot uses "bid"/"offer")
	PriceLevel  string `json:"price_level"`
	NewQuantity string `json:"new_quantity"`
	EventTime   string `json:"event_time"`
}

type tradesEvent struct {
	Type   string      `json:"type"`
	Trades []tradeWire `json:"trades"`
}

type tradeWire struct {
	TradeID   string    `json:"trade_id"`
	ProductID string    `json:"product_id"`
	Price     string    `json:"price"`
	Size      string    `json:"size"`
	Side      string    `json:"side"` // BUY | SELL, the aggressor
	Time      time.Time `json:"time"`
}

type candlesEvent struct {
	Type    string       `json:"type"`
	Candles []candleWire `json:"candles"`
}

// candleWire is one candle. Start is a unix-seconds count delivered as a string,
// and it is the candle's *open* time; cb_bars keys on the close (API spec
// section 5.1), so the handler adds the interval.
type candleWire struct {
	ProductID string `json:"product_id"`
	Start     string `json:"start"`
	Open      string `json:"open"`
	High      string `json:"high"`
	Low       string `json:"low"`
	Close     string `json:"close"`
	Volume    string `json:"volume"`
}

type statusEvent struct {
	Type     string       `json:"type"`
	Products []statusWire `json:"products"`
}

type statusWire struct {
	ID            string `json:"id"`
	Status        string `json:"status"` // "online" when tradable
	StatusMessage string `json:"status_message"`
}

// statusOnline is the product status that means the market is open. Anything
// else — and the venue does not document the full set — is treated as not
// tradable, which is the safe direction for a flag that gates order entry.
const statusOnline = "online"

// ---------------------------------------------------------------------------
// Parsing helpers
// ---------------------------------------------------------------------------

// parseDecimal converts a venue string to a decimal. An empty string is not an
// error: the venue sends "" for a field it has no value for (the perp's
// funding_rate is the live example), and that must read as absent rather than as
// zero.
func parseDecimal(field, s string) (decimal.NullDecimal, error) {
	if s == "" {
		return decimal.NullDecimal{}, nil
	}
	d, err := decimal.NewFromString(s)
	if err != nil {
		return decimal.NullDecimal{}, fmt.Errorf("%s=%q: %w", field, s, err)
	}
	return decimal.NullDecimal{Decimal: d, Valid: true}, nil
}

// requireDecimal is parseDecimal for a column the schema declares NOT NULL: an
// absent value there is a message the system cannot use.
func requireDecimal(field, s string) (decimal.Decimal, error) {
	v, err := parseDecimal(field, s)
	if err != nil {
		return decimal.Decimal{}, err
	}
	if !v.Valid {
		return decimal.Decimal{}, fmt.Errorf("%s: missing", field)
	}
	return v.Decimal, nil
}

// parseUnixSeconds reads the candle start, which the venue sends as a decimal
// string rather than a JSON number.
func parseUnixSeconds(s string) (time.Time, error) {
	secs, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("start=%q: %w", s, err)
	}
	return time.Unix(secs, 0).UTC(), nil
}
