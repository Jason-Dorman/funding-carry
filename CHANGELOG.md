# Changelog

Notable changes to the system and its contracts. Structural decisions get an ADR in [`docs/decisions/`](docs/decisions/README.md); build-part progress is tracked in the [build plan changelog](docs/build-plan.md#changelog--decision-record).

## [Unreleased]

### Writer shutdown correctness — 2026-08-18 (Part 3)

**Behavioral fix to a reliability guarantee.** `internal/db.Writer` handed the root context to pgx for its size- and interval-triggered flushes, so a `SIGTERM` arriving while a batch was in flight aborted the round trip, made `Run` return fatal, and skipped the drain-and-final-flush path entirely — losing rows producers had already been told were accepted. Every flush now runs on `context.WithoutCancel` bounded by `FlushTimeout`.

The general rule is now in [architecture §8](docs/architecture.md#8-reliability-design) and binds every later part: **cancellation stops producers, never I/O already in flight.** Anything the shutdown order depends on — a flush, an order cancel, a FIX logout — gets a detached context with a per-call timeout.

Two things worth noting for anyone auditing the test suite. The bug was found by *running* the Part 3 learning toy under backpressure rather than by reading the code, and the test that should have caught it (`TestWriterFlushesOnContextCancel`) passed for two parts because the fake database ignored the context it was handed. Fakes now fail a dead context the way a driver does, and regression tests are run against the unfixed code to confirm they fail there ([testing strategy](docs/testing-strategy.md)).

### Hardening from an adversarial review of Parts 1-3 — 2026-08-19

A six-lens multi-agent review with a refutation stage on every finding. Contract- and operator-visible outcomes:

- **`Secret` now redacts under `MarshalJSON`, `MarshalText` and `GoString`** as well as `String`/`LogValue`. slog resolves `LogValuer` only on the attribute value itself, so a `Secret` nested in a struct logged through the JSON handler — the committed container default — previously printed in full. Anything that renders config must be assumed to hit that path.
- **Every published Compose port now binds `127.0.0.1`.** If you reached Grafana, Prometheus or TimescaleDB from another machine on your network, that stops working by design; Docker's port rules sit ahead of the host firewall, so the previous defaults were exposed regardless of `ufw`/`firewalld`.
- **`go test -tags integration` with `DATABASE_URL` unset now fails instead of skipping.** It used to print `ok` for a run that executed nothing.
- **[API spec §3.5](docs/api-spec.md#35-decimal-rules) corrected.** The shopspring pgx codec does not prevent a float conversion — the unregistered fallback is textual and exact. It preserves *scale*: without it `4000.10` returns with exponent −1. The old wording overstated the mechanism the "no float money" rule depends on.

**Every insert is now idempotent** ([ADR-0012](docs/decisions/0012-idempotent-inserts-natural-keys.md), migration `000004_natural_keys`). Each of the fourteen tables carries the unique key that is the identity of one of its rows, and every insert uses `ON CONFLICT … DO NOTHING`. Consequences for anything writing to this schema:

- **`positions.id` is now a client-minted ULID (`text`), not a database identity.** `fills.position_id` and `funding_events.position_id` follow it to `text`. Whatever opens a position assigns its id.
- **`fills.venue_exec_id` is `NOT NULL`** — it is the identity of a fill. Every venue implementation supplies one.
- **`ts` must be the sampling or tick boundary**, not `time.Now()`, on the series tables and `cb_features`; and `funding_events.ts` is the start of the funding hour for an accrual. A nanosecond-resolution timestamp makes every row unique and the key decorative.
- **`rows_written_total` counts rows the database actually inserted**, from the command tag; `rows_conflicted_total` is new and counts the no-ops. A dashboard that summed submissions before now reads differently, and deliberately so.

The writer re-sends a batch whose commit status is unknown again — safely, because a repeat is a no-op — so a dropped connection no longer costs a restart and a hole in the self-recorded funding series. This supersedes the paragraph below.

**Previously: the writer no longer re-sends a batch whose commit status is unknown.** It used to retry any failure, including one raised after every statement had been acknowledged — which, if the batch had committed, inserted every row a second time into the nine tables with no unique constraint to absorb it. Re-sending is now limited to failures that prove nothing landed (`pgconn.SafeToRetry`, or a server error with a transient SQLSTATE), and deterministic rejections fail at once instead of burning three attempts. *Operator-visible:* a connection dropped mid-flush is now fatal rather than retried, so expect a restart where the writer previously recovered — the driver cannot distinguish "never sent" from "committed, acknowledgement lost", and a gap is recoverable where a duplicate is not.

Open item: `DATABASE_URL` is a plain `string` on `config.Common` and carries the database password. Recommendation is to type it as `Secret` before Part 4.

### Schema and persistence contracts — 2026-08-17 (Part 2)

The v1 schema landed as embedded migrations, and with it three things other components and other languages now depend on:

- **A decimal inside a `jsonb` column is a JSON string.** [ADR-0011](docs/decisions/0011-decimal-json-encoding.md). This binds the Python backtester and any dashboard reading `decisions.input_snapshot`: read the string and parse it with a decimal type, do not let a JSON parser turn it into a double. Numeric comparison in SQL needs an explicit `::numeric` cast.
- **Constraints and indexes are part of the schema contract**, now written down as [API spec §5.3](docs/api-spec.md#53-constraints-and-indexes): the unique keys that make Part-5 backfills and Part-8 FIX resends idempotent, and `CHECK` constraints for every closed vocabulary. Producers write these tables with `ON CONFLICT … DO NOTHING`.
- **The writer metrics take their binary's prefix** — `carry_rows_written_total` exists alongside `ingest_rows_written_total`, plus a new `*_write_queue_depth` gauge.

Operationally: `make migrate` applies the schema through a one-shot container ([ADR-0010](docs/decisions/0010-embedded-migrations.md)), `make test-integration` runs the schema and writer suites against a real TimescaleDB in a throwaway database, and TimescaleDB is published on host port **15432** instead of 5432 so it cannot collide with a Postgres already on the machine.

### CHANGE-001 — Perp venue migrated from Hyperliquid to Coinbase US perpetual-style futures — 2026-08-16

**Breaking change to venue selection.** Strategy, language, FIX loop, observability, and wallet doctrine are unchanged. Decision and full consequences: [ADR-0009](docs/decisions/0009-perp-venue-coinbase.md). Directive: [CHANGE-001](docs/CHANGE-001-venue-migration.md).

The perpetual leg moves to **Coinbase US perpetual-style futures** (nano Ether, `ETP-20DEC30-CDE`, on Coinbase Derivatives Exchange, cleared by Nodal Clear, accessed retail through Coinbase Financial Markets via the Advanced Trade API). Coinbase is now the only perp venue for market data, paper trading, and live execution. The spot leg is unchanged: long ETH on Base in the Alchemy Smart Wallet resolved from the ENS / .base.eth name.

**Why.** The operator is a U.S. person; Hyperliquid's terms exclude U.S. users and the app is geoblocked, so trading it via API would be a terms violation leaving an on-chain trace beside the named ENS wallet — self-defeating for a wallet built to survive compliance-aware review. Paper trading must also validate the exact venue that goes live, or the week-5 cutover becomes a second integration and the 30-day replay says nothing about the live book. Coinbase is additionally the target employer, and a single perp venue halves the funding-model and adapter surface for a solo build.

Hyperliquid is removed from the production path entirely — nothing in `cmd/`, `internal/`, `deploy/`, or the config schema references it. It remains available only as a **read-only public-data source in `research/`** for an optional cross-venue funding comparison notebook.

**What changed in the contracts:**

- **Venue facts** consolidated into [`docs/venue-coinbase-perps.md`](docs/venue-coinbase-perps.md) as the single source of truth; other docs link rather than restate. Its `TODO(verify)` items close in week 1 against the live API.
- **Contract quantization** — one contract = 0.10 ETH, integer contracts only. `TargetPosition.PerpQty` became `PerpContracts int64` plus `ResidualDelta`; sizing floors toward zero; the continuous Base spot leg trims delta to under half a contract (`DELTA_TOLERANCE_ETH ≤ 0.05`).
- **Local funding-rate estimator** — the retail API may not publish a funding rate for this product, so the system computes one hourly from the venue formula and records `funding_source` (`venue` \| `computed`), with `carry_funding_reconciliation_error` comparing accruals against applied cash adjustments.
- **Funding settlement** — accrues hourly, settles as cash twice daily; added `positions.settlement_pending_funding` and `funding_events.settled_at`.
- **Price references** — `oracle` removed everywhere; `basis = (futures_mark − spot_mark) / spot_mark`.
- **Margin model** — `margin_ratio = available_margin / liquidation_threshold` read from the venue replaces liquidation-price estimation; the liquidation-buffer hard stop becomes a margin-ratio floor. Intraday margin is never opted in; sizing stays ≤ 3× on overnight margin.
- **Maintenance window** — no orders or rebalances Fridays 17:00–18:00 ET; that hour records a funding gap, not a zero.
- **Tables renamed** `hl_*` → `cb_*` (`cb_venue_state`, `cb_bars`, `cb_book_snapshots`, `cb_trades_agg`, `cb_features`), and `hl_assets` → `cb_products`.
- **Adapter renamed** `hlVenue` → `cbVenue`; auth is CDP API key + JWT (ES256) rather than EIP-712 signing.
- **Config** — added `PERP_PRODUCT_ID`, `SPOT_PRODUCT_ID`, `CONTRACT_SIZE_ETH`, `MARGIN_RATIO_FLOOR`, `INTRADAY_MARGIN_OPT_IN`, `MAINTENANCE_BREAK`, `FUNDING_RECON_TOLERANCE_USD`; removed `HL_*`; renamed `DELTA_TOL_ETH` → `DELTA_TOLERANCE_ETH` and `POLL_INFO_SECS` → `POLL_REST_SECS`.
- **Wallet doctrine** — on-chain history now comes from the Base spot leg and wallet operations only; the perp leg is off-chain at a regulated venue by design, and the trade log completes the carry story. Milestone 0a replaces the exchange deposit step with Coinbase futures onboarding.
- **Open items** — the Go-client question is now Advanced Trade (hand-rolled REST+WS with JWT vs community package); the FIX question becomes whether any Coinbase FIX access is reachable for an individual, with `sim-venue` remaining the FIX counterparty otherwise.

**Post-review follow-ups (same session, PO review of the migrated spec):**

- Live-adapter edge labels changed from "v1.5 live" to "week 5 live" across all diagrams and headings, so the diagram no longer implies live is a later phase than the milestone table says.
- **Treasury timeouts redefined.** The old `SPEC.md` timeouts described Base↔exchange bridging and on-chain withdrawal settlement, neither of which exists in this design. Spec §6.8 now defines four transitions with their own parameters: `TREASURY_TRANSFER_TIMEOUT` (CBI→CFM auto-transfer visible), `TREASURY_SWEEP_TIMEOUT` (scheduled sweep landed), `TREASURY_SETTLEMENT_TIMEOUT` (expected funding settlement → observed adjustment), `TREASURY_BASE_TX_TIMEOUT` (Base tx submitted → confirmed). Added `carry_treasury_timeouts_total` and a `TreasuryTransitionTimeout` alert.
- **`cb_account_state` hypertable added** — the polled Coinbase account snapshot (available margin, liquidation threshold, margin ratio, CBI/CFM balances, buying power, contracts held, avg entry, unrealized P&L, intraday-margin flag). §6.1 said this was polled and stored and §6.6 read margin ratio from it, but nothing in the data model held it.
- **`funding_events` now records both sides of funding** — `kind='ACCRUAL'` (computed hourly) and `kind='SETTLEMENT'` (cash adjustment observed twice daily), linked by `settled_by`. Reconciliation needs both sides on record or it cannot be falsified; this replaces the single `settled_at` timestamp.
- **`SPEC.md` dependency removed.** It is superseded, not extended — it described a Rust / offshore-venue / bridge design. Its still-valid content (Alchemy Smart Wallet account model, session keys, Gas Manager, bundler RPC) is folded into spec §4 as a "Base leg (spot)" subsection, and the header now states this file is the only build spec. Note: `SPEC.md` is not in this repo; if a copy exists outside it, delete or deprecate it there.

**Second review round:**

- **Session-key provisioning is now an explicit deliverable, not an aside.** Spec §4 and milestone 5 describe creating it from the owner wallet as its own on-chain operation — router-scoped, allowance-capped, explicit expiry — and Part 17 makes it the first step of the part. `baseVenue` refuses to construct on a missing or past `SESSION_KEY_EXPIRY` and re-checks before every submit, with `carry_session_key_expiry_timestamp_seconds` and a `SessionKeyExpiring` alert 7 days out. Part 17 acceptance includes an expiry drill.
- **Open item 2 now carries its wallet cost.** Choosing Coinbase spot + withdraw for the automated Base leg puts the buy off-chain and leaves only periodic transfers on Base, thinning the wallet column — noted so the wk-4 decision is not made on slippage alone.
- **`CARRY_HORIZON_HOURS` added to the config surface.** The horizon `N` in `expected_carry_N_hours` drives the ENTER rule and appears as a feature column, but was never a configurable parameter — a genuine gap found while checking the break-even arithmetic.
- **Research task R1 added to the build plan** (before Part 13): carry break-even study. Because costs are a one-time round-trip toll and funding accrues hourly, break-even time is size-independent — the lever is the horizon, not the notional cap. R1 also checks whether the EXIT rule fires before break-even.

**Deviation from the directive, needs PO sign-off:** the directive's acceptance item 9 specified `docs/adr/0001-perp-venue.md`. This repo already has an ADR system at `docs/decisions/` with `0001` taken by the Go/Python decision, so the record was written as [`docs/decisions/0009-perp-venue-coinbase.md`](docs/decisions/0009-perp-venue-coinbase.md) to avoid a second ADR directory with a duplicate number. Say the word if you want the literal path instead.
