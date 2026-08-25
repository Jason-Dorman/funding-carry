package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// TickerHandler turns the ticker channel into quotes for the venue-state
// sampler. It writes no rows itself: cb_venue_state has one writer by design
// (see venuestate.go), and ticker messages arrive several times a second, far
// faster than that table is sampled.
type TickerHandler struct {
	products []string
	state    *VenueState
	// marks is the funding estimator's mid feed, and is nil when the REST half
	// of ingest is not running (Part 4 alone).
	marks *Marks
}

// NewTickerHandler subscribes both products: the perp is what is traded, and the
// spot reference is what the basis and the funding estimate are measured against.
func NewTickerHandler(products []string, state *VenueState, marks *Marks) *TickerHandler {
	return &TickerHandler{products: products, state: state, marks: marks}
}

// Subscribe names the channel and the products this handler wants.
func (h *TickerHandler) Subscribe() (string, []string) { return channelTicker, h.products }

// DataChannel is where ticker data arrives; here it matches the subscription.
func (h *TickerHandler) DataChannel() string { return channelTicker }

// Reset has nothing to drop. A quote is a point observation with no history
// behind it, and the venue sends a fresh snapshot on subscribe, so the first
// message after a reconnect replaces everything this handler holds. The
// staleness rule in the sampler is what covers the interval in between.
func (h *TickerHandler) Reset() {}

// Handle records each product's top of book with the sampler.
func (h *TickerHandler) Handle(_ context.Context, msg Message) error {
	var events []tickerEvent
	if err := json.Unmarshal(msg.Events, &events); err != nil {
		return fmt.Errorf("decode ticker events: %w", err)
	}

	// Every ticker in the message is applied, and the parse failures are
	// collected rather than returned at the first one: a malformed field on the
	// spot product must not throw away a good perp quote arriving beside it.
	var errs []error
	for _, e := range events {
		for _, t := range e.Tickers {
			q, err := quoteFrom(t, msg)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", t.ProductID, err))
				continue
			}
			h.state.Observe(t.ProductID, q)
			if h.marks != nil {
				// The same quote feeds the mid-TWAP fallback the venue's mark
				// definition falls back to when nothing trades.
				h.marks.ObserveQuote(t.ProductID, q)
			}
		}
	}
	return errors.Join(errs...)
}

func quoteFrom(t tickerWire, msg Message) (Quote, error) {
	bid, err := parseDecimal("best_bid", t.BestBid)
	if err != nil {
		return Quote{}, err
	}
	ask, err := parseDecimal("best_ask", t.BestAsk)
	if err != nil {
		return Quote{}, err
	}
	last, err := parseDecimal("price", t.Price)
	if err != nil {
		return Quote{}, err
	}
	return Quote{Bid: bid, Ask: ask, Last: last, At: msg.RecvTS, VenueT: msg.VenueTS}, nil
}
