package ingest

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/Jason-Dorman/funding-carry/internal/config"
	"github.com/Jason-Dorman/funding-carry/internal/db"
)

const sampleEvery = 5 * time.Second

func maintenanceWindow(t *testing.T) config.MaintenanceWindow {
	t.Helper()
	w, err := config.ParseMaintenanceWindow("Fri 17:00-18:00 America/New_York")
	if err != nil {
		t.Fatalf("parse maintenance window: %v", err)
	}
	return w
}

func newSampler(t *testing.T, sink *fakeSink) *VenueState {
	t.Helper()
	return NewVenueState(testPerp, testSpot, sampleEvery, maintenanceWindow(t), sink, testLogger(), nil)
}

func venueRows(t *testing.T, sink *fakeSink) map[string]db.VenueStateRow {
	t.Helper()
	rows := map[string]db.VenueStateRow{}
	for _, r := range sink.collected() {
		// A sink is shared across producers in the wired system, so other row
		// types are expected here rather than a mistake.
		if row, ok := r.(db.VenueStateRow); ok {
			rows[row.ProductID] = row
		}
	}
	return rows
}

func quote(t *testing.T, bid, ask, last string, at time.Time) Quote {
	t.Helper()
	q := Quote{At: at, VenueT: at}
	if bid != "" {
		q.Bid = db.Num(dec(t, bid))
	}
	if ask != "" {
		q.Ask = db.Num(dec(t, ask))
	}
	if last != "" {
		q.Last = db.Num(dec(t, last))
	}
	return q
}

func TestVenueStateSamplesMidAndSpreadOnTheBoundary(t *testing.T) {
	sink := &fakeSink{}
	v := newSampler(t, sink)

	// The sample runs partway through a window; the row must be stamped with the
	// boundary, not with the clock.
	at := epoch.Add(7 * time.Second)
	v.Observe(testPerp, quote(t, "99.5", "100.5", "100", at))

	if err := v.sample(context.Background(), at); err != nil {
		t.Fatalf("sample: %v", err)
	}

	row := venueRows(t, sink)[testPerp]
	if want := epoch.Add(5 * time.Second); !row.TS.Equal(want) {
		t.Errorf("ts = %s, want the boundary %s", row.TS, want)
	}
	if got := row.Mid.Decimal.String(); got != "100" {
		t.Errorf("mid = %s, want 100", got)
	}
	// (100.5 - 99.5) / 100 * 10000
	if got := row.SpreadBps.Decimal.String(); got != "100" {
		t.Errorf("spread_bps = %s, want 100", got)
	}

	// Everything Part 5 owns stays NULL. A premium computed against a mid
	// standing in for a three-minute VWAP would be a number the decision engine
	// acts on and nobody can reproduce.
	for name, v := range map[string]decimal.NullDecimal{
		"futures_mark":        row.FuturesMark,
		"spot_mark":           row.SpotMark,
		"funding_rate_hourly": row.FundingRateHourly,
		"funding_rate_est":    row.FundingRateEst,
		"premium_proxy":       row.PremiumProxy,
		"open_interest":       row.OpenInterest,
	} {
		if v.Valid {
			t.Errorf("%s = %v, want NULL until Part 5 has something real to put in it", name, v)
		}
	}
	if row.FundingSource != "" {
		t.Errorf("funding_source = %q, want empty (written as NULL)", row.FundingSource)
	}
}

func TestVenueStateStopsWritingWhenTheQuoteGoesStale(t *testing.T) {
	sink := &fakeSink{}
	v := newSampler(t, sink)

	quotedAt := epoch
	v.Observe(testPerp, quote(t, "99.5", "100.5", "100", quotedAt))

	// One interval past the freshness limit.
	at := quotedAt.Add(time.Duration(maxQuoteAge+1) * sampleEvery)
	if err := v.sample(context.Background(), at); err != nil {
		t.Fatalf("sample: %v", err)
	}

	// Carrying the last known mid forward would keep the series looking healthy
	// through a dead feed, and a frozen price is what the basis, the premium and
	// every downstream band are computed from. A missing row is a gap, which is
	// visible; a repeated row is a lie, which is not.
	if rows := sink.collected(); len(rows) != 0 {
		t.Fatalf("wrote %d rows from a stale quote", len(rows))
	}
}

func TestVenueStateSamplesBothProducts(t *testing.T) {
	sink := &fakeSink{}
	v := newSampler(t, sink)

	v.Observe(testPerp, quote(t, "2344.5", "2345", "2345", epoch))
	v.Observe(testSpot, quote(t, "2344.29", "2344.38", "2344.3", epoch))
	if err := v.sample(context.Background(), epoch); err != nil {
		t.Fatalf("sample: %v", err)
	}

	rows := venueRows(t, sink)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want one per product", len(rows))
	}
	// The perp quotes in half-dollar ticks and the spot in cents; both survive
	// as written, because numeric keeps scale and nothing here is a float.
	if got := rows[testPerp].Mid.Decimal.String(); got != "2344.75" {
		t.Errorf("perp mid = %s, want 2344.75", got)
	}
	if got := rows[testSpot].Mid.Decimal.String(); got != "2344.335" {
		t.Errorf("spot mid = %s, want 2344.335", got)
	}
}

func TestMaintenanceFlag(t *testing.T) {
	// 2026-08-21 is a Friday; 17:30 New York is inside the weekly break.
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	insideWindow := time.Date(2026, 8, 21, 17, 30, 0, 0, ny)
	outsideWindow := time.Date(2026, 8, 21, 12, 0, 0, 0, ny)

	for _, tc := range []struct {
		name       string
		at         time.Time
		status     string // "" = the status channel has not spoken yet
		wantPerp   bool
		wantReason string
	}{
		{
			name: "open market outside the window", at: outsideWindow, status: statusOnline,
			wantPerp: false,
		},
		{
			name: "the venue's published weekly break", at: insideWindow, status: statusOnline,
			wantPerp: true, wantReason: "the calendar alone is enough",
		},
		{
			name: "an unscheduled halt outside the window", at: outsideWindow, status: "offline",
			wantPerp: true, wantReason: "the venue saying so itself covers halts the calendar does not",
		},
		{
			name: "before the status channel has spoken", at: outsideWindow, status: "",
			wantPerp: false, wantReason: "assuming offline at startup would block entries for no reason",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sink := &fakeSink{}
			v := newSampler(t, sink)
			v.Observe(testPerp, quote(t, "99.5", "100.5", "100", tc.at))
			v.Observe(testSpot, quote(t, "99.5", "100.5", "100", tc.at))
			if tc.status != "" {
				v.ObserveStatus(testPerp, tc.status == statusOnline)
			}

			if err := v.sample(context.Background(), tc.at); err != nil {
				t.Fatalf("sample: %v", err)
			}

			rows := venueRows(t, sink)
			if got := rows[testPerp].MaintenanceWindow; got != tc.wantPerp {
				t.Errorf("perp maintenance_window = %v, want %v (%s)", got, tc.wantPerp, tc.wantReason)
			}
			// The spot reference is a Coinbase spot market; it does not close
			// when the futures venue does.
			if rows[testSpot].MaintenanceWindow {
				t.Error("spot maintenance_window = true, want false: the spot market does not close")
			}
		})
	}
}

func TestVenueStateStatusIsIgnoredForAnotherProduct(t *testing.T) {
	sink := &fakeSink{}
	v := newSampler(t, sink)
	v.Observe(testPerp, quote(t, "99.5", "100.5", "100", epoch))
	v.ObserveStatus(testSpot, false)

	if err := v.sample(context.Background(), epoch); err != nil {
		t.Fatalf("sample: %v", err)
	}
	if venueRows(t, sink)[testPerp].MaintenanceWindow {
		t.Error("another product's status set the perp's maintenance flag")
	}
}

func TestVenueStateRunStopsOnCancellation(t *testing.T) {
	sink := &fakeSink{}
	v := NewVenueState(testPerp, testSpot, time.Millisecond, maintenanceWindow(t), sink, testLogger(), nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); v.Run(ctx) }()

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the sampler kept running after cancellation")
	}
}
