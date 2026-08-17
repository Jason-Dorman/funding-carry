// Command ingest records market data: Coinbase Advanced Trade WebSocket streams,
// the Advanced Trade REST poller and funding estimator, and the Base wallet
// poller — all fanning into one writer goroutine that owns TimescaleDB.
//
// Part 1 wires the skeleton only: configuration, logging, signal handling and the
// metrics endpoint. The feeds arrive in Parts 4 to 6, hung off the same root
// context so shutdown stays a single cancel.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/jasondorman/delta-neutral/internal/config"
	"github.com/jasondorman/delta-neutral/internal/metrics"
)

const service = "ingest"

func main() {
	if err := run(); err != nil {
		// Configuration failures happen before there is a logger, so the
		// last-resort path is stderr.
		fmt.Fprintf(os.Stderr, "%s: %v\n", service, err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.LoadIngest()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	log := config.NewLogger(cfg.Log, os.Stdout).With("service", service)
	slog.SetDefault(log)

	// SIGINT and SIGTERM cancel the root context. Everything added in later parts
	// hangs off it, so `docker compose down` drains and flushes rather than kills.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.Info("starting",
		"perp_product", cfg.PerpProductID,
		"spot_product", cfg.SpotProductID,
		// Durations are logged as strings: slog's JSON handler would otherwise
		// render them as bare nanosecond counts.
		"poll_rest", cfg.Poll.REST.String(),
		"poll_base", cfg.Poll.Base.String(),
		"book_snapshot", cfg.Poll.BookSnap.String(),
		"maintenance_break", cfg.Maintenance.String(),
	)

	if err := metrics.NewServer(cfg.MetricsAddr, log).Serve(ctx); err != nil {
		return err
	}

	log.Info("stopped")
	return nil
}
