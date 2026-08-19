package db

import (
	"context"
	"fmt"

	shopspring "github.com/jackc/pgx-shopspring-decimal"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Connect opens the connection pool every binary shares.
//
// The one line that matters here is the codec registration, though not for the
// reason it is tempting to give. Removing it and re-running the precision suite
// (the experiment is worth repeating if this ever looks like dead weight) leaves
// every value round trip passing, 2^53 + 1 included: without a registered codec
// pgx falls back to shopspring's own sql.Scanner/driver.Valuer, which is textual
// and does not lose digits. There is no float in that path.
//
// What does break is scale. Unregistered, a numeric stored as 4000.10 comes back
// with exponent -1 rather than -2 — the value is right, the recorded precision is
// not. Registering the codec makes decimal.Decimal the native representation of
// `numeric` in both directions, so coefficient and exponent both survive, which
// is what lets a mark or a fee be read back exactly as the venue quoted it
// (API spec section 3.5). TestTrailingZerosSurviveAsScale is the test that fails
// if this line is removed.
//
// pgx redacts the password when it reports a bad connection string, so wrapping
// its error cannot leak a credential into a log line (spec section 9); the test
// for that is in pool_test.go.
func Connect(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}

	// AfterConnect runs on every new connection, including ones the pool opens
	// later to grow or to replace a dropped one.
	cfg.AfterConnect = func(_ context.Context, conn *pgx.Conn) error {
		shopspring.Register(conn.TypeMap())
		return nil
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open pool: %w", err)
	}

	// NewWithConfig is lazy. Ping here so a misconfigured database is a startup
	// failure with a clear message rather than a first-write failure minutes in.
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	return pool, nil
}
