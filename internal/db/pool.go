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
// The one line that matters here is the codec registration. Without it, pgx
// hands a `numeric` column to the generic pgtype.Numeric and a caller ends up
// converting through float64 somewhere — which is exactly the failure the
// "no float money" rule exists to prevent, and it is silent. Registering the
// shopspring codec on every connection makes decimal.Decimal the native wire
// representation of `numeric` in both directions, so a value's coefficient and
// exponent survive the round trip unchanged (API spec section 3.5).
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
