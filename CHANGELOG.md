# Changelog

Notable changes to the system and its contracts. Structural decisions get an ADR in [`docs/decisions/`](docs/decisions/README.md); build-part progress is tracked in the [build plan changelog](docs/build-plan.md#changelog--decision-record).

## [Unreleased]

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
