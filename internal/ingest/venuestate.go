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

	// funding, marks and openInterest are the Part 5 half of a row: everything
	// the REST poller and the funding estimator contribute. They live here, and
	// not in their own writer, because cb_venue_state has exactly one writer and
	// two producers colliding on (product_id, ts) would silently lose one
	// (ADR-0014).
	funding  map[string]fundingObservation
	marks    map[string]markObservation
	openInts map[string]decimal.NullDecimal

	// venueFunding is the rate the venue itself publishes, when it does. It
	// takes precedence over the local estimate for funding_rate_hourly, and the
	// estimate stays in funding_rate_est as the independent cross-check the
	// reconciliation is measured against — which is exactly the split the schema
	// was designed for (API spec section 5.1).
	venueFunding map[string]venueFundingObservation

	// session is the venue's own trading calendar for the perp, from the REST
	// product payload.
	session     Session
	sessionSeen time.Time

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

// maxMarkAge and maxFundingAge are how stale each Part 5 contribution may be
// before it stops appearing on a row. Both follow the same rule as the quote: a
// value carried forward past its useful life is indistinguishable from a fresh
// one, and these two are what the basis and the carry are computed from.
//
// A mark is a three-minute measurement, so two windows is already late. A rate
// is hourly and is legitimately the same number all hour, so it is allowed to
// describe the hour it belongs to and the one after it — beyond that the
// estimator has failed to produce a new one and the series should show it.
const (
	maxMarkAge    = 2 * SampleInterval
	maxFundingAge = 2 * time.Hour
	// A session is polled every POLL_REST_SECS and changes at most twice a week,
	// so a minute of tolerance is generous; beyond that the poller is down and
	// the calendar is the only signal left.
	maxSessionAge = time.Minute
)

// NewVenueState builds the sampler.
func NewVenueState(perp, spot string, interval time.Duration, w config.MaintenanceWindow,
	sink Sink, log *slog.Logger, now func() time.Time,
) *VenueState {
	if now == nil {
		now = time.Now
	}
	return &VenueState{
		perpProduct:  perp,
		spotProduct:  spot,
		interval:     interval,
		maintenance:  w,
		sink:         sink,
		log:          log.With("component", "venue_state"),
		now:          now,
		quotes:       make(map[string]Quote, 2),
		funding:      make(map[string]fundingObservation, 1),
		marks:        make(map[string]markObservation, 2),
		openInts:     make(map[string]decimal.NullDecimal, 1),
		venueFunding: make(map[string]venueFundingObservation, 1),
	}
}

// fundingObservation is the most recent hourly rate and where it came from.
type fundingObservation struct {
	funding Funding
	source  string
}

// markObservation is the most recent three-minute mark pair and the premium
// derived from it.
type markObservation struct {
	futures decimal.NullDecimal
	spot    decimal.NullDecimal
	premium decimal.NullDecimal
	at      time.Time
}

// ObserveFunding records the hourly rate. Called from the funding runner.
func (v *VenueState) ObserveFunding(product string, f Funding, source string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.funding[product] = fundingObservation{funding: f, source: source}
}

// ObserveMarks records the three-minute marks and the premium between them.
// Called from the mark sampler.
func (v *VenueState) ObserveMarks(product string, futures, spot decimal.Decimal, at time.Time) {
	premium := decimal.NullDecimal{}
	if !spot.IsZero() {
		premium = decimal.NullDecimal{Decimal: futures.Sub(spot).Div(spot), Valid: true}
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.marks[product] = markObservation{
		futures: db.Num(futures), spot: db.Num(spot), premium: premium, at: at,
	}
}

// ObserveSession records the venue's own trading calendar for the contract.
//
// It is a better maintenance signal than the configured weekly window, because
// it names this week's actual close rather than a rule, and better than the
// status channel, which was observed reporting "online" straight through the
// Friday break (Part 4 soak, 2026-08-21). All three are combined rather than
// ranked: the flag is true if any of them says the market is shut.
func (v *VenueState) ObserveSession(product string, s Session) {
	if product != v.perpProduct {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.session, v.sessionSeen = s, v.now()
}

// venueFundingObservation is a rate the venue published, with the time it says
// it applies to.
type venueFundingObservation struct {
	rate decimal.NullDecimal
	at   time.Time
	seen time.Time
}

// ObserveVenueFunding records the venue's own hourly rate. Called from the REST
// poller.
func (v *VenueState) ObserveVenueFunding(product string, rate decimal.NullDecimal, at, seen time.Time) {
	if !rate.Valid {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.venueFunding[product] = venueFundingObservation{rate: rate, at: at, seen: seen}
}

// ObserveOpenInterest records the figure from the REST product payload, which
// is the only place it appears.
func (v *VenueState) ObserveOpenInterest(product string, oi decimal.NullDecimal) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.openInts[product] = oi
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
	v.mu.Unlock()

	fresh := haveQuote && at.Sub(q.At) <= time.Duration(maxQuoteAge)*v.interval

	row := db.VenueStateRow{TS: ts, ProductID: product}
	if fresh {
		mid := midpoint(q)
		row.Mid = mid
		row.SpreadBps = spreadBps(q, mid)
	}

	v.applyPartFiveLocked(&row, product, at)

	if product == v.perpProduct {
		row.MaintenanceWindow = v.inMaintenance(ts, at)
	}

	if !fresh && !row.MaintenanceWindow {
		return db.VenueStateRow{}, false
	}
	return row, true
}

// applyPartFiveLocked adds the columns the REST poller and the funding
// estimator contribute: the marks and the premium between them, the hourly rate
// with its provenance, and open interest. Each is subject to its own staleness
// rule, because each has its own source and its own natural cadence.
func (v *VenueState) applyPartFiveLocked(row *db.VenueStateRow, product string, at time.Time) {
	v.mu.Lock()
	defer v.mu.Unlock()

	if mk, ok := v.marks[product]; ok && at.Sub(mk.at) <= maxMarkAge {
		row.FuturesMark, row.SpotMark, row.PremiumProxy = mk.futures, mk.spot, mk.premium
	}
	// The two rates are independent and the schema keeps them apart on purpose:
	// funding_rate_est is always what this system computed, funding_rate_hourly
	// is the venue's when the venue has one. Their difference is what
	// carry_funding_reconciliation_error measures, and it can only be measured
	// if the estimate is never quietly overwritten by the venue's number.
	if fo, ok := v.funding[product]; ok && at.Sub(fo.funding.HourStart) <= maxFundingAge {
		row.FundingRateEst = db.Num(fo.funding.Rate)
		row.FundingRateHourly = db.Num(fo.funding.Rate)
		row.FundingSource = fo.source
		row.FundingAnnualized = db.Num(fo.funding.Annualized)
	}
	if vf, ok := v.venueFunding[product]; ok && at.Sub(vf.seen) <= maxFundingAge {
		row.FundingRateHourly = vf.rate
		row.FundingSource = db.FundingSourceVenue
		row.FundingAnnualized = db.Num(vf.rate.Decimal.Mul(hoursPerYear))
	}
	// Open interest comes from the same poll as the session and ages with it:
	// carrying a figure forward through an outage would show a market whose
	// positioning had not moved for as long as the poller was down.
	if oi, ok := v.openInts[product]; ok && !v.sessionSeen.IsZero() && at.Sub(v.sessionSeen) <= maxSessionAge {
		row.OpenInterest = oi
	}
}

// inMaintenance combines the three signals that say the perp is not tradable.
//
// The flag is true if any of them says so, and they are combined rather than
// ranked because each covers what the others miss. The configured window is the
// venue's published weekly break. The venue's own session names this week's
// actual close rather than a rule. The status channel covers an unscheduled
// halt — and was observed reporting "online" straight through the Friday break
// in the Part 4 soak, which is why it cannot be the only one. Both polled
// signals age out; the calendar cannot go stale.
func (v *VenueState) inMaintenance(ts, at time.Time) bool {
	if v.maintenance.Contains(ts) {
		return true
	}

	v.mu.Lock()
	defer v.mu.Unlock()

	if v.statusSeen && !v.perpOnline {
		return true
	}
	sessionFresh := !v.sessionSeen.IsZero() && at.Sub(v.sessionSeen) <= maxSessionAge
	return sessionFresh && !v.session.IsOpen
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
