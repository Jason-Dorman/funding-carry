package ingest

import (
	"errors"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

// hourSamples builds n samples inside the hour starting at h, with a constant
// basis: futures = spot * (1 + basis).
//
// The first window inside an hour ENDS at h+3m, not at h: a sample stamped h
// describes the three minutes before the hour. Encoding that here rather than
// in each test is what keeps the convention in one place.
func hourSamples(t *testing.T, h time.Time, n int, spot, basis string) []MarkSample {
	t.Helper()
	s := dec(t, spot)
	b := dec(t, basis)
	out := make([]MarkSample, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, MarkSample{
			At:          h.Add(time.Duration(i) * SampleInterval),
			SpotMark:    s,
			FuturesMark: s.Add(s.Mul(b)),
		})
	}
	return out
}

func TestTheVenuesFormula(t *testing.T) {
	h := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)

	// A flat 0.24% basis over a whole hour. Every sample's premium is
	// 0.0024/24 = 0.0001, so the hour's mean premium is 0.0001 exactly.
	f, err := RateForHour(h, hourSamples(t, h, SamplesPerHour, "2000", "0.0024"), decimal.NullDecimal{})
	if err != nil {
		t.Fatalf("rate: %v", err)
	}
	if got := f.Premium.String(); got != "0.0001" {
		t.Errorf("premium = %s, want 0.0001 — (futures-spot)/spot/24", got)
	}
	// No previous rate: the recursion has no seed, so the premium stands as the
	// estimate rather than being smoothed against nothing.
	if got := f.Rate.String(); got != "0.0001" {
		t.Errorf("first-hour rate = %s, want the premium %s", got, f.Premium)
	}
	// 0.0001 x 24 x 365
	if got := f.Annualized.String(); got != "0.876" {
		t.Errorf("annualized = %s, want 0.876 (87.6%%)", got)
	}
	if f.Samples != SamplesPerHour {
		t.Errorf("samples = %d, want %d", f.Samples, SamplesPerHour)
	}
}

func TestSmoothingIsThreeQuartersNewOneQuarterOld(t *testing.T) {
	h := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	previous := decimal.NullDecimal{Decimal: dec(t, "0.0002"), Valid: true}

	f, err := RateForHour(h, hourSamples(t, h, SamplesPerHour, "2000", "0.0024"), previous)
	if err != nil {
		t.Fatalf("rate: %v", err)
	}
	// 0.75 x 0.0001 + 0.25 x 0.0002 = 0.000075 + 0.00005 = 0.000125
	if got := f.Rate.String(); got != "0.000125" {
		t.Errorf("rate = %s, want 0.000125 — 0.75 x premium + 0.25 x previous", got)
	}
}

func TestANegativeBasisGivesNegativeFunding(t *testing.T) {
	h := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)

	// Backwardation: the future trades below spot, so shorts pay longs and the
	// carry this system collects goes the other way. The sign has to survive.
	f, err := RateForHour(h, hourSamples(t, h, SamplesPerHour, "2000", "-0.0024"), decimal.NullDecimal{})
	if err != nil {
		t.Fatalf("rate: %v", err)
	}
	if !f.Rate.IsNegative() {
		t.Fatalf("rate = %s, want negative for a future below spot", f.Rate)
	}
	if got := f.Premium.String(); got != "-0.0001" {
		t.Errorf("premium = %s, want -0.0001", got)
	}
}

func TestAnHourWithTooFewSamplesIsAGapNotAZero(t *testing.T) {
	h := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)

	_, err := RateForHour(h, hourSamples(t, h, MinSamplesPerHour-1, "2000", "0.0024"), decimal.NullDecimal{})

	// Averaging whatever survived a feed gap would publish a rate derived from a
	// few minutes under a column claiming to describe the hour — and the
	// z-score would then be computed over it without knowing.
	var tooFew *ErrTooFewSamples
	if !errors.As(err, &tooFew) {
		t.Fatalf("err = %v, want ErrTooFewSamples", err)
	}
	if tooFew.Got != MinSamplesPerHour-1 {
		t.Errorf("reported %d samples, want %d", tooFew.Got, MinSamplesPerHour-1)
	}
}

func TestSamplesOutsideTheHourAreNotCounted(t *testing.T) {
	h := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)

	samples := hourSamples(t, h, SamplesPerHour, "2000", "0.0024")
	// A neighbouring hour at a wildly different basis. If the window leaked, the
	// premium would move.
	samples = append(samples, hourSamples(t, h.Add(time.Hour), SamplesPerHour, "2000", "0.05")...)
	samples = append(samples, hourSamples(t, h.Add(-time.Hour), SamplesPerHour, "2000", "-0.05")...)

	f, err := RateForHour(h, samples, decimal.NullDecimal{})
	if err != nil {
		t.Fatalf("rate: %v", err)
	}
	if got := f.Premium.String(); got != "0.0001" {
		t.Errorf("premium = %s, want 0.0001: the neighbouring hours leaked in", got)
	}
	if f.Samples != SamplesPerHour {
		t.Errorf("samples = %d, want exactly the hour's %d", f.Samples, SamplesPerHour)
	}
}

func TestAZeroSpotMarkIsRefused(t *testing.T) {
	h := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	samples := hourSamples(t, h, SamplesPerHour, "2000", "0.0024")
	samples[3].SpotMark = decimal.Zero

	// The premium divides by the spot mark. A zero would panic or, worse,
	// silently produce an enormous rate.
	if _, err := RateForHour(h, samples, decimal.NullDecimal{}); err == nil {
		t.Fatal("a zero spot mark was accepted")
	}
}

func TestSeriesCarriesTheSmoothingForward(t *testing.T) {
	h0 := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	var hours []time.Time
	var samples []MarkSample
	for i := range 3 {
		h := h0.Add(time.Duration(i) * time.Hour)
		hours = append(hours, h)
		samples = append(samples, hourSamples(t, h, SamplesPerHour, "2000", "0.0024")...)
	}

	out, skipped := Series(hours, samples, decimal.NullDecimal{})
	if len(skipped) != 0 || len(out) != 3 {
		t.Fatalf("got %d hours and %d skips, want 3 and 0", len(out), len(skipped))
	}
	// A constant basis smooths toward itself and must stay there: h0 has no
	// seed so it is the premium, and every later hour is 0.75x + 0.25x = x.
	for i, f := range out {
		if got := f.Rate.String(); got != "0.0001" {
			t.Errorf("hour %d rate = %s, want 0.0001", i, got)
		}
	}
}

func TestSeriesReportsAnUnmarkableHourAndDoesNotInventIt(t *testing.T) {
	h0 := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	hours := []time.Time{h0, h0.Add(time.Hour), h0.Add(2 * time.Hour)}

	var samples []MarkSample
	samples = append(samples, hourSamples(t, h0, SamplesPerHour, "2000", "0.0024")...)
	// The middle hour is the Friday maintenance break, or a dead feed: too thin
	// to mark.
	samples = append(samples, hourSamples(t, h0.Add(time.Hour), 2, "2000", "0.0024")...)
	samples = append(samples, hourSamples(t, h0.Add(2*time.Hour), SamplesPerHour, "2000", "0.0024")...)

	out, skipped := Series(hours, samples, decimal.NullDecimal{})
	if len(out) != 2 {
		t.Fatalf("hours computed = %d, want 2: the thin hour must not be filled in", len(out))
	}
	if len(skipped) != 1 {
		t.Fatalf("skips = %d, want 1", len(skipped))
	}
	// The hour that is missing is the one that was thin, and the series resumes
	// at the right place rather than shifting.
	if !out[1].HourStart.Equal(h0.Add(2 * time.Hour)) {
		t.Errorf("second computed hour = %s, want %s", out[1].HourStart, h0.Add(2*time.Hour))
	}
}

func TestHourAndSampleBoundaries(t *testing.T) {
	at := time.Date(2026, 8, 22, 12, 47, 31, 500_000_000, time.UTC)
	if got := HourOf(at); !got.Equal(time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("HourOf = %s, want the hour start", got)
	}
	// 12:47:31.5 falls in the three-minute window that opened at 12:45.
	if got := SampleBoundary(at); !got.Equal(time.Date(2026, 8, 22, 12, 45, 0, 0, time.UTC)) {
		t.Errorf("SampleBoundary = %s, want 12:45", got)
	}
}

func TestSamplesPerHourMatchesTheVenuesFormula(t *testing.T) {
	// The venue documents twenty three-minute samples per funding hour. If either
	// constant moves, the mean is over a different window than the formula says.
	if SamplesPerHour != 20 {
		t.Errorf("SamplesPerHour = %d, want 20 (venue doc section 3)", SamplesPerHour)
	}
	if SampleInterval != 3*time.Minute {
		t.Errorf("SampleInterval = %s, want 3m", SampleInterval)
	}
}

// An hour's twenty samples are the twenty windows that lie INSIDE it.
//
// MarkSample.At is the window's END, so the sample stamped exactly hourStart
// describes [hourStart-3m, hourStart) — data belonging entirely to the previous
// hour — while the window covering the hour's final three minutes is stamped
// hourStart+1h. Selecting on `At >= hourStart && At < hourStart+1h` therefore
// took one window from the hour before and dropped the last one of its own,
// shifting every rate in the series by three minutes.
//
// It matters most exactly where the market moves most. The last window before
// the Friday close is when makers pull and the spread blows out from ~4 bps to
// 65.6 bps (observed in the Part 4 soak); under the old filter that window was
// excluded from the hour it happened in and counted in the maintenance hour,
// which is skipped entirely — so it was discarded.
func TestAnHourUsesTheWindowsInsideIt(t *testing.T) {
	h := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	spot := dec(t, "2000")

	mark := func(at time.Time, basis string) MarkSample {
		b := dec(t, basis)
		return MarkSample{At: at, SpotMark: spot, FuturesMark: spot.Add(spot.Mul(b))}
	}

	var samples []MarkSample
	// The window ending exactly at h covers [h-3m, h): the PREVIOUS hour. Given
	// a wildly different basis so its inclusion is unmissable.
	samples = append(samples, mark(h, "1.0"))
	// The twenty windows inside the hour end at h+3m .. h+60m.
	for i := 1; i <= SamplesPerHour; i++ {
		samples = append(samples, mark(h.Add(time.Duration(i)*SampleInterval), "0.0024"))
	}

	f, err := RateForHour(h, samples, decimal.NullDecimal{})
	if err != nil {
		t.Fatalf("rate: %v", err)
	}

	if f.Samples != SamplesPerHour {
		t.Errorf("samples = %d, want %d", f.Samples, SamplesPerHour)
	}
	// Every window inside the hour has the same basis, so the premium is exact.
	// If the previous hour's window leaked in, this is nowhere near it.
	if got := f.Premium.String(); got != "0.0001" {
		t.Errorf("premium = %s, want 0.0001; a window from the previous hour was counted", got)
	}
}

func TestTheWindowClosingOnTheHourBelongsToTheHourItCovers(t *testing.T) {
	h := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	spot := dec(t, "2000")
	sample := MarkSample{At: h, SpotMark: spot, FuturesMark: spot}

	// A sample stamped 12:00 describes 11:57-12:00. It must count toward hour
	// 11, and must not count toward hour 12.
	if _, err := RateForHour(h, []MarkSample{sample}, decimal.NullDecimal{}); err == nil {
		t.Error("the window ending at the hour start was counted in that hour")
	}

	prev := h.Add(-time.Hour)
	var tooFew *ErrTooFewSamples
	_, err := RateForHour(prev, []MarkSample{sample}, decimal.NullDecimal{})
	if !errors.As(err, &tooFew) {
		t.Fatalf("err = %v, want ErrTooFewSamples", err)
	}
	if tooFew.Got != 1 {
		t.Errorf("the previous hour saw %d samples, want 1: the window belongs to the hour it covers", tooFew.Got)
	}
}

// The smoothing recursion must actually carry forward, and the coefficients
// must be the venue's.
//
// Every other test in this file uses a CONSTANT basis, where 0.75x + 0.25x = x
// for any pair of weights summing to one — so 0.5/0.5, or no smoothing at all,
// passed them. Deleting the carry-forward line in Series left the whole repo
// suite green, which is what this covers: two hours at DIFFERENT bases, where
// only the venue's weights give the right answer.
func TestSeriesSmoothingUsesTheVenuesWeights(t *testing.T) {
	h0 := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	h1 := h0.Add(time.Hour)

	var samples []MarkSample
	samples = append(samples, hourSamples(t, h0, SamplesPerHour, "2000", "0.0024")...) // premium 0.0001
	samples = append(samples, hourSamples(t, h1, SamplesPerHour, "2000", "0.0048")...) // premium 0.0002

	out, skipped := Series([]time.Time{h0, h1}, samples, decimal.NullDecimal{})
	if len(skipped) != 0 || len(out) != 2 {
		t.Fatalf("got %d hours, %d skips; want 2 and 0", len(out), len(skipped))
	}

	if got := out[0].Rate.String(); got != "0.0001" {
		t.Fatalf("first hour = %s, want its own premium 0.0001 (no previous rate to smooth against)", got)
	}
	// 0.75 x 0.0002 + 0.25 x 0.0001 = 0.00015 + 0.000025
	if got := out[1].Rate.String(); got != "0.000175" {
		t.Errorf("second hour = %s, want 0.000175. Unsmoothed would be 0.0002; "+
			"equal weights would be 0.00015", got)
	}
}

func TestSeriesSeedIsUsedForTheFirstHour(t *testing.T) {
	h := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	samples := hourSamples(t, h, SamplesPerHour, "2000", "0.0024") // premium 0.0001
	seed := decimal.NullDecimal{Decimal: dec(t, "0.0002"), Valid: true}

	out, _ := Series([]time.Time{h}, samples, seed)
	if len(out) != 1 {
		t.Fatalf("hours = %d, want 1", len(out))
	}
	// 0.75 x 0.0001 + 0.25 x 0.0002
	if got := out[0].Rate.String(); got != "0.000125" {
		t.Errorf("rate = %s, want 0.000125: the seed was ignored", got)
	}
}

// A gap must not advance the recursion.
//
// Smoothing an hour against a rate from three hours ago would present a stale
// number as a fresh one, with nothing on the row to say so.
func TestSmoothingDoesNotAdvanceAcrossAGap(t *testing.T) {
	h0 := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	h1, h2 := h0.Add(time.Hour), h0.Add(2*time.Hour)

	var samples []MarkSample
	samples = append(samples, hourSamples(t, h0, SamplesPerHour, "2000", "0.0024")...) // 0.0001
	samples = append(samples, hourSamples(t, h1, 2, "2000", "0.0480")...)              // too thin
	samples = append(samples, hourSamples(t, h2, SamplesPerHour, "2000", "0.0048")...) // 0.0002

	out, skipped := Series([]time.Time{h0, h1, h2}, samples, decimal.NullDecimal{})
	if len(out) != 2 || len(skipped) != 1 {
		t.Fatalf("got %d hours and %d skips, want 2 and 1", len(out), len(skipped))
	}
	// h2 smooths against h0's rate — the last one actually computed — not
	// against anything derived from the unmarkable hour between them.
	if got := out[1].Rate.String(); got != "0.000175" {
		t.Errorf("hour after the gap = %s, want 0.000175 (0.75 x 0.0002 + 0.25 x h0's 0.0001)", got)
	}
}

// The smoothed rate must not grow a digit every hour.
//
// Funding_t = 0.75*Premium_t + 0.25*Funding_{t-1} is a recursion, and shopspring
// Mul is exact: alpha comes out of Div at DivisionPrecision (16 places), so each
// multiplication adds 16 places to the scale and Add keeps the larger. The stored
// rate therefore grew by exactly 16 digits an hour — 18, 34, 50, 66 ... — reaching
// 15,810 digits after 980 hours of backfill, at an average of 7,914.
//
// That is not merely wasteful. PostgreSQL numeric holds at most 16,383 digits
// after the decimal point, so the series was roughly 1,024 hours — six weeks of
// continuous running — from an insert the database would refuse. A refused insert
// is not a retryable failure, so it would have surfaced as a fatal writer error
// and taken the binary down, weeks after the code that caused it shipped.
func TestTheSmoothedRateDoesNotGrowWithoutBound(t *testing.T) {
	h := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)

	// A basis that does not divide cleanly, so the premium uses its full
	// precision budget and the scale has something to compound.
	var hours []time.Time
	var samples []MarkSample
	for i := range 200 {
		hour := h.Add(time.Duration(i) * time.Hour)
		hours = append(hours, hour)
		samples = append(samples, hourSamples(t, hour, SamplesPerHour, "1999.77", "0.00317")...)
	}

	out, _ := Series(hours, samples, decimal.NullDecimal{})
	if len(out) != 200 {
		t.Fatalf("hours = %d, want 200", len(out))
	}

	first := -out[0].Rate.Exponent()
	last := -out[len(out)-1].Rate.Exponent()
	t.Logf("decimal places: hour 1 = %d, hour 200 = %d", first, last)

	// The ceiling the database imposes, with room to spare. Anything that grows
	// per hour fails this long before it fails in production.
	const maxPlaces = 32
	if last > maxPlaces {
		t.Errorf("hour 200 has %d decimal places (hour 1 had %d): the scale grows with the "+
			"recursion and PostgreSQL numeric stops at 16383", last, first)
	}
}
