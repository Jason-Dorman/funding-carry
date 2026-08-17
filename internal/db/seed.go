package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/shopspring/decimal"
)

// StatusUnverified marks a cb_products row that no venue response has confirmed.
// Part 5 replaces it with the status the products endpoint reports.
const StatusUnverified = "UNVERIFIED"

// execer is the seed's view of the database. The seed runs from cmd/migrate,
// which has no writer goroutine and no producers — it is a one-shot command, so
// executing directly here does not put a second writer in a running binary.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// The seed deliberately fills in only what configuration already knows: the
// product id and the contract size. Tick, tick value, leverage caps and fee
// tiers stay NULL until Part 5 reads them from the products endpoint, because a
// plausible placeholder in a fee column is worse than an empty one — it would be
// used, and nothing would say it had never been verified.
//
// DO NOTHING, not DO UPDATE: `make migrate` is re-run routinely, and it must
// never overwrite values the venue has since confirmed.
const seedPerpProductSQL = `
INSERT INTO cb_products (product_id, contract_size, status)
VALUES ($1, $2, $3)
ON CONFLICT (product_id) DO NOTHING`

// SeedPerpProduct inserts the perp product row the rest of the system reads
// contract size from, if it is not already there. It reports whether a row was
// inserted, so the migrate command can say which happened.
func SeedPerpProduct(ctx context.Context, db execer, productID string, contractSize decimal.Decimal) (bool, error) {
	tag, err := db.Exec(ctx, seedPerpProductSQL, productID, contractSize, StatusUnverified)
	if err != nil {
		return false, fmt.Errorf("seed product %s: %w", productID, err)
	}
	return tag.RowsAffected() > 0, nil
}
