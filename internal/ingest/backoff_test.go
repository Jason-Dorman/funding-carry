package ingest

import (
	"testing"
	"time"
)

func TestBackoffSchedule(t *testing.T) {
	// Identity jitter so the table is the exponential term alone; the jitter is
	// tested on its own below.
	b := Backoff{Base: time.Second, Max: 8 * time.Second, Jitter: func(d time.Duration) time.Duration { return d }}

	for _, tc := range []struct {
		attempt int
		want    time.Duration
	}{
		{attempt: 1, want: time.Second},
		{attempt: 2, want: 2 * time.Second},
		{attempt: 3, want: 4 * time.Second},
		{attempt: 4, want: 8 * time.Second},
		{attempt: 5, want: 8 * time.Second}, // held at the ceiling
		{attempt: 40, want: 8 * time.Second},
		// A stream failing for days must not shift its way to a negative
		// duration and start reconnecting instantly.
		{attempt: 10_000, want: 8 * time.Second},
	} {
		if got := b.delay(tc.attempt); got != tc.want {
			t.Errorf("delay(%d) = %s, want %s", tc.attempt, got, tc.want)
		}
	}
}

func TestEqualJitterKeepsAFloor(t *testing.T) {
	const window = time.Second
	for range 1000 {
		got := equalJitter(window)
		// The floor is the property that matters. Jitter uniform over the whole
		// window — the more commonly quoted "full jitter" — draws delays
		// arbitrarily close to zero, which would retry a venue that is refusing
		// connections in a tight loop.
		if got < window/2 || got >= window {
			t.Fatalf("equalJitter(%s) = %s, want [%s, %s)", window, got, window/2, window)
		}
	}
}

func TestBackoffDefaultsAreApplied(t *testing.T) {
	b := Backoff{}.withDefaults()
	if b.Base != defaultBackoffBase || b.Max != defaultBackoffMax || b.Jitter == nil {
		t.Fatalf("zero Backoff did not pick up defaults: %+v", b)
	}
}
