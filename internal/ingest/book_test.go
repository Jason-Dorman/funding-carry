package ingest

import (
	"testing"

	"github.com/shopspring/decimal"
)

// dec is the only way a decimal is built in these tests: from a string, never
// from a float (spec section 11, enforced module-wide by internal/guard).
func dec(t *testing.T, s string) decimal.Decimal {
	t.Helper()
	d, err := decimal.NewFromString(s)
	if err != nil {
		t.Fatalf("decimal %q: %v", s, err)
	}
	return d
}

// testBook is the book both the ordering and the derived-value tests read.
//
//	bids  100 @ 2, 99 @ 3, 98 @ 5
//	asks  101 @ 1, 102 @ 4
func testBook(t *testing.T) *book {
	t.Helper()
	b := newBook()
	for _, l := range []struct{ side, px, qty string }{
		{"bid", "98", "5"},
		{"bid", "100", "2"},
		{"bid", "99", "3"},
		{"offer", "102", "4"},
		{"offer", "101", "1"},
	} {
		b.apply(l.side, dec(t, l.px), dec(t, l.qty))
	}
	return b
}

func TestBookTopIsSortedInwards(t *testing.T) {
	b := testBook(t)

	bids := b.top("bid", 3)
	wantBids := []string{"100", "99", "98"}
	for i, l := range bids {
		if l.px.String() != wantBids[i] {
			t.Errorf("bid %d = %s, want %s (bids descend from the touch)", i, l.px, wantBids[i])
		}
	}

	asks := b.top("offer", 3)
	wantAsks := []string{"101", "102"}
	for i, l := range asks {
		if l.px.String() != wantAsks[i] {
			t.Errorf("ask %d = %s, want %s (asks ascend from the touch)", i, l.px, wantAsks[i])
		}
	}
}

func TestBookTopIsCappedAtN(t *testing.T) {
	if got := len(testBook(t).top("bid", 2)); got != 2 {
		t.Errorf("top(bid, 2) returned %d levels, want 2", got)
	}
}

func TestZeroQuantityRemovesALevel(t *testing.T) {
	b := testBook(t)
	b.apply("bid", dec(t, "100"), decimal.Zero)

	best := b.top("bid", 1)
	if len(best) != 1 || best[0].px.String() != "99" {
		t.Fatalf("best bid after removing 100 = %v, want 99", best)
	}
}

func TestLevelsAreKeyedByValueNotByTheVenueString(t *testing.T) {
	b := newBook()
	b.apply("bid", dec(t, "2357.5"), dec(t, "10"))
	b.apply("bid", dec(t, "2357.50"), dec(t, "4"))

	// Two renderings of one price are one level. Keying by the raw string would
	// leave both in the book and double the depth reported at that price.
	levels := b.top("bid", 5)
	if len(levels) != 1 {
		t.Fatalf("levels = %d, want 1: 2357.5 and 2357.50 are the same price", len(levels))
	}
	if got := levels[0].qty.String(); got != "4" {
		t.Errorf("depth = %s, want 4 (the later update replaces the earlier)", got)
	}
}

func TestAnUnknownSideIsIgnored(t *testing.T) {
	b := newBook()
	b.apply("buy", dec(t, "100"), dec(t, "1"))

	// If the venue ever renames a side, the book must go empty and visibly
	// stale rather than quietly file the level on the wrong side, where it
	// would invert the imbalance and the impact price.
	if len(b.top("bid", 5))+len(b.top("offer", 5)) != 0 {
		t.Error("a level with an unrecognised side was accepted")
	}
}

func TestResetEmptiesTheBook(t *testing.T) {
	b := testBook(t)
	b.reset()
	if len(b.top("bid", 5))+len(b.top("offer", 5)) != 0 {
		t.Error("reset left levels behind; a book carried across a reconnect is wrong and undetectably so")
	}
}

func TestImbalanceAndImpactOverTheStoredDepth(t *testing.T) {
	b := testBook(t)
	bids := b.top("bid", 2)   // 100 @ 2, 99 @ 3 -> depth 5
	asks := b.top("offer", 2) // 101 @ 1, 102 @ 4 -> depth 5

	if got := imbalance(bids, asks); !got.Valid || !got.Decimal.Equal(decimal.Zero) {
		t.Errorf("imbalance = %v, want 0 for equal depth", got)
	}

	// (100*2 + 99*3) / 5 = 497/5
	if got := impactPrice(bids); !got.Valid || !got.Decimal.Equal(dec(t, "99.4")) {
		t.Errorf("impact bid = %v, want 99.4", got)
	}
	// (101*1 + 102*4) / 5 = 509/5
	if got := impactPrice(asks); !got.Valid || !got.Decimal.Equal(dec(t, "101.8")) {
		t.Errorf("impact ask = %v, want 101.8", got)
	}
}

func TestImbalanceIsSignedTowardsTheHeavierSide(t *testing.T) {
	b := newBook()
	b.apply("bid", dec(t, "100"), dec(t, "3"))
	b.apply("offer", dec(t, "101"), dec(t, "1"))

	// (3 - 1) / 4
	got := imbalance(b.top("bid", 5), b.top("offer", 5))
	if !got.Valid || !got.Decimal.Equal(dec(t, "0.5")) {
		t.Errorf("imbalance = %v, want 0.5 with the bid three times the ask", got)
	}
}

func TestEmptySideReadsAsNull(t *testing.T) {
	// NULL rather than zero: an empty side is a book this process could not see,
	// and a zero imbalance would say the market was balanced.
	if got := imbalance(nil, nil); got.Valid {
		t.Errorf("imbalance of an empty book = %v, want NULL", got)
	}
	if got := impactPrice(nil); got.Valid {
		t.Errorf("impact price of an empty side = %v, want NULL", got)
	}
	if got := best(nil); got.Valid {
		t.Errorf("best of an empty side = %v, want NULL", got)
	}
}
