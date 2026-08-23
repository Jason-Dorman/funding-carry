package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Jason-Dorman/funding-carry/internal/db"
)

// Level2Handler maintains the perp order book and writes a top-N snapshot every
// BOOK_SNAP_SECS.
//
// Persisting every level2 update was never the plan (API spec section 1.1): the
// feature engine reads imbalance and impact price, not a book replay, and the
// update rate would put two orders of magnitude more rows into TimescaleDB than
// anything reads.
//
// Subscribed for the perp only. The spot leg trades on Base against a DEX
// aggregator, so a Coinbase spot book would describe a venue this system never
// executes on.
type Level2Handler struct {
	product  string
	interval time.Duration
	sink     Sink

	book *book
	// ready is false until the venue has sent a snapshot on this connection.
	// Updates applied to an empty book would build a book made only of the
	// levels that happened to change, which looks like a thin market rather than
	// like missing data.
	ready bool
	// lastSnap is the boundary of the most recently written snapshot, so the
	// same boundary cannot be written twice.
	lastSnap time.Time
	// lastUpdate is when the venue last changed the book. Snapshots stop when it
	// goes stale; see maxBookAge.
	lastUpdate time.Time
}

// maxBookAge is how long the book may go without an update and still be
// snapshotted.
//
// Tick is driven by frame arrival and every connection carries heartbeats, so a
// level2 channel that goes silent while the socket stays up would otherwise
// leave this handler writing the same frozen book at every boundary, presented
// each time as a fresh observation of the market. A repeated row is a lie a
// consumer cannot detect; a missing row is a gap it can.
//
// The bound is Level2Quiet rather than a small multiple of the snapshot
// interval, and that is a correction the soak forced. At three intervals (30 s)
// it fired during ordinary lulls — the perp book was measured quiet for up to
// 51 s while perfectly healthy — so it removed real observations. Sharing one
// constant with the gap threshold keeps "this feed is quiet enough to report"
// and "this feed is quiet enough to stop recording" from drifting apart.
const maxBookAge = Level2Quiet

// NewLevel2Handler builds the book handler for one product.
func NewLevel2Handler(product string, interval time.Duration, sink Sink) *Level2Handler {
	return &Level2Handler{product: product, interval: interval, sink: sink, book: newBook()}
}

// Subscribe names the channel and the product this handler wants.
func (h *Level2Handler) Subscribe() (string, []string) { return channelLevel2, []string{h.product} }

// DataChannel is where the level2 subscription actually arrives: the venue names
// the channel "level2" on the way in and "l2_data" on the way out.
func (h *Level2Handler) DataChannel() string { return channelLevel2Data }

// Reset discards the book. This is the reset that matters most in this package:
// a book is the accumulation of every update since a snapshot, so one carried
// across a dropped connection is wrong in a way nothing downstream could detect.
func (h *Level2Handler) Reset() {
	h.book.reset()
	h.ready = false
	h.lastUpdate = time.Time{}
}

// Handle applies a snapshot or a batch of level updates to the book.
func (h *Level2Handler) Handle(_ context.Context, msg Message) error {
	var events []l2Event
	if err := json.Unmarshal(msg.Events, &events); err != nil {
		return fmt.Errorf("decode l2 events: %w", err)
	}

	var errs []error
	for _, e := range events {
		if e.ProductID != h.product {
			continue
		}
		if e.Type == "snapshot" {
			h.book.reset()
			h.ready = true
		}
		if !h.ready {
			continue
		}
		for _, u := range e.Updates {
			if err := h.applyUpdate(u); err != nil {
				errs = append(errs, err)
			}
		}
		h.lastUpdate = msg.RecvTS
	}
	return errors.Join(errs...)
}

func (h *Level2Handler) applyUpdate(u l2Level) error {
	px, err := requireDecimal("price_level", u.PriceLevel)
	if err != nil {
		return err
	}
	qty, err := requireDecimal("new_quantity", u.NewQuantity)
	if err != nil {
		return err
	}
	h.book.apply(u.Side, px, qty)
	return nil
}

// Tick writes a snapshot when a sampling boundary has passed.
//
// It is driven by frame arrival rather than by a timer of its own, which is only
// dependable because every connection subscribes to heartbeats and therefore
// carries a frame a second whatever the book is doing. The alternative — a
// goroutine with a ticker — would need a mutex around the book for no gain, and
// would keep writing snapshots of a book that had stopped being updated.
func (h *Level2Handler) Tick(ctx context.Context, now time.Time) error {
	if !h.ready {
		return nil
	}
	// A book nobody has updated recently is not an observation of a calm market,
	// because this handler cannot tell that apart from a channel that has died
	// with its socket still up — which is exactly what heartbeats make possible.
	if now.Sub(h.lastUpdate) > maxBookAge {
		return nil
	}
	// The row is stamped with the boundary, not with the moment the snapshot was
	// taken: (product_id, ts) is the row's identity, and a clock reading would
	// make every row unique and the constraint decorative (API spec section 5.3).
	boundary := now.UTC().Truncate(h.interval)
	if !boundary.After(h.lastSnap) {
		return nil
	}
	h.lastSnap = boundary
	return submit(ctx, h.sink, h.snapshot(boundary))
}

func (h *Level2Handler) snapshot(ts time.Time) db.BookSnapshotRow {
	bids := h.book.top("bid", bookDepth)
	asks := h.book.top("offer", bookDepth)

	bidPx, bidDepth := pxAndDepth(bids)
	askPx, askDepth := pxAndDepth(asks)

	return db.BookSnapshotRow{
		TS:            ts,
		ProductID:     h.product,
		BestBid:       best(bids),
		BestAsk:       best(asks),
		BidPx:         bidPx,
		BidDepth:      bidDepth,
		AskPx:         askPx,
		AskDepth:      askDepth,
		ImbalanceTopN: imbalance(bids, asks),
		ImpactBidPx:   impactPrice(bids),
		ImpactAskPx:   impactPrice(asks),
	}
}
