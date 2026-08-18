package main

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/shopspring/decimal"
)

func testFeed(interval time.Duration) *feed {
	return &feed{
		product:  "ETH-USD",
		interval: interval,
		base:     decimal.New(345000, -2), // 3450.00
		step:     decimal.New(10, -2),     // 0.10
		m:        newMetrics(prometheus.NewRegistry()).feed,
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestFeedWalk(t *testing.T) {
	f := testFeed(time.Millisecond)

	cases := []struct {
		name string
		seq  int64
		want string
	}{
		{"start of the sawtooth", 1, "3449.1"},
		{"crossing the base", 10, "3450"},
		{"top of the sawtooth", 20, "3451"},
		{"wraps to the bottom", 21, "3449"},
		{"repeats a period later", 22, "3449.1"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := f.walk(tc.seq)
			// Equal, not ==: the operator compares representation, so a decimal
			// carrying a different exponent for the same value would pass or
			// fail at random.
			if !got.Equal(decimal.RequireFromString(tc.want)) {
				t.Errorf("walk(%d) = %s, want %s", tc.seq, got, tc.want)
			}
		})
	}
}

// A feed that cannot be stopped is a shutdown that never finishes, so this is
// the property that matters most about it.
func TestFeedStopsOnCancel(t *testing.T) {
	f := testFeed(time.Millisecond)
	out := make(chan tick, 8)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.run(ctx, out)
	}()

	// Wait until the feed is actually producing before stopping it; cancelling
	// first would pass even if the ticker branch never observed the context.
	<-out
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("feed did not return within a second of cancellation")
	}
}

// The send is guarded by its own select, so a producer facing a queue nobody is
// draining still stops. Without that guard this test hangs forever.
func TestFeedStopsWhenChannelIsFullAndContextIsCanceled(t *testing.T) {
	f := testFeed(time.Millisecond)
	out := make(chan tick) // unbuffered and never read

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.run(ctx, out)
	}()

	time.Sleep(10 * time.Millisecond) // let it park on the send
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("feed blocked on a full channel did not observe cancellation")
	}
}
