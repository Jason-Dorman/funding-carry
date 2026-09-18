package fix

import (
	"math/rand/v2"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

// The fill model's rules, one case per behaviour, so a reviewer can reconstruct
// API spec section 4.3 from the table (testing strategy).

func TestMarketableIsCrossOrJoin(t *testing.T) {
	t.Parallel()

	b := book{bid: dec("2344.50"), ask: dec("2345.00")}
	for _, tc := range []struct {
		name    string
		side    side
		limitPx string
		want    bool
	}{
		{"buy through the ask", buy, "2346.00", true},
		{"buy at the ask joins", buy, "2345.00", true},
		{"buy under the ask rests", buy, "2344.99", false},
		{"sell through the bid", sell, "2344.00", true},
		{"sell at the bid joins", sell, "2344.50", true},
		{"sell over the bid rests", sell, "2344.51", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := marketable(tc.side, dec(tc.limitPx), b); got != tc.want {
				t.Errorf("marketable = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSlippageIsAdverseAndNeverThroughTheLimit(t *testing.T) {
	t.Parallel()

	b := book{bid: dec("2344.50"), ask: dec("2345.00")}
	for _, tc := range []struct {
		name    string
		side    side
		limitPx string
		bps     string
		want    string
	}{
		// 2 bps of 2345 is 0.469, paid by the buyer.
		{"buy pays the touch plus slippage", buy, "2350.00", "2", "2345.469"},
		// and received worse by the seller: 2 bps of 2344.50 is 0.46890.
		{"sell receives the touch less slippage", sell, "2340.00", "2", "2344.03110"},
		// A limit that only just crosses caps the fill at the limit rather than
		// printing through it.
		{"buy is capped at its limit", buy, "2345.10", "2", "2345.10"},
		{"sell is floored at its limit", sell, "2344.40", "2", "2344.40"},
		// Zero slippage is the frictionless baseline, and it prints the touch.
		{"no slippage prints the touch", buy, "2350.00", "0", "2345"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := execPx(tc.side, dec(tc.limitPx), b, dec(tc.bps))
			if !got.Equal(dec(tc.want)) {
				t.Errorf("execPx = %s, want %s", got, tc.want)
			}
		})
	}
}

// The slices are what "orders larger than X fill in N slices" means in practice,
// and the property that matters is that they sum to the order: a partial
// sequence that lost or invented a contract would be a position the system
// cannot reconcile.
func TestSliceSizesSumToTheOrder(t *testing.T) {
	t.Parallel()

	threshold := dec("10")
	for _, tc := range []struct {
		name  string
		qty   string
		n     int
		want  []string
		total string
	}{
		{"at the threshold prints once", "10", 3, []string{"10"}, "10"},
		{"under the threshold prints once", "3", 3, []string{"3"}, "3"},
		{"an even split", "33", 3, []string{"11", "11", "11"}, "33"},
		// The remainder lands on the last slice rather than being spread by a
		// rule nobody could reproduce from the order.
		{"the remainder is on the last slice", "35", 3, []string{"11", "11", "13"}, "35"},
		// Over the threshold but smaller than the slice count: the order is cut
		// into as many whole contracts as it has, because a zero-quantity fill
		// is not a fill.
		{"fewer contracts than slices", "11", 20, []string{
			"1", "1", "1", "1", "1", "1", "1", "1", "1", "1", "1",
		}, "11"},
		{"one slice configured", "35", 1, []string{"35"}, "35"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := sliceSizes(dec(tc.qty), threshold, tc.n)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d slices %v, want %d %v", len(got), got, len(tc.want), tc.want)
			}
			total := decimal.Zero
			for i, g := range got {
				if !g.Equal(dec(tc.want[i])) {
					t.Errorf("slice %d = %s, want %s", i, g, tc.want[i])
				}
				if !g.IsPositive() {
					t.Errorf("slice %d is %s: a fill of nothing is not a fill", i, g)
				}
				total = total.Add(g)
			}
			if !total.Equal(dec(tc.total)) {
				t.Errorf("slices sum to %s, want %s", total, tc.total)
			}
		})
	}
}

// Entry validation makes a fractional quantity unreachable, but the model is
// reached from two places and the defensive answer matters: one slice of exactly
// what was asked, never a silent truncation to whole contracts.
func TestAFractionalQuantityIsNotSliced(t *testing.T) {
	t.Parallel()

	got := sliceSizes(dec("12.5"), dec("10"), 3)
	if len(got) != 1 || !got[0].Equal(dec("12.5")) {
		t.Fatalf("sliceSizes = %v, want one slice of 12.5", got)
	}
}

func TestLatencyJitterStaysAroundTheConfiguredValue(t *testing.T) {
	t.Parallel()

	rng := rand.New(rand.NewPCG(testSeed, testSeed))
	const latency = 20 * time.Millisecond
	for range 1000 {
		got := jitterLatency(rng, latency)
		if got < latency/2 || got >= latency+latency/2 {
			t.Fatalf("jitter = %s, outside [%s, %s)", got, latency/2, latency+latency/2)
		}
	}
	if got := jitterLatency(rng, 0); got != 0 {
		t.Errorf("jitter of a zero latency = %s, want 0", got)
	}
}

// Determinism is the requirement the seed exists for: the same seed produces the
// same sequence, and a different one does not.
func TestJitterIsDeterministicUnderASeed(t *testing.T) {
	t.Parallel()

	draw := func(seed uint64) []time.Duration {
		rng := rand.New(rand.NewPCG(seed, seed))
		out := make([]time.Duration, 10)
		for i := range out {
			out[i] = jitterLatency(rng, 20*time.Millisecond)
		}
		return out
	}

	first, second := draw(testSeed), draw(testSeed)
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("draw %d differs between runs of the same seed: %s vs %s", i, first[i], second[i])
		}
	}
	if same(first, draw(testSeed+1)) {
		t.Error("a different seed produced the same sequence; the seed is not reaching the generator")
	}
}

func same(a, b []time.Duration) bool {
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func dec(s string) decimal.Decimal { return decimal.RequireFromString(s) }
