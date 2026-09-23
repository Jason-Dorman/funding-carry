package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/shopspring/decimal"
	"go.uber.org/goleak"

	"github.com/Jason-Dorman/funding-carry/internal/carry"
	"github.com/Jason-Dorman/funding-carry/internal/config"
	"github.com/Jason-Dorman/funding-carry/internal/db"
	"github.com/Jason-Dorman/funding-carry/internal/exec"
	"github.com/Jason-Dorman/funding-carry/internal/fix"
)

// These tests cover the wiring in trade and the temporary probe, against an
// in-process sim-venue acceptor with a fake market and a fake writer. No
// database is needed.

func TestMain(m *testing.M) {
	// The three quickfixgo goroutines the fix package documents and ignores;
	// see internal/fix/main_test.go for the mechanism of each.
	goleak.VerifyTestMain(m,
		goleak.IgnoreTopFunction("github.com/quickfixgo/quickfix.(*session).initiateLogoutInReplyTo.func1"),
		goleak.IgnoreTopFunction("github.com/quickfixgo/quickfix.(*stateMachine).Connect.func1"),
		goleak.IgnoreTopFunction("github.com/quickfixgo/quickfix.(*session).waitForInSessionTime"),
	)
}

const testPerp = "ETP-20DEC30-CDE"

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func testConfig(t *testing.T, port int) *config.Carry {
	t.Helper()
	return &config.Carry{
		Common: config.Common{
			PerpProductID: testPerp,
			SpotProductID: "ETH-USD",
			MetricsAddr:   "127.0.0.1:0",
		},
		Execution: config.Execution{OrderTimeout: 2 * time.Second},
		// Short so the cache loop ticks a few times inside a test; its reads
		// hit emptyStore and cost nothing.
		VenueRefresh: 50 * time.Millisecond,
		Risk:         config.RiskLimits{StaleFeed: time.Minute},
		FIX: config.FIXSession{
			Sender: "CARRY", Target: "SIMV", Host: "127.0.0.1",
			Port: port, StorePath: t.TempDir(),
		},
	}
}

// fakeBooks is a market that never changes.
type fakeBooks struct{}

func (fakeBooks) LatestBook(context.Context, string) (db.StoredBook, bool, error) {
	return db.StoredBook{
		TS:      time.Now().UTC(),
		BestBid: decimal.RequireFromString("2344.50"),
		BestAsk: decimal.RequireFromString("2345.00"),
	}, true, nil
}

// recordingStore is a database nothing has been recorded in — every read
// answers "no row", which the cache reports as missing and stale — that also
// records what was asked of it.
//
// It records because two properties of this binary's wiring are visible
// nowhere else. The cache's own behaviour is tested in internal/venue against
// an injected ticker, and internal/venue's integration test builds its own
// Options; only here does anything exercise the real ticker branch of
// venue.Cache.Run and the product ids cmd/carry passes it. With a store that
// only answered, both a cache that refreshed once and froze and a perp/spot
// swap in the wiring left every suite green (Part 9 adversarial review).
type recordingStore struct {
	mu       sync.Mutex
	reads    int
	products []string
}

func (r *recordingStore) LatestVenueState(_ context.Context, product string) (db.VenueStateRow, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reads++
	r.products = append(r.products, product)
	return db.VenueStateRow{}, false, nil
}

func (r *recordingStore) LatestAccountState(context.Context) (db.AccountStateRow, bool, error) {
	return db.AccountStateRow{}, false, nil
}

func (r *recordingStore) LatestBaseState(context.Context) (db.BaseStateRow, bool, error) {
	return db.BaseStateRow{}, false, nil
}

func (r *recordingStore) Product(context.Context, string) (db.ProductRow, bool, error) {
	return db.ProductRow{}, false, nil
}

// refreshes is how many times the cache has been round the perp and spot
// reads: two venue-state reads per refresh.
func (r *recordingStore) refreshes() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reads / 2
}

// firstPair is the two products the cache read venue state for on its first
// refresh, in the order it asked.
//
// Order, not membership: a perp/spot swap in the wiring asks for the same two
// products, so a set comparison cannot see it. Refresh reads the perp source
// first and the spot source second, so the pair says which id was wired into
// which role.
func (r *recordingStore) firstPair() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.products) < 2 {
		return r.products
	}
	return r.products[:2]
}

// fakeSink accepts every row and keeps none: the venue's records are Part 7's
// concern, not this binary's.
type fakeSink struct{}

func (fakeSink) Submit(context.Context, db.Row) error { return nil }

// startSim runs an acceptor on a free port until the test ends.
func startSim(t *testing.T) config.FIXSession {
	t.Helper()

	session := config.FIXSession{
		Sender: "CARRY", Target: "SIMV", Host: "127.0.0.1", Port: freePort(t), StorePath: t.TempDir(),
	}
	a, err := fix.NewAcceptor(fix.Options{
		Product: testPerp,
		Session: session,
		Fill: config.FillModel{
			Latency:          time.Millisecond,
			SlippageBps:      decimal.RequireFromString("2"),
			PartialThreshold: decimal.RequireFromString("10"),
			PartialSlices:    3,
			BookPoll:         10 * time.Millisecond,
			BookMaxAge:       time.Minute,
		},
	}, fakeBooks{}, fakeSink{}, fix.NewMetrics(prometheus.NewRegistry()), testLogger())
	if err != nil {
		t.Fatalf("new acceptor: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("acceptor: %v", err)
		}
	})

	addr := net.JoinHostPort(session.Host, strconv.Itoa(session.Port))
	deadline := time.Now().Add(5 * time.Second)
	for {
		if conn, err := net.DialTimeout("tcp", addr, 50*time.Millisecond); err == nil {
			_ = conn.Close()
			return session
		}
		if time.Now().After(deadline) {
			t.Fatalf("acceptor never listened on %s", addr)
		}
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		t.Fatalf("release the port: %v", err)
	}
	return port
}

// The first acceptance item, through the binary's own wiring: a probe order
// goes to the simulator, its reports come back through the drain and the
// tracker, and the process exits cleanly once the order is terminal.
func TestProbeRunsAnOrderToTerminal(t *testing.T) {
	sim := startSim(t)
	cfg := testConfig(t, sim.Port)
	cfg.FIX.StorePath = t.TempDir()

	probe, err := parseProbe("side=buy,qty=3,px=2350.00,tif=GTC")
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- trade(context.Background(), cfg, &recordingStore{}, &probe, testLogger()) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("trade: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the probe never finished")
	}
}

// An order the venue will not fill is cancelled when the probe's wait runs
// out and the probe still exits cleanly — the cancel is the terminal state.
// The order here is ACKNOWLEDGED and resting, so the tracker does not call it
// overdue; bounding the fill is the probe's own business (probe.go).
func TestProbeCancelsAnOrderThatNeverFills(t *testing.T) {
	sim := startSim(t)
	cfg := testConfig(t, sim.Port)
	cfg.Execution.OrderTimeout = 500 * time.Millisecond

	probe, err := parseProbe("side=buy,qty=1,px=1000.00") // rests: far below the ask
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- trade(context.Background(), cfg, &recordingStore{}, &probe, testLogger()) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("trade: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the probe never finished")
	}
}

// The cancel is waited for under its own bound, not ORDER_TIMEOUT reused.
// With the fill bound at one millisecond the probe gives up on the fill at
// once and cancels; the venue's answer takes longer than a millisecond to
// arrive, so a cancel wait that reused the fill bound would report the order
// "still NEW" and fail — which is what the Part 8 version did under load with
// 500 ms, and what this test does deterministically. Passing here means the
// pass path took exactly as long as cancellation did and no longer.
func TestProbeCancelWaitDoesNotReuseTheFillBound(t *testing.T) {
	sim := startSim(t)
	cfg := testConfig(t, sim.Port)
	cfg.Execution.OrderTimeout = time.Millisecond

	probe, err := parseProbe("side=buy,qty=1,px=1000.00") // rests: far below the ask
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- trade(context.Background(), cfg, &recordingStore{}, &probe, testLogger()) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("trade: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the probe never finished")
	}
}

// The gauge watcher is the only production path that ever raises
// carry_orders_overdue: an order the venue never acknowledges produces no
// report at all, and Apply refreshes only the open gauge. Nothing guarded it —
// deleting the goroutine, stretching observeEvery to an hour, or emptying
// watch all left the suite green. It also has to leave a COHERENT last reading
// behind when it stops, because trade keeps the metrics endpoint alive past
// the shutdown for the final scrape: with the watcher dead while the drain was
// still applying reports, that scrape could read carry_orders_open=0 beside
// carry_orders_overdue=1, a pair the tracker cannot produce. Found by the
// Part 8 follow-up adversarial review.
func TestTheGaugeWatcherRefreshesAndStopsCoherently(t *testing.T) {
	reg := prometheus.NewRegistry()
	tracker := exec.NewTracker(exec.Options{Venue: db.VenueSim, Timeout: time.Nanosecond},
		exec.NewMetrics(reg), testLogger())
	if err := tracker.Track(carry.Order{ClOrdID: "P1", Qty: decimal.RequireFromString("1")},
		carry.Ack{At: time.Now()}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { defer close(stopped); watch(ctx, tracker) }()

	// Nothing but the ticker can raise this: no report has arrived, and an
	// order the venue never answers produces none.
	deadline := time.After(10 * time.Second)
	for gaugeValue(t, reg, "carry_orders_overdue") != 1 {
		select {
		case <-deadline:
			t.Fatal("carry_orders_overdue never rose: the watcher is not refreshing the gauges")
		case <-time.After(10 * time.Millisecond):
		}
	}

	// The order ends. Apply drops the open gauge and leaves the overdue gauge
	// to the watcher, so the watcher's last act is what the final scrape sees.
	tracker.Apply(carry.ExecReport{ClOrdID: "P1", ExecID: "E1", State: carry.StateCanceled, At: time.Now()})
	cancel()
	<-stopped

	open, overdue := gaugeValue(t, reg, "carry_orders_open"), gaugeValue(t, reg, "carry_orders_overdue")
	if open != 0 || overdue != 0 {
		t.Errorf("after the watcher stopped: carry_orders_open=%v, carry_orders_overdue=%v, want 0 and 0 — "+
			"the last scrape of the process must never read more orders overdue than open", open, overdue)
	}
}

// gaugeValue reads a gauge out of a registry by its exported name.
func gaugeValue(t *testing.T, reg *prometheus.Registry, name string) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		if n := len(f.GetMetric()); n != 1 {
			t.Fatalf("%s: %d series, want 1", name, n)
		}
		return f.GetMetric()[0].GetGauge().GetValue()
	}
	t.Fatalf("%s: no such series in the registry", name)
	return 0
}

// Told to stop with the probe order still resting, the probe leaves the order
// at the venue and the binary exits cleanly. The first live run of the probe
// went on to cancel the order on the dead context and reported an error; the
// order it left working is exactly what the restart case recovers.
func TestProbeLeavesARestingOrderWhenStopped(t *testing.T) {
	sim := startSim(t)
	cfg := testConfig(t, sim.Port)
	cfg.Execution.OrderTimeout = time.Minute

	probe, err := parseProbe("side=buy,qty=1,px=1000.00") // rests
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- trade(ctx, cfg, &recordingStore{}, &probe, testLogger()) }()

	// The NEW is the probe's first update; once it has arrived the order is
	// resting and the stop is a stop mid-order.
	select {
	case <-probe.updates:
	case <-time.After(30 * time.Second):
		t.Fatal("the probe order was never acknowledged")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("stopping with an order resting returned an error: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("trade did not return after cancellation")
	}
}

// With no probe the binary runs the session until it is told to stop, and
// stops cleanly: session logged out, drain ended, endpoint down.
func TestTradeShutsDownCleanlyOnCancellation(t *testing.T) {
	sim := startSim(t)
	cfg := testConfig(t, sim.Port)
	ctx, cancel := context.WithCancel(context.Background())

	store := &recordingStore{}
	done := make(chan error, 1)
	go func() { done <- trade(ctx, cfg, store, nil, testLogger()) }()

	// Give the session time to come up, then stop. The point is the exit, not
	// the logon; a cancellation during the connect must be clean too, which is
	// why this does not wait on anything first.
	time.Sleep(300 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("clean shutdown returned an error: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("trade did not return after cancellation")
	}

	// 300ms at a 50ms refresh. Two refreshes is the assertion that matters:
	// the first is Run's immediate one, so only the real ticker can produce a
	// second, and every test in internal/venue injects its ticker instead.
	if got := store.refreshes(); got < 2 {
		t.Errorf("cache refreshed %d times in 300ms at a 50ms interval; want at least 2 — "+
			"one refresh means the ticker never fired and the flags would freeze at startup", got)
	}
	// The products the binary wires into the cache are visible nowhere else:
	// internal/venue's tests build Options themselves. Swapped, every consumer
	// would read the spot row as the perp — spot mid where futures_mark
	// belongs, and no funding rate at all — and both CI jobs would stay green.
	want := []string{cfg.PerpProductID, cfg.SpotProductID}
	if got := store.firstPair(); !slices.Equal(got, want) {
		t.Errorf("cache read venue state for %v, want %v (perp first, then spot)", got, want)
	}
}

// A metrics endpoint that cannot bind fails the startup, rather than leaving
// the binary running with a live FIX session, a live cache and no way to
// observe either (metrics.Listen).
//
// The counted reads are the point: they prove the bind is checked before the
// cache and the session start, so there is nothing to unwind. Compose restart
// policies fire on exit, not on unhealthy, so a binary that kept running here
// stayed up with no /metrics and no /healthz until someone stopped it.
func TestTradeFailsWhenTheMetricsEndpointCannotBind(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupy a port: %v", err)
	}
	defer func() { _ = occupied.Close() }()

	cfg := testConfig(t, freePort(t))
	cfg.MetricsAddr = occupied.Addr().String()

	store := &recordingStore{}
	done := make(chan error, 1)
	go func() { done <- trade(context.Background(), cfg, store, nil, testLogger()) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("trade succeeded with a metrics address it could not bind")
		}
		if !strings.Contains(err.Error(), "listen on") {
			t.Errorf("error is %v, want the bind failure named", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("trade kept running with no metrics endpoint")
	}
	if got := store.refreshes(); got != 0 {
		t.Errorf("the cache refreshed %d times before the bind was checked; want 0", got)
	}
}

// A store path that cannot be created fails the startup rather than starting
// a session whose sequence numbers have nowhere to live.
func TestTradeFailsOnAnUnusableStorePath(t *testing.T) {
	cfg := testConfig(t, freePort(t))
	cfg.FIX.StorePath = "/proc/carry-cannot-create-this"

	if err := trade(context.Background(), cfg, &recordingStore{}, nil, testLogger()); err == nil {
		t.Fatal("trade succeeded with a store path it could not create")
	}
}

// The probe's little language, and what it refuses.
func TestParseProbe(t *testing.T) {
	t.Parallel()

	good, err := parseProbe("side=sell, qty=2, px=2400.50, tif=IOC")
	if err != nil {
		t.Fatal(err)
	}
	if good.order.Side != carry.Sell || !good.order.Qty.Equal(decimal.RequireFromString("2")) ||
		!good.order.LimitPx.Equal(decimal.RequireFromString("2400.50")) || good.order.TIF != carry.IOC {
		t.Errorf("parsed %+v", good.order)
	}
	if good.order.Leg != carry.LegPerp || good.order.Type != carry.Limit || good.order.ClOrdID == "" {
		t.Errorf("a probe order is a perp limit with a ClOrdID, got %+v", good.order)
	}
	if def, _ := parseProbe("side=buy,qty=1,px=1"); def.order.TIF != carry.GTC {
		t.Errorf("tif defaults to GTC, got %q", def.order.TIF)
	}

	for _, bad := range []string{
		"", "side=buy", "qty=1,px=1", "side=short,qty=1,px=1", "side=buy,qty=x,px=1",
		"side=buy,qty=1,px=1,color=red", "side buy",
	} {
		if _, err := parseProbe(bad); err == nil {
			t.Errorf("%q parsed, want an error", bad)
		}
	}
}

// probeOrder.observe never blocks the drain, even with nobody reading.
func TestProbeObserveNeverBlocks(t *testing.T) {
	t.Parallel()

	p, err := parseProbe("side=buy,qty=1,px=1")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range cap(p.updates) + 10 {
			p.observe(exec.Status{ClOrdID: p.order.ClOrdID}, exec.OutcomeApplied)
		}
	}()
	waitDone := make(chan struct{})
	go func() { wg.Wait(); close(waitDone) }()
	select {
	case <-waitDone:
	case <-time.After(5 * time.Second):
		t.Fatal("observe blocked once the buffer filled")
	}
}
