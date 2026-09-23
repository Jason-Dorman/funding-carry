package venue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/Jason-Dorman/funding-carry/internal/db"
)

// Store is what the cache needs from the database: the newest row of each
// source, and the product's metadata. It is declared here, at the consumer
// (architecture section 12); *db.Reader satisfies it, and a test satisfies it
// with a fake.
type Store interface {
	LatestVenueState(ctx context.Context, product string) (db.VenueStateRow, bool, error)
	LatestAccountState(ctx context.Context) (db.AccountStateRow, bool, error)
	LatestBaseState(ctx context.Context) (db.BaseStateRow, bool, error)
	Product(ctx context.Context, product string) (db.ProductRow, bool, error)
}

// Source names one independently fed input to the cache.
//
// Staleness is judged per source rather than for the cache as a whole because
// each has its own producer in ingest, its own cadence and its own way of
// dying: the perp quote stops at the Friday close while the wallet poller
// carries on, the account poller is absent entirely on a stack with no
// credential, and a Base RPC outage touches nothing but the wallet.
type Source string

// The four sources, each one table (or one product's slice of one).
const (
	SourcePerp    Source = "perp"    // cb_venue_state for the perp: marks, funding, premium, OI, maintenance flag
	SourceSpot    Source = "spot"    // cb_venue_state for the spot product: mid and spread
	SourceAccount Source = "account" // cb_account_state: margin, balances, contracts held
	SourceWallet  Source = "wallet"  // base_state: wallet inventory, Base spot price, gas
)

// Sources lists every source, in the order the metrics are initialized.
var Sources = []Source{SourcePerp, SourceSpot, SourceAccount, SourceWallet}

// Freshness is one source's age as of the refresh that computed it.
type Freshness struct {
	// At is the newest row's own timestamp — the sampling boundary ingest
	// wrote it on — so Age measures the age of the observation, not of the
	// query that fetched it. Zero when nothing is recorded.
	At time.Time
	// Age is the refresh time minus At. It has no meaning when Missing.
	Age time.Duration
	// Missing is true when the source has no row at all: an empty table, or
	// a product nothing has been recorded for.
	Missing bool
	// Stale is the typed flag the risk engine consumes: Missing, or Age past
	// the configured limit (STALE_FEED_SECS). It is derived here, once, so
	// that every consumer applies the same rule and the dashboard shows the
	// value the code decided on rather than one recomputed from the age.
	Stale bool
}

// Staleness is the freshness of every source, as one value.
type Staleness struct {
	Perp    Freshness
	Spot    Freshness
	Account Freshness
	Wallet  Freshness
}

// Of returns one source's freshness by name.
func (s Staleness) Of(src Source) Freshness {
	switch src {
	case SourcePerp:
		return s.Perp
	case SourceSpot:
		return s.Spot
	case SourceAccount:
		return s.Account
	case SourceWallet:
		return s.Wallet
	}
	return Freshness{Missing: true, Stale: true}
}

// FeedOK is the feed-staleness flag the decision rules name ("feed not
// stale", spec section 8): both market-data sources fresh. It is MARKET-DATA
// freshness, not account state, and on its own it is never "safe to act".
//
// It covers the perp and the spot rows and deliberately not the account or the
// wallet. "Feed" in architecture section 8 is the market data — the thing
// ingest's last_seen gauge and the FeedStale alert measure — and the two
// polled sources are absent by design on a stack without a credential or a
// wallet address. Folding them in would leave the public stack BLOCKED
// forever and would hide, inside one boolean, a distinction the risk engine
// needs: a stale margin ratio blocks a *size* check, not the feed. Their
// flags are exposed beside this one, and anything that gates ORDER FLOW gates
// on the conjunction — FeedOK && !Account.Stale && !Wallet.Stale (TradeOK) —
// with FeedOK alone being the public-stack reading. (Part 9 decision,
// confirmed by the PO with that condition; the gate itself is Part 13's.)
func (s Staleness) FeedOK() bool { return !s.Perp.Stale && !s.Spot.Stale }

// TradeOK is the conjunction order flow must gate on: every source fresh,
// the account and the wallet included. It exists so that the trading stack
// has one name for the right question and cannot reach for FeedOK by
// mistake; Part 13's pre-trade checks consume it. On the public stack, which
// has no account and no wallet source, it is always false — and that is the
// correct answer there, because nothing on that stack may trade.
func (s Staleness) TradeOK() bool {
	return s.FeedOK() && !s.Account.Stale && !s.Wallet.Stale
}

// State is carry's read model: the newest row of every source, the perp's
// metadata, and how old each source was when it was last read (spec section
// 6.2).
//
// It is a value. Latest returns a copy, so a reader holds a consistent picture
// of one refresh and nothing a later refresh does can change it underneath.
type State struct {
	// RefreshedAt is the clock reading the staleness was computed against.
	// Zero until the first refresh has completed.
	RefreshedAt time.Time

	// Perp is the perp product's newest cb_venue_state row: futures and spot
	// marks, mid, premium proxy, spread, the hourly funding rate with its
	// provenance beside the local estimate, open interest, and the
	// maintenance-window flag.
	//
	// Two rates, per the schema (API spec section 5.1): FundingRateHourly is
	// funding *now* — the venue's published rate when it has one, else the
	// estimate — and FundingRateEst is the system's own estimate, which is
	// the "estimated next funding" of spec section 6.2: the venue publishes
	// its rate one hour behind the estimator (Part 5 evidence), so the local
	// number for the hour just closed is the best available estimate of what
	// the venue will publish next.
	Perp db.VenueStateRow
	// Spot is the spot product's newest cb_venue_state row. Only Mid and
	// SpreadBps are ever populated for it; funding and marks are the perp's.
	Spot db.VenueStateRow
	// Account is the newest polled margin and balance snapshot. MarginRatio is
	// NULL when the account holds no position, and that is not zero (API spec
	// section 5.2).
	Account db.AccountStateRow
	// Wallet is the newest Base poll: inventory, the Base spot price with the
	// market it came from, and gas.
	Wallet db.BaseStateRow

	// Product is the perp's metadata: contract size, tick, leverage caps and
	// fee tier as far as the venue has confirmed them (columns the venue has
	// not are NULL, never a placeholder). ProductKnown is false when the row
	// does not exist at all, which means `make migrate` has not run.
	Product      db.ProductRow
	ProductKnown bool

	// Staleness is per source, computed against RefreshedAt.
	Staleness Staleness
}

// Options configure a Cache.
type Options struct {
	PerpProduct string
	SpotProduct string
	// StaleAfter is the age past which a source is stale: STALE_FEED_SECS.
	StaleAfter time.Duration
	// Now is the clock; nil means time.Now. Injected so staleness is testable
	// at exact boundaries rather than by sleeping.
	Now func() time.Time

	// ticks replaces Run's ticker in tests, so the loop is driven rather than
	// slept through, and onRefresh is told by Run once each refresh has
	// landed and been logged, so a test waits on a channel rather than
	// polling. Both unexported: production uses the real clock and needs no
	// notification.
	ticks     <-chan time.Time
	onRefresh func(State)
}

// Cache holds the latest State and refreshes it from the Store.
//
// Refresh is called by one goroutine — the tick loop — and Latest by anyone.
// The mutex is held only to swap or copy the value; no I/O happens under it.
type Cache struct {
	store Store
	opts  Options
	m     *Metrics
	log   *slog.Logger

	mu    sync.RWMutex
	state State
}

// New builds a cache. Nothing is read until Refresh or Run; until then Latest
// reports every source missing and stale, and the metrics say the same.
func New(store Store, opts Options, m *Metrics, log *slog.Logger) *Cache {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	c := &Cache{store: store, opts: opts, m: m, log: log.With("component", "venue_state")}
	// Before the first refresh nothing is known, and the flags must say so
	// rather than read as the zero value's "fresh".
	c.state.Staleness = staleness(State{}, time.Time{}, opts.StaleAfter)
	return c
}

// Latest returns a copy of the most recent state.
func (c *Cache) Latest() State {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.state
}

// Refresh reads the newest row of every source and recomputes staleness
// against the clock.
//
// A source whose read fails keeps its previous row — the last good data,
// which is what architecture section 8 says hard-stop evaluation continues
// on — and that row's age is recomputed against now like every other. So a
// database that stops answering degrades exactly as a feed that stops
// writing does: each source crosses StaleAfter on its own schedule and trips
// its flag, and FeedOK goes false without anyone crashing. The error is
// returned so the caller can say what happened; it is never a reason to
// throw the previous state away.
//
// The reads are sequential, and each returns one indexed row — but that is not
// why a refresh is fast, and the difference matters. On a warm connection the
// five statements are cached and planned generically, and a refresh costs a
// few milliseconds. On a FRESH backend the same five pay catalog and plan
// construction against hypertables whose plans enumerate every chunk, which
// measured 47-85 ms on this stack and grows linearly with chunk count. pgx
// retires a connection every MaxConnLifetime, so that cost recurs on a
// schedule rather than only at startup. The two regimes have two budgets, 50 ms
// warm and 500 ms cold, because one number could not describe both; a cold
// refresh past 500 ms is the signal to revisit the query shape rather than the
// budget (build plan, Part 9). An index does not flatten this — the one that
// would is already there and already used — because the cost is one plan node
// per chunk, not the lookup.
func (c *Cache) Refresh(ctx context.Context) error {
	started := c.opts.Now()
	next := c.Latest()
	var errs []error

	if row, ok, err := c.store.LatestVenueState(ctx, c.opts.PerpProduct); err != nil {
		errs = append(errs, fmt.Errorf("%s: %w", SourcePerp, err))
	} else {
		next.Perp = rowOrNone(row, ok)
	}
	if row, ok, err := c.store.LatestVenueState(ctx, c.opts.SpotProduct); err != nil {
		errs = append(errs, fmt.Errorf("%s: %w", SourceSpot, err))
	} else {
		next.Spot = rowOrNone(row, ok)
	}
	if row, ok, err := c.store.LatestAccountState(ctx); err != nil {
		errs = append(errs, fmt.Errorf("%s: %w", SourceAccount, err))
	} else {
		next.Account = rowOrNone(row, ok)
	}
	if row, ok, err := c.store.LatestBaseState(ctx); err != nil {
		errs = append(errs, fmt.Errorf("%s: %w", SourceWallet, err))
	} else {
		next.Wallet = rowOrNone(row, ok)
	}
	if row, ok, err := c.store.Product(ctx, c.opts.PerpProduct); err != nil {
		errs = append(errs, fmt.Errorf("product: %w", err))
	} else {
		next.Product, next.ProductKnown = rowOrNone(row, ok), ok
	}

	now := c.opts.Now()
	next.RefreshedAt = now
	next.Staleness = staleness(next, now, c.opts.StaleAfter)

	c.mu.Lock()
	c.state = next
	c.mu.Unlock()

	if c.m != nil {
		c.m.observe(next, now.Sub(started))
	}
	if len(errs) > 0 {
		return fmt.Errorf("refresh venue state: %w", errors.Join(errs...))
	}
	return nil
}

// rowOrNone is the zero row when the source has nothing, so a source that
// once had a row and now reports none (a product deleted under a running
// process, say) reads as missing rather than as its last value forever.
func rowOrNone[T any](row T, ok bool) T {
	if !ok {
		var none T
		return none
	}
	return row
}

// staleness derives every source's flags from its row timestamp and the
// clock. A zero timestamp is a missing source.
func staleness(s State, now time.Time, limit time.Duration) Staleness {
	return Staleness{
		Perp:    freshness(s.Perp.TS, now, limit),
		Spot:    freshness(s.Spot.TS, now, limit),
		Account: freshness(s.Account.TS, now, limit),
		Wallet:  freshness(s.Wallet.TS, now, limit),
	}
}

func freshness(at, now time.Time, limit time.Duration) Freshness {
	if at.IsZero() {
		return Freshness{Missing: true, Stale: true}
	}
	age := now.Sub(at)
	// Strictly greater: a row exactly STALE_FEED_SECS old is on the limit,
	// not past it. Equality is the one case a boundary test can pin.
	return Freshness{At: at, Age: age, Stale: age > limit}
}
