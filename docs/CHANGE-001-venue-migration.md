# CHANGE-001: Venue migration — Hyperliquid → Coinbase US Perpetual-Style Futures

**Applies to:** every document derived from `basis-carry-build-spec.md` (the spec itself, architecture docs, milestone plans, data model docs, config schemas, READMEs, ADRs, task lists, anything in `docs/`) plus any scaffolded code, config, or Compose files that reference the perp venue.
**Type:** breaking change to venue selection. Strategy, language, FIX loop, observability, and wallet doctrine are unchanged.
**How to apply:** read this whole file first, then update every affected document so it reads as if it had always been written this way. Do not leave "previously Hyperliquid" notes inline; put migration history only in the changelog. After editing, run the acceptance checklist at the bottom and report every document touched.

---

## 1. The decision

The perpetual leg moves from **Hyperliquid** to **Coinbase US Perpetual-Style Futures** (traded on Coinbase Derivatives Exchange, accessed by retail via Coinbase Financial Markets through the Coinbase Advanced Trade API). Coinbase becomes the *only* perp venue for market data, paper trading, and live execution.

Hyperliquid is removed from the production path entirely. It survives only as an optional public-data source inside `research/` for cross-venue funding comparison notebooks. Nothing in `cmd/`, `internal/`, `deploy/`, or the config schema may reference Hyperliquid after this change.

The spot leg is unchanged: long ETH held on Base in the Alchemy Smart Wallet resolved from the ENS / .base.eth name, bought via a Base DEX (manual first, then via `baseVenue`).

## 2. Why (record this in the ADR, not in every doc)

1. The operator is a U.S. person. Hyperliquid's terms exclude U.S. users and the app is geoblocked; trading it from the U.S. via API is a terms violation and leaves an on-chain trace next to the named ENS wallet that a compliance-aware reviewer at a regulated exchange would notice. That contradicts the project's purpose.
2. Paper trading must validate the exact venue that goes live. Paper on Hyperliquid + live on Coinbase would make the week-5 cutover a second integration and make the 30-day replay meaningless for the live book.
3. Coinbase is the target employer. Building against its own perp product, including contract sizing and funding-settlement quirks, is a stronger interview artifact than a crypto-native venue.
4. Single perp venue halves the funding-model, basis-definition, and adapter surface for a solo build.
5. The `Venue` interface already isolates venue specifics; if Hyperliquid becomes available to U.S. users, it is one adapter later, not a rewrite.

## 3. Global find-and-replace guidance

Do these mechanically first, then fix the semantic items in §4.

| Old | New | Notes |
|---|---|---|
| Hyperliquid, HL, `hl_` (as the perp venue) | Coinbase US Perpetual-Style Futures, CB perps, `cb_` | Tables `hl_venue_state`, `hl_bars`, `hl_book_snapshots`, `hl_trades_agg`, `hl_features`, `hl_assets` → `cb_*`. Rename `hlVenue` → `cbVenue`. |
| "Hyperliquid WS", "Hyperliquid Info API", "Hyperliquid exchange endpoint" | "Coinbase Advanced Trade WebSocket", "Coinbase Advanced Trade REST (futures/perps endpoints)", same for order entry | Public market-data channels for data; authenticated REST for orders and account state. |
| "ETH-PERP" (HL symbol) | `ETP-20DEC30-CDE` | Store in config as `perp_product_id`; never hard-code. Spot reference product is `ETH-USD`. |
| "hourly funding from an 8h premium sampled every 5s, 4%/hr cap" | Coinbase's actual funding computation and settlement schedule | See §4.1. Pull from official docs; cite the URL in the venue-facts doc. |
| "mark vs oracle" | "futures mark vs spot mark" | Coinbase's funding formula uses a futures mark and a spot mark (see venue doc §3). Basis = (futures_mark − spot_mark)/spot_mark. Feature/column names: `futures_mark`, `spot_mark`, `basis`. Update every formula, feature name, dashboard label, and metric that says oracle or index. |
| "any size above minimum" | fixed contract sizes (e.g. nano contracts) | See §4.2. |
| "USDC deposited to Hyperliquid from the named address" (wallet doctrine, milestone 0a) | "USDC/USD funded into the Coinbase perp portfolio" | This leg is off-chain either way; the on-chain history comes from the Base spot leg and wallet operations. Say so explicitly. |
| "Hyperliquid Go client: community SDK vs hand-rolled wrapper" (open item 1) | "Coinbase Advanced Trade Go client: official/community SDK vs hand-rolled REST+WS wrapper with JWT auth" | Decide wk 1 after reading the auth code. |
| Open item 3 (point FIX at Coinbase Exchange sandbox "to make FIX real") | Reword: Coinbase Derivatives Exchange offers FIX/SBE/UDP to institutional participants via FCMs; investigate at wk 7 whether any sandbox is reachable for an individual. Otherwise sim-venue remains the FIX counterparty. | Keep FIX loop as-is. |

Do **not** replace Hyperliquid inside `research/` docs that describe the optional cross-venue comparison notebook; that is the one legitimate remaining mention. Add a one-line note there that Hyperliquid data is public/read-only and no trading is done on it.

## 4. Semantic changes by area

### 4.1 Venue facts (spec §4 and any "venue mechanics" doc)
**The venue facts have already been researched and are provided in `docs/venue-coinbase-perps.md` (shipped alongside this directive). Copy that file into `docs/` unchanged. Do not re-research; do not invent values for its `TODO(verify)` items — leave them as-is for the operator to fill in during week 1.** Rewrite spec §4 as a short summary that links to that file. For reference, the topics it covers:
- Product structure: long-dated futures with perp-like funding; expiration horizon; 24/7 trading.
- Contract specification for the ETH product: contract size (nano = fraction of one ETH), tick size, minimum order, product ID.
- Funding: how the rate is computed, settlement/accrual frequency, how it is credited/debited, where it is exposed in the API (REST field, WS channel).
- Margin and leverage: max leverage, initial/maintenance margin, liquidation mechanics, whether the mark or last price drives margin.
- Price references: mark price, index price, last trade; which one drives margin and unrealized PnL.
- Fees: maker/taker schedule for non-professional users.
- API surface: REST endpoints for products, candles, ticker, order book, funding, positions, orders; WS channels available; auth scheme (API key + JWT); rate limits.
- Eligibility/onboarding: perps onboarding step in Advanced Trade UI, USDC/USD margin funding, min notional per order.
- Tax note: futures on a CFTC-regulated DCM may receive Section 1256 treatment; flag as a note, not advice.

`docs/venue-coinbase-perps.md` is the single source of truth for these facts. Other docs link to it instead of repeating numbers.

### 4.2 Contract-size handling (risk engine, decision engine, data model)
Contract = 0.10 ETH, integer contracts only (venue doc §2). Because perp size is quantized:
- `TargetPosition.perp_qty` becomes an integer number of contracts; add `contract_size` to venue state and a helper that converts desired notional → contracts (floor toward zero).
- Net delta will carry a residual; add `delta_tolerance` in config expressed in units of ETH and enforce that residual ≤ half a contract before REBALANCE fires. Document that spot on Base is the fine-grained leg used to trim delta.
- Update §8 position-size row and any pseudocode.
- Risk engine reads `margin_ratio = available_margin / liquidation_threshold` from the balance-summary endpoint / `futures_balance_summary` WS channel instead of computing a liquidation price; hard-stop floor is expressed as a minimum margin ratio (config). Keep intraday leverage opted out; size ≤ 3× on overnight margin.
- Add a trading-hours guard: no new orders and no rebalances during the Friday 5–6 pm ET maintenance window; treat that hour as "no funding published."

### 4.3 Funding math (feature engine, pressure engine, formulas doc, config schema)
- Funding interval is **1 hour** (same as before), so `funding_zscore`, `cumulative_funding`, `expected_carry_N_hours` keep their hourly semantics. Add `funding_annualized = rate × 24 × 365`.
- Add a **local funding-rate estimator** implementing the venue doc §3 formula: 3-min futures mark (VWAP, fallback mid TWAP) and spot mark from `ETH-USD`, 1-hour TWAP of premium ÷ 24, then `0.75 × premium + 0.25 × prev`. Store as `funding_rate_est` every hour; store the venue-published rate as `funding_rate_hourly` if the API exposes one, else set `funding_rate_hourly = funding_rate_est` and flag `funding_source = "computed"`. Reconcile against funding cash adjustments seen on the account (twice daily) and emit a `funding_reconciliation_error` metric.
- `basis = (futures_mark − spot_mark)/spot_mark`. `trade_premium` compares futures mid vs spot mark. Remove `oracle` everywhere.
- Add `settlement_pending_funding` (accrued since last midday/EOD cash adjustment) to position state and the P&L panel, since funding is credited twice daily, not hourly.
- Config schema: thresholds are already hourly; add `perp_product_id`, `spot_product_id`, `contract_size_eth = 0.10`, `delta_tolerance_eth ≤ 0.05`, `intraday_margin_opt_in = false`, `maintenance_break = Fri 17:00–18:00 ET`. Update `.env.private` template and public placeholders.

### 4.4 Ingestion (spec §6.1, ingest docs, `internal/ingest/` scaffolding)
- One goroutine per Coinbase WS channel actually needed (ticker/mark, level2 top-of-book, trades, and whatever channel carries funding/mark for perps), one poller for REST-only data (funding history, product metadata, account/positions).
- Backfill script pulls historical funding and candles from Coinbase REST on first run. Note whether history depth is limited; self-recorded data remains primary history.
- Base poller unchanged.
- Remove any mention of Hyperliquid's WS subscription names.

### 4.5 Execution (spec §6.7, `internal/exec/`, `Venue` implementations)
- Paper engine simulates Coinbase perp fills: contract-quantized, Coinbase fee schedule, funding applied at Coinbase's settlement schedule, mark-based unrealized PnL.
- Live adapter is `cbVenue`: Coinbase Advanced Trade authenticated REST for orders/cancels, WS user channel for fills. Kill switch unchanged.
- FIX initiator + `sim-venue` unchanged. Sim-venue fill model should now be described as "fills against Coinbase top-of-book" for the perp leg.

### 4.6 Wallet doctrine and milestones (spec §1 and §10, milestone docs, task lists)
- Milestone 0a: replace the Hyperliquid deposit step with: complete Coinbase perps onboarding in Advanced Trade, fund the perps portfolio with a small USDC balance, set leverage ≤ 3× (or the product's cap if lower). Keep the ENS / .base.eth / Base wallet funding steps unchanged.
- Milestone 0b and all "manual carry" rows: the perp short is opened on Coinbase US perps by hand from the same Coinbase account that will hold API keys later; the spot leg is unchanged. Log contract count, mark, index, funding accrued.
- Wallet column: state plainly that on-chain history comes from the Base spot leg and wallet ops only; the perp leg is off-chain at a regulated venue by design.
- Interview surface (§11): replace "Hyperliquid mechanics" bullet with "Coinbase US perp-style futures mechanics: contract sizing, funding settlement, margin, and how they differ from offshore perps"; add "cross-venue funding comparison (research)" if the notebook exists.

### 4.7 Research layer (`research/`, backtester docs)
- Backtester replays Coinbase data. Add optional Hyperliquid public-data ingestion *only* in a notebook for cross-venue funding comparison; mark clearly as read-only and out of the production path.

### 4.8 Architecture diagram
Update the Mermaid diagram: `HLWS`/`HLINFO` → `CBWS`/`CBREST`, `HLX` → `CBLIVE`, labels accordingly. Add a dashed `research` box that may read Hyperliquid public data.

## 5. Things that must NOT change
- Go for services, Python for research. Rust remains deferred.
- Carry as core, funding-pressure engine as brain, directional overlay as v2.
- FIX 4.4 initiator + quickfixgo `sim-venue`, sequence persistence, ExecutionReport state machine.
- One-writer goroutine pattern, `Venue` interface defined at the consumer.
- TimescaleDB, Prometheus, Grafana, Alertmanager, Compose layout, `make up/replay/test`.
- Public/private split; real thresholds only in `.env.private`.
- Live adapters at week 5, gated on week 4 hard stops. Wallet-history-first ordering rule.
- ENS / .base.eth / Alchemy Smart Wallet / Base spot leg.

## 6. Acceptance checklist (report results)
1. `grep -ri hyperliquid` across the repo returns hits only under `research/` and in the changelog/ADR.
2. `grep -ri oracle` and `grep -ri "index price"` return no hits in production docs or code (futures mark / spot mark replaced them).
3. No table, metric, struct, or file is still prefixed `hl_`/`hl`.
4. `docs/venue-coinbase-perps.md` is present verbatim from the provided file, and spec §4 links to it rather than restating numbers.
5. Contract-size quantization appears in the risk engine doc, decision engine doc, data model, and §8 formulas.
6. Local funding-rate estimator, `funding_source` flag, `settlement_pending_funding`, margin-ratio hard stop, and Friday maintenance guard all appear in the relevant docs and config schema.
7. Milestone 0a/0b and the wallet column read correctly for Coinbase perps + Base spot.
8. Architecture diagram updated.
9. `CHANGELOG.md` has an entry for CHANGE-001 summarizing §1–§2, and an ADR file (`docs/adr/0001-perp-venue.md`) records the decision and alternatives considered (Hyperliquid, dual-venue).
10. List every file modified.
