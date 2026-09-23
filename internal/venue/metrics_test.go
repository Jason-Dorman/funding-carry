package venue

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// Before anything is read, every source's series exists and says "missing":
// an absent series is a flag nothing can alert on.
func TestMetricsStartWithEverySourceMissing(t *testing.T) {
	reg := prometheus.NewRegistry()
	NewMetrics(reg)

	want := `
# HELP carry_feed_ok MARKET-DATA freshness, not account state: 1 when both the perp and the spot source are fresh, else 0. Never a 'safe to act' signal on its own - anything that gates order flow gates on this AND carry_venue_state_stale{source="account"} == 0 AND carry_venue_state_stale{source="wallet"} == 0. Alone it is the public-stack reading, where those two sources do not exist.
# TYPE carry_feed_ok gauge
carry_feed_ok 0
# HELP carry_venue_state_age_seconds Age of the newest row per source as of the last refresh: now minus the row's own sampling timestamp, so the age of the observation and not of the query. +Inf when the source has no row at all.
# TYPE carry_venue_state_age_seconds gauge
carry_venue_state_age_seconds{source="account"} +Inf
carry_venue_state_age_seconds{source="perp"} +Inf
carry_venue_state_age_seconds{source="spot"} +Inf
carry_venue_state_age_seconds{source="wallet"} +Inf
# HELP carry_venue_state_stale 1 when the source is missing or older than STALE_FEED_SECS, else 0. This is the typed flag the risk engine reads, exported as decided rather than recomputed from the age.
# TYPE carry_venue_state_stale gauge
carry_venue_state_stale{source="account"} 1
carry_venue_state_stale{source="perp"} 1
carry_venue_state_stale{source="spot"} 1
carry_venue_state_stale{source="wallet"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want),
		"carry_feed_ok", "carry_venue_state_age_seconds", "carry_venue_state_stale"); err != nil {
		t.Error(err)
	}
}

// After a refresh the gauges are the state's flags, as decided, and the
// histogram has one observation.
func TestMetricsFollowTheRefreshedState(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	store := fullStore()
	store.haveWallet = false
	// Each source gets a distinct age. With every row on one timestamp, perp,
	// spot and account all published 61 / 1 and a transposed label was
	// invisible — the per-source design is the whole point of these gauges, so
	// the fixture has to be able to tell the sources apart (Part 9 review).
	store.spot.TS = t0.Add(-time.Second)
	store.account.TS = t0.Add(-2 * time.Second)
	c, _ := newCache(store, t0.Add(testLimit+time.Second), m)
	// Perp and spot are past the limit, account too, the wallet is missing.
	if err := c.Refresh(t.Context()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	want := `
# HELP carry_feed_ok MARKET-DATA freshness, not account state: 1 when both the perp and the spot source are fresh, else 0. Never a 'safe to act' signal on its own - anything that gates order flow gates on this AND carry_venue_state_stale{source="account"} == 0 AND carry_venue_state_stale{source="wallet"} == 0. Alone it is the public-stack reading, where those two sources do not exist.
# TYPE carry_feed_ok gauge
carry_feed_ok 0
# HELP carry_venue_state_age_seconds Age of the newest row per source as of the last refresh: now minus the row's own sampling timestamp, so the age of the observation and not of the query. +Inf when the source has no row at all.
# TYPE carry_venue_state_age_seconds gauge
carry_venue_state_age_seconds{source="account"} 63
carry_venue_state_age_seconds{source="perp"} 61
carry_venue_state_age_seconds{source="spot"} 62
carry_venue_state_age_seconds{source="wallet"} +Inf
# HELP carry_venue_state_stale 1 when the source is missing or older than STALE_FEED_SECS, else 0. This is the typed flag the risk engine reads, exported as decided rather than recomputed from the age.
# TYPE carry_venue_state_stale gauge
carry_venue_state_stale{source="account"} 1
carry_venue_state_stale{source="perp"} 1
carry_venue_state_stale{source="spot"} 1
carry_venue_state_stale{source="wallet"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want),
		"carry_feed_ok", "carry_venue_state_age_seconds", "carry_venue_state_stale"); err != nil {
		t.Error(err)
	}
	// Sample count, not series count. A plain histogram is one series from
	// registration onward, observed or not, so counting series let the
	// observation itself be deleted with every suite still green — the
	// acceptance criterion's only instrument, silently empty.
	if got := refreshSamples(t, reg); got != 1 {
		t.Errorf("carry_venue_refresh_seconds has %d observations after one refresh, want 1", got)
	}

	// Fresh rows flip everything back, including feed_ok.
	store.haveWallet = true
	c.opts.Now = func() time.Time { return t0.Add(time.Second) }
	if err := c.Refresh(t.Context()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if got := testutil.ToFloat64(m.feedOK); got != 1 {
		t.Errorf("carry_feed_ok = %v after a fresh refresh, want 1", got)
	}
	if got := testutil.ToFloat64(m.stale.WithLabelValues("wallet")); got != 0 {
		t.Errorf("carry_venue_state_stale{wallet} = %v with a fresh row, want 0", got)
	}
	if got := testutil.ToFloat64(m.age.WithLabelValues("wallet")); got != 1 {
		t.Errorf("carry_venue_state_age_seconds{wallet} = %v, want 1", got)
	}
	// One observation per refresh, which also pins that the second refresh
	// recorded its own.
	if got := refreshSamples(t, reg); got != 2 {
		t.Errorf("carry_venue_refresh_seconds has %d observations after two refreshes, want 2", got)
	}
}

// refreshSamples is how many durations the refresh histogram has actually
// observed.
func refreshSamples(t *testing.T, reg *prometheus.Registry) uint64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() == "carry_venue_refresh_seconds" {
			return f.GetMetric()[0].GetHistogram().GetSampleCount()
		}
	}
	t.Fatal("carry_venue_refresh_seconds is not registered")
	return 0
}

// The acceptance criterion is a refresh under fifty milliseconds, so 0.05 must
// be a bucket boundary — otherwise the histogram cannot answer the question
// the part is accepted on. Asserted against the catalogue's list (API spec
// section 6).
func TestRefreshHistogramBucketsMatchTheCatalogue(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	m.refresh.Observe(0.01)

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var got []float64
	for _, f := range families {
		if f.GetName() != "carry_venue_refresh_seconds" {
			continue
		}
		for _, b := range f.GetMetric()[0].GetHistogram().GetBucket() {
			got = append(got, b.GetUpperBound())
		}
	}
	want := []float64{0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1}
	if len(got) != len(want) {
		t.Fatalf("buckets = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("bucket %d = %v, want %v", i, got[i], want[i])
		}
	}
}
