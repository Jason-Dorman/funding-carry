// Command sim-venue is the exchange simulator: a FIX 4.4 acceptor with a fill
// model driven by recorded top-of-book, so the order-entry stack can be exercised
// end to end — including latency, slippage, partial fills and sequence recovery —
// without touching a real venue.
//
// Part 1 wires the skeleton only: configuration, logging, signal handling and the
// metrics endpoint. The acceptor and fill model are Part 7.
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

const service = "sim-venue"

func main() {
	if err := run(); err != nil {
		// Configuration failures happen before there is a logger, so the
		// last-resort path is stderr.
		fmt.Fprintf(os.Stderr, "%s: %v\n", service, err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.LoadSimVenue()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	log := config.NewLogger(cfg.Log, os.Stdout).With("service", service)
	slog.SetDefault(log)

	// SIGINT and SIGTERM cancel the root context; in Part 7 that becomes a FIX
	// logout with the sequence store flushed to disk.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.Info("starting",
		"perp_product", cfg.PerpProductID,
		"fix_session", fmt.Sprintf("%s->%s", cfg.FIX.Target, cfg.FIX.Sender),
		"fix_port", cfg.FIX.Port,
		// Logged as a string: slog's JSON handler renders a Duration as a bare
		// nanosecond count.
		"fill_latency", cfg.Fill.Latency.String(),
		"fill_slippage_bps", cfg.Fill.SlippageBps.String(),
	)

	if err := metrics.NewServer(cfg.MetricsAddr, log).Serve(ctx); err != nil {
		return err
	}

	log.Info("stopped")
	return nil
}
