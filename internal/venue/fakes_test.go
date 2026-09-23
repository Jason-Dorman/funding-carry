package venue

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/shopspring/decimal"

	"github.com/Jason-Dorman/funding-carry/internal/db"
)

const (
	testPerp = "ETP-20DEC30-CDE"
	testSpot = "ETH-USD"
	// testLimit is STALE_FEED_SECS in the tests.
	testLimit = time.Minute
)

// t0 is the sampling boundary every seeded row sits on.
var t0 = time.Date(2026, 9, 22, 3, 0, 0, 0, time.UTC)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func num(s string) decimal.NullDecimal { return db.Num(decimal.RequireFromString(s)) }

// fakeStore is a database with at most one row per source.
//
// It fails where the real one fails: a canceled context is an error, and err,
// when set, is returned by every read — which is what a database that has
// gone away looks like from the cache's side.
type fakeStore struct {
	mu sync.Mutex

	perp, spot         db.VenueStateRow
	havePerp, haveSpot bool
	account            db.AccountStateRow
	haveAccount        bool
	wallet             db.BaseStateRow
	haveWallet         bool
	product            db.ProductRow
	haveProduct        bool

	err   error
	reads int
}

// fullStore is a store with a fresh row in every source at t0.
func fullStore() *fakeStore {
	return &fakeStore{
		perp: db.VenueStateRow{
			TS: t0, ProductID: testPerp,
			FuturesMark: num("2738.4945066781559672"), SpotMark: num("2738.7169524818549976"),
			Mid: num("2742.50"), FundingRateHourly: num("0.000014"), FundingRateEst: num("0.0000156029036177"),
			FundingSource: db.FundingSourceVenue, PremiumProxy: num("-0.0000812"), SpreadBps: num("3.65"),
			OpenInterest: num("302010"),
		},
		havePerp: true,
		spot: db.VenueStateRow{
			TS: t0, ProductID: testSpot, Mid: num("2742.005"), SpreadBps: num("0.036"),
		},
		haveSpot: true,
		account: db.AccountStateRow{
			TS: t0, AvailableMargin: num("3.15"), LiquidationThreshold: num("0"),
			IntradayMarginEnabled: db.Opt(false),
		},
		haveAccount: true,
		wallet: db.BaseStateRow{
			TS: t0, SpotPx: num("2738.5415794350070664"), SpotPxSource: db.SpotSourceDEX,
			WalletETH: num("0.002274242082897966"), WalletUSDC: num("0.000000"), GasGwei: num("0.006"),
		},
		haveWallet: true,
		product: db.ProductRow{
			ProductID: testPerp, ContractSize: num("0.1"), Tick: num("0.5"), UpdatedAt: t0.Add(-time.Hour),
		},
		haveProduct: true,
	}
}

func (f *fakeStore) begin(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if err := ctx.Err(); err != nil {
		return err
	}
	return f.err
}

func (f *fakeStore) LatestVenueState(ctx context.Context, product string) (db.VenueStateRow, bool, error) {
	if err := f.begin(ctx); err != nil {
		return db.VenueStateRow{}, false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch product {
	case testPerp:
		return f.perp, f.havePerp, nil
	case testSpot:
		return f.spot, f.haveSpot, nil
	}
	return db.VenueStateRow{}, false, nil
}

func (f *fakeStore) LatestAccountState(ctx context.Context) (db.AccountStateRow, bool, error) {
	if err := f.begin(ctx); err != nil {
		return db.AccountStateRow{}, false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.account, f.haveAccount, nil
}

func (f *fakeStore) LatestBaseState(ctx context.Context) (db.BaseStateRow, bool, error) {
	if err := f.begin(ctx); err != nil {
		return db.BaseStateRow{}, false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.wallet, f.haveWallet, nil
}

// Product filters on the product id because Reader.Product does
// (WHERE product_id = $1). Ignoring the argument made this fake more forgiving
// than the real one and hid a swap: asking cb_products for the SPOT product —
// a catalogue that holds the perp only — returns no row, so ProductKnown goes
// false and the contract size the quantisation rule depends on is absent.
func (f *fakeStore) Product(ctx context.Context, product string) (db.ProductRow, bool, error) {
	if err := f.begin(ctx); err != nil {
		return db.ProductRow{}, false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if product != f.product.ProductID {
		return db.ProductRow{}, false, nil
	}
	return f.product, f.haveProduct, nil
}

func (f *fakeStore) fail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakeStore) readCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads
}

// clock is a settable time source.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

// newCache builds a cache over the store with the clock at now.
func newCache(store Store, now time.Time, m *Metrics) (*Cache, *clock) {
	clk := &clock{now: now}
	c := New(store, Options{
		PerpProduct: testPerp, SpotProduct: testSpot, StaleAfter: testLimit, Now: clk.Now,
	}, m, testLogger())
	return c, clk
}
