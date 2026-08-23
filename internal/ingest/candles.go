package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/Jason-Dorman/funding-carry/internal/db"
)

const (
	// CandleInterval is the only granularity the WebSocket candles channel
	// serves. Subscribing with granularity "ONE_MINUTE", with 60, and with no
	// granularity at all all return candles 300 seconds apart (verified against
	// the live socket, 2026-08-20); the field is ignored. One-minute bars come
	// from the REST candles endpoint, which does honour the parameter, with the
	// rest of the backfill in Part 5.
	CandleInterval = 5 * time.Minute

	// CandleTF is the tf column these bars are written under. It is part of
	// cb_bars' identity, so 1m bars from REST and 5m bars from the socket
	// coexist rather than colliding.
	CandleTF = "5m"
)

// CandlesHandler persists completed candles.
//
// The venue sends a `snapshot` of roughly the last hundred candles on subscribe
// and then `update` messages carrying exactly one candle: the one currently
// forming, re-sent as its OHLCV moves. So "completed" cannot mean "a later
// candle appears beside it in this message" — on the five-minute roll the update
// carries only the new candle, and the one that just closed is never mentioned
// again. That rule cost almost every bar: cb_bars gained one row in thirty
// minutes of the Part 4 soak.
//
// What it means instead is "a later candle has been observed at all". Each
// product keeps the newest start it has ever seen and the latest version of
// every candle not yet written; a candle is written once a strictly newer start
// turns up, carrying the last values it was seen with. Writing the snapshot's
// history too is what makes a reconnect refill whatever the disconnection cost,
// with no backfill machinery.
type CandlesHandler struct {
	products []string
	sink     Sink
	state    map[string]*productCandles
}

type productCandles struct {
	// lastStart is the open time of the newest candle already written. It is
	// deliberately not per-connection state: it records what is in the database,
	// which a dropped socket does not change.
	lastStart time.Time
	// seeded marks that the first window has been consumed. Gaps inside that
	// first window are the venue's own history and say nothing about this
	// process, so discontinuity is only counted from the second message on.
	seeded bool
	// newest is the newest candle start ever observed for this product, across
	// messages. It is what makes a candle "closed": one strictly older than this
	// cannot receive another update.
	newest time.Time
	// pending holds the latest version of every observed candle not yet written,
	// keyed by open time. In steady state it holds exactly one — the forming
	// candle — and briefly the whole subscribe window before that is drained.
	pending map[time.Time]db.BarRow
}

// NewCandlesHandler builds the candle handler for the given products.
func NewCandlesHandler(products []string, sink Sink) *CandlesHandler {
	h := &CandlesHandler{products: products, sink: sink, state: make(map[string]*productCandles, len(products))}
	for _, p := range products {
		h.state[p] = &productCandles{pending: map[time.Time]db.BarRow{}}
	}
	return h
}

// Subscribe names the channel and the products this handler wants.
func (h *CandlesHandler) Subscribe() (string, []string) { return channelCandles, h.products }

// DataChannel is where candle data arrives; here it matches the subscription.
func (h *CandlesHandler) DataChannel() string { return channelCandles }

// Reset has nothing to drop, for the reason given on productCandles.lastStart:
// this handler's state describes rows already persisted, not the connection they
// arrived over.
func (h *CandlesHandler) Reset() {}

// Handle writes every candle in the window that has closed since the last one.
func (h *CandlesHandler) Handle(ctx context.Context, msg Message) error {
	var events []candlesEvent
	if err := json.Unmarshal(msg.Events, &events); err != nil {
		return fmt.Errorf("decode candle events: %w", err)
	}

	byProduct, errs := h.parse(events)
	for product, candles := range byProduct {
		if err := h.persist(ctx, product, candles); err != nil {
			if errors.Is(err, errWriterGone) {
				return err
			}
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// candle is one parsed candle, keyed by its open time.
type candle struct {
	start time.Time
	row   db.BarRow
}

func (h *CandlesHandler) parse(events []candlesEvent) (map[string][]candle, []error) {
	byProduct := map[string][]candle{}
	var errs []error

	for _, e := range events {
		for _, c := range e.Candles {
			if _, ok := h.state[c.ProductID]; !ok {
				continue
			}
			parsed, err := parseCandle(c)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s candle %s: %w", c.ProductID, c.Start, err))
				continue
			}
			byProduct[c.ProductID] = append(byProduct[c.ProductID], parsed)
		}
	}
	return byProduct, errs
}

func parseCandle(c candleWire) (candle, error) {
	start, err := parseUnixSeconds(c.Start)
	if err != nil {
		return candle{}, err
	}
	open, err := requireDecimal("open", c.Open)
	if err != nil {
		return candle{}, err
	}
	high, err := requireDecimal("high", c.High)
	if err != nil {
		return candle{}, err
	}
	low, err := requireDecimal("low", c.Low)
	if err != nil {
		return candle{}, err
	}
	closePx, err := requireDecimal("close", c.Close)
	if err != nil {
		return candle{}, err
	}
	volume, err := requireDecimal("volume", c.Volume)
	if err != nil {
		return candle{}, err
	}

	return candle{start: start, row: db.BarRow{
		// ts is the bar's close (API spec section 5.1); the venue sends its open.
		TS:        start.Add(CandleInterval),
		ProductID: c.ProductID,
		TF:        CandleTF,
		Open:      open,
		High:      high,
		Low:       low,
		Close:     closePx,
		Volume:    volume,
		// TradeCount stays NULL: the candles channel does not carry one, and a
		// zero would read as a bar in which nothing traded.
	}}, nil
}

// persist records what arrived and writes whatever that closed.
func (h *CandlesHandler) persist(ctx context.Context, product string, candles []candle) error {
	p := h.state[product]
	p.record(candles)
	return h.writeClosed(ctx, product, p)
}

// record absorbs one message's candles. A later version of a candle replaces an
// earlier one, which is how the forming candle accumulates its final OHLCV
// before anyone writes it.
func (p *productCandles) record(candles []candle) {
	for _, c := range candles {
		// Already written. The venue replays its window on every subscribe, so
		// this is the ordinary case after a reconnect.
		if !p.lastStart.IsZero() && !c.start.After(p.lastStart) {
			continue
		}
		p.pending[c.start] = c.row
		if c.start.After(p.newest) {
			p.newest = c.start
		}
	}
}

// closed lists the pending candles a newer start has superseded, oldest first.
func (p *productCandles) closed() []time.Time {
	starts := make([]time.Time, 0, len(p.pending))
	for start := range p.pending {
		if start.Before(p.newest) {
			starts = append(starts, start)
		}
	}
	sort.Slice(starts, func(i, j int) bool { return starts[i].Before(starts[j]) })
	return starts
}

// writeClosed submits every candle that can no longer change, reporting a
// discontinuity as it goes. A jump is a gap in the series, not a reason to
// discard the bar that follows it.
func (h *CandlesHandler) writeClosed(ctx context.Context, product string, p *productCandles) error {
	var errs []error
	for _, start := range p.closed() {
		if p.seeded && !p.lastStart.IsZero() && !start.Equal(p.lastStart.Add(CandleInterval)) {
			errs = append(errs, fmt.Errorf("%s: candle time jumped from %s to %s",
				product, p.lastStart.Format(time.RFC3339), start.Format(time.RFC3339)))
		}
		if err := submit(ctx, h.sink, p.pending[start]); err != nil {
			return errors.Join(append(errs, err)...)
		}
		delete(p.pending, start)
		p.lastStart = start
	}
	p.seeded = true
	return errors.Join(errs...)
}
