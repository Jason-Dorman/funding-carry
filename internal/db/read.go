package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"
)

// Reading is the exception to "nothing but the writer touches the database", and
// the exception is narrow on purpose.
//
// The rule exists to keep writes serialized through one goroutine, so that
// insert order, batching and failure handling all have exactly one home. A read
// is none of those things. What earns its place here is the backfill: it has to
// know what it already has before it decides what to fetch, and answering that
// from a fetch would mean re-downloading forty-five days to discover that
// forty-five days are already stored.
//
// It also fixes something subtler. A reconstruction computed from one run's
// download is a function of whatever that download happened to return; a
// reconstruction computed from the stored bars is a function of the database,
// which means anyone can recompute it and get the same answer. For the series
// this system's whole signal rests on, being reproducible matters more than
// being marginally simpler.

// Querier is the read side of the pool, named here so a reader can be tested
// against a fake and so this package does not depend on pgxpool.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Reader answers "what is already stored?" for the components that need it.
type Reader struct {
	q Querier
}

// NewReader binds a reader to the pool.
func NewReader(q Querier) *Reader { return &Reader{q: q} }

// StoredBar is one bar as it exists in cb_bars. TS is the bar's close, matching
// the column.
type StoredBar struct {
	TS     time.Time
	Open   decimal.Decimal
	High   decimal.Decimal
	Low    decimal.Decimal
	Close  decimal.Decimal
	Volume decimal.Decimal
}

// ReadBars returns the stored bars for one product and timeframe over
// [from, to), oldest first.
func (r *Reader) ReadBars(ctx context.Context, product, tf string, from, to time.Time) ([]StoredBar, error) {
	rows, err := r.q.Query(ctx,
		`SELECT ts, open, high, low, close, volume
		   FROM cb_bars
		  WHERE product_id = $1 AND tf = $2 AND ts >= $3 AND ts < $4
		  ORDER BY ts`,
		product, tf, from, to)
	if err != nil {
		return nil, fmt.Errorf("read bars for %s: %w", product, err)
	}
	defer rows.Close()

	var out []StoredBar
	for rows.Next() {
		var b StoredBar
		if err := rows.Scan(&b.TS, &b.Open, &b.High, &b.Low, &b.Close, &b.Volume); err != nil {
			return nil, fmt.Errorf("scan bar for %s: %w", product, err)
		}
		b.TS = b.TS.UTC()
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read bars for %s: %w", product, err)
	}
	return out, nil
}

// ReadFundingHours returns the hours that already carry an ACCRUAL row for a
// product over [from, to).
//
// It deliberately does not filter on funding_source: an hour recorded live is
// just as much "already done" as one reconstructed, and re-deriving it would
// only produce a row the natural key discards.
func (r *Reader) ReadFundingHours(ctx context.Context, product string, from, to time.Time) ([]time.Time, error) {
	rows, err := r.q.Query(ctx,
		`SELECT ts FROM funding_events
		  WHERE product_id = $1 AND kind = $2 AND ts >= $3 AND ts < $4
		  ORDER BY ts`,
		product, FundingKindAccrual, from, to)
	if err != nil {
		return nil, fmt.Errorf("read funding hours for %s: %w", product, err)
	}
	defer rows.Close()

	var out []time.Time
	for rows.Next() {
		var ts time.Time
		if err := rows.Scan(&ts); err != nil {
			return nil, fmt.Errorf("scan funding hour for %s: %w", product, err)
		}
		out = append(out, ts.UTC())
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read funding hours for %s: %w", product, err)
	}
	return out, nil
}

// LatestFundingRate returns the most recent recorded ACCRUAL rate for a product
// before `before`, and whether there was one.
//
// It exists for the live runner's smoothing seed. The backfill can hand over the
// hour it just computed, but the common restart computes nothing — the history
// is already complete — and without this the first live hour would be published
// as its own raw premium, restarting the recursion from nothing every boot.
func (r *Reader) LatestFundingRate(ctx context.Context, product string, before time.Time) (decimal.Decimal, time.Time, bool, error) {
	rows, err := r.q.Query(ctx,
		`SELECT ts, rate_hourly FROM funding_events
		  WHERE product_id = $1 AND kind = $2 AND ts < $3 AND rate_hourly IS NOT NULL
		  ORDER BY ts DESC LIMIT 1`,
		product, FundingKindAccrual, before)
	if err != nil {
		return decimal.Decimal{}, time.Time{}, false, fmt.Errorf("read latest funding rate for %s: %w", product, err)
	}
	defer rows.Close()

	if !rows.Next() {
		return decimal.Decimal{}, time.Time{}, false, rows.Err()
	}
	var ts time.Time
	var rate decimal.Decimal
	if err := rows.Scan(&ts, &rate); err != nil {
		return decimal.Decimal{}, time.Time{}, false, fmt.Errorf("scan latest funding rate: %w", err)
	}
	// Close before reading Err, the same way latest.go's one does: pgx assigns
	// the drain error inside Close, and Go evaluates a return expression before
	// the deferred call runs — so `return ..., rows.Err()` reads the error
	// before it can be set and reports success on a connection that dropped
	// between the row and ReadyForQuery. The scanned value is still correct;
	// what was lost was the signal. Found by the Part 9 review, which noted
	// that this file and latest.go had come to teach opposite rules.
	rows.Close()
	if err := rows.Err(); err != nil {
		return decimal.Decimal{}, time.Time{}, false, fmt.Errorf("read latest funding rate for %s: %w", product, err)
	}
	return rate, ts.UTC(), true, nil
}

// StoredBook is the top of the book as cb_book_snapshots recorded it.
//
// TS is the snapshot's own timestamp — the sampling boundary ingest wrote it on,
// not the moment it was read back — so a consumer measuring staleness is
// measuring the age of the market data rather than the age of its own query.
type StoredBook struct {
	TS      time.Time
	BestBid decimal.Decimal
	BestAsk decimal.Decimal
}

// LatestBook returns the most recent snapshot for a product that carries both
// sides of the touch, and whether there was one.
//
// Both sides are required because a caller that prices against the book needs a
// bid and an ask, and every column in the schema is nullable: a snapshot written
// while one side of the market was empty is a real row, and it is not a book
// anyone can fill against. Skipping it here rather than returning it and letting
// the caller discover the NULL keeps "there is no usable market" one answer
// instead of two.
func (r *Reader) LatestBook(ctx context.Context, product string) (StoredBook, bool, error) {
	rows, err := r.q.Query(ctx,
		`SELECT ts, best_bid, best_ask
		   FROM cb_book_snapshots
		  WHERE product_id = $1 AND best_bid IS NOT NULL AND best_ask IS NOT NULL
		  ORDER BY ts DESC LIMIT 1`,
		product)
	if err != nil {
		return StoredBook{}, false, fmt.Errorf("read latest book for %s: %w", product, err)
	}
	defer rows.Close()

	if !rows.Next() {
		return StoredBook{}, false, rows.Err()
	}
	var b StoredBook
	if err := rows.Scan(&b.TS, &b.BestBid, &b.BestAsk); err != nil {
		return StoredBook{}, false, fmt.Errorf("scan latest book for %s: %w", product, err)
	}
	b.TS = b.TS.UTC()
	// Close before Err; see the note in LatestFundingRate.
	rows.Close()
	if err := rows.Err(); err != nil {
		return StoredBook{}, false, fmt.Errorf("read latest book for %s: %w", product, err)
	}
	return b, true, nil
}
