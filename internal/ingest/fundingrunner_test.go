package ingest

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/Jason-Dorman/funding-carry/internal/db"
)

func accrualRows(t *testing.T, sink *fakeSink) []db.FundingEventRow {
	t.Helper()
	var out []db.FundingEventRow
	for _, r := range sink.collected() {
		if row, ok := r.(db.FundingEventRow); ok {
			out = append(out, row)
		}
	}
	return out
}

// feedHour supplies n of the hour's windows. They end at h+3m .. h+n*3m: a
// window ending exactly at h belongs to the previous hour.
func feedHour(r *FundingRunner, h time.Time, n int, spot, basis decimal.Decimal) {
	for i := 1; i <= n; i++ {
		f := spot.Add(spot.Mul(basis))
		r.Observe(MarkSample{At: h.Add(time.Duration(i) * SampleInterval), FuturesMark: f, SpotMark: spot})
	}
}

func TestFundingRunnerWritesAnAccrualAndFeedsTheSampler(t *testing.T) {
	sink := &fakeSink{}
	state := newSampler(t, sink)
	r := NewFundingRunner(testPerp, maintenanceWindow(t), state, sink, testLogger(), nil)

	h := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	feedHour(r, h, SamplesPerHour, dec(t, "2000"), dec(t, "0.0024"))

	if err := r.Emit(context.Background(), h); err != nil {
		t.Fatalf("emit: %v", err)
	}

	rows := accrualRows(t, sink)
	if len(rows) != 1 {
		t.Fatalf("funding_events rows = %d, want 1", len(rows))
	}
	row := rows[0]
	if row.Kind != db.FundingKindAccrual {
		t.Errorf("kind = %q, want ACCRUAL", row.Kind)
	}
	if row.FundingSource != db.FundingSourceComputed {
		t.Errorf("funding_source = %q, want computed: these rows are this estimator's own output, not the venue's", row.FundingSource)
	}
	// ts is the start of the funding hour — the rate is a property of the hour,
	// not of the moment it was computed (API spec section 5.3).
	if !row.TS.Equal(h) {
		t.Errorf("ts = %s, want the hour start %s", row.TS, h)
	}
	if got := row.RateHourly.Decimal.String(); got != "0.0001" {
		t.Errorf("rate_hourly = %s, want 0.0001", got)
	}
	// The observed series holds no position, so nothing accrued to anyone.
	if !row.Amount.IsZero() || row.PositionID != nil {
		t.Errorf("amount=%s position=%v, want 0 and NULL for the observed series", row.Amount, row.PositionID)
	}

	// And the rate reaches cb_venue_state through the sampler, never by a
	// second writer (ADR-0014).
	state.Observe(testPerp, quote(t, "1999", "2001", "2000", h))
	if err := state.sample(context.Background(), h); err != nil {
		t.Fatalf("sample: %v", err)
	}
	vs := venueRows(t, sink)[testPerp]
	if !vs.FundingRateEst.Valid || vs.FundingRateEst.Decimal.String() != "0.0001" {
		t.Errorf("funding_rate_est on the venue row = %v, want 0.0001", vs.FundingRateEst)
	}
	if vs.FundingSource != db.FundingSourceComputed {
		t.Errorf("venue row funding_source = %q, want computed", vs.FundingSource)
	}
}

func TestNoRateIsWrittenForTheMaintenanceHour(t *testing.T) {
	sink := &fakeSink{}
	r := NewFundingRunner(testPerp, maintenanceWindow(t), newSampler(t, sink), sink, testLogger(), nil)

	// Friday 17:00 New York, the venue's weekly break.
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	h := time.Date(2026, 8, 21, 17, 0, 0, 0, ny).UTC()
	feedHour(r, h, SamplesPerHour, dec(t, "2000"), dec(t, "0.0024"))

	if err := r.Emit(context.Background(), h); err != nil {
		t.Fatalf("emit: %v", err)
	}

	// The venue publishes no rate for an hour it was shut, and neither does
	// this. A zero would be a tradable-looking number on an untradable hour.
	if rows := accrualRows(t, sink); len(rows) != 0 {
		t.Fatalf("wrote %d rows for the maintenance hour, want 0 — that hour is a gap", len(rows))
	}
}

func TestAnUnmarkableHourWritesNothingButDoesNotStopTheRunner(t *testing.T) {
	sink := &fakeSink{}
	r := NewFundingRunner(testPerp, maintenanceWindow(t), newSampler(t, sink), sink, testLogger(), nil)
	ctx := context.Background()

	h := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	feedHour(r, h, 2, dec(t, "2000"), dec(t, "0.0024")) // far too few

	if err := r.Emit(ctx, h); err != nil {
		t.Fatalf("emit returned an error for a thin hour: %v", err)
	}
	if rows := accrualRows(t, sink); len(rows) != 0 {
		t.Fatalf("wrote %d rows for a thin hour, want 0", len(rows))
	}

	// And the next, complete hour still works.
	next := h.Add(time.Hour)
	feedHour(r, next, SamplesPerHour, dec(t, "2000"), dec(t, "0.0024"))
	if err := r.Emit(ctx, next); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if rows := accrualRows(t, sink); len(rows) != 1 {
		t.Fatalf("rows = %d, want the following hour to be written", len(rows))
	}
}

func TestAnHourIsNeverWrittenTwice(t *testing.T) {
	sink := &fakeSink{}
	r := NewFundingRunner(testPerp, maintenanceWindow(t), newSampler(t, sink), sink, testLogger(), nil)
	ctx := context.Background()

	h := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	feedHour(r, h, SamplesPerHour, dec(t, "2000"), dec(t, "0.0024"))

	for range 3 {
		if err := r.Emit(ctx, h); err != nil {
			t.Fatalf("emit: %v", err)
		}
	}
	// The natural key would absorb the repeats, but a re-emitted hour would also
	// re-run the smoothing and change the rate. Once is once.
	if rows := accrualRows(t, sink); len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
}

func TestSeedPrimesTheSmoothingFromBackfilledHistory(t *testing.T) {
	sink := &fakeSink{}
	r := NewFundingRunner(testPerp, maintenanceWindow(t), newSampler(t, sink), sink, testLogger(), nil)

	h := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	// The newest reconstructed hour, handed forward so the first live hour is
	// smoothed against real history rather than standing alone as its premium.
	r.Seed(dec(t, "0.0002"), h.Add(-time.Hour))
	feedHour(r, h, SamplesPerHour, dec(t, "2000"), dec(t, "0.0024"))

	if err := r.Emit(context.Background(), h); err != nil {
		t.Fatalf("emit: %v", err)
	}
	rows := accrualRows(t, sink)
	// 0.75 x 0.0001 + 0.25 x 0.0002
	if got := rows[0].RateHourly.Decimal.String(); got != "0.000125" {
		t.Errorf("rate = %s, want 0.000125 — the seed was not applied", got)
	}
}

// A tick delayed past an hour boundary must not skip the hour it stepped over.
//
// Run emitted only the single most recent completed hour per tick, so a late
// tick lost the hours in between silently. It is not hypothetical: a timer set
// for 28 minutes was observed in production firing 2.5 minutes late under host
// load, and a longer delay costs whole hours of the series.
func TestALateTickCatchesUpTheHoursItSteppedOver(t *testing.T) {
	sink := &fakeSink{}
	r := NewFundingRunner(testPerp, maintenanceWindow(t), newSampler(t, sink), sink, testLogger(), nil)
	ctx := context.Background()

	h0 := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)
	spot, basis := dec(t, "2000"), dec(t, "0.0024")

	// The first hour is fed and recorded normally. Feeding it before the later
	// hours matters: the runner keeps only sampleRetention of marks, so an hour
	// fed and left unrecorded while two more arrive is pruned before it can be
	// computed — which is the behaviour firstUnrecorded is bounded by.
	feedHour(r, h0, SamplesPerHour, spot, basis)
	if err := r.Emit(ctx, h0); err != nil {
		t.Fatalf("emit: %v", err)
	}

	// Then two hours pass before the next tick, which is what a delayed timer
	// looks like.
	feedHour(r, h0.Add(time.Hour), SamplesPerHour, spot, basis)
	feedHour(r, h0.Add(2*time.Hour), SamplesPerHour, spot, basis)
	newest := h0.Add(2 * time.Hour)
	for h := r.firstUnrecorded(newest); !h.After(newest); h = h.Add(time.Hour) {
		if err := r.Emit(ctx, h); err != nil {
			t.Fatalf("catch-up emit: %v", err)
		}
	}

	rows := accrualRows(t, sink)
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3: the hour the late tick stepped over was skipped", len(rows))
	}
	for i, want := range []time.Time{h0, h0.Add(time.Hour), h0.Add(2 * time.Hour)} {
		if !rows[i].TS.Equal(want) {
			t.Errorf("row %d is hour %s, want %s", i, rows[i].TS, want)
		}
	}
}

func TestCatchUpDoesNotReachBackFurtherThanTheSamplesGo(t *testing.T) {
	r := NewFundingRunner(testPerp, maintenanceWindow(t), nil, &fakeSink{}, testLogger(), nil)

	newest := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	// A runner that has been idle for a day must not try to emit a day of hours
	// it has no marks for; that would only produce a run of "not markable" lines.
	r.Seed(dec(t, "0.0001"), newest.Add(-24*time.Hour))

	first := r.firstUnrecorded(newest)
	if oldest := newest.Add(-sampleRetention); first.Before(oldest) {
		t.Errorf("catch-up starts at %s, older than the %s of samples kept", first, sampleRetention)
	}
}
