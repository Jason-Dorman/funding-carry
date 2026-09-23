// Package metrics owns each binary's Prometheus registry and the HTTP endpoint
// Prometheus scrapes.
//
// Every binary constructs exactly one Server. Components register their
// collectors against Server.Registry rather than the client_golang default
// registry, so what a binary exports is a visible, testable property of that
// binary instead of a global side effect of whichever packages were imported.
package metrics

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// shutdownGrace bounds the wait for in-flight scrapes once the root context is
// canceled. A scrape takes milliseconds; this only exists so shutdown cannot
// hang on a stuck connection.
const shutdownGrace = 5 * time.Second

// Server exposes one binary's metrics over HTTP.
type Server struct {
	addr     string
	registry *prometheus.Registry
	log      *slog.Logger

	// listener is bound by Listen and served by Serve. Binding is a separate,
	// synchronous step so a binary fails at startup on an address it cannot
	// have, rather than running on with no endpoint — see Listen.
	listener net.Listener
}

// NewServer builds the registry and the HTTP handlers. It does not bind a port;
// Listen does that, so a construction failure and a bind failure stay distinct
// — and so the bind is a synchronous startup step the caller can fail on.
func NewServer(addr string, log *slog.Logger) *Server {
	registry := prometheus.NewRegistry()
	// Go runtime and process collectors are the baseline every binary exports:
	// goroutine count is how a leak shows up in Grafana before it shows up as a
	// stall.
	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return &Server{addr: addr, registry: registry, log: log}
}

// Registry is where components register their collectors.
func (s *Server) Registry() prometheus.Registerer { return s.registry }

// Listen binds the configured address, and is the reason a metrics failure is
// a startup failure rather than a silent one.
//
// It is separate from Serve because Serve runs in a goroutine whose error
// nothing reads until shutdown: a bind that failed there left the binary
// running with a live FIX session and a live decision loop, no /metrics and no
// /healthz, until someone sent it SIGTERM — and Compose restart policies fire
// on exit, not on unhealthy, so the container stayed up in that state
// indefinitely. That is the failure shape architecture section 8 already
// rules on for the writer: a subsystem that dies while the process lives
// defeats the restart policy the reliability table depends on.
//
// Every binary calls this before it starts anything else, so the error is
// returned while there is nothing to unwind. Binding first is also what makes
// the fix safe: after the bind, Serve returns only on failure or cancellation,
// so the case this catches is always a bind in the first milliseconds — before
// any session is up and before any position can exist.
//
// Found by the Part 9 adversarial review in Part 1/8 wiring shared by all three
// binaries; the tiebreak rejected reacting to the error during shutdown, which
// would have edited ordering two earlier reviews had already corrected.
func (s *Server) Listen(ctx context.Context) error {
	var lc net.ListenConfig
	listener, err := lc.Listen(ctx, "tcp", s.addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.addr, err)
	}
	s.listener = listener
	return nil
}

// Serve serves the endpoint until ctx is canceled, then shuts down gracefully.
// It returns nil on a clean shutdown. Listen must have succeeded first.
func (s *Server) Serve(ctx context.Context) error {
	if s.listener == nil {
		// A programming error, not a runtime condition: the address is bound by
		// Listen precisely so that a failure to bind is caught at startup.
		return errors.New("metrics server: Serve called before Listen")
	}
	return s.serve(ctx, s.listener)
}

// serve is separated from Serve so tests can supply their own listener on port
// zero and know the address that was actually bound.
func (s *Server) serve(ctx context.Context, listener net.Listener) error {
	srv := &http.Server{
		Handler:           s.handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	errc := make(chan error, 1)
	go func() {
		errc <- srv.Serve(listener)
	}()

	s.log.Info("metrics server listening", "addr", listener.Addr().String())

	select {
	case err := <-errc:
		// Serve only returns on failure here: nothing else closes the listener.
		return fmt.Errorf("metrics server: %w", err)
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("metrics server shutdown: %w", err)
	}

	// Shutdown makes the in-flight Serve return ErrServerClosed; draining it
	// keeps the goroutine from outliving this call.
	if err := <-errc; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("metrics server: %w", err)
	}
	s.log.Info("metrics server stopped")
	return nil
}

func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(s.registry, promhttp.HandlerOpts{
		ErrorHandling: promhttp.ContinueOnError,
		Registry:      s.registry,
	}))
	// Compose health checks hit /healthz. It reports that the process is serving,
	// nothing more — feed health is a metric and an alert, not a liveness probe,
	// because a stale feed must not restart a binary that is holding a position.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte("ok\n")); err != nil {
			s.log.Debug("healthz write failed", "error", err)
		}
	})
	return mux
}
