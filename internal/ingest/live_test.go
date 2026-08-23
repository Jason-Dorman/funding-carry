//go:build live

// Package ingest's live test connects to the real Coinbase Advanced Trade
// market-data socket.
//
// It is behind a build tag for the same reason the integration suite is: it
// needs something the unit suite must never depend on — here a working network
// and a venue that is up. Building with the tag is a request to run it, so it
// fails rather than skips when it cannot reach the venue; a `live` run printing
// ok for a test that connected to nothing would be a green tick meaning the
// opposite of what it looks like.
//
//	go test -tags live -run TestLive -timeout 10m -v ./internal/ingest/
//
// What it demonstrates is the Part 4 acceptance minus the database: the real
// streams, against the real venue, producing rows for all four tables, with
// reconnects and gaps counted. The writer is a counting sink rather than
// internal/db.Writer, so it needs no TimescaleDB — the writer's own behaviour is
// covered by the Part 2 integration suite.
package ingest

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/Jason-Dorman/funding-carry/internal/config"
	"github.com/Jason-Dorman/funding-carry/internal/db"
)

// liveRun is long enough for every table to produce a row: venue state every
// five seconds, a book snapshot every ten, a trade bucket on the minute after
// the one skipped at startup, and bars from the candle window the venue replays
// on subscribe.
const liveRun = 150 * time.Second

// countingSink records what the writer would have been handed, by table.
type countingSink struct {
	mu     sync.Mutex
	counts map[string]int
	first  map[string]db.Row
}

func (s *countingSink) Submit(ctx context.Context, r db.Row) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	table := tableOf(r)
	s.counts[table]++
	if _, ok := s.first[table]; !ok {
		s.first[table] = r
	}
	return nil
}

func tableOf(r db.Row) string {
	switch r.(type) {
	case db.VenueStateRow:
		return "cb_venue_state"
	case db.BarRow:
		return "cb_bars"
	case db.BookSnapshotRow:
		return "cb_book_snapshots"
	case db.TradesAggRow:
		return "cb_trades_agg"
	default:
		return "unknown"
	}
}

func TestLiveIngestProducesRowsForEveryTable(t *testing.T) {
	wsURL := os.Getenv("CB_WS_URL")
	if wsURL == "" {
		wsURL = "wss://advanced-trade-ws.coinbase.com"
	}
	perp := envOr("PERP_PRODUCT_ID", "ETP-20DEC30-CDE")
	spot := envOr("SPOT_PRODUCT_ID", "ETH-USD")

	window, err := config.ParseMaintenanceWindow("Fri 17:00-18:00 America/New_York")
	if err != nil {
		t.Fatal(err)
	}

	sink := &countingSink{counts: map[string]int{}, first: map[string]db.Row{}}
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	in := New(Options{
		PerpProduct:      perp,
		SpotProduct:      spot,
		SampleInterval:   5 * time.Second,
		BookSnapInterval: 10 * time.Second,
		Maintenance:      window,
		Dialer:           WSDialer{URL: wsURL},
	}, sink, m, log)

	ctx, cancel := context.WithTimeout(context.Background(), liveRun)
	defer cancel()

	start := time.Now()
	in.Run(ctx)
	t.Logf("ran for %s against %s", time.Since(start).Round(time.Second), wsURL)

	sink.mu.Lock()
	defer sink.mu.Unlock()
	for _, table := range []string{"cb_venue_state", "cb_bars", "cb_book_snapshots", "cb_trades_agg"} {
		if sink.counts[table] == 0 {
			t.Errorf("%s: no rows produced in %s", table, liveRun)
			continue
		}
		t.Logf("%-18s %5d rows  first: %+v", table, sink.counts[table], sink.first[table])
	}
	if sink.counts["unknown"] != 0 {
		t.Errorf("%d rows of a type this test does not recognise", sink.counts["unknown"])
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
