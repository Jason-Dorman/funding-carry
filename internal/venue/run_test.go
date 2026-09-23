package venue

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Jason-Dorman/funding-carry/internal/db"
)

// startRun runs the loop under a driven ticker and returns the ticks channel,
// a channel that receives each refreshed State, and a stop that cancels the
// loop and waits for it.
func startRun(t *testing.T, store Store, log *slog.Logger) (chan<- time.Time, <-chan State, func()) {
	t.Helper()
	ticks := make(chan time.Time)
	refreshed := make(chan State, 16)
	clk := &clock{now: t0.Add(time.Second)}
	c := New(store, Options{
		PerpProduct: testPerp, SpotProduct: testSpot, StaleAfter: testLimit, Now: clk.Now,
		ticks:     ticks,
		onRefresh: func(s State) { refreshed <- s },
	}, nil, log)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Run(ctx, time.Second)
	}()
	stop := func() {
		cancel()
		<-done
	}
	t.Cleanup(stop)
	return ticks, refreshed, stop
}

func next(t *testing.T, refreshed <-chan State) State {
	t.Helper()
	select {
	case s := <-refreshed:
		return s
	case <-time.After(5 * time.Second):
		t.Fatal("no refresh landed")
		return State{}
	}
}

// One refresh at start, one per tick.
func TestRunRefreshesImmediatelyAndOnEveryTick(t *testing.T) {
	store := fullStore()
	ticks, refreshed, _ := startRun(t, store, testLogger())

	if s := next(t, refreshed); s.Staleness.Perp.Missing {
		t.Error("the immediate refresh read nothing")
	}
	for i := range 3 {
		ticks <- t0
		if s := next(t, refreshed); !s.Staleness.FeedOK() {
			t.Errorf("tick %d: FeedOK = false with a full store", i)
		}
	}
	if got := store.readCount(); got != 4*5 {
		t.Errorf("store read %d times, want 4 refreshes x 5 sources", got)
	}
}

// A failing store does not end the loop, and the log says so once per
// outage rather than once per tick.
func TestRunSurvivesAFailingStoreAndLogsTheTransition(t *testing.T) {
	var logged bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))
	store := fullStore()
	store.fail(errors.New("connection refused"))
	ticks, refreshed, _ := startRun(t, store, log)

	next(t, refreshed)
	for range 2 {
		ticks <- t0
		next(t, refreshed)
	}
	if got := strings.Count(logged.String(), "refresh failing"); got != 1 {
		t.Errorf("failure logged %d times over three failed refreshes, want once:\n%s", got, logged.String())
	}

	store.fail(nil)
	ticks <- t0
	if s := next(t, refreshed); !s.Staleness.FeedOK() {
		t.Error("the loop did not recover once the store answered again")
	}
	if got := strings.Count(logged.String(), "recovered"); got != 1 {
		t.Errorf("recovery logged %d times, want once:\n%s", got, logged.String())
	}
	// Still healthy: nothing more to say.
	ticks <- t0
	next(t, refreshed)
	if got := strings.Count(logged.String(), "recovered"); got != 1 {
		t.Errorf("recovery logged again on a healthy tick:\n%s", logged.String())
	}
}

// hangingStore answers only when its context ends, like a database that
// accepted the connection and never replies.
type hangingStore struct{ *fakeStore }

func (h *hangingStore) LatestVenueState(ctx context.Context, product string) (db.VenueStateRow, bool, error) {
	<-ctx.Done()
	return h.fakeStore.LatestVenueState(ctx, product)
}

// Each refresh is bounded to the interval, so a database that never answers
// produces a failed refresh every tick rather than a loop stuck on its first.
//
// Both halves of that sentence are asserted against the interval, not against
// a generous ceiling. An earlier version waited five seconds for one refresh,
// which a bound of a hundred intervals still satisfied — and a hundred
// intervals is eight minutes on the shipped cadence, with every staleness flag
// frozen at whatever it last read. The tick is what makes it "each": the loop
// must bound the second refresh as well as the first.
func TestRunBoundsEachRefreshToTheInterval(t *testing.T) {
	const every = 20 * time.Millisecond
	// Ten intervals: far enough above the bound to be insensitive to a loaded
	// machine, far enough below a wrong bound to catch one.
	const allowed = 10 * every

	store := &hangingStore{fakeStore: fullStore()}
	ticks := make(chan time.Time)
	refreshed := make(chan State, 4)
	c := New(store, Options{
		PerpProduct: testPerp, SpotProduct: testSpot, StaleAfter: testLimit,
		ticks:     ticks,
		onRefresh: func(s State) { refreshed <- s },
	}, nil, testLogger())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Run(ctx, every)
	}()
	defer func() {
		cancel()
		<-done
	}()

	awaitBounded := func(which string) {
		t.Helper()
		started := time.Now()
		select {
		case s := <-refreshed:
			if took := time.Since(started); took > allowed {
				t.Errorf("the %s refresh took %s against a %s interval; the per-refresh bound is "+
					"not the interval, so a hung database freezes the flags for %s at a time",
					which, took, every, took)
			}
			if !s.Staleness.Perp.Missing {
				t.Errorf("the %s refresh timed out but reported a row", which)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("the %s refresh never ended: nothing is bounding it", which)
		}
	}

	awaitBounded("first")
	ticks <- t0
	awaitBounded("second")
}
