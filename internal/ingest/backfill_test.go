package ingest

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/Jason-Dorman/funding-carry/internal/db"
)

// candleSeries builds one-minute candles over n minutes at a constant price and
// volume, keyed by start the way the backfill holds them.
func candleSeries(t *testing.T, start time.Time, n int, price, volume string) map[time.Time]Candle {
	t.Helper()
	out := map[time.Time]Candle{}
	px, vol := dec(t, price), dec(t, volume)
	for i := range n {
		s := start.Add(time.Duration(i) * time.Minute)
		out[s] = Candle{Start: s, Open: px, High: px, Low: px, Close: px, Volume: vol}
	}
	return out
}

func TestCandlesBecomeThreeMinuteMarks(t *testing.T) {
	start := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	perp := candleSeries(t, start, 6, "2024", "10")
	spot := candleSeries(t, start, 6, "2000", "10")

	samples := MarkSamplesFromCandles(perp, spot)

	if len(samples) != 2 {
		t.Fatalf("samples = %d, want 2 (six minutes is two windows)", len(samples))
	}
	// Stamped with the window end, matching the live path, so an hour's twenty
	// samples are the same twenty either way.
	if !samples[0].At.Equal(start.Add(SampleInterval)) {
		t.Errorf("first sample at %s, want %s", samples[0].At, start.Add(SampleInterval))
	}
	if got := samples[0].FuturesMark.String(); got != "2024" {
		t.Errorf("futures mark = %s, want 2024", got)
	}
}

func TestAWindowIsSkippedUnlessBothProductsTraded(t *testing.T) {
	start := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	perp := candleSeries(t, start, 6, "2024", "10")
	spot := candleSeries(t, start, 3, "2000", "10") // spot stops after one window

	samples := MarkSamplesFromCandles(perp, spot)

	// A premium needs both marks. Marking one side against a price that was
	// never observed would fabricate the basis, which is the whole signal.
	if len(samples) != 1 {
		t.Fatalf("samples = %d, want 1: the second window has no spot mark", len(samples))
	}
}

func TestZeroVolumeCandlesDoNotMark(t *testing.T) {
	start := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	perp := candleSeries(t, start, 3, "2024", "0")
	spot := candleSeries(t, start, 3, "2000", "10")

	// The perp publishes candles with zero volume overnight. Weighting by volume
	// makes those contribute nothing, so the window has no futures mark at all
	// rather than one derived from a price nothing traded at.
	if got := MarkSamplesFromCandles(perp, spot); len(got) != 0 {
		t.Fatalf("samples = %d, want 0 when the perp had no volume", len(got))
	}
}

func TestMarkIsVolumeWeightedAcrossTheWindow(t *testing.T) {
	start := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	perp := map[time.Time]Candle{
		start:                      {Start: start, Close: dec(t, "2000"), Volume: dec(t, "1")},
		start.Add(time.Minute):     {Start: start.Add(time.Minute), Close: dec(t, "2100"), Volume: dec(t, "3")},
		start.Add(2 * time.Minute): {Start: start.Add(2 * time.Minute), Close: dec(t, "2000"), Volume: dec(t, "0")},
	}
	spot := candleSeries(t, start, 3, "2000", "10")

	samples := MarkSamplesFromCandles(perp, spot)
	if len(samples) != 1 {
		t.Fatalf("samples = %d, want 1", len(samples))
	}
	// (2000*1 + 2100*3) / 4 = 8300/4. The zero-volume minute contributes
	// nothing rather than dragging the mark toward a price nothing traded at.
	if got := samples[0].FuturesMark.String(); got != "2075" {
		t.Errorf("futures mark = %s, want 2075", got)
	}
}

func TestReconstructedSeriesMatchesTheLiveFormula(t *testing.T) {
	// The point of building both paths on RateForHour: given the same marks,
	// the reconstruction and the live path must agree exactly. If they ever
	// diverge, the backfilled history is not comparable with what follows it.
	start := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	perp := candleSeries(t, start, 60, "2004.8", "10")
	spot := candleSeries(t, start, 60, "2000", "10")

	samples := MarkSamplesFromCandles(perp, spot)
	if len(samples) < MinSamplesPerHour {
		t.Fatalf("only %d samples; the hour would be unmarkable", len(samples))
	}

	f, err := RateForHour(start, samples, decimal.NullDecimal{})
	if err != nil {
		t.Fatalf("rate: %v", err)
	}
	// basis 4.8/2000 = 0.0024, premium 0.0024/24 = 0.0001
	if got := f.Rate.String(); got != "0.0001" {
		t.Errorf("reconstructed rate = %s, want 0.0001 — the same number the live path computes", got)
	}
}

// fakeStore stands in for the database's read side.
type fakeStore struct {
	bars  map[string][]db.StoredBar
	hours map[string][]time.Time
	reads int
}

func (f *fakeStore) ReadBars(_ context.Context, product, _ string, from, to time.Time) ([]db.StoredBar, error) {
	f.reads++
	var out []db.StoredBar
	for _, b := range f.bars[product] {
		if !b.TS.Before(from) && b.TS.Before(to) {
			out = append(out, b)
		}
	}
	return out, nil
}

func (f *fakeStore) LatestFundingRate(_ context.Context, product string, before time.Time) (decimal.Decimal, time.Time, bool, error) {
	var best time.Time
	for _, h := range f.hours[product] {
		if h.Before(before) && h.After(best) {
			best = h
		}
	}
	if best.IsZero() {
		return decimal.Decimal{}, time.Time{}, false, nil
	}
	return decimal.NewFromInt(1), best, true, nil
}

func (f *fakeStore) ReadFundingHours(_ context.Context, product string, from, to time.Time) ([]time.Time, error) {
	var out []time.Time
	for _, h := range f.hours[product] {
		if !h.Before(from) && h.Before(to) {
			out = append(out, h)
		}
	}
	return out, nil
}

// storeWithMinutes builds a store holding one bar per minute over [from, to),
// except the minutes listed as absent.
func storeWithMinutes(t *testing.T, product string, from, to time.Time, absent ...time.Time) *fakeStore {
	t.Helper()
	gone := map[time.Time]struct{}{}
	for _, a := range absent {
		gone[a] = struct{}{}
	}
	s := &fakeStore{bars: map[string][]db.StoredBar{}, hours: map[string][]time.Time{}}
	for at := from; at.Before(to); at = at.Add(time.Minute) {
		if _, skip := gone[at]; skip {
			continue
		}
		// cb_bars.ts is the bar's CLOSE, one interval past the candle start.
		s.bars[product] = append(s.bars[product], db.StoredBar{
			TS: at.Add(time.Minute), Open: dec(t, "2000"), High: dec(t, "2000"),
			Low: dec(t, "2000"), Close: dec(t, "2000"), Volume: dec(t, "10"),
		})
	}
	return s
}

func TestMissingRangesCoalescesHoles(t *testing.T) {
	start := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	end := start.Add(10 * time.Minute)

	have := map[time.Time]Candle{}
	for i := range 10 {
		have[start.Add(time.Duration(i)*time.Minute)] = Candle{}
	}
	// Punch out 12:02, then 12:05-12:07.
	delete(have, start.Add(2*time.Minute))
	for i := 5; i <= 7; i++ {
		delete(have, start.Add(time.Duration(i)*time.Minute))
	}

	got := missingRanges(have, start, end)

	// Two ranges, not four requests: scattered holes must coalesce or a day with
	// a thousand gaps becomes a thousand round trips.
	if len(got) != 2 {
		t.Fatalf("ranges = %d, want 2: %+v", len(got), got)
	}
	if !got[0].from.Equal(start.Add(2*time.Minute)) || !got[0].to.Equal(start.Add(3*time.Minute)) {
		t.Errorf("first range = %v..%v, want a single minute at 12:02", got[0].from, got[0].to)
	}
	if !got[1].from.Equal(start.Add(5*time.Minute)) || !got[1].to.Equal(start.Add(8*time.Minute)) {
		t.Errorf("second range = %v..%v, want 12:05..12:08", got[1].from, got[1].to)
	}
}

func TestCompleteHistoryDownloadsNothing(t *testing.T) {
	end := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	window := 2 * time.Hour
	start := end.Add(-window)

	store := storeWithMinutes(t, testSpot, start, end)
	store.bars[testPerp] = store.bars[testSpot]
	// Every hour already has a rate, so there is nothing to compute either.
	for h := start; h.Before(end); h = h.Add(time.Hour) {
		store.hours[testPerp] = append(store.hours[testPerp], h)
	}

	sink := &fakeSink{}
	// A nil REST client is the assertion: touching the network panics.
	b := NewBackfill(testPerp, testSpot, nil, store, sink, maintenanceWindow(t), testLogger())

	res, err := b.Run(context.Background(), end, window)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Gaps != 0 || res.Fetched != 0 {
		t.Errorf("gaps=%d fetched=%d, want 0 and 0 when the history is already stored", res.Gaps, res.Fetched)
	}
	if rows := sink.collected(); len(rows) != 0 {
		t.Errorf("wrote %d rows for history that was already complete", len(rows))
	}
	if res.Bars == 0 {
		t.Error("computed from 0 bars: the stored history was not read at all")
	}
}

func TestAGapIsDetectedAndOnlyTheGapIsFetched(t *testing.T) {
	end := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	window := time.Hour
	start := end.Add(-window)

	// One missing minute in the middle of an otherwise complete hour.
	missing := start.Add(30 * time.Minute)
	store := storeWithMinutes(t, testSpot, start, end, missing)
	store.bars[testPerp] = store.bars[testSpot]

	gaps := missingRanges(candlesFromStore(t, store, testSpot), start, end)
	if len(gaps) != 1 {
		t.Fatalf("ranges = %d, want 1", len(gaps))
	}
	if !gaps[0].from.Equal(missing) || !gaps[0].to.Equal(missing.Add(time.Minute)) {
		t.Errorf("range = %v..%v, want exactly the missing minute", gaps[0].from, gaps[0].to)
	}
}

// candlesFromStore mirrors what Backfill.stored builds, so the gap test works on
// the same shape the production path does.
func candlesFromStore(t *testing.T, s *fakeStore, product string) map[time.Time]Candle {
	t.Helper()
	out := map[time.Time]Candle{}
	for _, b := range s.bars[product] {
		at := b.TS.Add(-time.Minute)
		out[at] = Candle{Start: at, Close: b.Close, Volume: b.Volume}
	}
	return out
}

func TestHoursAlreadyRecordedAreNotRecomputed(t *testing.T) {
	end := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	start := end.Add(-3 * time.Hour)

	store := &fakeStore{bars: map[string][]db.StoredBar{}, hours: map[string][]time.Time{
		testPerp: {start, start.Add(time.Hour)},
	}}
	b := NewBackfill(testPerp, testSpot, nil, store, &fakeSink{}, maintenanceWindow(t), testLogger())

	hours, recorded, err := b.hoursToCompute(context.Background(), start, end)
	if err != nil {
		t.Fatalf("hoursToCompute: %v", err)
	}
	if recorded != 2 {
		t.Errorf("recorded = %d, want 2", recorded)
	}
	if len(hours) != 1 || !hours[0].Equal(start.Add(2*time.Hour)) {
		t.Fatalf("hours = %v, want only the one hour with no rate yet", hours)
	}
}
