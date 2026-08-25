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

// sampleRetention is how much mark history the runner keeps: one hour to
// compute and one being filled. Anything older has already been emitted or
// missed its chance, and it also bounds how far a catch-up can usefully reach.
const sampleRetention = 2 * time.Hour

// FundingRunner turns the stream of three-minute marks into the hourly funding
// series: an ACCRUAL row in funding_events, and the funding columns on the
// venue-state row that the sampler writes.
//
// It writes funding_events itself but never cb_venue_state — that table has one
// writer by design (ADR-0014), so the rate is handed to the sampler and appears
// on its next row.
//
// No rate exists for an hour the market was shut. The venue publishes none
// (venue doc section 3) and neither does this: that hour is a gap, and a gap is
// a row that is absent, not a row containing zero.
type FundingRunner struct {
	product     string
	maintenance config.MaintenanceWindow
	state       *VenueState
	sink        Sink
	log         *slog.Logger
	now         func() time.Time

	mu       sync.Mutex
	samples  []MarkSample
	previous decimal.NullDecimal
	lastHour time.Time
}

// NewFundingRunner builds the runner. product is the perp: the funding rate is
// a property of the future, and the spot leg only contributes its mark.
func NewFundingRunner(product string, w config.MaintenanceWindow, state *VenueState,
	sink Sink, log *slog.Logger, now func() time.Time,
) *FundingRunner {
	if now == nil {
		now = time.Now
	}
	return &FundingRunner{
		product:     product,
		maintenance: w,
		state:       state,
		sink:        sink,
		log:         log.With("component", "funding"),
		now:         now,
	}
}

// Observe takes one three-minute mark. This is the method Marks calls.
func (f *FundingRunner) Observe(s MarkSample) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.samples = append(f.samples, s)
	cutoff := s.At.Add(-sampleRetention)
	kept := f.samples[:0]
	for _, x := range f.samples {
		if !x.At.Before(cutoff) {
			kept = append(kept, x)
		}
	}
	f.samples = kept
}

// Seed primes the smoothing recursion with the last rate already known — the
// newest row a backfill wrote, typically. Without it the first live hour has no
// previous rate and stands as its own premium, which is correct but discards
// history that exists.
func (f *FundingRunner) Seed(rate decimal.Decimal, hour time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.previous = decimal.NullDecimal{Decimal: rate, Valid: true}
	f.lastHour = hour
}

// Run emits one rate per completed hour until ctx is canceled.
func (f *FundingRunner) Run(ctx context.Context) {
	for {
		timer := time.NewTimer(untilNextBoundary(f.now(), time.Hour))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		// Every hour that has completed since the last one recorded, not just the
		// most recent. A tick is not guaranteed to land on the boundary — one set
		// for 28 minutes was observed firing 2.5 minutes late under host load —
		// and a tick delayed past a whole hour would otherwise skip that hour in
		// silence. Catching up costs nothing when nothing was missed, because the
		// loop runs once.
		newest := HourOf(f.now()).Add(-time.Hour)
		for h := f.firstUnrecorded(newest); !h.After(newest); h = h.Add(time.Hour) {
			if err := f.Emit(ctx, h); err != nil {
				if ctx.Err() == nil {
					f.log.Info("funding runner stopping", "reason", err)
				}
				return
			}
		}
	}
}

// firstUnrecorded is the oldest hour still to compute.
//
// It is bounded by how long samples are kept: an hour older than that cannot be
// marked anyway, so reaching further back would only produce a run of "not
// markable" lines. Without a prior hour there is nothing to catch up on and the
// newest is the only candidate.
func (f *FundingRunner) firstUnrecorded(newest time.Time) time.Time {
	f.mu.Lock()
	last := f.lastHour
	f.mu.Unlock()

	if last.IsZero() {
		return newest
	}
	if oldest := newest.Add(-sampleRetention); last.Before(oldest) {
		return oldest
	}
	return last.Add(time.Hour)
}

// Emit computes and records one hour. A hour that cannot be marked is logged
// and skipped; only a writer that has gone stops the runner.
func (f *FundingRunner) Emit(ctx context.Context, hour time.Time) error {
	hour = HourOf(hour)

	f.mu.Lock()
	if !hour.After(f.lastHour) && !f.lastHour.IsZero() {
		f.mu.Unlock()
		return nil
	}
	samples := append([]MarkSample(nil), f.samples...)
	previous := f.previous
	f.mu.Unlock()

	// The venue publishes no rate for an hour it was closed, so neither does
	// this. Recording a zero would put a tradable-looking number on an hour
	// nothing could have been traded in.
	if f.maintenance.Contains(hour) {
		f.log.Info("no funding rate for the maintenance hour", "hour", hour)
		f.mu.Lock()
		f.lastHour = hour
		f.mu.Unlock()
		return nil
	}

	funding, err := RateForHour(hour, samples, previous)
	if err != nil {
		f.log.Warn("hour not markable", "hour", hour, "error", err)
		f.mu.Lock()
		f.lastHour = hour
		f.mu.Unlock()
		return nil
	}

	if err := f.record(ctx, funding, db.FundingSourceComputed); err != nil {
		return err
	}

	f.mu.Lock()
	f.previous = decimal.NullDecimal{Decimal: funding.Rate, Valid: true}
	f.lastHour = hour
	f.mu.Unlock()

	f.log.Info("funding hour",
		"hour", funding.HourStart,
		"rate", funding.Rate.StringFixed(10),
		"annualized_pct", funding.Annualized.Mul(decimal.NewFromInt(100)).StringFixed(3),
		"samples", funding.Samples)
	return nil
}

// record writes the ACCRUAL row and hands the rate to the venue-state sampler.
func (f *FundingRunner) record(ctx context.Context, funding Funding, source string) error {
	if f.state != nil {
		f.state.ObserveFunding(f.product, funding, source)
	}
	return submit(ctx, f.sink, AccrualRow(f.product, funding, source, decimal.NullDecimal{}))
}

// AccrualRow builds the observed-series funding row.
//
// amount is zero and position_id is NULL because this is the observed series,
// not a position's ledger: nothing was held, so nothing accrued to anyone here.
// The rate is the payload; the money column belongs to the rows Part 14 writes
// against an open position. ts is the start of the funding hour, because the
// rate is a property of the hour rather than of the moment it was computed
// (API spec section 5.3).
func AccrualRow(product string, funding Funding, source string, spotMark decimal.NullDecimal) db.FundingEventRow {
	return db.FundingEventRow{
		TS:            funding.HourStart,
		ProductID:     product,
		Kind:          db.FundingKindAccrual,
		RateHourly:    db.Num(funding.Rate),
		FundingSource: source,
		Amount:        decimal.Zero,
		SpotMark:      spotMark,
	}
}
