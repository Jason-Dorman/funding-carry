// Command onramp is the Part 3 learning exercise: cmd/ingest in miniature.
//
// Two fake feeds run as goroutines and fan into one channel, a single writer
// goroutine batches the ticks into TimescaleDB over pgx, and the binary serves
// /metrics. Every structural rule the real services follow is here at a size
// that fits in one reading: one writer, context through the stack, interfaces at
// the consumer, decimal money, graceful shutdown.
//
// It is not production code. It writes to its own throwaway table rather than
// the migrated schema, and it is configured by flags rather than
// internal/config, so the exercise cannot widen either contract. README.md in
// this directory walks the code.
//
// Usage:
//
//	go run ./research/onramp -run-for 10s
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/Jason-Dorman/funding-carry/internal/config"
	"github.com/Jason-Dorman/funding-carry/internal/db"
	"github.com/Jason-Dorman/funding-carry/internal/metrics"
)

const service = "onramp"

// schemaSQL creates the toy's table at startup. It is not a migration: the
// production schema is a contract owned by internal/db/migrations, and a
// learning exercise must not be able to change it. Dropping onramp_ticks costs
// nothing.
const schemaSQL = `
CREATE TABLE IF NOT EXISTS onramp_ticks (
    at      timestamptz NOT NULL,
    product text        NOT NULL,
    seq     bigint      NOT NULL,
    price   numeric     NOT NULL
)`

func main() {
	if err := run(); err != nil {
		// Flag and configuration failures happen before there is a logger, so
		// the last-resort path is stderr.
		fmt.Fprintf(os.Stderr, "%s: %v\n", service, err)
		os.Exit(1)
	}
}

// options is the whole configuration surface. Flags rather than environment
// variables: these knobs exist to make the exercise observable by hand, and
// none of them belongs in api-spec section 7.
type options struct {
	databaseURL string
	metricsAddr string
	perpProduct string
	spotProduct string
	interval    time.Duration
	flushEvery  time.Duration
	batchSize   int
	queueSize   int
	runFor      time.Duration
	logFormat   string
}

func parseFlags(args []string) (options, error) {
	fs := flag.NewFlagSet(service, flag.ContinueOnError)

	var o options
	// Deliberately not defaulted to os.Getenv("DATABASE_URL"): flag.PrintDefaults
	// renders a string flag's default verbatim, and ContinueOnError calls it on
	// -h and on any bad flag — which would print the database password to stderr.
	// The environment is read after parsing instead.
	fs.StringVar(&o.databaseURL, "database-url", "", "TimescaleDB connection string (default $DATABASE_URL)")
	fs.StringVar(&o.metricsAddr, "metrics-addr", ":9109", "address for /metrics and /healthz")
	fs.StringVar(&o.perpProduct, "perp", "ETP-20DEC30-CDE", "perp product id for the first fake feed")
	fs.StringVar(&o.spotProduct, "spot", "ETH-USD", "spot product id for the second fake feed")
	fs.DurationVar(&o.interval, "interval", 250*time.Millisecond, "time between ticks on each feed")
	fs.DurationVar(&o.flushEvery, "flush-every", 2*time.Second, "longest a tick waits before being written")
	fs.IntVar(&o.batchSize, "batch-size", 20, "pending ticks that trigger an immediate flush")
	fs.IntVar(&o.queueSize, "queue", 256, "fan-in channel buffer; producers block once it fills")
	fs.DurationVar(&o.runFor, "run-for", 0, "stop after this long (0 runs until SIGINT/SIGTERM)")
	fs.StringVar(&o.logFormat, "log-format", config.FormatText, "json or text")

	if err := fs.Parse(args); err != nil {
		return options{}, err
	}
	if o.databaseURL == "" {
		o.databaseURL = os.Getenv("DATABASE_URL")
	}
	if o.databaseURL == "" {
		return options{}, errors.New("no database: set DATABASE_URL or pass -database-url")
	}
	return o, nil
}

func run() error {
	opts, err := parseFlags(os.Args[1:])
	if err != nil {
		return err
	}

	log := config.NewLogger(config.LogConfig{
		Level:  slog.LevelInfo,
		Format: opts.logFormat,
	}, os.Stdout).With("service", service)

	// SIGINT and SIGTERM cancel the root context; -run-for adds a deadline to
	// the same context, so a timed run and a Ctrl-C shut down by the identical
	// path rather than one of them being a special case.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if opts.runFor > 0 {
		timed, cancel := context.WithTimeout(ctx, opts.runFor)
		defer cancel()
		ctx = timed
	}

	pool, err := db.Connect(ctx, opts.databaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	if err := ensureTable(ctx, pool); err != nil {
		return err
	}

	log.Info("starting",
		"perp_product", opts.perpProduct,
		"spot_product", opts.spotProduct,
		"interval", opts.interval.String(),
		"batch_size", opts.batchSize,
		"metrics_addr", opts.metricsAddr)

	return pipeline(ctx, opts, pool, log)
}

func ensureTable(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(ctx, schemaSQL); err != nil {
		return fmt.Errorf("create onramp_ticks: %w", err)
	}
	return nil
}

// feeds builds the two producers. Their bases differ by roughly the basis the
// real system trades on, which makes a glance at the table tell you the fan-in
// worked: two products, two price levels, interleaved.
func feeds(opts options, m *feedMetrics, log *slog.Logger) []*feed {
	return []*feed{
		{
			product:  opts.perpProduct,
			interval: opts.interval,
			base:     decimal.New(345120, -2), // 3451.20
			step:     decimal.New(25, -2),     // 0.25
			m:        m,
			log:      log,
		},
		{
			product:  opts.spotProduct,
			interval: opts.interval,
			base:     decimal.New(345000, -2), // 3450.00
			step:     decimal.New(10, -2),     // 0.10
			m:        m,
			log:      log,
		},
	}
}

// pipeline wires the goroutines and blocks until they have all stopped. The
// order of the statements below is the shutdown order, read backwards.
func pipeline(ctx context.Context, opts options, pool *pgxpool.Pool, log *slog.Logger) error {
	srv := metrics.NewServer(opts.metricsAddr, log)
	m := newMetrics(srv.Registry())

	// The metrics endpoint outlives the root context on purpose. The writer's
	// final flush happens after cancellation, and an endpoint that stopped with
	// everything else would make the last rows unobservable in the one scrape
	// where it matters.
	metricsCtx, stopMetrics := context.WithCancel(context.WithoutCancel(ctx))
	defer stopMetrics()
	metricsErr := make(chan error, 1)
	go func() { metricsErr <- srv.Serve(metricsCtx) }()

	ticks := make(chan tick, opts.queueSize)

	var producers sync.WaitGroup
	for _, f := range feeds(opts, m.feed, log) {
		producers.Add(1)
		go func() {
			defer producers.Done()
			f.run(ctx, ticks)
		}()
	}

	// Only a sender may close a channel, and only once every sender is finished
	// — which is what the WaitGroup is for. The closed channel is then the
	// writer's stop signal, so shutdown order falls out of the data flow instead
	// of needing to be coordinated: cancel, feeds return, channel closes, writer
	// drains and flushes.
	go func() {
		producers.Wait()
		close(ticks)
	}()

	writeErr := newWriter(pool, opts, m.writer, log).run(ctx, ticks)

	stopMetrics()
	// Both errors are reported: a failed write and a failed metrics endpoint are
	// independent failures, and swallowing either to return the other loses the
	// one that explains the other.
	return errors.Join(writeErr, <-metricsErr)
}
