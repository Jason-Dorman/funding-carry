# ADR-0010: Migrations are embedded in the binaries and applied by a dedicated `cmd/migrate`

**Date:** 2026-08-17 · **Status:** accepted · **Source:** build plan Part 2

## Context

Part 2 asks for `internal/db/migrations/*.sql` applied by `make migrate`, using "golang-migrate or equivalent". Three things had to be decided around that: which tool, where the SQL lives at runtime, and which process runs it.

The schema is the contract between `ingest`, `carry` and `research` (ADR-0002, ADR-0003), so a deployment running code that expects a column the database does not have is a correctness failure, not an inconvenience. The stack is Compose, and the service images are `alpine` + one static binary — nothing else is installed.

## Decision

**golang-migrate as a library**, with its `pgx/v5` driver and an `iofs` source over a `//go:embed` of `internal/db/migrations`. The SQL is compiled into every binary that imports `internal/db`.

**A fourth binary, `cmd/migrate`**, applies them and then seeds the `cb_products` row for the configured perp product. `make migrate` runs it through Compose as a one-shot service kept out of `make up` by a profile. It exits non-zero if the schema cannot be brought up to date.

The seed writes only what configuration already knows — product id and contract size — and leaves tick, tick value, leverage caps and fee tiers NULL with `status = 'UNVERIFIED'`, using `ON CONFLICT DO NOTHING` so a re-run never overwrites what Part 5 later reads from the venue.

## Alternatives considered

- **A hand-rolled runner** (~60 lines: sorted files, one transaction each, a `schema_migrations` table). Fewer dependencies, but it re-implements versioning, dirty-state detection and locking that a standard tool already has, and "we wrote our own migration tool" is a worse answer than "we used the standard one".
- **The golang-migrate CLI in the image or a separate migration container image.** Adds a binary and a copy of the SQL to keep in step with the code. Embedding means the schema a binary was built against travels with it.
- **Migrating on service startup.** Three services racing to migrate, and a schema change becoming a side effect of a restart. `make migrate` should be a decision someone makes.
- **`golang-migrate`'s `postgres` driver** rather than `pgx/v5`. It is `lib/pq`-based and would put a second Postgres driver in the module.

## Consequences

`make migrate` needs no Go toolchain, no migration CLI, and no files on disk — only `DATABASE_URL`. The integration suite calls the same `db.Migrate` the command does, so the migrations are exercised on every test run rather than only at deploy time.

The cost is a URL rewrite: golang-migrate resolves its driver by scheme, and its pgx v5 driver registers as `pgx5`, so `db.Migrate` swaps the scheme of the otherwise-normal `postgres://` connection string. That is the one place in the system where `DATABASE_URL` is rewritten, and it is a single documented function.
