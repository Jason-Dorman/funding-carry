package db

import (
	"context"
	"strings"
	"testing"
)

// DATABASE_URL carries the database password. A connection failure is exactly
// the moment that string gets wrapped into an error and logged, so the error
// must not contain it (spec section 9).
func TestConnectDoesNotLeakThePassword(t *testing.T) {
	t.Parallel()

	const password = "hunter2-would-be-a-real-password"
	_, err := Connect(context.Background(), "postgres://carry:"+password+"@localhost:not-a-port/carry")
	if err == nil {
		t.Fatal("expected a connection failure")
	}
	if strings.Contains(err.Error(), password) {
		t.Fatalf("password leaked into the error: %v", err)
	}
}

// The migrator is the one place the connection string is rewritten, because
// golang-migrate resolves its driver by URL scheme.
func TestMigrateURLSwapsOnlyTheScheme(t *testing.T) {
	t.Parallel()

	got, err := migrateURL("postgres://carry:secret@timescaledb:5432/carry?sslmode=disable")
	if err != nil {
		t.Fatalf("migrateURL: %v", err)
	}
	const want = "pgx5://carry:secret@timescaledb:5432/carry?sslmode=disable"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
