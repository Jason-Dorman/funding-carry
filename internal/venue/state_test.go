package venue

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

// The cache's job in one test: every source's newest row, copied through
// unchanged, with its age measured from the row's own timestamp.
func TestRefreshCopiesTheNewestRowOfEverySource(t *testing.T) {
	store := fullStore()
	c, _ := newCache(store, t0.Add(3*time.Second), nil)

	if err := c.Refresh(t.Context()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	s := c.Latest()

	if !s.Perp.FuturesMark.Decimal.Equal(decimal.RequireFromString("2738.4945066781559672")) {
		t.Errorf("Perp.FuturesMark = %s, want the seeded mark", s.Perp.FuturesMark.Decimal)
	}
	if s.Perp.FundingSource != "venue" || !s.Perp.FundingRateHourly.Decimal.Equal(decimal.RequireFromString("0.000014")) {
		t.Errorf("Perp funding = %s (%s), want 0.000014 (venue)", s.Perp.FundingRateHourly.Decimal, s.Perp.FundingSource)
	}
	if !s.Perp.FundingRateEst.Decimal.Equal(decimal.RequireFromString("0.0000156029036177")) {
		t.Errorf("Perp.FundingRateEst = %s, want the seeded estimate", s.Perp.FundingRateEst.Decimal)
	}
	if !s.Spot.Mid.Decimal.Equal(decimal.RequireFromString("2742.005")) {
		t.Errorf("Spot.Mid = %s, want 2742.005", s.Spot.Mid.Decimal)
	}
	// NULL in, NULL out: an empty account has no margin ratio, and the cache
	// must not invent one (API spec section 5.2).
	if s.Account.MarginRatio.Valid {
		t.Errorf("Account.MarginRatio = %s, want NULL", s.Account.MarginRatio.Decimal)
	}
	if !s.Account.AvailableMargin.Decimal.Equal(decimal.RequireFromString("3.15")) {
		t.Errorf("Account.AvailableMargin = %s, want 3.15", s.Account.AvailableMargin.Decimal)
	}
	if !s.Wallet.WalletETH.Decimal.Equal(decimal.RequireFromString("0.002274242082897966")) || s.Wallet.SpotPxSource != "dex" {
		t.Errorf("Wallet = %s ETH, spot from %q; want the seeded row", s.Wallet.WalletETH.Decimal, s.Wallet.SpotPxSource)
	}
	if !s.ProductKnown || !s.Product.ContractSize.Decimal.Equal(decimal.RequireFromString("0.1")) {
		t.Errorf("Product known=%v size=%s, want known, 0.1", s.ProductKnown, s.Product.ContractSize.Decimal)
	}
	if s.Product.MakerFeeBps.Valid {
		t.Error("Product.MakerFeeBps is set; the venue has never confirmed a fee and the cache must not supply one")
	}

	if !s.RefreshedAt.Equal(t0.Add(3 * time.Second)) {
		t.Errorf("RefreshedAt = %s, want the clock", s.RefreshedAt)
	}
	for _, src := range Sources {
		f := s.Staleness.Of(src)
		if f.Missing || f.Stale || f.Age != 3*time.Second || !f.At.Equal(t0) {
			t.Errorf("%s: %+v, want fresh, age 3s, at t0", src, f)
		}
	}
	if !s.Staleness.FeedOK() {
		t.Error("FeedOK = false with every source fresh")
	}
}

// A row exactly STALE_FEED_SECS old is on the limit; one past it is stale.
func TestStalenessTripsJustPastTheLimit(t *testing.T) {
	for _, tc := range []struct {
		name  string
		age   time.Duration
		stale bool
	}{
		{"fresh", time.Second, false},
		{"one short of the limit", testLimit - time.Nanosecond, false},
		{"exactly the limit", testLimit, false},
		{"one past the limit", testLimit + time.Nanosecond, true},
		{"long past", 24 * time.Hour, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newCache(fullStore(), t0.Add(tc.age), nil)
			if err := c.Refresh(t.Context()); err != nil {
				t.Fatalf("refresh: %v", err)
			}
			s := c.Latest()
			for _, src := range Sources {
				if got := s.Staleness.Of(src).Stale; got != tc.stale {
					t.Errorf("%s at age %s: stale = %v, want %v", src, tc.age, got, tc.stale)
				}
			}
			if got := s.Staleness.FeedOK(); got != !tc.stale {
				t.Errorf("FeedOK = %v at age %s", got, tc.age)
			}
		})
	}
}

// FeedOK is the market data. A missing account or wallet is reported on its
// own flag and does not block the feed: the public stack has neither, and a
// margin check that has no margin ratio is the size check's problem, not the
// feed's (Part 9 decision).
func TestFeedOKCoversTheMarketDataSourcesOnly(t *testing.T) {
	for _, tc := range []struct {
		missing Source
		feedOK  bool
	}{
		{SourcePerp, false},
		{SourceSpot, false},
		{SourceAccount, true},
		{SourceWallet, true},
	} {
		t.Run(string(tc.missing)+" missing", func(t *testing.T) {
			store := fullStore()
			switch tc.missing {
			case SourcePerp:
				store.havePerp = false
			case SourceSpot:
				store.haveSpot = false
			case SourceAccount:
				store.haveAccount = false
			case SourceWallet:
				store.haveWallet = false
			}
			c, _ := newCache(store, t0.Add(time.Second), nil)
			if err := c.Refresh(t.Context()); err != nil {
				t.Fatalf("refresh: %v", err)
			}
			s := c.Latest()
			f := s.Staleness.Of(tc.missing)
			if !f.Missing || !f.Stale {
				t.Errorf("%s: %+v, want missing and stale", tc.missing, f)
			}
			for _, src := range Sources {
				if src != tc.missing && s.Staleness.Of(src).Stale {
					t.Errorf("%s is stale with a fresh row", src)
				}
			}
			if got := s.Staleness.FeedOK(); got != tc.feedOK {
				t.Errorf("FeedOK = %v with %s missing, want %v", got, tc.missing, tc.feedOK)
			}
		})
	}
}

// The degradation rule. A database that stops answering must look, from the
// risk engine's side, exactly like a feed that stopped writing: the last good
// rows stay, their ages keep growing, and the flags trip at the limit — not
// frozen at "fresh" because nothing could overwrite them, and not thrown away
// because a query failed.
func TestAFailedReadKeepsTheLastRowAndLetsItAge(t *testing.T) {
	store := fullStore()
	c, clk := newCache(store, t0.Add(time.Second), nil)
	if err := c.Refresh(t.Context()); err != nil {
		t.Fatalf("first refresh: %v", err)
	}

	dbDown := errors.New("connection refused")
	store.fail(dbDown)

	// Inside the limit: the failure is reported, the rows are kept, nothing
	// is stale yet.
	clk.Set(t0.Add(30 * time.Second))
	err := c.Refresh(t.Context())
	if !errors.Is(err, dbDown) {
		t.Fatalf("refresh error = %v, want the store's failure wrapped", err)
	}
	s := c.Latest()
	if !s.Perp.FuturesMark.Valid || s.Staleness.Perp.Stale || s.Staleness.Perp.Age != 30*time.Second {
		t.Errorf("after a failed read inside the limit: mark valid=%v, %+v; want the last row kept, age 30s, fresh",
			s.Perp.FuturesMark.Valid, s.Staleness.Perp)
	}
	if !s.RefreshedAt.Equal(t0.Add(30 * time.Second)) {
		t.Errorf("RefreshedAt = %s, want advanced to the failed refresh's clock", s.RefreshedAt)
	}

	// Past the limit, still failing: every source trips on the age of the row
	// it last read.
	clk.Set(t0.Add(testLimit + time.Second))
	if err := c.Refresh(t.Context()); !errors.Is(err, dbDown) {
		t.Fatalf("refresh error = %v, want the store's failure wrapped", err)
	}
	s = c.Latest()
	for _, src := range Sources {
		f := s.Staleness.Of(src)
		if f.Missing || !f.Stale {
			t.Errorf("%s past the limit with the database down: %+v, want kept and stale", src, f)
		}
	}
	if s.Staleness.FeedOK() {
		t.Error("FeedOK = true with the database down for longer than the limit")
	}
	if !s.Perp.FuturesMark.Decimal.Equal(decimal.RequireFromString("2738.4945066781559672")) {
		t.Error("the last good mark was thrown away; hard-stop evaluation continues on last good data")
	}

	// Recovery: newer rows, read successfully, and everything is fresh again.
	store.fail(nil)
	later := t0.Add(2 * testLimit)
	store.mu.Lock()
	store.perp.TS, store.spot.TS, store.account.TS, store.wallet.TS = later, later, later, later
	store.mu.Unlock()
	clk.Set(later.Add(time.Second))
	if err := c.Refresh(t.Context()); err != nil {
		t.Fatalf("refresh after recovery: %v", err)
	}
	if s := c.Latest(); !s.Staleness.FeedOK() || s.Staleness.Wallet.Stale {
		t.Errorf("after recovery: %+v, want everything fresh", s.Staleness)
	}
}

// A refresh that never succeeded has nothing to keep: every source is missing,
// and the error says so.
func TestAFailedFirstRefreshLeavesEverythingMissing(t *testing.T) {
	store := fullStore()
	store.fail(errors.New("connection refused"))
	c, _ := newCache(store, t0, nil)
	if err := c.Refresh(t.Context()); err == nil {
		t.Fatal("refresh returned nil with every read failing")
	}
	s := c.Latest()
	for _, src := range Sources {
		if f := s.Staleness.Of(src); !f.Missing || !f.Stale {
			t.Errorf("%s: %+v, want missing and stale", src, f)
		}
	}
	if s.ProductKnown || s.Staleness.FeedOK() {
		t.Errorf("ProductKnown=%v FeedOK=%v after a failed first refresh, want neither", s.ProductKnown, s.Staleness.FeedOK())
	}
}

// Before anything has been read, the flags must say "unknown", which for a
// staleness flag means stale — the zero value of a bool reads as fresh.
func TestBeforeTheFirstRefreshEverythingIsStale(t *testing.T) {
	c, _ := newCache(fullStore(), t0, nil)
	s := c.Latest()
	if !s.RefreshedAt.IsZero() {
		t.Errorf("RefreshedAt = %s before any refresh", s.RefreshedAt)
	}
	for _, src := range Sources {
		if f := s.Staleness.Of(src); !f.Missing || !f.Stale {
			t.Errorf("%s before the first refresh: %+v, want missing and stale", src, f)
		}
	}
	if s.Staleness.FeedOK() {
		t.Error("FeedOK = true before anything was read")
	}
}

// A source that had a row and now has none reads as missing, not as its last
// value forever. The failure case above keeps the last row; this is the
// success case that says "there is nothing", and the two must not be confused.
func TestASourceThatDisappearsReadsAsMissing(t *testing.T) {
	store := fullStore()
	c, clk := newCache(store, t0.Add(time.Second), nil)
	if err := c.Refresh(t.Context()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	store.mu.Lock()
	store.haveAccount = false
	store.mu.Unlock()
	clk.Set(t0.Add(2 * time.Second))
	if err := c.Refresh(t.Context()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	s := c.Latest()
	if !s.Staleness.Account.Missing || s.Account.AvailableMargin.Valid {
		t.Errorf("account after its row went away: %+v, margin valid=%v; want missing with no row",
			s.Staleness.Account, s.Account.AvailableMargin.Valid)
	}
	if s.Staleness.Perp.Missing || !s.Staleness.FeedOK() {
		t.Error("the other sources were disturbed by one going missing")
	}
}

// Cancellation is a failure of every read, and is handled as one: the state
// is untouched apart from its ages.
func TestRefreshHonoursCancellation(t *testing.T) {
	c, _ := newCache(fullStore(), t0, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.Refresh(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("refresh on a canceled context = %v, want context.Canceled", err)
	}
	if s := c.Latest(); s.Perp.Mid.Valid || !s.Staleness.Perp.Missing {
		t.Error("a canceled refresh changed the rows")
	}
}

// An unknown source name is reported stale rather than fresh: the safe answer
// for a flag the risk engine might look up by a misspelled label.
func TestUnknownSourceIsStale(t *testing.T) {
	if f := (Staleness{}).Of(Source("nope")); !f.Missing || !f.Stale {
		t.Errorf("Of(unknown) = %+v, want missing and stale", f)
	}
}

// TradeOK is the conjunction order flow gates on. FeedOK alone is the
// public-stack reading; a stale account or wallet must fail TradeOK while
// leaving FeedOK true, so the two cannot be confused for each other.
func TestTradeOKRequiresEverySourceFresh(t *testing.T) {
	for _, tc := range []struct {
		name    string
		missing Source
		feedOK  bool
		tradeOK bool
	}{
		{"all fresh", "", true, true},
		{"perp missing", SourcePerp, false, false},
		{"spot missing", SourceSpot, false, false},
		{"account missing", SourceAccount, true, false},
		{"wallet missing", SourceWallet, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := fullStore()
			switch tc.missing {
			case SourcePerp:
				store.havePerp = false
			case SourceSpot:
				store.haveSpot = false
			case SourceAccount:
				store.haveAccount = false
			case SourceWallet:
				store.haveWallet = false
			}
			c, _ := newCache(store, t0.Add(time.Second), nil)
			if err := c.Refresh(t.Context()); err != nil {
				t.Fatalf("refresh: %v", err)
			}
			st := c.Latest().Staleness
			if got := st.FeedOK(); got != tc.feedOK {
				t.Errorf("FeedOK = %v, want %v", got, tc.feedOK)
			}
			if got := st.TradeOK(); got != tc.tradeOK {
				t.Errorf("TradeOK = %v, want %v", got, tc.tradeOK)
			}
		})
	}
}

// Replica lag, not outage: the database answers every read successfully but
// keeps returning the same old row. Freshness keys off the row's own
// timestamp and never off read success, so this ages out on exactly the same
// clock as a stopped feed or a failed read — one staleness model whatever the
// cause. (Edge named by the PO for the Part 9 review.)
func TestARowServedSuccessfullyButNeverAdvancingAgesOut(t *testing.T) {
	store := fullStore() // every row at t0, and the store never moves them
	c, clk := newCache(store, t0.Add(time.Second), nil)

	for _, step := range []struct {
		at    time.Duration
		stale bool
	}{
		{time.Second, false},
		{30 * time.Second, false},
		{testLimit, false},
		{testLimit + time.Second, true},
		{10 * testLimit, true},
	} {
		clk.Set(t0.Add(step.at))
		if err := c.Refresh(t.Context()); err != nil {
			t.Fatalf("refresh at +%s: %v — every read succeeded, so no error is expected", step.at, err)
		}
		s := c.Latest()
		for _, src := range Sources {
			f := s.Staleness.Of(src)
			if f.Missing || f.Stale != step.stale || f.Age != step.at {
				t.Errorf("%s at +%s: %+v, want stale=%v age=%s, not missing", src, step.at, f, step.stale, step.at)
			}
		}
		if got := s.Staleness.TradeOK(); got != !step.stale {
			t.Errorf("TradeOK = %v at +%s", got, step.at)
		}
	}
	if got := store.readCount(); got != 5*5 {
		t.Errorf("store read %d times, want 5 refreshes x 5 sources, all successful", got)
	}
}
