// Command carry is the brain: it reads venue state from TimescaleDB, computes
// features, judges funding pressure, emits decisions, and — only through the risk
// engine — emits orders to the configured venues.
//
// Part 1 wires the skeleton only: configuration, logging, signal handling and the
// metrics endpoint. The decision tick is assembled in Parts 9 to 15.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/Jason-Dorman/funding-carry/internal/config"
	"github.com/Jason-Dorman/funding-carry/internal/metrics"
)

const service = "carry"

func main() {
	if err := run(); err != nil {
		// Configuration failures happen before there is a logger, so the
		// last-resort path is stderr.
		fmt.Fprintf(os.Stderr, "%s: %v\n", service, err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.LoadCarry()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	log := config.NewLogger(cfg.Log, os.Stdout).With("service", service)
	slog.SetDefault(log)

	// SIGINT and SIGTERM cancel the root context. The shutdown order in
	// architecture section 8 — stop deciding, cancel orders, flush, FIX logout,
	// close the pool — is built on this single cancel.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Thresholds are deliberately not logged: the committed values are synthetic,
	// the real ones live in .env.private, and a log line is the easiest way for a
	// fitted parameter to escape (spec section 9).
	log.Info("starting",
		"perp_product", cfg.PerpProductID,
		"spot_product", cfg.SpotProductID,
		"contract_size_eth", cfg.ContractSizeETH.String(),
		"kill_switch", cfg.Risk.KillSwitch,
		"intraday_margin_opt_in", cfg.Risk.IntradayMarginOptIn,
		"maintenance_break", cfg.Maintenance.String(),
		"fix_session", fmt.Sprintf("%s->%s@%s:%d", cfg.FIX.Sender, cfg.FIX.Target, cfg.FIX.Host, cfg.FIX.Port),
	)

	if err := metrics.NewServer(cfg.MetricsAddr, log).Serve(ctx); err != nil {
		return err
	}

	log.Info("stopped")
	return nil
}
