package ingest

import (
	"testing"
	"time"

	"github.com/Jason-Dorman/funding-carry/internal/db"
)

func collectMarks() (*[]MarkSample, func(MarkSample)) {
	got := &[]MarkSample{}
	return got, func(s MarkSample) { *got = append(*got, s) }
}

func bucket(t *testing.T, product string, end time.Time, vwap, vol string) db.TradesAggRow {
	t.Helper()
	return db.TradesAggRow{
		ProductID: product, TS: end,
		VWAP:   db.Num(dec(t, vwap)),
		BuyVol: dec(t, vol), SellVol: dec(t, "0"),
	}
}

func TestMarkPrefersVWAPOverMids(t *testing.T) {
	got, out := collectMarks()
	m := NewMarks(testPerp, testSpot, out, testLogger(), nil)

	// Trades in the window for both products, plus mids that disagree. The
	// venue's definition is VWAP first; the mids must not get a vote.
	m.ObserveBucket(bucket(t, testPerp, epoch.Add(time.Minute), "2010", "10"))
	m.ObserveBucket(bucket(t, testPerp, epoch.Add(2*time.Minute), "2030", "10"))
	m.ObserveBucket(bucket(t, testSpot, epoch.Add(time.Minute), "2000", "5"))
	m.ObserveQuote(testPerp, quote(t, "1", "1", "1", epoch.Add(time.Minute)))
	m.ObserveQuote(testSpot, quote(t, "1", "1", "1", epoch.Add(time.Minute)))

	m.Sample(epoch.Add(SampleInterval))

	if len(*got) != 1 {
		t.Fatalf("samples = %d, want 1", len(*got))
	}
	s := (*got)[0]
	// (2010*10 + 2030*10) / 20
	if v := s.FuturesMark.String(); v != "2020" {
		t.Errorf("futures mark = %s, want 2020 (volume weighted, not the mid)", v)
	}
	if v := s.SpotMark.String(); v != "2000" {
		t.Errorf("spot mark = %s, want 2000", v)
	}
	// The sample is stamped with the window's end, which is what files it under
	// the right funding hour.
	if !s.At.Equal(epoch.Add(SampleInterval)) {
		t.Errorf("sample at %s, want the window end %s", s.At, epoch.Add(SampleInterval))
	}
}

func TestMarkFallsBackToMidTWAPWhenNothingTraded(t *testing.T) {
	got, out := collectMarks()
	m := NewMarks(testPerp, testSpot, out, testLogger(), nil)

	// No buckets at all — an overnight lull on the perp is exactly this.
	m.ObserveQuote(testPerp, quote(t, "2009", "2011", "2010", epoch.Add(time.Minute)))
	m.ObserveQuote(testPerp, quote(t, "2029", "2031", "2030", epoch.Add(2*time.Minute)))
	m.ObserveQuote(testSpot, quote(t, "1999", "2001", "2000", epoch.Add(time.Minute)))

	m.Sample(epoch.Add(SampleInterval))

	if len(*got) != 1 {
		t.Fatalf("samples = %d, want 1: a quoted-but-untraded window is still markable", len(*got))
	}
	// (2010 + 2030) / 2
	if v := (*got)[0].FuturesMark.String(); v != "2020" {
		t.Errorf("futures mark = %s, want the mid TWAP 2020", v)
	}
}

func TestMarkCarriesThePreviousGapWhenAProductGoesDark(t *testing.T) {
	got, out := collectMarks()
	m := NewMarks(testPerp, testSpot, out, testLogger(), nil)

	// A first window where both are known establishes the gap: 2020 - 2000 = 20.
	m.ObserveBucket(bucket(t, testPerp, epoch.Add(time.Minute), "2020", "10"))
	m.ObserveBucket(bucket(t, testSpot, epoch.Add(time.Minute), "2000", "10"))
	m.Sample(epoch.Add(SampleInterval))

	// In the next window only spot prints. The perp is marked from spot plus
	// the carried gap rather than left absent, because an absent futures mark
	// would make the hour unmarkable and a zero premium would be a lie.
	m.ObserveBucket(bucket(t, testSpot, epoch.Add(4*time.Minute), "2100", "10"))
	m.Sample(epoch.Add(2 * SampleInterval))

	if len(*got) != 2 {
		t.Fatalf("samples = %d, want 2: the second window should be marked from the carried gap", len(*got))
	}
	if v := (*got)[1].FuturesMark.String(); v != "2120" {
		t.Errorf("carried futures mark = %s, want 2120 (spot 2100 + gap 20)", v)
	}
}

func TestAWindowWithNothingAtAllIsNotMarked(t *testing.T) {
	got, out := collectMarks()
	m := NewMarks(testPerp, testSpot, out, testLogger(), nil)

	m.Sample(epoch.Add(SampleInterval))

	// No trades, no quotes, no prior gap. Emitting anything here would be
	// invention, and the hour is better short a sample than holding a made-up one.
	if len(*got) != 0 {
		t.Fatalf("samples = %d, want 0 for a window with no observations", len(*got))
	}
}

func TestMarksIgnoreDataOutsideTheWindow(t *testing.T) {
	got, out := collectMarks()
	m := NewMarks(testPerp, testSpot, out, testLogger(), nil)

	// A bucket from the previous window and one from the next.
	m.ObserveBucket(bucket(t, testPerp, epoch, "9999", "10"))
	m.ObserveBucket(bucket(t, testPerp, epoch.Add(2*time.Minute), "2020", "10"))
	m.ObserveBucket(bucket(t, testPerp, epoch.Add(10*time.Minute), "8888", "10"))
	m.ObserveBucket(bucket(t, testSpot, epoch.Add(2*time.Minute), "2000", "10"))

	m.Sample(epoch.Add(SampleInterval))

	if len(*got) != 1 {
		t.Fatalf("samples = %d, want 1", len(*got))
	}
	if v := (*got)[0].FuturesMark.String(); v != "2020" {
		t.Errorf("futures mark = %s, want 2020: neighbouring windows leaked in", v)
	}
}

func TestOneSamplePerBoundary(t *testing.T) {
	got, out := collectMarks()
	m := NewMarks(testPerp, testSpot, out, testLogger(), nil)

	m.ObserveBucket(bucket(t, testPerp, epoch.Add(time.Minute), "2020", "10"))
	m.ObserveBucket(bucket(t, testSpot, epoch.Add(time.Minute), "2000", "10"))

	m.Sample(epoch.Add(SampleInterval))
	m.Sample(epoch.Add(SampleInterval).Add(time.Second))

	// Two samples for one window would double that window's weight in the
	// hour's mean.
	if len(*got) != 1 {
		t.Fatalf("samples = %d, want 1 per boundary", len(*got))
	}
}
