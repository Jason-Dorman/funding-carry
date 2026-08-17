package metrics

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/goleak"
)

// The metrics server starts a goroutine per binary and lives for the whole
// process, so a leak here is a leak everywhere.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// serveOnRandomPort starts the server on a loopback port chosen by the kernel and
// returns its base URL plus a stop function that shuts it down and reports the
// server's exit error.
func serveOnRandomPort(t *testing.T, s *Server) (string, func() error) {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.serve(ctx, listener) }()

	return "http://" + listener.Addr().String(), func() error {
		cancel()
		select {
		case err := <-done:
			return err
		case <-time.After(5 * time.Second):
			t.Fatal("server did not shut down within 5s of context cancellation")
			return nil
		}
	}
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, string(body)
}

func TestServerExposesMetricsAndHealth(t *testing.T) {
	server := NewServer("127.0.0.1:0", discardLogger())

	// A collector registered by a component shows up on the endpoint: this is the
	// contract every later part relies on.
	counter := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "ingest_rows_written_total",
		Help: "rows written by the writer goroutine",
	})
	server.Registry().MustRegister(counter)
	counter.Inc()

	base, stop := serveOnRandomPort(t, server)

	status, body := get(t, base+"/metrics")
	if status != http.StatusOK {
		t.Fatalf("GET /metrics status = %d, want %d", status, http.StatusOK)
	}
	for _, want := range []string{"ingest_rows_written_total 1", "go_goroutines", "process_start_time_seconds"} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics does not contain %q", want)
		}
	}

	status, body = get(t, base+"/healthz")
	if status != http.StatusOK {
		t.Errorf("GET /healthz status = %d, want %d", status, http.StatusOK)
	}
	if got, want := strings.TrimSpace(body), "ok"; got != want {
		t.Errorf("GET /healthz body = %q, want %q", got, want)
	}

	if err := stop(); err != nil {
		t.Errorf("shutdown returned %v, want nil", err)
	}
}

func TestServerUsesItsOwnRegistry(t *testing.T) {
	// Registering against the default registry must not leak into a binary's
	// endpoint: what a binary exports is a property of that binary, not of the
	// import graph.
	strayName := "stray_default_registry_metric"
	stray := prometheus.NewCounter(prometheus.CounterOpts{Name: strayName, Help: "not ours"})
	prometheus.MustRegister(stray)
	t.Cleanup(func() { prometheus.Unregister(stray) })

	base, stop := serveOnRandomPort(t, NewServer("127.0.0.1:0", discardLogger()))
	_, body := get(t, base+"/metrics")
	if strings.Contains(body, strayName) {
		t.Errorf("/metrics exposed %q from the default registry", strayName)
	}

	if err := stop(); err != nil {
		t.Errorf("shutdown returned %v, want nil", err)
	}
}

func TestServeReportsBindFailure(t *testing.T) {
	// Take a port, then ask the server for the same one.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = listener.Close() }()

	err = NewServer(listener.Addr().String(), discardLogger()).Serve(context.Background())
	if err == nil {
		t.Fatal("Serve on a taken port: want error, got nil")
	}
	if !strings.Contains(err.Error(), "listen") {
		t.Errorf("error = %v, want it to name the failed listen", err)
	}
}

func TestServeStopsWhenContextIsAlreadyCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := NewServer("127.0.0.1:0", discardLogger()).Serve(ctx); err != nil {
		t.Errorf("Serve with a canceled context returned %v, want nil", err)
	}
}
