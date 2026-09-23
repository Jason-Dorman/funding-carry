//go:build integration

// Package dbtest prepares a throwaway TimescaleDB for integration tests in
// packages other than internal/db.
//
// It follows the same rules internal/db's own integration suite does, for the
// same reasons (testing strategy): the suite is run on purpose or not at all —
// no DATABASE_URL is a failure, never a skip — and it never touches the
// database that URL points at, because the recorded market history there is
// the system's primary asset. Each caller names its own throwaway database, so
// packages whose tests run in parallel cannot drop each other's.
//
// internal/db cannot use this package itself: its tests are internal to the
// package and this package imports it, which would be a cycle. The ~40 lines
// of setup exist twice for that reason and no other.
package dbtest

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/Jason-Dorman/funding-carry/internal/db"
)

// Open drops and recreates the named database beside the one DATABASE_URL
// points at, applies every migration to it, and returns a pool on it that is
// closed when the test ends.
func Open(t *testing.T, name string) *pgxpool.Pool {
	t.Helper()
	admin := os.Getenv("DATABASE_URL")
	if admin == "" {
		t.Fatal("DATABASE_URL is not set. Run the suite with `make test-integration`, " +
			"which points it at the Compose TimescaleDB on 127.0.0.1:15432.")
	}
	ctx := t.Context()

	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatalf("connect to administer %s: %v", name, err)
	}
	// FORCE disconnects anything still attached from a previous run.
	if _, err = conn.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
		t.Fatalf("drop %s: %v", name, err)
	}
	if _, err = conn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	if err = conn.Close(ctx); err != nil {
		t.Fatalf("close admin connection: %v", err)
	}

	target, err := replaceDatabase(admin, name)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Migrate(target, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("migrate %s: %v", name, err)
	}

	pool, err := db.Connect(ctx, target)
	if err != nil {
		t.Fatalf("connect to %s: %v", name, err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func replaceDatabase(rawURL, database string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("parse database url: not a valid url")
	}
	u.Path = "/" + database
	return u.String(), nil
}

// Seed writes rows through the real writer and waits for them to land, so a
// test's fixture reaches the table by the same path production rows do.
func Seed(t *testing.T, pool *pgxpool.Pool, rows ...db.Row) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	w := db.NewWriter(pool, db.WriterOptions{}, log, db.NewWriterMetrics(prometheus.NewRegistry(), "dbtest"))

	ran := make(chan error, 1)
	go func() { ran <- w.Run(context.Background()) }()
	for _, r := range rows {
		if err := w.Submit(t.Context(), r); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("seed: %v", err)
	}
	<-ran
}
