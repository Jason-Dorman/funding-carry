package ingest

import (
	"sort"

	"github.com/shopspring/decimal"
)

// bookDepth is the N in "top-N book snapshot": how many price levels per side
// are stored, and — see impactPrice — the depth the impact prices are measured
// over.
//
// Ten levels covers the range the microstructure features read (touch imbalance,
// impact imbalance, a slippage estimate for a clip this system will never
// exceed) while keeping a snapshot row small enough to write every few seconds
// for months.
const bookDepth = 10

// level is one price level.
type level struct {
	px  decimal.Decimal
	qty decimal.Decimal
}

// book is the order book assembled from the level2 channel: a snapshot followed
// by updates, each update setting a level's new quantity, with zero meaning the
// level is gone.
//
// Levels are keyed by the canonical decimal rendering of the price rather than
// by the venue's string, so "2357.5" and "2357.50" cannot become two levels at
// the same price.
type book struct {
	bids map[string]level
	asks map[string]level
}

func newBook() *book {
	return &book{bids: map[string]level{}, asks: map[string]level{}}
}

// reset empties the book. A level2 snapshot replaces the whole state, and so
// does a reconnect: a book carried across a dropped socket would be missing
// every update that happened while it was down, and there is no way to tell
// from the book itself that it is wrong.
func (b *book) reset() {
	clear(b.bids)
	clear(b.asks)
}

// apply sets one level. It is the only mutation the level2 channel has.
func (b *book) apply(side string, px, qty decimal.Decimal) {
	m := b.sideMap(side)
	if m == nil {
		return
	}
	key := px.String()
	if qty.IsZero() {
		delete(m, key)
		return
	}
	m[key] = level{px: px, qty: qty}
}

// sideMap maps the venue's side names. The level2 channel says "bid" and
// "offer"; an unrecognised side returns nil so a vocabulary change shows up as
// a book that stops updating rather than as levels silently landing on the
// wrong side.
func (b *book) sideMap(side string) map[string]level {
	switch side {
	case "bid":
		return b.bids
	case "offer", "ask":
		return b.asks
	default:
		return nil
	}
}

// top returns the best N levels of a side: bids descending, asks ascending.
func (b *book) top(side string, n int) []level {
	m := b.bids
	descending := true
	if side != "bid" {
		m, descending = b.asks, false
	}

	levels := make([]level, 0, len(m))
	for _, l := range m {
		levels = append(levels, l)
	}
	sort.Slice(levels, func(i, j int) bool {
		if descending {
			return levels[i].px.GreaterThan(levels[j].px)
		}
		return levels[i].px.LessThan(levels[j].px)
	})
	if len(levels) > n {
		levels = levels[:n]
	}
	return levels
}

// pxAndDepth splits levels into the two parallel arrays the snapshot row stores.
func pxAndDepth(levels []level) (px, depth []decimal.Decimal) {
	px = make([]decimal.Decimal, len(levels))
	depth = make([]decimal.Decimal, len(levels))
	for i, l := range levels {
		px[i], depth[i] = l.px, l.qty
	}
	return px, depth
}

// totalDepth sums the sizes.
func totalDepth(levels []level) decimal.Decimal {
	sum := decimal.Zero
	for _, l := range levels {
		sum = sum.Add(l.qty)
	}
	return sum
}

// imbalance is (bid − ask) / (bid + ask) over the stored depth: +1 is all bid,
// −1 all ask, 0 balanced. Undefined on an empty book, which reads NULL.
func imbalance(bids, asks []level) decimal.NullDecimal {
	bid, ask := totalDepth(bids), totalDepth(asks)
	total := bid.Add(ask)
	if total.IsZero() {
		return decimal.NullDecimal{}
	}
	return decimal.NullDecimal{Decimal: bid.Sub(ask).Div(total), Valid: true}
}

// impactPrice is the depth-weighted average price of the levels handed to it:
// what a trade large enough to consume exactly this much of the book would
// average.
//
// The depth is the same top-N the snapshot stores, and that is a deliberate
// choice over the more usual "price to fill a fixed notional". A fixed notional
// needs a size constant that means nothing to anyone reading it, and any size
// this system will actually trade — a whole position is a handful of contracts
// against a touch holding hundreds — is consumed entirely by the best level, so
// the column would just restate best_bid and carry no information at all.
// Measuring over the stored depth instead gives a number that moves when the
// book's mass moves, and it is reproducible from bid_px/bid_depth by anyone
// reading the row, which a hidden constant would not be.
func impactPrice(levels []level) decimal.NullDecimal {
	depth := totalDepth(levels)
	if depth.IsZero() {
		return decimal.NullDecimal{}
	}
	notional := decimal.Zero
	for _, l := range levels {
		notional = notional.Add(l.px.Mul(l.qty))
	}
	return decimal.NullDecimal{Decimal: notional.Div(depth), Valid: true}
}

// best returns the touch price of a side, or NULL if that side is empty.
func best(levels []level) decimal.NullDecimal {
	if len(levels) == 0 {
		return decimal.NullDecimal{}
	}
	return decimal.NullDecimal{Decimal: levels[0].px, Valid: true}
}
