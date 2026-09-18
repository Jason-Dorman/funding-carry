package fix

import (
	"math/rand/v2"
	"time"

	"github.com/shopspring/decimal"
)

// The fill model (API spec section 4.3), as pure functions over a touch and an
// order, plus the one seeded source of randomness the simulator has.
//
// It is separated from the engine that drives it because this is the half that
// has to be provable. "Fill decisions are deterministic under a fixed seed" is a
// claim about these functions, and the only way to demonstrate it is to be able
// to run them without a socket, a database or a clock.

// side is which way an order goes: FIX tag 54 and the fills.side column in one
// vocabulary. It is deliberately not the carry.Side of API spec section 3.2 —
// that type belongs to the initiator half of the system, which is Part 8, and
// the acceptor is a venue: it knows what arrived on the wire, not what the
// strategy meant by it.
type side string

const (
	buy  side = "buy"
	sell side = "sell"
)

// tif is the subset of TimeInForce the simulator honours. Anything else is
// refused at entry rather than silently treated as one of these.
type tif string

const (
	gtc tif = "GTC"
	ioc tif = "IOC"
)

// book is the top of the market one evaluation prices against: the two sides of
// the touch and the timestamp of the snapshot they came from.
type book struct {
	ts  time.Time
	bid decimal.Decimal
	ask decimal.Decimal
}

// age is how stale the snapshot is at now. It is measured against the venue
// timestamp the row carries, not against when the row was read, so a database
// that answers quickly with old data is still reported as old data.
func (b book) age(now time.Time) time.Duration { return now.Sub(b.ts) }

// one is the multiplicative identity, kept as a package value so the slippage
// arithmetic below reads as arithmetic.
var one = decimal.NewFromInt(1)

// marketable reports whether a limit order can print against the touch.
//
// It is "crosses or joins": a buy at exactly the ask fills. A venue with a real
// order queue would make joining depend on what is resting in front of you, and
// this simulator does not model a queue — claiming it did would produce fills
// nobody could predict from the row they were priced against, which is the one
// property a fill model has to have.
func marketable(s side, limitPx decimal.Decimal, b book) bool {
	if s == buy {
		return b.ask.LessThanOrEqual(limitPx)
	}
	return b.bid.GreaterThanOrEqual(limitPx)
}

// execPx is the price one slice prints at: the touch, moved adversely by the
// configured slippage, and never through the order's own limit.
//
// Two things are deliberate. Slippage is always applied *against* the order —
// up for a buy, down for a sell — because a simulator that sometimes improved a
// price would flatter exactly the number this system exists to measure. And the
// result is capped at the limit, because a limit order that printed worse than
// its limit is an execution the real venue could not have produced; at the
// venue you get your price or better.
func execPx(s side, limitPx decimal.Decimal, b book, slippageBps decimal.Decimal) decimal.Decimal {
	// Basis points are ten-thousandths, and Shift moves the decimal point
	// exactly where a Div by 10000 would round at DivisionPrecision.
	slip := slippageBps.Shift(-4)
	if s == buy {
		px := b.ask.Mul(one.Add(slip))
		if px.GreaterThan(limitPx) {
			return limitPx
		}
		return px
	}
	px := b.bid.Mul(one.Sub(slip))
	if px.LessThan(limitPx) {
		return limitPx
	}
	return px
}

// sliceSizes splits an order into the fills it will print: one report for an
// ordinary order, n for one above the partial threshold.
//
// The arithmetic is integer because the quantity is whole contracts — the perp
// leg is quantized (spec section 11) and the acceptor refuses a fractional one
// at entry — so the slices sum to the order exactly, with the remainder on the
// last of them rather than spread by a rounding rule nobody could reproduce.
//
// Two edges are handled rather than assumed away: an order that is over the
// threshold but smaller than n contracts is cut into as many whole contracts as
// it has, because a zero-quantity fill is not a fill; and a quantity that is not
// integral, which entry validation makes unreachable, prints as one slice rather
// than being silently truncated.
func sliceSizes(qty, threshold decimal.Decimal, n int) []decimal.Decimal {
	if n <= 1 || qty.LessThanOrEqual(threshold) || !qty.Equal(qty.Truncate(0)) {
		return []decimal.Decimal{qty}
	}

	q := qty.IntPart()
	k := int64(n)
	if k > q {
		k = q
	}
	if k <= 1 {
		return []decimal.Decimal{qty}
	}

	base := q / k
	out := make([]decimal.Decimal, k)
	for i := range out[:k-1] {
		out[i] = decimal.NewFromInt(base)
	}
	out[k-1] = decimal.NewFromInt(q - base*(k-1))
	return out
}

// jitterLatency spreads the configured latency over [d/2, 3d/2).
//
// Centred on the configured value rather than the top-half scheme the reconnect
// backoff uses, because the two are answering different questions: backoff
// jitter exists to decorrelate sockets and must keep a floor, while this is
// modelling a venue whose response time varies around a mean. A configured 20 ms
// that only ever produced 10-20 ms would be a 15 ms venue wearing a 20 ms label.
//
// math/rand, not crypto/rand, and seeded by the caller: determinism under a
// fixed seed is the requirement (API spec section 4.3).
func jitterLatency(rng *rand.Rand, d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return d/2 + time.Duration(rng.Int64N(int64(d)))
}
