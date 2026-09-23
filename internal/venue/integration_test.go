//go:build integration

package venue

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/Jason-Dorman/funding-carry/internal/db"
	"github.com/Jason-Dorman/funding-carry/internal/db/dbtest"
)

// The part's acceptance test, end to end against a real schema: rows seeded
// through the real writer, read back by the real reader, judged by the cache.
// Two rows per series, so "newest" is proven rather than assumed; then the
// clock moves past STALE_FEED_SECS and every flag trips.
func TestRefreshReadsSeededRowsAndTripsStalenessAtTheLimit(t *testing.T) {
	pool := dbtest.Open(t, "carry_integration_venue")
	older := t0.Add(-5 * time.Second)
	dbtest.Seed(t, pool,
		db.VenueStateRow{TS: older, ProductID: testPerp, Mid: num("2700.00"), FuturesMark: num("2699.00")},
		db.VenueStateRow{TS: t0, ProductID: testPerp, Mid: num("2742.50"),
			FuturesMark: num("2738.4945066781559672"), SpotMark: num("2738.7169524818549976"),
			FundingRateHourly: num("0.000014"), FundingRateEst: num("0.0000156029036177"),
			FundingSource: db.FundingSourceVenue, OpenInterest: num("302010")},
		db.VenueStateRow{TS: older, ProductID: testSpot, Mid: num("2700.10")},
		db.VenueStateRow{TS: t0, ProductID: testSpot, Mid: num("2742.005"), SpreadBps: num("0.036")},
		db.AccountStateRow{TS: older, AvailableMargin: num("1.00")},
		db.AccountStateRow{TS: t0, AvailableMargin: num("3.15"), LiquidationThreshold: num("0"),
			IntradayMarginEnabled: db.Opt(false)},
		db.BaseStateRow{TS: older, SpotPx: num("2700.20"), SpotPxSource: db.SpotSourceCoinbase},
		db.BaseStateRow{TS: t0, SpotPx: num("2738.5415794350070664"), SpotPxSource: db.SpotSourceDEX,
			WalletETH: num("0.002274242082897966"), GasGwei: num("0.006")},
		db.ProductRow{ProductID: testPerp, ContractSize: num("0.10"), Tick: num("0.5"), UpdatedAt: older},
	)

	c, clk := newCache(db.NewReader(pool), t0.Add(3*time.Second), nil)
	started := time.Now()
	if err := c.Refresh(t.Context()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	t.Logf("refresh over five seeded sources took %s", time.Since(started))

	s := c.Latest()
	if !s.Perp.TS.Equal(t0) || !s.Perp.FuturesMark.Decimal.Equal(decimal.RequireFromString("2738.4945066781559672")) {
		t.Errorf("Perp = row at %s mark %s, want the newest row at t0", s.Perp.TS, s.Perp.FuturesMark.Decimal)
	}
	if s.Perp.FundingSource != db.FundingSourceVenue || !s.Perp.FundingRateEst.Decimal.Equal(decimal.RequireFromString("0.0000156029036177")) {
		t.Errorf("Perp funding source %q est %s, want venue / the seeded estimate", s.Perp.FundingSource, s.Perp.FundingRateEst.Decimal)
	}
	if !s.Spot.TS.Equal(t0) || !s.Spot.Mid.Decimal.Equal(decimal.RequireFromString("2742.005")) {
		t.Errorf("Spot = row at %s mid %s, want the newest row at t0", s.Spot.TS, s.Spot.Mid.Decimal)
	}
	if !s.Account.TS.Equal(t0) || !s.Account.AvailableMargin.Decimal.Equal(decimal.RequireFromString("3.15")) || s.Account.MarginRatio.Valid {
		t.Errorf("Account = %+v, want the newest row with a NULL margin ratio", s.Account)
	}
	if s.Account.IntradayMarginEnabled == nil || *s.Account.IntradayMarginEnabled {
		t.Error("Account.IntradayMarginEnabled is not the stored false")
	}
	if !s.Wallet.TS.Equal(t0) || s.Wallet.SpotPxSource != db.SpotSourceDEX ||
		!s.Wallet.WalletETH.Decimal.Equal(decimal.RequireFromString("0.002274242082897966")) {
		t.Errorf("Wallet = %+v, want the newest row from the dex", s.Wallet)
	}
	// Scale survives the round trip: 0.10 comes back as 0.10, not 0.1 (API
	// spec section 3.5).
	if !s.ProductKnown || s.Product.ContractSize.Decimal.String() != "0.1" || s.Product.ContractSize.Decimal.Exponent() != -2 {
		t.Errorf("Product known=%v size=%s exp=%d, want known, 0.10 at scale 2",
			s.ProductKnown, s.Product.ContractSize.Decimal, s.Product.ContractSize.Decimal.Exponent())
	}
	for _, src := range Sources {
		if f := s.Staleness.Of(src); f.Stale || f.Age != 3*time.Second {
			t.Errorf("%s: %+v, want fresh at age 3s", src, f)
		}
	}
	if !s.Staleness.FeedOK() {
		t.Error("FeedOK = false with every source three seconds old")
	}

	// Ingest stops: nothing new is written, the clock moves on, and at
	// STALE_FEED_SECS plus one the flags trip.
	clk.Set(t0.Add(testLimit + time.Second))
	if err := c.Refresh(t.Context()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	s = c.Latest()
	for _, src := range Sources {
		if f := s.Staleness.Of(src); !f.Stale || f.Missing {
			t.Errorf("%s past the limit: %+v, want stale, not missing", src, f)
		}
	}
	if s.Staleness.FeedOK() {
		t.Error("FeedOK = true with every source past the limit")
	}

	// Nothing recorded for a product is missing, not an error.
	c2, _ := newCache(db.NewReader(pool), t0, nil)
	c2.opts.PerpProduct = "NOTHING-RECORDED"
	if err := c2.Refresh(t.Context()); err != nil {
		t.Fatalf("refresh with an unrecorded product: %v", err)
	}
	if s := c2.Latest(); !s.Staleness.Perp.Missing || s.ProductKnown {
		t.Errorf("unrecorded product: perp %+v known=%v, want missing and unknown", s.Staleness.Perp, s.ProductKnown)
	}
}
