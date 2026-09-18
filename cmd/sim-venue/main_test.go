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

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/quickfixgo/quickfix"
	filelog "github.com/quickfixgo/quickfix/log/file"
	filestore "github.com/quickfixgo/quickfix/store/file"
	"github.com/shopspring/decimal"

	"github.com/Jason-Dorman/funding-carry/internal/config"
	"github.com/Jason-Dorman/funding-carry/internal/db"
)

// These tests cover the wiring in simulate, which is where this binary's
// shutdown ordering lives and therefore the one place it can be got wrong. They
// need no database: the writer's database is a fake sender, and the recorded
// market is a fake book.

const testPerp = "ETP-20DEC30-CDE"

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func testConfig(t *testing.T) *config.SimVenue {
	t.Helper()
	return &config.SimVenue{
		Common: config.Common{
			PerpProductID: testPerp,
			SpotProductID: "ETH-USD",
			// Port zero: the metrics server binds whatever is free, so these
			// tests never collide with a running stack or with each other.
			MetricsAddr: "127.0.0.1:0",
		},
		FIX: config.FIXSession{
			Sender: "CARRY", Target: "SIMV", Host: "127.0.0.1",
			Port: freePort(t), StorePath: t.TempDir(),
		},
		Fill: config.FillModel{
			Latency:          time.Millisecond,
			SlippageBps:      decimal.RequireFromString("2"),
			PartialThreshold: decimal.RequireFromString("10"),
			PartialSlices:    3,
			BookPoll:         10 * time.Millisecond,
			BookMaxAge:       time.Minute,
		},
	}
}

// fakeBooks is a market that never changes, which is all these tests need: what
// is under test is the wiring, not the fill model.
type fakeBooks struct{}

func (fakeBooks) LatestBook(context.Context, string) (db.StoredBook, bool, error) {
	return db.StoredBook{
		TS:      time.Now().UTC(),
		BestBid: decimal.RequireFromString("2344.50"),
		BestAsk: decimal.RequireFromString("2345.00"),
	}, true, nil
}

// failingSender rejects every batch with an error the writer knows a retry
// cannot clear, so it reaches its fatal path immediately rather than after the
// retry schedule.
type failingSender struct{}

func (failingSender) SendBatch(_ context.Context, b *pgx.Batch) pgx.BatchResults {
	return &fakeResults{err: &pgconn.PgError{Code: "42703", Message: "column does not exist"}}
}

type fakeResults struct{ err error }

func (r *fakeResults) Exec() (pgconn.CommandTag, error) {
	if r.err != nil {
		return pgconn.CommandTag{}, r.err
	}
	return pgconn.NewCommandTag("INSERT 0 1"), nil
}

func (r *fakeResults) Query() (pgx.Rows, error) { return nil, r.err }
func (r *fakeResults) QueryRow() pgx.Row        { return nil }
func (r *fakeResults) Close() error             { return nil }

type recordingSender struct {
	mu sync.Mutex
	n  int
}

func (s *recordingSender) SendBatch(_ context.Context, b *pgx.Batch) pgx.BatchResults {
	s.mu.Lock()
	s.n++
	s.mu.Unlock()
	return &fakeResults{}
}

func (s *recordingSender) batches() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n
}

// A fatal write failure must bring the binary down. Architecture section 8
// treats an unreachable database as an invariant violation and its recovery is
// "process crash -> Compose restart policy", which only works if the process
// actually exits.
//
// For a simulator the stake is higher than for a recorder: a venue that keeps
// answering orders while its record of them goes nowhere is reporting fills that
// exist only on the wire. The path is the one ingest had to be fixed for — the
// acceptor learns the writer has died only because the writer's goroutine
// cancels it.
func TestSimulateExitsWhenTheWriterDies(t *testing.T) {
	cfg := testConfig(t)

	done := make(chan error, 1)
	go func() {
		// A root context that is never canceled: the only thing that can end
		// this run is the writer failing.
		done <- simulate(context.Background(), cfg, failingSender{}, fakeBooks{}, testLogger())
	}()

	// A logon is what makes the binary write its first row — the session record —
	// which is what fails.
	client := connectClient(t, cfg.FIX)
	defer client.Stop()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("simulate returned nil after a fatal write failure; the process would exit 0 and Compose would not restart it")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("simulate did not return after the writer died: the venue keeps answering orders " +
			"with nothing recording them, while /healthz still reports 200")
	}
}

// The shutdown order in architecture section 8 is cancel, drain, flush, close,
// and the drain's precondition is that producers have stopped. What is
// observable from here is the outcome: cancellation shuts the binary down
// promptly and reports no error, with the database having been written to
// throughout.
func TestSimulateShutsDownCleanlyOnCancellation(t *testing.T) {
	cfg := testConfig(t)
	sender := &recordingSender{}
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- simulate(ctx, cfg, sender, fakeBooks{}, testLogger()) }()

	client := connectClient(t, cfg.FIX)
	defer client.Stop()

	deadline := time.After(30 * time.Second)
	for sender.batches() == 0 {
		select {
		case <-deadline:
			t.Fatal("no batch reached the database before cancellation")
		case err := <-done:
			t.Fatalf("simulate returned early: %v", err)
		case <-time.After(10 * time.Millisecond):
		}
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("clean shutdown returned an error: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("simulate did not return after cancellation")
	}
}

// A store path that cannot be created fails the startup rather than starting a
// venue whose sequence numbers have nowhere to live.
func TestSimulateFailsOnAnUnusableStorePath(t *testing.T) {
	cfg := testConfig(t)
	cfg.FIX.StorePath = "/proc/sim-venue-cannot-create-this"

	err := simulate(context.Background(), cfg, &recordingSender{}, fakeBooks{}, testLogger())
	if err == nil {
		t.Fatal("simulate succeeded with a store path it could not create")
	}
}

// connectClient is a bare FIX initiator: enough to log on, which is all these
// tests need it to do.
func connectClient(t *testing.T, s config.FIXSession) *quickfix.Initiator {
	t.Helper()

	settings := quickfix.NewSettings()
	store := t.TempDir()
	settings.GlobalSettings().Set("FileStorePath", store)
	settings.GlobalSettings().Set("FileLogPath", store)
	settings.GlobalSettings().Set("ReconnectInterval", "1")

	session := quickfix.NewSessionSettings()
	session.Set("ConnectionType", "initiator")
	session.Set("BeginString", quickfix.BeginStringFIX44)
	session.Set("SenderCompID", s.Sender)
	session.Set("TargetCompID", s.Target)
	session.Set("SocketConnectHost", s.Host)
	session.Set("SocketConnectPort", strconv.Itoa(s.Port))
	session.Set("HeartBtInt", "30")
	if _, err := settings.AddSession(session); err != nil {
		t.Fatalf("client settings: %v", err)
	}

	logFactory, err := filelog.NewLogFactory(settings)
	if err != nil {
		t.Fatalf("client log factory: %v", err)
	}
	initiator, err := quickfix.NewInitiator(silentClient{}, filestore.NewStoreFactory(settings), settings, logFactory)
	if err != nil {
		t.Fatalf("new initiator: %v", err)
	}
	if err := initiator.Start(); err != nil {
		t.Fatalf("start initiator: %v", err)
	}
	return initiator
}

type silentClient struct{}

func (silentClient) OnCreate(quickfix.SessionID)                       {}
func (silentClient) OnLogon(quickfix.SessionID)                        {}
func (silentClient) OnLogout(quickfix.SessionID)                       {}
func (silentClient) ToAdmin(*quickfix.Message, quickfix.SessionID)     {}
func (silentClient) ToApp(*quickfix.Message, quickfix.SessionID) error { return nil }
func (silentClient) FromAdmin(*quickfix.Message, quickfix.SessionID) quickfix.MessageRejectError {
	return nil
}
func (silentClient) FromApp(*quickfix.Message, quickfix.SessionID) quickfix.MessageRejectError {
	return nil
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
