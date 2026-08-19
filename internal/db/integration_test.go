//go:build integration

// Integration tests for the persistence layer, run by `make test-integration`
// against the Compose TimescaleDB. The schema is the contract between ingest,
// carry and research, so it is exercised for real here — real hypertables, real
// migrations, real numeric round trips — rather than against a fake.
//
// They never touch the database DATABASE_URL points at. Everything runs in a
// throwaway database this file drops and recreates on every run, because the
// recorded market history in the working database is the system's primary asset
// and a test suite must not be able to delete it.
package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/shopspring/decimal"
)

// testDatabase is the throwaway database every test in this file uses.
const testDatabase = "carry_integration"

// testDatabaseURL points at it, once beforeTests has built it. Empty means the
// suite has nothing to run against and every test skips.
var testDatabaseURL string

func beforeTests() {
	admin := os.Getenv("DATABASE_URL")
	if admin == "" {
		// Building with the integration tag is an explicit request to run these
		// tests. Skipping quietly would print "ok" and exit 0 for a run that
		// executed nothing — a green tick that means the opposite of what it
		// looks like, in CI most of all.
		fmt.Fprintln(os.Stderr,
			"integration setup: DATABASE_URL is not set. Run the suite with `make test-integration`, "+
				"which points it at the Compose TimescaleDB on 127.0.0.1:15432.")
		os.Exit(1)
	}

	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelWarn}))

	if err := recreateTestDatabase(ctx, admin); err != nil {
		fmt.Fprintf(os.Stderr, "integration setup: %v\n", err)
		os.Exit(1)
	}

	target, err := replaceDatabase(admin, testDatabase)
	if err != nil {
		fmt.Fprintf(os.Stderr, "integration setup: %v\n", err)
		os.Exit(1)
	}

	// This is the same call `make migrate` makes, so the acceptance criterion
	// "fresh make up && make migrate creates all tables" is what every test in
	// this file runs against.
	if err := Migrate(target, log); err != nil {
		fmt.Fprintf(os.Stderr, "integration setup: %v\n", err)
		os.Exit(1)
	}
	testDatabaseURL = target
}

// recreateTestDatabase drops and recreates the throwaway database. FORCE
// disconnects anything still attached from a previous run rather than failing.
func recreateTestDatabase(ctx context.Context, adminURL string) error {
	conn, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		return fmt.Errorf("connect to administer %s: %w", testDatabase, err)
	}
	defer func() { _ = conn.Close(ctx) }()

	if _, err := conn.Exec(ctx, "DROP DATABASE IF EXISTS "+testDatabase+" WITH (FORCE)"); err != nil {
		return fmt.Errorf("drop %s: %w", testDatabase, err)
	}
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+testDatabase); err != nil {
		return fmt.Errorf("create %s: %w", testDatabase, err)
	}
	return nil
}

func replaceDatabase(rawURL, database string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("parse database url: not a valid url")
	}
	u.Path = "/" + database
	return u.String(), nil
}

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if testDatabaseURL == "" {
		// Unreachable: beforeTests exits when DATABASE_URL is unset. Kept as a
		// failure rather than a skip so a future change to that cannot turn the
		// whole suite into a silent pass.
		t.Fatal("integration database was never prepared")
	}

	pool, err := Connect(t.Context(), testDatabaseURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// ---------------------------------------------------------------------------
// Schema
// ---------------------------------------------------------------------------

// hypertables carries the API spec section 5.1 list. Everything else in the
// schema is a plain state table.
var hypertables = []string{
	"cb_venue_state",
	"cb_bars",
	"cb_book_snapshots",
	"cb_trades_agg",
	"cb_features",
	"base_state",
	"cb_account_state",
}

func TestMigrationsCreateEveryTable(t *testing.T) {
	pool := testPool(t)
	ctx := t.Context()

	for _, r := range everyRowType() {
		table := r.row().table
		var exists bool
		err := pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM information_schema.tables
			                WHERE table_schema = 'public' AND table_name = $1)`,
			table).Scan(&exists)
		if err != nil {
			t.Fatalf("look up %s: %v", table, err)
		}
		if !exists {
			t.Errorf("table %s was not created", table)
		}
	}
}

// Hypertables are the point of running TimescaleDB rather than Postgres: if a
// series table were created as a plain table the system would still work, get
// slower every week, and nothing would say so.
func TestSeriesTablesAreHypertables(t *testing.T) {
	pool := testPool(t)
	ctx := t.Context()

	rows, err := pool.Query(ctx,
		`SELECT hypertable_name FROM timescaledb_information.hypertables
		 WHERE hypertable_schema = 'public'`)
	if err != nil {
		t.Fatalf("query hypertables: %v", err)
	}
	defer rows.Close()

	found := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		found[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate: %v", err)
	}

	for _, table := range hypertables {
		if !found[table] {
			t.Errorf("%s is not a hypertable", table)
		}
		delete(found, table)
	}
	for extra := range found {
		t.Errorf("%s is a hypertable but is not in the API spec section 5.1 list", extra)
	}
}

// The Go row types and the migrations are two descriptions of one schema. This
// is what keeps them from drifting: every column of every table is written by
// its row type, and every column a row type names exists.
func TestRowTypesMatchTheSchema(t *testing.T) {
	pool := testPool(t)
	ctx := t.Context()

	for _, r := range everyRowType() {
		d := r.row()
		t.Run(d.table, func(t *testing.T) {
			rows, err := pool.Query(ctx,
				`SELECT column_name, is_identity
				 FROM information_schema.columns
				 WHERE table_schema = 'public' AND table_name = $1`, d.table)
			if err != nil {
				t.Fatalf("describe %s: %v", d.table, err)
			}
			defer rows.Close()

			inSchema := map[string]bool{}
			for rows.Next() {
				var name, identity string
				if err := rows.Scan(&name, &identity); err != nil {
					t.Fatalf("scan: %v", err)
				}
				// Identity columns are generated by the database and must not be
				// written by a producer.
				if identity == "YES" {
					continue
				}
				inSchema[name] = true
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("iterate: %v", err)
			}

			for _, column := range d.columns {
				if !inSchema[column] {
					t.Errorf("%s.%s is written by the row type but is not in the schema", d.table, column)
				}
				delete(inSchema, column)
			}
			for column := range inSchema {
				t.Errorf("%s.%s exists in the schema but no row type can write it", d.table, column)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Precision — the Part 1 carry-forward
// ---------------------------------------------------------------------------

// A decimal survives decimal -> numeric -> decimal unchanged, including digits a
// float64 cannot represent. Any code path that routed through a float would fail
// this on the very first value: 2^53 + 1 is the smallest integer a float64
// rounds, and it would come back one lower.
func TestDecimalRoundTripSurvivesNumeric(t *testing.T) {
	pool := testPool(t)
	ctx := t.Context()

	values := []string{
		"9007199254740993",                      // 2^53 + 1: the first integer float64 cannot hold
		"9007199254740993.0000000000000001",     // and again, past the decimal point
		"0.1000000000000000055511151231257827",  // the binary expansion of float64(0.1)
		"123456789012345678901234567890.123456", // wider than any float
		"-0.0000000000000000000000000001",       // a signed value at the far end of the scale
	}

	product := "PRECISION-TEST"
	base := time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC)

	for i, raw := range values {
		want := decimal.RequireFromString(raw)
		ts := base.Add(time.Duration(i) * time.Second)

		row := VenueStateRow{TS: ts, ProductID: product, FuturesMark: Num(want)}.row()
		if _, err := pool.Exec(ctx, insertStatement(row), row.values...); err != nil {
			t.Fatalf("insert %s: %v", raw, err)
		}

		var got decimal.Decimal
		err := pool.QueryRow(ctx,
			`SELECT futures_mark FROM cb_venue_state WHERE product_id = $1 AND ts = $2`,
			product, ts).Scan(&got)
		if err != nil {
			t.Fatalf("read back %s: %v", raw, err)
		}

		if got.String() != raw {
			t.Errorf("round trip of %s came back as %s", raw, got.String())
		}
		if !got.Equal(want) {
			t.Errorf("round trip of %s is not equal to what went in", raw)
		}
	}
}

// Postgres preserves a numeric's scale, so trailing zeros survive the database
// even though decimal.String drops them on the way out. Money-facing output uses
// StringFixed for exactly this reason (API spec section 3.5).
func TestTrailingZerosSurviveAsScale(t *testing.T) {
	pool := testPool(t)
	ctx := t.Context()

	want := decimal.RequireFromString("4000.10")
	ts := time.Date(2026, 8, 17, 1, 0, 0, 0, time.UTC)
	row := VenueStateRow{TS: ts, ProductID: "SCALE-TEST", FuturesMark: Num(want)}.row()
	if _, err := pool.Exec(ctx, insertStatement(row), row.values...); err != nil {
		t.Fatalf("insert: %v", err)
	}

	var got decimal.Decimal
	err := pool.QueryRow(ctx,
		`SELECT futures_mark FROM cb_venue_state WHERE product_id = 'SCALE-TEST' AND ts = $1`,
		ts).Scan(&got)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}

	if !got.Equal(want) {
		t.Fatalf("got %s, want %s", got, want)
	}
	if got.Exponent() != want.Exponent() {
		t.Errorf("scale changed: exponent %d, want %d", got.Exponent(), want.Exponent())
	}
	if got.StringFixed(2) != "4000.10" {
		t.Errorf("StringFixed(2) = %s, want 4000.10", got.StringFixed(2))
	}
}

// An absent optional numeric is NULL, not zero. The distinction is the whole
// reason the row types use NullDecimal.
func TestAbsentDecimalWritesNull(t *testing.T) {
	pool := testPool(t)
	ctx := t.Context()

	ts := time.Date(2026, 8, 17, 2, 0, 0, 0, time.UTC)
	row := VenueStateRow{TS: ts, ProductID: "NULL-TEST"}.row()
	if _, err := pool.Exec(ctx, insertStatement(row), row.values...); err != nil {
		t.Fatalf("insert: %v", err)
	}

	var mark decimal.NullDecimal
	var source *string
	err := pool.QueryRow(ctx,
		`SELECT futures_mark, funding_source FROM cb_venue_state
		 WHERE product_id = 'NULL-TEST' AND ts = $1`, ts).Scan(&mark, &source)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if mark.Valid {
		t.Errorf("an unset mark came back as %s, want NULL", mark.Decimal)
	}
	if source != nil {
		t.Errorf("an unset funding_source came back as %q, want NULL", *source)
	}
}

// The other half of the Part 1 carry-forward: the jsonb encoding of a decision's
// inputs. Postgres keeps a bare JSON number exact, so the corruption this guards
// against is invisible from inside the system — it only appears when the
// backtester or a dashboard reads the column back through a float.
func TestSnapshotRoundTripsThroughJSONB(t *testing.T) {
	pool := testPool(t)
	ctx := t.Context()

	snap := featureSnapshot{
		FundingRate: decimal.RequireFromString("0.0000123456789012345678"),
		SpotMark:    decimal.RequireFromString("9007199254740993.0000000000000001"),
		Contracts:   -7,
	}
	encoded, err := EncodeSnapshot(snap)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	ts := time.Date(2026, 8, 17, 3, 0, 0, 0, time.UTC)
	row := DecisionRow{
		TS:            ts,
		State:         "BLOCKED",
		ReasonCodes:   []string{"FEED_STALE"},
		InputSnapshot: encoded,
	}.row()
	if _, insertErr := pool.Exec(ctx, insertStatement(row), row.values...); insertErr != nil {
		t.Fatalf("insert: %v", insertErr)
	}

	var stored string
	var codes []string
	err = pool.QueryRow(ctx,
		`SELECT input_snapshot::text, reason_codes FROM decisions WHERE ts = $1`, ts).
		Scan(&stored, &codes)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}

	// Stored as a JSON string, which is what survives a consumer that parses
	// JSON numbers as doubles.
	if !strings.Contains(stored, `"9007199254740993.0000000000000001"`) {
		t.Errorf("spot mark is not a quoted string in the stored document: %s", stored)
	}

	var back featureSnapshot
	if err := json.Unmarshal([]byte(stored), &back); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if back.SpotMark.String() != snap.SpotMark.String() {
		t.Errorf("spot mark round trip: got %s, want %s", back.SpotMark, snap.SpotMark)
	}
	if back.FundingRate.String() != snap.FundingRate.String() {
		t.Errorf("funding rate round trip: got %s, want %s", back.FundingRate, snap.FundingRate)
	}
	if len(codes) != 1 || codes[0] != "FEED_STALE" {
		t.Errorf("reason codes round trip: got %v", codes)
	}
}

// numeric[] is the one array type in the schema, and it goes through the same
// codec as every scalar numeric.
func TestNumericArraysRoundTrip(t *testing.T) {
	pool := testPool(t)
	ctx := t.Context()

	depth := []decimal.Decimal{
		decimal.RequireFromString("1.25"),
		decimal.RequireFromString("0.0000000000000000000001"),
		decimal.RequireFromString("9007199254740993"),
	}
	ts := time.Date(2026, 8, 17, 4, 0, 0, 0, time.UTC)
	row := BookSnapshotRow{TS: ts, ProductID: "ARRAY-TEST", BidDepth: depth}.row()
	if _, err := pool.Exec(ctx, insertStatement(row), row.values...); err != nil {
		t.Fatalf("insert: %v", err)
	}

	var got []decimal.Decimal
	err := pool.QueryRow(ctx,
		`SELECT bid_depth FROM cb_book_snapshots WHERE product_id = 'ARRAY-TEST' AND ts = $1`,
		ts).Scan(&got)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(got) != len(depth) {
		t.Fatalf("got %d levels, want %d", len(got), len(depth))
	}
	for i := range depth {
		if got[i].String() != depth[i].String() {
			t.Errorf("level %d: got %s, want %s", i, got[i], depth[i])
		}
	}
}

// ---------------------------------------------------------------------------
// Writer
// ---------------------------------------------------------------------------

// The Part 2 acceptance run: ten thousand rows across three tables through one
// writer, all persisted, with the remainder flushed on close.
func TestWriterPersistsTenThousandRows(t *testing.T) {
	pool := testPool(t)
	ctx := t.Context()

	const (
		total     = 10_000
		batchSize = 500
		product   = "WRITER-TEST"
	)

	registry := prometheus.NewRegistry()
	w := NewWriter(pool, WriterOptions{
		BatchSize: batchSize,
		// Long enough that the interval never fires: every flush in this test is
		// either the batch-size trigger or the one on close.
		FlushInterval: time.Hour,
	}, slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelWarn})),
		NewWriterMetrics(registry, "test"))

	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	base := time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC)
	counts := map[string]int{}
	for i := range total {
		ts := base.Add(time.Duration(i) * time.Millisecond)
		var row Row
		switch i % 3 {
		case 0:
			row = VenueStateRow{TS: ts, ProductID: product,
				FuturesMark: Num(decimal.RequireFromString("4000.12345678901234567890"))}
			counts["cb_venue_state"]++
		case 1:
			row = sampleBar(product, ts)
			counts["cb_bars"]++
		default:
			row = BaseStateRow{TS: ts, WalletETH: Num(decimal.RequireFromString("0.4210"))}
			counts["base_state"]++
		}
		if err := w.Submit(ctx, row); err != nil {
			t.Fatalf("submit row %d: %v", i, err)
		}
	}

	// 10,000 is not a multiple of 500 per table, so rows are still pending when
	// the producers stop: closing has to flush them or the counts below miss.
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}

	// base_state has no product id, so the window the writer wrote into is what
	// bounds the count for all three tables.
	until := base.Add(total * time.Millisecond)
	for table, want := range counts {
		var got int
		err := pool.QueryRow(ctx,
			"SELECT count(*) FROM "+table+" WHERE ts >= $1 AND ts <= $2", base, until).Scan(&got)
		if err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if got != want {
			t.Errorf("%s: %d rows persisted, want %d", table, got, want)
		}
	}

	// The metric agrees with the database, which is what makes the Grafana panel
	// trustworthy.
	for table, want := range counts {
		if got := counterValue(t, registry, "test_rows_written_total", table); got != float64(want) {
			t.Errorf("test_rows_written_total{table=%q} = %v, want %d", table, got, want)
		}
	}
}

// Idempotent inserts: replaying a backfill inserts nothing new, which is what
// Part 5 depends on.
func TestIdempotentInsertsAreNoOps(t *testing.T) {
	pool := testPool(t)
	ctx := t.Context()

	ts := time.Date(2026, 8, 19, 0, 0, 0, 0, time.UTC)
	bar := sampleBar("IDEMPOTENT-TEST", ts).row()
	for range 3 {
		if _, err := pool.Exec(ctx, insertStatement(bar), bar.values...); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	var got int
	err := pool.QueryRow(ctx,
		`SELECT count(*) FROM cb_bars WHERE product_id = 'IDEMPOTENT-TEST'`).Scan(&got)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if got != 1 {
		t.Errorf("three inserts of the same candle produced %d rows, want 1", got)
	}
}

// The seed runs on every `make migrate`. It must not overwrite what Part 5 later
// reads from the venue.
func TestSeedDoesNotOverwriteVerifiedProductData(t *testing.T) {
	pool := testPool(t)
	ctx := t.Context()

	const product = "SEED-TEST"
	inserted, err := SeedPerpProduct(ctx, pool, product, decimal.RequireFromString("0.10"))
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if !inserted {
		t.Fatal("the first seed should have inserted a row")
	}

	// Stand in for Part 5 filling the row in from the products endpoint.
	if _, updateErr := pool.Exec(ctx,
		`UPDATE cb_products SET tick = 0.50, status = 'online' WHERE product_id = $1`,
		product); updateErr != nil {
		t.Fatalf("simulate the poller: %v", updateErr)
	}

	inserted, err = SeedPerpProduct(ctx, pool, product, decimal.RequireFromString("0.99"))
	if err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	if inserted {
		t.Error("the second seed should have inserted nothing")
	}

	var size decimal.Decimal
	var tick decimal.NullDecimal
	var status string
	err = pool.QueryRow(ctx,
		`SELECT contract_size, tick, status FROM cb_products WHERE product_id = $1`, product).
		Scan(&size, &tick, &status)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if size.String() != "0.1" {
		t.Errorf("contract size was overwritten: %s", size)
	}
	if !tick.Valid || tick.Decimal.String() != "0.5" {
		t.Errorf("tick was overwritten: %v", tick)
	}
	if status != "online" {
		t.Errorf("status was overwritten: %s", status)
	}
}

// The CHECK constraints are part of the contract: a value outside a closed
// vocabulary is rejected by the database, not stored and discovered later.
func TestClosedVocabulariesAreEnforced(t *testing.T) {
	pool := testPool(t)
	ctx := t.Context()

	ts := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	row := VenueStateRow{TS: ts, ProductID: "CHECK-TEST", FundingSource: "guessed"}.row()
	if _, badSource := pool.Exec(ctx, insertStatement(row), row.values...); badSource == nil {
		t.Error("an unknown funding_source was accepted")
	}

	// A settlement carries no rate: it is an observed cash movement, and a rate
	// on one would break the accrual-versus-settlement reconciliation.
	bad := FundingEventRow{
		TS: ts, ProductID: "CHECK-TEST", Kind: FundingKindSettlement,
		RateHourly: Num(decimal.RequireFromString("0.0001")),
		Amount:     decimal.RequireFromString("-1.23"),
	}.row()
	if _, badRate := pool.Exec(ctx, insertStatement(bad), bad.values...); badRate == nil {
		t.Error("a settlement with an hourly rate was accepted")
	}
}

// A flush is one implicit transaction: pgx sends the whole batch as a single
// pipeline terminated by one sync, so Postgres either applies all of it or none
// of it. The writer's retry policy is built on that — a statement failure the
// server reports means the batch rolled back, which is what makes re-sending it
// safe — so the property is tested rather than assumed.
//
// The giveaway is that the first statement reports success and still leaves no
// row behind: reading a CommandComplete is not the same as having committed.
func TestBatchIsAtomic(t *testing.T) {
	pool := testPool(t)
	ctx := t.Context()

	const product = "ATOMICITY-TEST"
	ts := time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC)

	good := VenueStateRow{TS: ts, ProductID: product}.row()
	// Rejected by cb_venue_state_funding_source_check.
	bad := VenueStateRow{TS: ts.Add(time.Second), ProductID: product, FundingSource: "guessed"}.row()
	alsoGood := VenueStateRow{TS: ts.Add(2 * time.Second), ProductID: product}.row()

	batch := &pgx.Batch{}
	batch.Queue(insertStatement(good), good.values...)
	batch.Queue(insertStatement(bad), bad.values...)
	batch.Queue(insertStatement(alsoGood), alsoGood.values...)

	results := pool.SendBatch(ctx, batch)
	var failedAt int
	var execErr error
	for i := range batch.Len() {
		if _, err := results.Exec(); err != nil {
			failedAt, execErr = i+1, err
			break
		}
	}
	_ = results.Close()

	if failedAt != 2 {
		t.Fatalf("expected the second statement to be rejected, got failure at %d (%v)", failedAt, execErr)
	}

	// The server answered, which is the signal the writer keys its retry policy
	// off. If this ever stops being a PgError, worthRetrying stops working.
	var pgErr *pgconn.PgError
	if !errors.As(execErr, &pgErr) {
		t.Fatalf("expected a *pgconn.PgError from the server, got %T: %v", execErr, execErr)
	}
	if pgErr.Code != "23514" {
		t.Errorf("SQLSTATE = %s, want 23514 (check_violation)", pgErr.Code)
	}
	if worthRetrying(execErr) {
		t.Error("a check violation is deterministic: retrying it only spends the flush timeout")
	}

	var landed int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM cb_venue_state WHERE product_id = $1`, product).Scan(&landed); err != nil {
		t.Fatalf("count: %v", err)
	}
	if landed != 0 {
		t.Fatalf("%d rows survived a failed batch; the flush is not atomic, and the "+
			"writer's retry policy depends on it being so", landed)
	}
}

// Every table takes the same row twice and keeps one. This is the property the
// writer's retry depends on, checked against the real schema rather than against
// the conflict clauses in isolation: a clause naming a key the database does not
// have is a runtime error, not a compile error, and TestEveryRowTypeIsIdempotent
// cannot see that.
//
// It also exercises the conflict-target inference Postgres does, which is fussy
// about partial indexes and about the column list matching the index.
func TestEveryInsertIsIdempotent(t *testing.T) {
	pool := testPool(t)
	ctx := t.Context()

	const (
		product  = "IDEMPOTENCY-TEST"
		position = "01JZZZPOSITIONULID00000000"
	)
	ts := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	num := Num(decimal.RequireFromString("1.25"))

	// positions first: fills and funding events point at it.
	rows := []Row{
		PositionRow{ID: position, OpenedAt: ts, Venue: VenuePaper,
			SpotQty: decimal.RequireFromString("0.7"), PerpContracts: -7, Status: "OPEN"},
		VenueStateRow{TS: ts, ProductID: product, FuturesMark: num},
		sampleBar(product, ts),
		BookSnapshotRow{TS: ts, ProductID: product, BestBid: num},
		TradesAggRow{TS: ts, ProductID: product, BucketSecs: 180,
			BuyVol: decimal.Zero, SellVol: decimal.Zero, TradeCount: 0},
		FeatureRow{TS: ts, ProductID: product, FundingZScore: num},
		BaseStateRow{TS: ts, SpotPx: num},
		AccountStateRow{TS: ts, MarginRatio: num},
		ProductRow{ProductID: product, ContractSize: num, UpdatedAt: ts},
		DecisionRow{TS: ts, State: "HOLD", ReasonCodes: []string{"HOLD_OK"},
			InputSnapshot: Snapshot(`{"funding_rate":"0.0001"}`)},
		FillRow{TS: ts, PositionID: Opt(position), ClOrdID: "01JCLORDID", Venue: VenuePaper,
			Leg: "perp", Side: "sell", Qty: decimal.RequireFromString("7"),
			Px: num.Decimal, ExecState: "FILLED", VenueExecID: "exec-1"},
		FundingEventRow{TS: ts, ProductID: product, Kind: FundingKindAccrual,
			RateHourly: num, FundingSource: FundingSourceComputed,
			Amount: decimal.RequireFromString("0.42")},
		RiskEventRow{TS: ts, Kind: "HARD_STOP_MARGIN_RATIO"},
		FIXSessionRow{SessionID: "FIX.4.4:CARRY->SIMV", StartedAt: ts},
	}

	if len(rows) != len(everyRowType()) {
		t.Fatalf("this test covers %d row types but there are %d", len(rows), len(everyRowType()))
	}

	for _, r := range rows {
		d := r.row()
		t.Run(d.table, func(t *testing.T) {
			stmt := insertStatement(d)

			first, err := pool.Exec(ctx, stmt, d.values...)
			if err != nil {
				t.Fatalf("first insert: %v", err)
			}
			if first.RowsAffected() != 1 {
				t.Fatalf("first insert affected %d rows, want 1", first.RowsAffected())
			}

			second, err := pool.Exec(ctx, stmt, d.values...)
			if err != nil {
				t.Fatalf("second insert (this is what a retry does): %v", err)
			}
			// cb_products upserts rather than doing nothing — rewriting the same
			// values is still idempotent — so it reports one row either way.
			if d.table != "cb_products" && second.RowsAffected() != 0 {
				t.Fatalf("second insert affected %d rows, want 0: a retry would duplicate",
					second.RowsAffected())
			}

			// Counted by the natural key rather than over the whole table: other
			// tests in this suite write to these tables too, and a count(*) here
			// made this test depend on the order it ran in.
			where, args := whereNaturalKey(t, d)
			var n int
			if err := pool.QueryRow(ctx,
				"SELECT count(*) FROM "+d.table+" WHERE "+where, args...).Scan(&n); err != nil {
				t.Fatalf("count by key: %v", err)
			}
			if n != 1 {
				t.Fatalf("%s holds %d rows matching the key it was inserted with, want 1",
					d.table, n)
			}
		})
	}
}

// keyColumns pulls the conflict target out of a row's ON CONFLICT clause.
var keyColumns = regexp.MustCompile(`^ON CONFLICT \(([^)]*)\)`)

// whereNaturalKey builds the predicate that selects exactly the row just
// inserted, from the key the table declares as its identity. IS NOT DISTINCT
// FROM rather than =, because a key column may legitimately be NULL —
// funding_events.position_id on an observed-series row.
func whereNaturalKey(t *testing.T, d rowData) (string, []any) {
	t.Helper()

	m := keyColumns.FindStringSubmatch(d.conflict)
	if m == nil {
		t.Fatalf("%s: cannot read a conflict target out of %q", d.table, d.conflict)
	}

	var (
		clauses []string
		args    []any
	)
	for _, column := range strings.Split(m[1], ",") {
		column = strings.TrimSpace(column)
		i := indexOf(d.columns, column)
		args = append(args, d.values[i])
		clauses = append(clauses, fmt.Sprintf("%s IS NOT DISTINCT FROM $%d", column, len(args)))
	}
	return strings.Join(clauses, " AND "), args
}
