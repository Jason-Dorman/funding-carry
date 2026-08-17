// Command migrate applies the database schema and seeds the perp product row.
//
// It is what `make migrate` runs, and it is a one-shot: it exits non-zero if the
// schema could not be brought up to date, so a deployment that cannot migrate
// fails loudly instead of starting services against a schema they do not match.
//
// The migrations are embedded in the binary (internal/db), so this needs no
// files on disk and no migration CLI installed — only DATABASE_URL.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/Jason-Dorman/funding-carry/internal/config"
	"github.com/Jason-Dorman/funding-carry/internal/db"
)

const service = "migrate"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", service, err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.LoadMigrateCmd()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	log := config.NewLogger(cfg.Log, os.Stdout).With("service", service)
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err = db.Migrate(cfg.DatabaseURL, log); err != nil {
		return err
	}

	// The seed needs a connection of its own: golang-migrate holds its own, and
	// closing the migrator releases it.
	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	inserted, err := db.SeedPerpProduct(ctx, pool, cfg.PerpProductID, cfg.ContractSizeETH)
	if err != nil {
		return err
	}
	if inserted {
		log.Info("seeded perp product",
			"product_id", cfg.PerpProductID,
			"contract_size_eth", cfg.ContractSizeETH.String(),
			"status", db.StatusUnverified)
	} else {
		log.Info("perp product already present, left untouched", "product_id", cfg.PerpProductID)
	}

	return nil
}
