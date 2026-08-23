package ingest

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/shopspring/decimal"

	"github.com/Jason-Dorman/funding-carry/internal/config"
	"github.com/Jason-Dorman/funding-carry/internal/db"
)

// VenueState is the single writer of cb_venue_state.
//
// It exists as its own component rather than as a line in the ticker handler
// because that table has two producers by design: the WebSocket quote this part
// records, and the marks, funding rate and open interest the REST poller adds in
// Part 5. Rows are keyed (product_id, ts) and every insert is
// ON CONFLICT DO NOTHING, so two producers writing their own rows on the same
// boundary would mean one of them silently losing — the second row would be
// dropped and its columns would never appear. Funnelling both into one sampler
// that emits one complete row per boundary is what keeps that from happening;
// Part 5 adds its observations here rather than beside here.
//
// What Part 4 fills in is mid, spread and the maintenance flag. Marks, funding
// and premium stay NULL until there is something real to put in them: a premium
// computed against a mid standing in for a 3-minute VWAP would be a number the
// decision engine would act on and nobody could reproduce.
type VenueState struct {
	perpProduct string
	spotProduct string
	interval    time.Duration
	maintenance config.MaintenanceWindow
	sink        Sink
	log         *slog.Logger
	now         func() time.Time

	// mu guards everything below: quotes arrive on the ticker stream's
	// goroutine, the product status on the status stream's, and the sample runs
	// on this component's own.
	mu         sync.Mutex
	quotes     map[string]Quote
	perpOnline bool
	statusSeen bool

	// lastBoundary is owned by the sampling goroutine alone. It is what keeps two
	// ticks that land in the same window from producing two rows for one
	// boundary: the second would be discarded by the database as a conflict,
	// which is indistinguishable from a healthy retry, and it would be the
	// fresher of the two observations that was thrown away.
	lastBoundary time.Time
}

// Quote is the top of book for one product as the ticker channel reports it.
type Quote struct {
	Bid    decimal.NullDecimal
	Ask    decimal.NullDecimal
	Last   decimal.NullDecimal
	At     time.Time // receive time, for freshness
	VenueT time.Time
}

// maxQuoteAge multiples of the sample interval a quote may be stale before the
// sampler stops writing rows for that product.
//
// The alternative — carrying the last known mid forward — would keep the series
// looking healthy through a dead feed, and a frozen price is the single most
// dangerous thing this table can contain: it is what the basis, the premium and
// every downstream band are computed from. A missing row is a gap, which is
// visible; a repeated row is a lie, which is not.
const maxQuoteAge = 3

// NewVenueState builds the sampler.
func NewVenueState(perp, spot string, interval time.Duration, w config.MaintenanceWindow,
	sink Sink, log *slog.Logger, now func() time.Time,
) *VenueState {
	if now == nil {
		now = time.Now
	}
	return &VenueState{
		perpProduct: perp,
		spotProduct: spot,
		interval:    interval,
		maintenance: w,
		sink:        sink,
		log:         log.With("component", "venue_state"),
		now:         now,
		quotes:      make(map[string]Quote, 2),
	}
}

// Observe records the latest quote for a product. Called from the ticker
// stream's goroutine.
func (v *VenueState) Observe(product string, q Quote) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.quotes[product] = q
}

// ObserveStatus records whether the perp is tradable. Called from the status
// stream's goroutine.
func (v *VenueState) ObserveStatus(product string, online bool) {
	if product != v.perpProduct {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.perpOnline = online
	v.statusSeen = true
}

// untilNextBoundary is how long to wait, from t, for the next sampling
// boundary. It always returns a positive duration, so a wake that lands exactly
// on a boundary schedules the following one rather than spinning.
func untilNextBoundary(t time.Time, interval time.Duration) time.Duration {
	return t.Truncate(interval).Add(interval).Sub(t)
}

// Run samples on the interval until ctx is canceled.
//
// It waits for each boundary rather than ticking on a period, and the difference
// is not cosmetic. A time.Ticker keeps its period but not its phase relative to
// the wall clock, and the boundary here is derived by truncating the time the
// tick arrived. With that phase sitting near a boundary edge, a millisecond of
// jitter is enough for a tick to land just *below* the boundary it was meant
// for, truncate onto the previous one, be rejected as a duplicate, and take its
// own boundary with it. That is not hypothetical: over 19 hours of the Part 4
// soak it silently dropped 914 rows, 6.4% of the series, always one at a time.
// A timer fires at or after its deadline and never before, so waiting for the
// boundary itself removes the whole class.
//
// The row timestamp is still the boundary, not the moment the sample ran: the
// idempotency key is (product_id, ts), and a clock reading would make every row
// unique and the constraint decorative (API spec section 5.3, which names Parts
// 4 to 6 as the owners of this rule).
func (v *VenueState) Run(ctx context.Context) {
	for {
		timer := time.NewTimer(untilNextBoundary(v.now(), v.interval))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}

		// Both failures submit can produce are terminal — the writer has
		// stopped, or the context has ended — so this returns rather than
		// carrying on. There is no transient case to retry: a full queue blocks
		// inside Submit rather than failing.
		if err := v.sample(ctx, v.now()); err != nil {
			if ctx.Err() == nil {
				v.log.Info("venue state sampler stopping", "reason", err)
			}
			return
		}
	}
}

func (v *VenueState) sample(ctx context.Context, at time.Time) error {
	ts := at.UTC().Truncate(v.interval)
	if !ts.After(v.lastBoundary) {
		return nil
	}
	v.lastBoundary = ts

	for _, product := range []string{v.perpProduct, v.spotProduct} {
		row, ok := v.rowFor(product, ts, at)
		if !ok {
			continue
		}
		if err := submit(ctx, v.sink, row); err != nil {
			return err
		}
	}
	return nil
}

// rowFor builds one product's row, reporting false when there is nothing to say.
//
// The two halves of the row have independent sources and independent health. The
// quote comes from the ticker stream and is dropped when it goes stale — carrying
// the last known mid forward would keep the series looking healthy through a
// dead feed, and a frozen price is what the basis, the premium and every
// downstream band are computed from. The maintenance flag does not come from the
// ticker at all: it comes from the venue calendar and from the status stream,
// which is a different connection. Gating it on quote freshness would drop the
// one observation the column exists to record at exactly the moment it matters,
// since a halt is when the ticker is most likely to have stopped too.
//
// A row is written when either half has something in it. A row with neither is
// not written: a missing row is a visible gap, which is what "we were not
// looking" should read as.
func (v *VenueState) rowFor(product string, ts, at time.Time) (db.VenueStateRow, bool) {
	v.mu.Lock()
	q, haveQuote := v.quotes[product]
	perpOnline, statusSeen := v.perpOnline, v.statusSeen
	v.mu.Unlock()

	fresh := haveQuote && at.Sub(q.At) <= time.Duration(maxQuoteAge)*v.interval

	row := db.VenueStateRow{TS: ts, ProductID: product}
	if fresh {
		mid := midpoint(q)
		row.Mid = mid
		row.SpreadBps = spreadBps(q, mid)
	}
	if product == v.perpProduct {
		// Two independent reasons the market is not tradable, and the flag is
		// true if either says so. The configured window is the venue's published
		// weekly break; the status channel is the venue saying so itself, which
		// also covers an unscheduled halt. Before the status channel has said
		// anything, only the calendar is consulted rather than assuming offline —
		// a false maintenance flag at startup would block entries for no reason.
		row.MaintenanceWindow = v.maintenance.Contains(ts) || (statusSeen && !perpOnline)
	}

	if !fresh && !row.MaintenanceWindow {
		return db.VenueStateRow{}, false
	}
	return row, true
}

var two = decimal.NewFromInt(2)

// midpoint is the average of the two sides, or NULL.
//
// It deliberately does not fall back to the ticker's last trade price when the
// book is one-sided. That price is a print, not a midpoint, and it carries
// forward unchanged for as long as nothing trades — so the substitution would
// put an arbitrarily old number into a column documented as a mid, with nothing
// on the row to mark it. The freshness rule above cannot catch it either,
// because it measures the age of the ticker message, not the age of the trade.
//
// Division by two is exact for any price the venue quotes, and a mid is a
// derived observation rather than a figure that has to reconcile against a
// statement (API spec section 3.5).
func midpoint(q Quote) decimal.NullDecimal {
	if !q.Bid.Valid || !q.Ask.Valid {
		return decimal.NullDecimal{}
	}
	return decimal.NullDecimal{Decimal: q.Bid.Decimal.Add(q.Ask.Decimal).Div(two), Valid: true}
}

var tenThousand = decimal.NewFromInt(10000)

// spreadBps is the quoted spread as a fraction of the mid, in basis points. It
// needs both sides and a non-zero mid; anything else reads NULL.
func spreadBps(q Quote, mid decimal.NullDecimal) decimal.NullDecimal {
	if !q.Bid.Valid || !q.Ask.Valid || !mid.Valid || mid.Decimal.IsZero() {
		return decimal.NullDecimal{}
	}
	spread := q.Ask.Decimal.Sub(q.Bid.Decimal)
	return decimal.NullDecimal{Decimal: spread.Div(mid.Decimal).Mul(tenThousand), Valid: true}
}
