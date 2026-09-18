package fix

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/Jason-Dorman/funding-carry/internal/config"
)

// simVenue is a running acceptor a test can stop and restart on the same port
// and store — the venue side of every socket-level test in this package,
// whether the client is the scripted one (Part 7) or the initiator (Part 8).
type simVenue struct {
	t       *testing.T
	session config.FIXSession
	fill    config.FillModel
	books   *fakeBooks
	sink    *fakeSink

	mu      sync.Mutex
	cancel  context.CancelFunc
	done    chan error
	stopped bool
}

// startSimVenue starts an acceptor on a free port and returns once it is
// accepting connections.
//
// A port is reserved and released before it can be bound, so losing the race
// is possible and retrying with a fresh one is the honest fix — the
// alternative is a suite that fails a few times a week for a reason that has
// nothing to do with what it tests.
func startSimVenue(t *testing.T, fill config.FillModel) *simVenue {
	t.Helper()

	s := &simVenue{
		t: t,
		session: config.FIXSession{
			Sender: "CARRY", Target: "SIMV", Host: "127.0.0.1", StorePath: t.TempDir(),
		},
		fill:  fill,
		books: newBooks(time.Now().UTC(), "2344.50", "2345.00"),
		sink:  &fakeSink{},
	}

	var err error
	for attempt := range 3 {
		s.session.Port = freePort(t)
		if err = s.start(); err == nil {
			break
		}
		t.Logf("attempt %d: %v; retrying on another port", attempt+1, err)
	}
	if err != nil {
		t.Fatalf("could not start the simulator: %v", err)
	}
	t.Cleanup(s.stop)
	return s
}

// start starts an acceptor and returns only once it is actually accepting
// connections, or reports why it could not.
//
// Both halves matter, and the flake that prompted them was real: a bind
// failure surfaced on a goroutine, and the test that saw it reported "timed
// out waiting for the client to log on" — a symptom five steps from its cause.
// Waiting for the listener also removes the second of reconnect delay the
// client used to spend when it dialled first.
func (s *simVenue) start() error {
	a, err := NewAcceptor(Options{Product: testPerp, Session: s.session, Fill: s.fill},
		s.books, s.sink, NewMetrics(prometheus.NewRegistry()), testLogger())
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()

	addr := net.JoinHostPort(s.session.Host, strconv.Itoa(s.session.Port))
	deadline := time.Now().Add(5 * time.Second)
	for {
		select {
		case err := <-done:
			cancel()
			return fmt.Errorf("acceptor stopped before it listened: %w", err)
		default:
		}
		if conn, derr := net.DialTimeout("tcp", addr, 50*time.Millisecond); derr == nil {
			_ = conn.Close()
			s.mu.Lock()
			s.cancel, s.done, s.stopped = cancel, done, false
			s.mu.Unlock()
			return nil
		}
		if time.Now().After(deadline) {
			cancel()
			<-done
			return fmt.Errorf("acceptor never listened on %s", addr)
		}
	}
}

// stop shuts the venue down and reports its exit error. Safe to call twice.
func (s *simVenue) stop() {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	cancel, done := s.cancel, s.done
	s.mu.Unlock()

	cancel()
	if err := <-done; err != nil {
		s.t.Errorf("acceptor: %v", err)
	}
}

// restart brings the venue back on the same port and store, which is what
// the sequence-continuity tests need.
func (s *simVenue) restart() {
	s.t.Helper()
	s.stop()
	if err := s.start(); err != nil {
		s.t.Fatalf("restart: %v", err)
	}
}

// freePort asks the kernel for a port and gives it straight back, which is how a
// test binds something that cannot collide with a running stack.
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
