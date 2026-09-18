package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"strconv"
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
	go func() { done <- trade(context.Background(), cfg, &probe, testLogger()) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("trade: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the probe never finished")
	}
}

// An order the venue will not fill is cancelled at the order timeout and the
// probe still exits cleanly — the cancel is the terminal state.
func TestProbeCancelsAnOverdueOrder(t *testing.T) {
	sim := startSim(t)
	cfg := testConfig(t, sim.Port)
	cfg.Execution.OrderTimeout = 500 * time.Millisecond

	probe, err := parseProbe("side=buy,qty=1,px=1000.00") // rests: far below the ask
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- trade(context.Background(), cfg, &probe, testLogger()) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("trade: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the probe never finished")
	}
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
	go func() { done <- trade(ctx, cfg, &probe, testLogger()) }()

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

	done := make(chan error, 1)
	go func() { done <- trade(ctx, cfg, nil, testLogger()) }()

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
}

// A store path that cannot be created fails the startup rather than starting
// a session whose sequence numbers have nowhere to live.
func TestTradeFailsOnAnUnusableStorePath(t *testing.T) {
	cfg := testConfig(t, freePort(t))
	cfg.FIX.StorePath = "/proc/carry-cannot-create-this"

	if err := trade(context.Background(), cfg, nil, testLogger()); err == nil {
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
