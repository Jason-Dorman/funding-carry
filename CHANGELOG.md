# Changelog

Notable changes to the system and its contracts. Structural decisions get an ADR in [`docs/decisions/`](docs/decisions/README.md); build-part progress is tracked in the [build plan changelog](docs/build-plan.md#changelog--decision-record).

## [Unreleased]

### Base wallet poller — 2026-09-04 (Part 6, built; live acceptance pending)

`cmd/ingest` gains a fifth producer: a Base L2 poller writing `base_state` every `POLL_BASE_SECS` — wallet ETH, wallet USDC, gas price, and an ETH/USDC reference price. Three things in it are contracts other parts will read against:

- **One row is one block.** The block height is read first and every balance and price is answered at it, so a row is a single moment on the chain rather than a smear across three. A failure to read the height is the only failure that skips the whole row; each of the four columns otherwise degrades to NULL on its own, and a poll that can read *nothing* writes no row at all — a missing row is a visible gap, a row of NULLs is not.
- **`spot_px` comes from the Base pool itself, not from a DEX aggregator quote** ([ADR-0018](docs/decisions/0018-base-spot-px-from-the-pool.md)): `slot0()` on the configured Uniswap v3 WETH/USDC pool, with the in-process Coinbase mid as the fallback. The pool is *verified* against the configured WETH and USDC on first read rather than trusted — a pool holding some other pair answers `slot0` happily, and in the mutation test that induced it the wrong pair produced `400000000.0000000000000001`. Each token's `decimals()` is read too, not assumed at 18 and 6, because bridged USDbC sits beside native USDC on the same chain. Every row records which source it holds in **`spot_px_source`** (`'dex'` | `'coinbase'`, migration `000006`), `CHECK`-paired with `spot_px` in both directions so a price always names its market and a source never floats free.
- **The chain client is hand-rolled JSON-RPC, not `go-ethereum`** ([ADR-0017](docs/decisions/0017-hand-rolled-base-rpc.md), amending spec §3 to v3.3). Four RPC methods, five contract calls, no dynamic ABI types, nothing signed. Method ids are derived by keccak-256 from their signatures rather than pasted, which costs one dependency (`golang.org/x/crypto/sha3`) and turns the famous constants into a test. `BASE_RPC_URL` carries the Alchemy key in its path, so **the URL is the credential**: it is never logged, and the transport-failure path deliberately does not wrap `*url.Error`, which would quote it.

New config: `BASE_USDC_CONTRACT`, `BASE_WETH_CONTRACT`, `BASE_SPOT_POOL` (Base mainnet defaults; chain-scoped, so Sepolia needs different ones). New metrics: `ingest_base_last_block` — this feed's staleness signal, since it has no WebSocket stream and therefore no `last_seen` — and `ingest_base_spot_px_source_total{source}`.

**Accepted live against Base mainnet.** Every column of a row was re-read from a second, independent node at the same block and agrees to the last digit, the price to all sixteen decimal places; the recorded price matched exactly one block in a scanned window of forty, which is the block pinning demonstrated rather than described. Degradation was induced against a refusing endpoint: one warning across ~7 poll intervals, classified non-retryable, 0 restarts, 0 ERROR lines. Run against a public Base address — `WALLET_ADDRESS` remains the operator's to set, and without it (or without `BASE_RPC_URL`) the poller is simply absent and the rest of the stack runs unchanged.

### Boundary sampling, in one place — 2026-09-04 (found by the Part 6 live run)

A row keyed on a *truncated* timestamp must be sampled by **waiting for the boundary**, never by ticking on a period. A `time.Ticker` keeps its period but not its phase against the wall clock, so a late tick truncates onto a boundary already written, is dropped by `ON CONFLICT … DO NOTHING` — indistinguishable from a healthy retry — and takes its own boundary with it.

Part 4 found this in the venue-state sampler after it silently dropped 914 rows over 19 hours and fixed it there. Parts 5 and 6 then both used `time.Ticker` anyway. Measuring a running stack: **`base_state` missing 2 of 20 boundaries, `cb_account_state` 11 of 120**, against 1 of 121 for the sampler that had been fixed. Both are now on the shared `runOnBoundary`, which is the actual remedy — the reasoning had been written down and was still not enough, so it now lives in one function all three callers share. After the fix, over 11 minutes: `base_state` **0 missing of 22**, `cb_account_state` 3 of 128 and `cb_venue_state` 1 of 128 — and one of those three is an instant both missed, so it is a process stall rather than a sampling bug. The account poller's remaining two are a three-call authenticated poll occasionally running past its 5-second interval, which is a boundary that genuinely could not be observed rather than a row computed and dropped.

`cb_account_state` is Part 5's table and the fix is outside Part 6's scope. It was made anyway: the measurement was in hand, and a known defect that survives a phase boundary is one nobody re-finds.

### Coinbase WebSocket ingest — 2026-08-20 (Part 4, in progress)

`cmd/ingest` now reads live market data. Five streams — `ticker`, `level2`, `market_trades`, `candles`, `status` — each on its own connection and goroutine, feeding `cb_venue_state`, `cb_bars`, `cb_book_snapshots` and `cb_trades_agg` through the one writer. Three findings from the live venue change contracts other parts were written against:

- **The WebSocket `candles` channel serves 5-minute candles only.** It ignores a `granularity` argument — verified with `"ONE_MINUTE"`, with `60`, and with none. WS bars are written with `tf='5m'`; the plan's 1-minute bars now come from the REST candles endpoint in Part 5. Anything reading `cb_bars` must filter on `tf` rather than assume one series.
- **Market data needs no authentication.** All five channels serve `ETP-20DEC30-CDE` unauthenticated, so `cmd/ingest` holds no credential and mints no JWT. JWT moves to Part 5 (account endpoints) and Part 16 (order entry).
- **The retail API publishes no funding rate for this product.** `future_product_details.perpetual_details.funding_rate` comes back as `""` with a null `funding_time`. The local estimator in Part 5 is therefore the primary source, not a fallback, and `funding_source` will read `computed`.

Two decisions bind later parts. `cb_venue_state` has **one sampler** that every producer feeds ([ADR-0014](docs/decisions/0014-one-sampler-owns-venue-state.md)) — Part 5's poller adds observations to it rather than writing its own rows, because two producers writing on the same `(product_id, ts)` boundary would lose one to `ON CONFLICT … DO NOTHING` in a way that looks exactly like a healthy retry. And the WebSocket client is [`github.com/coder/websocket`](docs/decisions/0013-websocket-client-coder.md), behind a three-method interface, chosen because its reads take a context and so cancellation stays one mechanism rather than two.

Smaller things that are visible from outside the package: every connection also subscribes to `heartbeats`, which never advance `ingest_last_seen_timestamp_seconds`; a `cb_trades_agg` row with `trade_count = 0` is a real observation of a minute with no trades, while a *missing* row is a bucket that spanned a reconnect; `impact_bid_px`/`impact_ask_px` are the depth-weighted average of the ten stored levels, reproducible from `bid_px`/`bid_depth` on the same row. There is a new `live` build tag for tests that hit the real venue, and captured venue frames are committed as golden fixtures under `internal/ingest/testdata/` (raw, except the level2 snapshot, which is trimmed to the top twenty levels a side).

**Not yet accepted.** The 1-hour soak, the `kill -TERM` clean-flush demonstration and the mid-run network pull all need a running TimescaleDB, and `docker` is unavailable in the development WSL distro.

### The backfill is self-healing, and the funding series now verifies — 2026-08-24

**On every start, ingest checks what history is present and recovers what is missing.** The backfill reads `cb_bars`, downloads only the ranges absent from it, and reconstructs from the two together. A start with nothing missing costs **0.24 s and no HTTP requests** (previously ~70 s and ~370 requests, unconditionally), so it is safe to leave on: a container down for an hour recovers that hour by itself.

**The same change fixed a real fidelity problem.** Computing from the *stored* bars rather than from one download matters because a single download is incomplete — 45,525 perp candles against the 46,655 the store had accumulated. Measured against an independent reimplementation of the venue's formula, agreement went from **931 of 985 hours to 1,000 of 1,002**. It also makes the series reproducible: anyone can recompute it from `cb_bars` and get the same number, which was not previously true of the system's primary asset.

**A latent failure was also found and fixed:** the smoothed rate grew 16 decimal digits every hour (18 at hour 1, 15,810 by hour 980), because `alpha` carried `DivisionPrecision`'s sixteen places into an exact multiplication inside a recursion. PostgreSQL `numeric` stops at 16,383 places, so the series was roughly six weeks of continuous running from an insert the database would refuse — a fatal writer error. Storage for the column fell from 486 kB to 8,865 bytes.

*Operationally:* `BACKFILL_WINDOW` now means "how far back history must reach", not "how much to download each start". `internal/db.Reader` is a new, narrow read path — the first thing besides the writer to touch the database, documented as an exception in [architecture §3](docs/architecture.md#3-concurrency-model-ingest).

### The venue does publish a funding rate — corrected — 2026-08-24 (Part 5 review)

**A verified "fact" in this project was wrong.** The docs recorded that Coinbase publishes no funding rate for `ETP-20DEC30-CDE`, on the strength of `future_product_details.perpetual_details.funding_rate` coming back empty. It publishes one at `future_product_details.funding_rate` — one level up, same field name, `0.000019`/hr (~16.6% annualised), with `funding_time` and `funding_interval` beside it.

*What changes for consumers:* `funding_rate_hourly` now carries the venue's rate with `funding_source='venue'`, and `funding_rate_est` is always the local computation. That split is what the schema was designed for, and their difference is what `carry_funding_reconciliation_error` will measure. The candle backfill is unaffected — the venue publishes only the current rate, so reconstruction is still the only route to history.

The same payload also publishes `overnight_margin_rate` (`{long 0.24525, short 0.33475}`), closing a `TODO(verify)` that had been waiting on an account, plus `index_price` and `settlement_price`.

**Two arithmetic bugs in the estimator, found by the same review.** Every funding hour was averaging the wrong twenty three-minute windows — a sample is stamped with its window's *end*, so the hour filter took one window from the previous hour and dropped the last of its own, shifting the series by three minutes (the regression test showed a 22× premium error from one leaked window). And every live mark dropped the final minute of its own window, because the sampler fired before the last trade bucket had closed. Anything computed from `funding_rate_est` before this date should be recomputed.

### The funding series exists, with 45 days of history — 2026-08-24 (Part 5)

`cmd/ingest` now computes the funding rate the venue does not publish, and reconstructs the history it was not running for.

- **45 days of funding history, backfilled from candles.** `fundingHistory` does not publish for this product, but one-minute candles do — back to the perp's launch — and the estimator's only inputs are a futures mark and a spot mark. One startup wrote 110,294 `cb_bars` rows at `tf='1m'` and **981 funding hours**, averaging +8.80% annualised. The z-score has a warm start on day one instead of day thirty ([ADR-0015](docs/decisions/0015-backfilled-funding-provenance.md)).
- **Reconstructed hours are marked `funding_source='backfilled'`** (migration `000005`). They are the venue's formula on coarser inputs — candle closes rather than trade VWAPs — so **anything consuming the funding series must decide what to do with them**. A z-score fitted across the boundary mixes two measurements.
- **`cb_account_state` is live**, polled from a view-only Ed25519 CDP key, with intraday margin asserted off every poll.
- **`margin_ratio` is NULL, not zero, when there is no position.** The venue reports `liquidation_threshold = 0`, so the ratio is undefined. Anything reading it must treat NULL as "nothing at risk" — read as zero it looks like imminent liquidation on an empty account.
- **`buy_vol + sell_vol`** remains the *known* portion of a bucket's volume, and `funding_events` ACCRUAL rows for the observed series carry `amount = 0` and `position_id` NULL: the rate is the payload, the money column belongs to rows written against a position.
- New config `BACKFILL_WINDOW` (default 45 days; `0` disables).

Open item #1 from spec §12 is closed: the Advanced Trade client is hand-rolled, with the credential isolated in its own package ([ADR-0016](docs/decisions/0016-hand-rolled-coinbase-client.md)).

### JWT signing is EdDSA (Ed25519), not ES256 — 2026-08-23 (Part 5)

**Contract correction, caught before any key existed.** [API spec §1](docs/api-spec.md#1-coinbase-advanced-trade-apis-consumed) specified ES256, and the onboarding checklist had been written to match it. The CDP portal marks ECDSA as being for legacy SDKs and recommends Ed25519; a document of ours does not get to overrule the venue about the venue. Authenticated calls now sign EdDSA over an Ed25519 key.

Two practical consequences. `CB_API_PRIVATE_KEY` is a single base64 string rather than a multi-line PEM, so it needs no newline escaping to survive Compose's `env_file`. And signing needs only `crypto/ed25519` from the standard library, where the ECDSA path would have wanted a JWT dependency.

### Two data-fidelity fixes from 22 hours of continuous running — 2026-08-22

Both were found by auditing the recorded series against what should be there, not by tests, and both were silent.

- **The venue-state sampler was dropping 6.4% of its boundaries** — 914 rows over 19 hours, always exactly one at a time. `time.Ticker` holds its period but not its phase against the wall clock, and the boundary was derived by truncating whatever time a tick arrived at; with the phase sitting near a boundary edge, a millisecond of jitter made a tick land just below the boundary it was meant for, truncate onto the previous one, and be rejected as a duplicate — taking its own boundary with it. The sampler now waits for each boundary rather than for a period. Timers fire at or after their deadline, never before, so the class is gone.
- **Trades with `side: "UNKNOWN_ORDER_SIDE"` were being discarded entirely.** The venue really sends it — 330 trades in 22 hours. Rejecting the trade threw away its price and size along with its side, understating `trade_count`, `vwap` and `max_single_sz`. Such trades now count everywhere except the buy/sell split, which is the only part that is genuinely unknown. **Consequence for consumers: `buy_vol + sell_vol` is the *known* portion of a bucket's volume, not its total** ([API spec §5.1](docs/api-spec.md#51-hypertables)). A side outside the vocabulary is still an error, because that would mean the vocabulary moved.

### Candle persistence corrected; level2 thresholds tuned from measurement — 2026-08-21 (Part 4 soak)

**`cb_bars` was silently losing almost every bar.** The rule for "this candle is complete" looked for a newer candle in the same message. The venue sends a ~100-candle `snapshot` on subscribe and then `update` messages carrying exactly one candle — the forming one — so on each five-minute roll the candle that had just closed was never mentioned again and was never written. In thirty minutes of the soak `cb_bars` gained one row instead of twelve, with nothing in the logs. Completion now means "a strictly newer start has been observed", tracked across messages, and a bar carries the last values it was seen with.

*Anyone who ran the previous build should re-check `cb_bars` for holes.* The bug self-heals on restart — the subscribe snapshot replays roughly eight hours of history, and those bars are written on reconnect — so gaps shorter than that filled themselves in and only longer outages left permanent holes.

**`level2` silence thresholds were mistuned and are now measured.** The perp's book is fresh in 95% of samples but goes quiet for up to 51 seconds while perfectly healthy; the 30-second threshold counted false gaps and, through the book-staleness bound, removed real `cb_book_snapshots` rows during those lulls. Both now derive from one constant (`Level2Quiet = 2m`), which produced zero gaps across 44 minutes of open market.

### Writer refuses rows once shutdown begins — 2026-08-20 (found in Part 4's review)

**Behavioral fix to accepted Part 2 code, in the same area as the Part 3 fix below.** `internal/db.Writer` closed its `stop` channel only *after* shutdown had finished draining and flushing. For the whole width of that final round trip `Submit` still returned `nil` — and every row accepted there landed in a channel whose only reader had already passed the drain. The producer was told the row was safe; nothing was logged, no metric moved, and the row was gone. `stop` now closes before the drain, so `Submit`'s documented contract ("returns `ErrWriterStopped` once the writer has begun shutting down") is true of the implementation.

*Operator-visible:* a producer still running during shutdown now gets `ErrWriterStopped` where it previously got `nil`. That is the point — a visible refusal instead of a silent loss.

The companion rule is in [architecture §8](docs/architecture.md#8-reliability-design) and binds every binary: **a writer must not be given the root context.** Its drain is only correct once producers have stopped, so it runs on `context.WithoutCancel` and is stopped by `Close` after the last producer returns. And a writer that dies fatally must cancel its producers, because not every producer submits — in `cmd/ingest` two of the five streams never call `Submit`, and without that cancellation a fatal write left the process alive and hung with a dead database, still answering `/healthz` with 200.

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
