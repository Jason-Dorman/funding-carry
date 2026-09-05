package ingest

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestExportedSeriesMatchTheCatalogue(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	m.Stream("ticker")

	// The names in API spec section 6, spelled out here so a rename has to be a
	// deliberate edit in two places rather than a silent break of every alert
	// and dashboard that reads them.
	// The base series has one label with three initialized values, so it is
	// counted separately from the single-series names below.
	if n := testutil.CollectAndCount(reg, "ingest_base_spot_px_source_total"); n != 3 {
		t.Errorf("ingest_base_spot_px_source_total: %d series, want 3 (dex, coinbase, none)", n)
	}

	for _, name := range []string{
		"ingest_ws_reconnects_total",
		"ingest_ws_gaps_total",
		"ingest_last_seen_timestamp_seconds",
		"ingest_base_last_block",
	} {
		if n := testutil.CollectAndCount(reg, name); n != 1 {
			t.Errorf("%s: %d series, want 1", name, n)
		}
	}
}

func TestAStreamsSeriesExistBeforeItHasSeenAnything(t *testing.T) {
	reg := prometheus.NewRegistry()
	NewMetrics(reg).Stream("candles")

	// FeedStale is `time() - ingest_last_seen_timestamp_seconds > 60`, and a
	// PromQL expression over a series that does not exist yields nothing rather
	// than firing. A stream that never connected at all is exactly the case the
	// alert most needs to catch, so the series is created at zero.
	const want = `
# HELP ingest_last_seen_timestamp_seconds Venue timestamp of the last data message on each stream. Heartbeats do not advance it.
# TYPE ingest_last_seen_timestamp_seconds gauge
ingest_last_seen_timestamp_seconds{stream="candles"} 0
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "ingest_last_seen_timestamp_seconds"); err != nil {
		t.Error(err)
	}
}

func TestLastSeenIsRecordedInUnixSeconds(t *testing.T) {
	reg := prometheus.NewRegistry()
	s := NewMetrics(reg).Stream("ticker")

	at := time.Date(2026, 8, 20, 12, 0, 0, 500_000_000, time.UTC)
	s.seen(at)

	// Prometheus compares this against its own time(), so it has to be unix
	// seconds — sub-second precision included, since the alert threshold is
	// measured in seconds of age.
	if got := testutil.ToFloat64(s.lastSeen); got != float64(at.UnixNano())/1e9 {
		t.Errorf("last_seen = %f, want %f", got, float64(at.UnixNano())/1e9)
	}
}
