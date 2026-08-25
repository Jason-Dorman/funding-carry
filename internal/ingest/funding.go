package ingest

import (
	"fmt"
	"sort"
	"time"

	"github.com/shopspring/decimal"
)

// The funding estimator.
//
// The venue does publish an hourly funding rate (venue doc section 3), so this
// estimator is not the only source of a current rate. It stays the primary
// series anyway, for two reasons the venue's number cannot cover: it is the
// only route to *history* — the venue serves the current rate and nothing
// earlier — and running it beside the published rate is what makes that rate
// checkable at all, which is what carry_funding_reconciliation_error measures.
// The formula is the venue's own, reproduced exactly:
//
//	premium_i = (futures_mark_i - spot_mark_i) / spot_mark_i / 24   per 3-min sample
//	Premium_t = mean(premium_i over the hour)                       20 samples
//	Funding_t = 0.75 x Premium_t + 0.25 x Funding_{t-1}             alpha = 0.75
//
// Everything here is a pure function of its inputs, which is what lets the same
// code produce both the live series and the reconstructed history: live, the
// marks are 3-minute VWAPs of trades off the socket; backfilled, they are built
// from one-minute candles. Same arithmetic, different provenance, and the row
// records which (ADR-0015).

const (
	// SampleInterval is the venue's sampling period for the premium.
	SampleInterval = 3 * time.Minute

	// SamplesPerHour is how many samples a whole funding hour contains.
	SamplesPerHour = int(time.Hour / SampleInterval) // 20

	// MinSamplesPerHour is how many of them must be present for the hour to
	// produce a rate at all.
	//
	// The venue's formula assumes twenty. A feed gap leaves fewer, and averaging
	// whatever survived would publish a rate derived from a handful of minutes
	// under a column that claims to describe the hour — the same class of
	// mistake as writing a zero where the truth is unknown. Below this an hour
	// is a gap, exactly as the maintenance hour is.
	//
	// Two thirds is a judgment, not a measurement: there is no history yet to
	// fit it against. It is deliberately written as one constant so that when
	// there is, moving it is one line and one changed test.
	MinSamplesPerHour = SamplesPerHour * 2 / 3 // 13

	// FundingScale is the number of decimal places a computed rate keeps.
	//
	// It exists because the smoothing is a recursion and shopspring Mul is exact:
	// without rounding, every hour adds the multiplier's scale to the result's,
	// and the stored rate grew by sixteen digits an hour — 15,810 of them after
	// 980 hours. PostgreSQL numeric stops at 16,383 places, so the series was
	// about six weeks of continuous running from an insert the database would
	// refuse, which is a fatal writer error rather than a retryable one.
	//
	// Sixteen places matches decimal.DivisionPrecision, the budget this project
	// already declares for derived ratios (API spec section 3.5). On a rate of
	// order 1e-5 that is eleven significant figures — far beyond anything the
	// venue publishes or the strategy can act on — and the rounding error it
	// introduces cannot accumulate, because the recursion is contractive: each
	// step keeps only a quarter of the history.
	FundingScale = 16
)

var (
	twentyFour   = decimal.NewFromInt(24)
	hoursPerYear = decimal.NewFromInt(24 * 365)
	// The venue's 0.75 smoothing, written exactly. Deriving it with Div would
	// give it DivisionPrecision's sixteen places, and every multiplication would
	// then hand all sixteen of them to the result.
	alpha         = decimal.RequireFromString("0.75")
	oneMinusAlpha = decimal.RequireFromString("0.25")
)

// MarkSample is one three-minute observation of both marks.
//
// At is the sample boundary — the end of the three-minute window it summarises —
// so an hour's samples are the twenty whose At falls inside it.
type MarkSample struct {
	At          time.Time
	FuturesMark decimal.Decimal
	SpotMark    decimal.Decimal
}

// premium is this sample's contribution: the basis, scaled to an hourly rate.
//
// Div is legitimate here and only here: a premium is a ratio, and ratios round
// to DivisionPrecision by design (API spec section 3.5). Nothing in this file
// divides a quantity that has to reconcile against a venue statement.
func (s MarkSample) premium() (decimal.Decimal, error) {
	if s.SpotMark.IsZero() {
		return decimal.Decimal{}, fmt.Errorf("sample at %s: spot mark is zero", s.At.Format(time.RFC3339))
	}
	return s.FuturesMark.Sub(s.SpotMark).Div(s.SpotMark).Div(twentyFour), nil
}

// Funding is one hour's computed rate and the pieces it was built from, so a
// row can be re-derived from storage rather than trusted.
type Funding struct {
	// HourStart is what the rate is a property of — the hour, not the moment it
	// was computed (API spec section 5.3).
	HourStart time.Time
	// Premium is the hour's mean sample premium, before smoothing.
	Premium decimal.Decimal
	// Rate is the smoothed hourly funding rate.
	Rate decimal.Decimal
	// Annualized is Rate x 24 x 365, which is how every dashboard reads it.
	Annualized decimal.Decimal
	// Samples is how many of the twenty were present. A consumer weighting the
	// series by confidence needs it, and the reconciliation needs it to explain
	// a divergence.
	Samples int
}

// ErrTooFewSamples reports an hour that cannot be marked. It is a gap, not a
// zero, and the caller writes no row for it.
type ErrTooFewSamples struct {
	HourStart time.Time
	Got, Want int
}

func (e *ErrTooFewSamples) Error() string {
	return fmt.Sprintf("hour %s: %d of %d samples, need %d",
		e.HourStart.Format(time.RFC3339), e.Got, SamplesPerHour, e.Want)
}

// RateForHour computes one hour's funding from its samples and the previous
// hour's rate.
//
// previous being absent is the first hour of a series: with no prior rate to
// smooth against, the venue's recursion has no seed, and the premium itself is
// the best available estimate. That makes the first hour of any run — and the
// first hour of a backfill — slightly different from its neighbours, which is
// why Samples and Premium are on the result rather than only the rate.
func RateForHour(hourStart time.Time, samples []MarkSample, previous decimal.NullDecimal) (Funding, error) {
	// An hour's samples are the windows that lie INSIDE it. At is the window's
	// END, so the interval is half-open the other way round from the obvious
	// reading: the window ending exactly at hourStart covers the three minutes
	// before the hour and belongs to the previous one, while the window ending
	// at hourStart+1h covers the hour's last three minutes and belongs to this
	// one. Getting this backwards shifts every rate in the series by three
	// minutes and, at the Friday close, throws away the most informative window
	// of the week into an hour that is skipped anyway.
	end := hourStart.Add(time.Hour)
	inHour := make([]MarkSample, 0, SamplesPerHour)
	for _, s := range samples {
		if s.At.After(hourStart) && !s.At.After(end) {
			inHour = append(inHour, s)
		}
	}
	if len(inHour) < MinSamplesPerHour {
		return Funding{}, &ErrTooFewSamples{HourStart: hourStart, Got: len(inHour), Want: MinSamplesPerHour}
	}

	sum := decimal.Zero
	for _, s := range inHour {
		p, err := s.premium()
		if err != nil {
			return Funding{}, err
		}
		sum = sum.Add(p)
	}
	premium := sum.Div(decimal.NewFromInt(int64(len(inHour))))

	rate := premium
	if previous.Valid {
		rate = alpha.Mul(premium).Add(oneMinusAlpha.Mul(previous.Decimal))
	}
	// Bound the scale before the value re-enters the recursion as the next
	// hour's `previous`; see FundingScale.
	rate = rate.Round(FundingScale)

	return Funding{
		HourStart:  hourStart,
		Premium:    premium,
		Rate:       rate,
		Annualized: rate.Mul(hoursPerYear),
		Samples:    len(inHour),
	}, nil
}

// HourOf is the funding hour a time falls in. The rate is a property of the
// hour, so this is what stamps both the cb_venue_state row and the ACCRUAL.
func HourOf(t time.Time) time.Time { return t.UTC().Truncate(time.Hour) }

// SampleBoundary is the three-minute boundary a time falls on.
func SampleBoundary(t time.Time) time.Time { return t.UTC().Truncate(SampleInterval) }

// Series computes a run of consecutive hours, carrying the smoothing forward.
//
// It is the shape the backfill needs — a whole history at once — and it is also
// what makes the live path testable against the reconstructed one, since both
// reduce to the same call. Hours that cannot be marked are skipped and reported
// rather than filled: a gap in the funding series must stay a gap, or the
// z-score is computed over invented data.
func Series(hours []time.Time, samples []MarkSample, seed decimal.NullDecimal) ([]Funding, []error) {
	sort.Slice(hours, func(i, j int) bool { return hours[i].Before(hours[j]) })

	out := make([]Funding, 0, len(hours))
	var skipped []error
	previous := seed
	for _, h := range hours {
		f, err := RateForHour(h, samples, previous)
		if err != nil {
			skipped = append(skipped, err)
			// The recursion is deliberately not advanced across a gap: smoothing
			// an hour against a rate from two hours ago would quietly present a
			// stale number as a fresh one. The next markable hour starts from
			// the last rate that was actually computed, and its Samples count
			// says the series was interrupted.
			continue
		}
		out = append(out, f)
		previous = decimal.NullDecimal{Decimal: f.Rate, Valid: true}
	}
	return out, skipped
}
