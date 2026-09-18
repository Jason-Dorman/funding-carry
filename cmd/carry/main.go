// Command carry is the brain: it reads venue state from TimescaleDB, computes
// features, judges funding pressure, emits decisions, and — only through the risk
// engine — emits orders to the configured venues.
//
// Part 1 wired the skeleton: configuration, logging, signal handling and the
// metrics endpoint. Part 8 adds the order-entry stack — the FIX initiator as
// the first Venue, and the order-state machine every report flows through —
// with a temporary command-line trigger to send one order, which Part 15's
// router replaces. The decision tick is assembled in Parts 9 to 15.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/Jason-Dorman/funding-carry/internal/config"
)

const service = "carry"

func main() {
	if err := run(os.Args[1:]); err != nil {
		// Configuration failures happen before there is a logger, so the
		// last-resort path is stderr.
		fmt.Fprintf(os.Stderr, "%s: %v\n", service, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	flags := flag.NewFlagSet(service, flag.ContinueOnError)
	probeSpec := flags.String("probe-order", "", "TEMPORARY (Part 8): send one order to the FIX venue and "+
		"exit once it is terminal, e.g. side=sell,qty=1,px=2400.00,tif=GTC. Removed by Part 15's router.")
	if err := flags.Parse(args); err != nil {
		return err
	}
	var probe *probeOrder
	if *probeSpec != "" {
		p, err := parseProbe(*probeSpec)
		if err != nil {
			return fmt.Errorf("-probe-order: %w", err)
		}
		probe = &p
	}

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
		"fix_store", cfg.FIX.StorePath,
		"order_timeout", cfg.Execution.OrderTimeout.String(),
		"probe", probe != nil,
	)

	if err := trade(ctx, cfg, probe, log); err != nil {
		return err
	}
	log.Info("stopped")
	return nil
}
