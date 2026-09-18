// Command sim-venue is the exchange simulator: a FIX 4.4 acceptor with a fill
// model driven by recorded top-of-book, so the order-entry stack can be exercised
// end to end — including latency, slippage, partial fills and sequence recovery —
// without touching a real venue.
//
// Part 7 builds it: the acceptor, the fill model, fills persisted to the fills
// table under venue='sim', and the FIX session catalogue. It reads market data
// from TimescaleDB and writes to it through one writer goroutine, the same shape
// as ingest.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/jackc/pgx/v5"

	"github.com/Jason-Dorman/funding-carry/internal/config"
	"github.com/Jason-Dorman/funding-carry/internal/db"
	"github.com/Jason-Dorman/funding-carry/internal/fix"
	"github.com/Jason-Dorman/funding-carry/internal/metrics"
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

	// SIGINT and SIGTERM cancel the root context: the engine stops evaluating,
	// the session logs out with its sequence numbers flushed to disk, and the
	// writer drains.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	log.Info("starting",
		"perp_product", cfg.PerpProductID,
		"fix_session", fmt.Sprintf("%s->%s", cfg.FIX.Target, cfg.FIX.Sender),
		"fix_port", cfg.FIX.Port,
		"fix_store", cfg.FIX.StorePath,
		// Logged as strings: slog's JSON handler renders a Duration as a bare
		// nanosecond count.
		"fill_latency", cfg.Fill.Latency.String(),
		"fill_slippage_bps", cfg.Fill.SlippageBps.String(),
		"partial_threshold", cfg.Fill.PartialThreshold.String(),
		"partial_slices", cfg.Fill.PartialSlices,
		"book_poll", cfg.Fill.BookPoll.String(),
		"book_max_age", cfg.Fill.BookMaxAge.String(),
	)

	return simulate(ctx, cfg, pool, db.NewReader(pool), log)
}

// rowSender is the database as this binary's writer needs it: one batched send.
// Named here rather than passing *pgxpool.Pool so the wiring below — which is
// where the shutdown ordering lives, and therefore where it can be got wrong —
// is testable without a database.
type rowSender interface {
	SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults
}

// simulate wires the goroutines and blocks until they have stopped. Read it
// backwards and it is the shutdown order from architecture section 8: cancel,
// the engine stops, the session logs out, the writer drains and flushes, the
// pool closes.
func simulate(ctx context.Context, cfg *config.SimVenue, sender rowSender, books fix.BookSource,
	log *slog.Logger,
) error {
	srv := metrics.NewServer(cfg.MetricsAddr, log)

	// The metrics endpoint outlives the root context deliberately, the same way
	// ingest's does: the writer's final flush happens after cancellation, and an
	// endpoint that stopped with everything else would make the last rows
	// written unobservable in the one scrape where it matters.
	metricsCtx, stopMetrics := context.WithCancel(context.WithoutCancel(ctx))
	defer stopMetrics()
	metricsErr := make(chan error, 1)
	go func() { metricsErr <- srv.Serve(metricsCtx) }()

	// The acceptor stops for either of two reasons: the root context ending, or
	// the writer dying under it. The second matters — a simulator whose fills
	// are no longer being recorded must not keep printing them.
	acceptorCtx, stopAcceptor := context.WithCancel(ctx)
	defer stopAcceptor()

	writer := db.NewWriter(sender, db.WriterOptions{}, log, db.NewWriterMetrics(srv.Registry(), fix.Namespace))
	// The writer is deliberately not given the root context: its shutdown drains
	// what producers have already handed over, and that drain is only correct
	// once the producers have stopped. Detached, it stops at Close, which is
	// after the acceptor has returned.
	//
	// Run's error is discarded here and collected from Close, which returns the
	// same value; reading it from both would report one failure twice.
	go func() {
		defer stopAcceptor()
		_ = writer.Run(context.WithoutCancel(ctx))
	}()

	acceptor, err := fix.NewAcceptor(fix.Options{
		Product: cfg.PerpProductID,
		Session: cfg.FIX,
		Fill:    cfg.Fill,
	}, books, writer, fix.NewMetrics(srv.Registry()), log)
	if err != nil {
		// Unwound in the same order a clean run unwinds in: stop the producers,
		// close the writer, then the endpoint. Reading metricsErr before
		// stopMetrics would wait forever on a server that has not been told to
		// stop.
		stopAcceptor()
		writeErr := writer.Close()
		stopMetrics()
		return errors.Join(err, writeErr, <-metricsErr)
	}

	runErr := acceptor.Run(acceptorCtx)

	// Close waits for the final flush and returns whatever Run returned, so a
	// fatal write failure is not lost by shutting down cleanly around it.
	writeErr := writer.Close()
	stopMetrics()

	// Every failure is reported rather than the first one found: a dead writer,
	// a failed listen and a failed metrics endpoint are independent, and
	// swallowing any of them to return another loses the one that explains it.
	return errors.Join(runErr, writeErr, <-metricsErr)
}
