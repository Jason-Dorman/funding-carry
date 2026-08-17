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
}

// NewServer builds the registry and the HTTP handlers. It does not bind a port;
// Serve does that, so a construction failure and a bind failure stay distinct.
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

// Serve binds the configured address and serves until ctx is canceled, then
// shuts down gracefully. It returns nil on a clean shutdown.
func (s *Server) Serve(ctx context.Context) error {
	var lc net.ListenConfig
	listener, err := lc.Listen(ctx, "tcp", s.addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.addr, err)
	}
	return s.serve(ctx, listener)
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
