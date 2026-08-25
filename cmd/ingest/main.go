// Command ingest records market data: Coinbase Advanced Trade WebSocket streams,
// the Advanced Trade REST poller and funding estimator, and the Base wallet
// poller — all fanning into one writer goroutine that owns TimescaleDB.
//
// Part 4 wires the WebSocket half: five streams, each with its own connection,
// reconnect loop and gap detection, feeding cb_venue_state, cb_bars,
// cb_book_snapshots and cb_trades_agg. The REST poller and funding estimator
// (Part 5) and the Base poller (Part 6) hang off the same root context.
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

	"github.com/Jason-Dorman/funding-carry/internal/coinbase"
	"github.com/Jason-Dorman/funding-carry/internal/config"
	"github.com/Jason-Dorman/funding-carry/internal/db"
	"github.com/Jason-Dorman/funding-carry/internal/ingest"
	"github.com/Jason-Dorman/funding-carry/internal/metrics"
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

	// SIGINT and SIGTERM cancel the root context. Every goroutine below hangs off
	// it, so `docker compose down` drains and flushes rather than kills.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	log.Info("starting",
		"perp_product", cfg.PerpProductID,
		"spot_product", cfg.SpotProductID,
		"ws_url", cfg.Coinbase.WSURL,
		// Durations are logged as strings: slog's JSON handler would otherwise
		// render them as bare nanosecond counts.
		"poll_rest", cfg.Poll.REST.String(),
		"poll_base", cfg.Poll.Base.String(),
		"book_snapshot", cfg.Poll.BookSnap.String(),
		"backfill", cfg.Backfill.String(),
		"maintenance_break", cfg.Maintenance.String(),
	)

	return pipeline(ctx, cfg, pool, db.NewReader(pool), ingest.WSDialer{URL: cfg.Coinbase.WSURL}, log)
}

// accountSource builds the authenticated account client, or nil.
//
// Nil is a supported state, not a failure: the public stack must start and run
// without a credential, so an absent key means the account half is absent. A
// key that is present but malformed is the opposite — that is an operator
// mistake, and it fails the startup rather than degrading quietly into the same
// state as having no key at all.
func accountSource(cfg *config.Ingest, log *slog.Logger) (ingest.AccountSource, error) {
	name, key := cfg.Secrets.CBAPIKeyName, cfg.Secrets.CBAPIPrivateKey
	if !name.IsSet() && !key.IsSet() {
		log.Info("no CDP credential; cb_account_state polling is off",
			"add", "CB_API_KEY_NAME and CB_API_PRIVATE_KEY in .env.private")
		return nil, nil
	}
	signer, err := coinbase.NewSigner(name.Reveal(), key.Reveal())
	if err != nil {
		return nil, fmt.Errorf("cdp credential: %w", err)
	}
	log.Info("CDP credential loaded; cb_account_state polling is on")
	return coinbase.NewClient(signer), nil
}

// rowSender is the database as this binary's writer needs it: one batched send.
// Named here rather than passing *pgxpool.Pool so that the wiring below — which
// is where the shutdown ordering lives, and therefore where it can be got wrong
// — is testable without a database.
type rowSender interface {
	SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults
}

// pipeline wires the goroutines and blocks until they have all stopped. Read the
// statements backwards and they are the shutdown order from architecture
// section 8: cancel, streams stop, writer drains and flushes, pool closes.
func pipeline(ctx context.Context, cfg *config.Ingest, sender rowSender, store ingest.BarStore,
	dialer ingest.Dialer, log *slog.Logger,
) error {
	srv := metrics.NewServer(cfg.MetricsAddr, log)

	// The metrics endpoint outlives the root context deliberately. The writer's
	// final flush happens after cancellation, and an endpoint that stopped with
	// everything else would make the last rows written unobservable in the one
	// scrape where it matters.
	metricsCtx, stopMetrics := context.WithCancel(context.WithoutCancel(ctx))
	defer stopMetrics()
	metricsErr := make(chan error, 1)
	go func() { metricsErr <- srv.Serve(metricsCtx) }()

	// Streams stop for either of two reasons, so they get their own context: the
	// root context ending, or the writer dying. The second is not optional. A
	// stream only learns the writer has gone by being told ErrWriterStopped from
	// a Submit, and two of the five never submit — the ticker hands its quotes to
	// the venue-state sampler and the status stream hands it a flag. Without this
	// cancellation those two would keep reading a socket forever after a fatal
	// write, Ingest.Run would never return, Close below would never be reached,
	// and the process would sit alive with a dead database still answering
	// /healthz with 200. TestPipelineExitsWhenTheWriterDies is the regression.
	streamCtx, stopStreams := context.WithCancel(ctx)
	defer stopStreams()

	writer := db.NewWriter(sender, db.WriterOptions{}, log, db.NewWriterMetrics(srv.Registry(), ingest.Namespace))
	// The writer is deliberately not given the root context.
	//
	// Its shutdown drains the rows producers have already handed over, and that
	// drain is only safe once the producers have stopped. Handing it the root
	// context would start the drain on SIGTERM while five stream goroutines were
	// still submitting — a frame already read when the signal lands is dispatched
	// on the canceled context and submits from there — and a row accepted after
	// the drain had passed would be stranded in the queue with its producer told
	// nil. Detached, the writer stops only at Close, which is after streams.Run
	// has returned, so the ordering the comment below claims is actually true.
	//
	// Run's error is discarded here and collected from Close, which returns the
	// same value; reading it from both would report one failure twice.
	go func() {
		defer stopStreams()
		_ = writer.Run(context.WithoutCancel(ctx))
	}()

	account, err := accountSource(cfg, log)
	if err != nil {
		stopStreams()
		return err
	}

	streams := ingest.New(ingest.Options{
		PerpProduct:      cfg.PerpProductID,
		SpotProduct:      cfg.SpotProductID,
		SampleInterval:   cfg.Poll.REST,
		BookSnapInterval: cfg.Poll.BookSnap,
		Maintenance:      cfg.Maintenance,
		Dialer:           dialer,
		RESTClient:       ingest.NewRESTClient(cfg.Coinbase.APIURL, nil),
		// The backfill reads what is already stored so it can download only what
		// is missing, which is what lets it run on every start.
		BarStore: store,
		Account:  account,
		Backfill: cfg.Backfill,
	}, writer, ingest.NewMetrics(srv.Registry()), log)

	// Run returns only once every stream goroutine has stopped, which is the
	// precondition the writer's drain depends on: Close takes the rows producers
	// have already handed over, and a producer still running could hand over one
	// more after the drain had passed.
	streams.Run(streamCtx)

	// Close waits for the final flush and returns whatever Run returned, so a
	// fatal write failure is not lost by shutting down cleanly around it.
	writeErr := writer.Close()
	stopMetrics()

	// Both failures are reported rather than the first one found. A failed write
	// and a failed metrics endpoint are independent, and swallowing either to
	// return the other loses the one that explains it.
	return errors.Join(writeErr, <-metricsErr)
}
