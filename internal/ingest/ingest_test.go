package ingest

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

func testOptions(t *testing.T, d Dialer) Options {
	t.Helper()
	return Options{
		PerpProduct:      testPerp,
		SpotProduct:      testSpot,
		SampleInterval:   sampleEvery,
		BookSnapInterval: 10 * time.Second,
		Maintenance:      maintenanceWindow(t),
		Dialer:           d,
		Backoff:          fastBackoff,
		now:              newClock(epoch).now,
	}
}

func TestSubscriptionPlan(t *testing.T) {
	in := New(testOptions(t, &scriptedDialer{}), &fakeSink{}, NewMetrics(prometheus.NewRegistry()), testLogger())

	if len(in.streams) != 5 {
		t.Fatalf("streams = %d, want 5 (one connection and one goroutine per channel)", len(in.streams))
	}

	got := map[string][]string{}
	for _, s := range in.streams {
		channel, products := s.handler.Subscribe()
		got[s.Name()] = products
		if channel != s.Name() {
			t.Errorf("stream %q subscribes to %q", s.Name(), channel)
		}
	}

	// Which products go on which channel is a design decision, not an
	// arrangement: the spot reference is carried where it carries information
	// (the quote the basis is measured against, the trades its mark is a VWAP
	// of, its candles), and left off level2 and status, because the spot leg
	// executes on Base and the spot market does not close.
	want := map[string][]string{
		channelTicker:       {testPerp, testSpot},
		channelMarketTrades: {testPerp, testSpot},
		channelCandles:      {testPerp, testSpot},
		channelLevel2:       {testPerp},
		channelStatus:       {testPerp},
	}
	for stream, wantProducts := range want {
		gotProducts, ok := got[stream]
		if !ok {
			t.Errorf("no stream named %q", stream)
			continue
		}
		if len(gotProducts) != len(wantProducts) {
			t.Errorf("%s products = %v, want %v", stream, gotProducts, wantProducts)
			continue
		}
		for i := range wantProducts {
			if gotProducts[i] != wantProducts[i] {
				t.Errorf("%s products = %v, want %v", stream, gotProducts, wantProducts)
				break
			}
		}
	}
}

func TestRunStopsEveryGoroutineOnCancellation(t *testing.T) {
	// The dialer's script is empty, so every stream is in its reconnect loop —
	// the state a shutdown is most likely to catch them in, and the one where a
	// stream that only checked the context between connections would hang.
	in := New(testOptions(t, &scriptedDialer{}), &fakeSink{}, NewMetrics(prometheus.NewRegistry()), testLogger())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); in.Run(ctx) }()

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		// Run returning is the writer's precondition for draining safely: a
		// producer still running could hand over a row after the drain had
		// passed, and that row would be lost on the one path that exists to
		// prevent exactly that.
		t.Fatal("Run did not return after cancellation; the writer could not drain safely")
	}
}
