# API Specification — Basis Carry System

**Source of truth for requirements:** [`basis-carry-build-spec.md`](basis-carry-build-spec.md) (v3.1)
**Companion docs:** [Architecture](architecture.md) · [PRD](prd.md) · [Build Plan](build-plan.md) · [Venue Facts](venue-coinbase-perps.md)

The system exposes no public HTTP API in v1. This document therefore specifies every *contract* the system depends on or defines:

1. External APIs consumed (Coinbase Advanced Trade, Base/Alchemy)
2. Internal Go interfaces and message types
3. FIX 4.4 message dictionary and session behavior
4. Database schema (the contract between `ingest`, `carry`, and `research/`)
5. Prometheus metrics and alert rules
6. Configuration surface

> Exact external payload shapes marked **[verify]** must be re-checked against live venue docs during the relevant build part — venue APIs drift, and this doc records intent, not a snapshot of vendor docs.

---

## 1. Coinbase Advanced Trade APIs (consumed)

Base URL `https://api.coinbase.com/api/v3/brokerage/`. Authenticated calls use a CDP API key with JWT bearer tokens (ES256), the same scheme for REST and WS. **Market data is not one of them:** the public REST market endpoints and every WebSocket market-data channel in §1.1 serve the perp product unauthenticated (verified 2026-08-20), so `cmd/ingest` holds no credential. Auth is needed for the account and order-entry surfaces — §1.2's `cfm/*` endpoints (Part 5) and §1.3 (Part 16). Perp product `ETP-20DEC30-CDE`, spot reference product `ETH-USD` — both read from config as `PERP_PRODUCT_ID` / `SPOT_PRODUCT_ID`, never hard-coded.

Venue mechanics (contract size, funding formula, margin model, fee schedule, `TODO(verify)` items) live in **[venue-coinbase-perps.md](venue-coinbase-perps.md)** and are not restated here.

### 1.1 WebSocket channels (`internal/ingest`)

One goroutine **and one connection** per subscribed channel. Verified against the live socket 2026-08-20: every channel below accepts `ETP-20DEC30-CDE`, and **market data needs no authentication** — no JWT is minted or sent by `cmd/ingest`. JWT arrives with the account endpoints (§1.2, Part 5) and order entry (§1.3, Part 16), which do require it.

| Channel | Payload (intent) | Products | Persisted to |
|---|---|---|---|
| `ticker` | price, best bid/ask, 24h stats | perp + spot | `cb_venue_state` (mid, spread) via the sampler, [ADR-0014](decisions/0014-one-sampler-owns-venue-state.md) |
| `level2` | book updates by price level; arrives labelled **`l2_data`** | perp | `cb_book_snapshots` (top-N, periodic snapshot, not every update) |
| `market_trades` | px, sz, side, time | perp + spot | `cb_trades_agg` (bucketed aggregates); feeds 3-min VWAP marks |
| `candles` | OHLCV, **5-minute granularity only** | perp + spot | `cb_bars` with `tf='5m'` (completed candles only) |
| `status` | product status | perp | maintenance-window flag in venue state |
| `heartbeats` | 1/s liveness, no product | every connection | not persisted — see below |
| `user` *(live only)* | order and fill updates | perp | `fills`, `ExecReport` stream for `cbVenue` |
| `futures_balance_summary` *(live only)* | margin fields incl. `available_margin`, `liquidation_threshold` | account | margin ratio for the risk engine |

**A candle is complete when a later one has been *observed*, not when a later one shares its message.** The channel sends a `snapshot` of ~100 candles on subscribe and then `update` messages carrying **exactly one** candle — the one currently forming, re-sent as its OHLCV moves. So on the five-minute roll the update carries only the new candle; the one that just closed is never mentioned again. A client that waits for a newer candle in the same message writes almost no bars, silently (build plan Part 4, found by the soak). Ingest keeps the newest start seen across messages and the latest version of each unwritten candle, which also means a bar is persisted with its final values rather than with whichever snapshot happened to mention it.

**The `candles` channel serves five-minute candles and nothing else.** Subscribing with `granularity: "ONE_MINUTE"`, with `granularity: 60`, and with no granularity at all all returned candles 300 seconds apart. The plan's one-minute bars come from `GET products/{id}/candles`, which does honour the parameter (§1.2, Part 5); `cb_bars` keys on `(product_id, tf, ts)` so the two coexist rather than colliding.

**Every connection also subscribes to `heartbeats`.** It is not persisted and it never advances a stream's `last_seen`. It exists so that every socket carries a frame a second regardless of how quiet its data channel is, which turns two problems into one mechanism: the read deadline becomes a reliable liveness check on every stream rather than only the busy ones, and the per-stream silence check and the time-driven handlers (book snapshot, trade-bucket close) run on the read goroutine without a timer or a mutex.

Client obligations:

- Reconnect with exponential backoff + jitter; **resubscribe every channel on the new connection** — the venue carries no subscription across a socket — and drop every piece of per-connection state, the order book above all.
- Detect gaps per stream: envelope `sequence_num` discontinuity, candle-time discontinuity, trade-time regression between messages, or silence on the stream's own channel beyond a per-channel threshold.
- Treat an error frame (`{"type":"error", …}`) as a dead connection. The venue answers an unknown channel name with `"authentication failure"` and then goes quiet, so a client that waited it out would sit connected and empty.
- Honor rate-limit headers; back off rather than retry-storm.

`sequence_num` counts frames on the **connection**, not on the channel — the subscription acknowledgement and the heartbeats share the counter with the data. With one connection per channel that is exactly the property gap detection wants.

### 1.2 REST poller

Polled on a ticker (default 5s for market/account context; hourly aligned for funding).

| Endpoint | Used for | Persisted to |
|---|---|---|
| `GET products?product_type=FUTURE` (venue `FCM`), `GET products/{id}` | product metadata: contract size, tick, status, `future_product_details` | `cb_products` |
| `GET products/{id}/candles` | candle backfill after gaps and on first run | `cb_bars` |
| `GET products/{id}/product_book`, `best_bid_ask` | book/quote snapshot when WS is degraded | `cb_book_snapshots` |
| `GET products/{id}/market_trades` | trade backfill; VWAP mark inputs | `cb_trades_agg` |
| `GET cfm/balance_summary` | `available_margin`, `liquidation_threshold`, buying power, CBI/CFM balances | `cb_account_state` → margin ratio, treasury |
| `GET cfm/positions`, `cfm/positions/{id}` | `number_of_contracts`, `side`, `avg_entry_price`, `unrealized_pnl` | `cb_account_state`, `positions` reconciliation |
| `GET cfm/intraday/current_margin_window`, `margin_setting` | confirm intraday margin is **off**, read overnight margin | risk config assertion |
| `GET/POST/DELETE cfm/sweeps` | move idle margin back to spot | treasury (Part 18) |
| Fees / transaction summary (`product_type=FUTURE`, `product_venue=FCM`) | live fee tier | `cb_products` fee fields |
| Funding rate, if exposed | official hourly rate | `cb_venue_state.funding_rate_hourly` |

**Do not filter that listing on `contract_expiry_type=PERPETUAL`.** The perp reports `EXPIRING` with a 2030-12-20 expiry (verified 2026-08-20, [venue doc §6](venue-coinbase-perps.md#6-api-surface-retail-advanced-trade)), so the filter the plan originally specified returns nothing. Select the product by `product_id`, which is config, not a filter.

**Funding is a first-class open question, not a field lookup.** The retail API may not publish a funding rate for this product (`TODO(verify)`, venue doc §3). The ingest layer therefore always computes its own estimate and records which source was used:

- `funding_rate_est` — computed hourly from the venue's published formula: 3-minute futures mark (VWAP, falling back to mid TWAP) and spot mark from the spot product, a 1-hour TWAP of `(futures_mark − spot_mark)/spot_mark/24`, then `0.75 × premium + 0.25 × previous`.
- `funding_rate_hourly` — the venue-published rate when available, otherwise a copy of `funding_rate_est`.
- `funding_source` — `"venue"` or `"computed"`.
- Reconciliation: accrued funding is compared against the cash adjustments actually applied to the account (twice daily), exposed as `carry_funding_reconciliation_error`.

No rate is published for an hour the market is closed (Friday 17:00–18:00 ET maintenance); that hour records a gap, not a zero.

### 1.3 Order entry (live path, week 5, gated)

`POST orders` with JWT auth; cancels and edits via the corresponding endpoints; fills arrive on the WS `user` channel. Orders carry `product_id` and size **in whole contracts**. Market and limit are supported for US derivatives; other order types **[verify in Part 16]**.

Constraints honored by `cbVenue`:

- Size is an integer contract count; price rounded to the venue tick. Fractional contracts are never submitted (**[verify]** that the API rejects them).
- JWT minted per request from the CDP key in `.env.private`; client order id = `ClOrdID`.
- Leverage ≤ 3× on overnight margin; intraday margin never opted in.
- No submissions inside the maintenance window.
- Kill switch checked before every submit; when tripped, `Submit` returns an error without network I/O.

## 2. Base / Alchemy APIs (consumed)

### 2.1 Read path (`internal/ingest` Base poller)

JSON-RPC over Alchemy: `eth_getBalance` (wallet ETH), `eth_call` (USDC `balanceOf`), `eth_gasPrice`. Spot ETH/USDC reference price from a DEX quote (aggregator quote endpoint) or Coinbase spot **[decide wk 4, open item #2]**. Persisted to `base_state` every poll (default 30s).

### 2.2 Write path (`baseVenue`, week 5)

ERC-4337 UserOperations via Alchemy bundler RPC (`eth_sendUserOperation`, `eth_getUserOperationReceipt`), signed with a **session key** scoped to the swap route, gas sponsored via Alchemy Gas Manager. Alchemy's SDK is TypeScript, so calls are raw JSON-RPC from Go (spec §2).

**Session-key preflight (fail loudly, never silently).** The key is router-scoped with a capped allowance and an explicit expiry, provisioned by hand from the owner wallet. `baseVenue` refuses to construct if `SESSION_KEY_EXPIRY` is absent or in the past, and every `Submit` re-checks it before building a UserOp — a lapsed key must surface as a startup failure or a rejected order with a clear reason, not as an opaque bundler error mid-carry. `carry_session_key_expiry_timestamp_seconds` is exported so the alert fires days ahead of the lapse.

The only operation in v1: swap USDC↔ETH through a DEX aggregator router from the smart wallet, with slippage bound from config. Receipt → `ExecReport` (FILLED with effective px = amountOut/amountIn, fees = gas if unsponsored + aggregator fee).

---

## 3. Internal Go contracts

### 3.1 `Venue` interface (defined in `internal/carry`, the consumer)

```go
type Venue interface {
    Submit(ctx context.Context, o Order) (Ack, error)
    Cancel(ctx context.Context, id OrderID) error
    ExecReports() <-chan ExecReport
}
```

Implementations: `fixVenue`, `paperVenue`, `cbVenue` (week 5), `baseVenue` (week 5). See [architecture §6](architecture.md#6-execution-architecture).

### 3.2 Core types

```go
type Leg string        // "spot" | "perp"
type Side string       // "buy" | "sell"
type OrdType string    // "limit" | "market"  (market ⇒ IOC limit at protected px)
type TIF string        // "GTC" | "IOC" | "ALO"

type Order struct {
    ClOrdID   OrderID   // client-assigned, unique, ULID
    Asset     string    // "ETH"
    Leg       Leg
    Side      Side
    Type      OrdType
    TIF       TIF
    Qty       decimal.Decimal // spot leg: ETH. perp leg: whole contracts (integral)
    LimitPx   decimal.Decimal
    ReduceOnly bool
}

type Ack struct {
    ClOrdID  OrderID
    VenueID  string    // venue-assigned id, if synchronous
    At       time.Time
}

type ExecState string  // "NEW" | "PARTIAL" | "FILLED" | "CANCELED" | "REJECTED"

type ExecReport struct {
    ClOrdID   OrderID
    VenueID   string
    State     ExecState
    LastQty   decimal.Decimal // this fill
    LastPx    decimal.Decimal
    CumQty    decimal.Decimal
    LeavesQty decimal.Decimal
    AvgPx     decimal.Decimal
    Fee       decimal.Decimal // venue fee or gas, quote currency
    Reason    string          // reject/cancel reason
    At        time.Time
}
```

State machine (enforced in one place, `internal/exec`): `NEW → PARTIAL* → FILLED | CANCELED | REJECTED`. Out-of-order reports are sequenced by `CumQty` monotonicity; duplicates are idempotent.

### 3.3 Decision output

```go
type DecisionState string // "ENTER" | "HOLD" | "REBALANCE" | "EXIT" | "BLOCKED"

type TargetPosition struct {
    Asset          string
    SpotQty        decimal.Decimal // signed target inventory, ETH — continuous leg
    PerpContracts  int64           // signed, short = negative — quantized leg, whole contracts
    ResidualDelta  decimal.Decimal // SpotQty - PerpContracts*contract_size, must be <= tolerance
    State          DecisionState
    ReasonCodes    []string        // e.g. "Z_ENTER_MET", "CARRY_GT_KCOSTS", "FEED_STALE"
    Confidence     float64         // 0..1
    Inputs         FeatureSnapshot // persisted verbatim to decisions.input_snapshot
}
```

The perp leg is an **integer contract count** (contract size 0.10 ETH), sized by flooring toward zero; the spot leg is continuous and absorbs the remainder, so `ResidualDelta` is always under half a contract. Sizing helper:

```go
// ContractsForNotional truncates toward zero — never round up into more risk.
// Use Truncate/IntPart, NOT Floor: a short is a negative contract count, and
// Floor(-7.9) is -8, which is MORE short exposure, not less.
func ContractsForNotional(notionalUSD, mark, contractSize decimal.Decimal) int64
```

Reason codes are a closed enum (append-only), because they are queried in SQL and shown in Grafana. Every emitted `TargetPosition` is persisted before it is acted on.

### 3.4 Pressure engine output

```go
type PressureOut struct {
    CrowdedSide   string  // "LONG" | "SHORT" | "NONE"
    PressureLevel string  // "NORMAL" | "ELEVATED" | "EXTREME" | "FORCED" (z-score bands)
    ExpectedPain  decimal.Decimal // projected funding cost to crowded side over horizon
    Exhaustion    bool    // |imbalance| > threshold AND momentum slope flattening
}
```

### 3.5 Decimal rules

Money is `decimal.Decimal` in Go and `numeric` in SQL, end to end (spec §11). The library has three behaviors that are silent when violated, so each has an enforcement mechanism rather than a convention:

| Rule | Why | Enforced by |
|---|---|---|
| Never `==` or `!=` on a `decimal.Decimal` — use `Equal` | `Decimal` is a struct holding a `*big.Int`, so `==` compiles and compares representation. It is false even for two decimals parsed from the same string, which makes `x == decimal.Zero` a check that can never fire | `internal/guard` (type-aware AST test over the whole module) |
| Never construct a decimal from a float (`NewFromFloat*`) | The only path by which binary rounding error enters. Values come from strings, integers, or the database | `internal/guard` |
| Truncate toward zero when quantizing contracts, never `Floor` | A short is a negative contract count; `Floor(-7.9) = -8` rounds *up* into more risk | table test with negative cases, build plan Part 13 |
| A decimal in a `jsonb` column is a JSON **string**, never a JSON number | Postgres keeps a bare JSON number exact, so nothing inside the system notices; the loss lands on every consumer outside Go — the Python backtester, `jq`, a browser reading a Grafana panel — which parse JSON numbers as IEEE-754 doubles. A decision's recorded inputs then stop matching the decision that was made from them | `decimal.MarshalJSONWithoutQuotes` pinned false in `internal/config`; `internal/db.EncodeSnapshot` refuses to encode if it is ever true; round trip through `jsonb` asserted in the integration suite ([ADR-0011](decisions/0011-decimal-json-encoding.md)) |

`decimal` ↔ `numeric` is exact in both directions, proven by the integration suite with values a `float64` cannot hold (`2^53 + 1` and wider). `internal/db` registers the shopspring codec on every pgx connection, and it is worth being precise about what that buys, because the obvious explanation is wrong: removing the registration leaves every *value* round trip passing, since pgx then falls back to shopspring's own textual `sql.Scanner`/`driver.Valuer` and no float is involved on either path. What the codec preserves is **scale** — unregistered, a numeric stored as `4000.10` returns with exponent −1 instead of −2, so the value is right and the recorded precision is not. Registering it makes `decimal.Decimal` the native representation of `numeric`, and coefficient and exponent both survive.

Scale matters separately from value because `String()` drops trailing zeros: `0.10` stored and read back is still scale 2 and still renders as `0.1`, which is why money-facing output uses `StringFixed(n)`.

`Add`, `Sub` and `Mul` are exact at arbitrary precision. `Div` is not: it rounds to `decimal.DivisionPrecision`, a mutable package global pinned to 16 by `internal/config`'s `init` so division is identical across processes, tests and replays. Both that pin and the JSON one are set to the values shopspring already defaults to — they are locks against a dependency moving a mutable global, not changes in behaviour, and they are asserted through what they control (a ratio rounding to 16 places, a decimal marshalling as a string) rather than by reading the globals back. Division is for **ratios** (margin ratio, basis, premium proxy); a value that must reconcile against a venue statement is added and multiplied, never divided.

For display, `String()` drops trailing zeros (`0.10` renders as `0.1`). Anything money-facing — reports, dashboards, reconciliation output — uses `StringFixed(n)`.

### 3.6 Ingest channel messages

Each stream decodes its channel's frames into the row types `internal/db` defines and hands them to the writer through a one-method `Sink` interface declared in `internal/ingest` (the consumer). `db.Row`'s only method is unexported, so the set of shapes that can reach the database is enumerable by reading one file and a producer cannot invent one — which is why the mapping lives with the row types rather than in a second layer of per-stream structs.

Every frame is decoded into a `Message` carrying the channel, the connection's `sequence_num`, the **venue timestamp** and the **receive timestamp**. Those two are what the earlier draft of this section called for and they are used as it intended: the venue timestamp is what `ingest_last_seen_timestamp_seconds` reports, so the staleness gauge measures the age of the data rather than the age of the socket, and the receive timestamp is what the silence check and the sampling boundaries run on.

Handlers optionally implement `TimeKeeper` (`Tick(ctx, now)`), which the stream calls on **every** frame including heartbeats. Output that is due at a wall-clock boundary rather than on message arrival — a book snapshot every `BOOK_SNAP_SECS`, a trade bucket closing on the minute — runs there, on the stream's own goroutine, so nothing in the package needs a timer beside the read loop or a lock around state the read loop owns.

---

## 4. FIX 4.4 specification

Initiator: `carry` (`internal/fix`). Acceptor: `sim-venue` (later optionally a Coinbase Exchange FIX sandbox for the spot leg — open item #3).

### 4.1 Session

| Setting | Value |
|---|---|
| BeginString | FIX.4.4 |
| HeartBtInt | 30 |
| SenderCompID / TargetCompID | `CARRY` / `SIMV` (config) |
| Sequence store | file-backed (quickfixgo FileStore), survives restart |
| Resend | standard ResendRequest / SequenceReset-GapFill handling |
| ResetOnLogon | **N** — sequence continuity is the point |
| Logs | quickfixgo FileLog, retained for demo/replay |

### 4.2 Messages

**NewOrderSingle (35=D)** — carry → sim-venue

| Tag | Field | Use |
|---|---|---|
| 11 | ClOrdID | ULID from `Order.ClOrdID` |
| 55 | Symbol | perp product id, e.g. `ETP-20DEC30-CDE` (sim symbology mirrors the live product) |
| 54 | Side | 1=Buy, 2=Sell |
| 38 | OrderQty | qty |
| 40 | OrdType | 2=Limit |
| 44 | Price | limit px |
| 59 | TimeInForce | 1=GTC, 3=IOC, 6/post-only per venue convention (sim: custom tag if needed) |
| 60 | TransactTime | UTC |

**ExecutionReport (35=8)** — sim-venue → carry

| Tag | Field | Use |
|---|---|---|
| 37 | OrderID | venue id |
| 11 / 41 | ClOrdID / OrigClOrdID | correlation |
| 17 | ExecID | unique per report |
| 150 | ExecType | 0=New, F=Trade, 4=Canceled, 8=Rejected |
| 39 | OrdStatus | 0=New, 1=Partial, 2=Filled, 4=Canceled, 8=Rejected |
| 31 / 32 | LastPx / LastQty | this fill |
| 14 / 151 | CumQty / LeavesQty | totals |
| 6 | AvgPx | average |
| 58 | Text | reject reason |

**OrderCancelRequest (35=F):** 41 OrigClOrdID, 11 new ClOrdID, 55, 54, 60. Answered by ExecutionReport (4) or OrderCancelReject (35=9, tag 102 reason).

**Reject (35=3):** 45 RefSeqNum, 371 RefTagID, 372 RefMsgType, 373 SessionRejectReason — surfaced as a metric and log, never silently dropped.

### 4.3 sim-venue fill model

Fills against last recorded top-of-book from TimescaleDB with configurable: latency (ms, jittered), slippage (bps), partial-fill threshold (orders larger than X fill in N slices). Config via env. Fill decisions are deterministic under a fixed seed for tests.

---

## 5. Database schema

TimescaleDB; all `ts` columns `timestamptz`, hypertables partitioned on `ts` with a one-day chunk interval. Prices/quantities are `numeric` with no declared precision — arbitrary precision — never floats. Migrations live in `internal/db/migrations`, are embedded in the binaries, and are applied by `make migrate` (which also seeds the perp `cb_products` row); see [ADR-0010](decisions/0010-embedded-migrations.md).

Every column below is nullable unless the table's notes say otherwise: a poll that did not return a field, or a rolling window that has not filled, must read as NULL. A zero would be a value the decision engine acts on.

### 5.1 Hypertables

```sql
cb_venue_state (
  ts, product_id text,
  futures_mark numeric,            -- 3-min VWAP of the future (TWAP mid fallback)
  spot_mark numeric,               -- 3-min VWAP of the spot product
  mid numeric,
  funding_rate_hourly numeric,     -- venue-published if available, else = est
  funding_rate_est numeric,        -- always computed locally, venue doc §3 formula
  funding_source text,             -- 'venue' | 'computed'
  funding_annualized numeric,      -- rate * 24 * 365
  premium_proxy numeric,           -- (futures_mid - spot_mark) / spot_mark
  spread_bps numeric,
  open_interest numeric,
  maintenance_window bool          -- true during Fri 17:00-18:00 ET break
)

cb_bars (
  ts,                              -- bar close time (the venue sends the open; ingest adds the interval)
  product_id text, tf text,        -- '5m' from the WS candles channel, '1m' from REST (Part 5)
  open numeric, high numeric, low numeric, close numeric,
  volume numeric, trade_count int  -- trade_count is NULL on WS bars: the channel does not carry one
)

cb_book_snapshots (                -- perp only; N = 10 levels a side
  ts, product_id text,
  best_bid numeric, best_ask numeric,
  bid_depth numeric[], ask_depth numeric[],   -- top-N sizes, in contracts for the perp
  bid_px numeric[], ask_px numeric[],
  imbalance_top_n numeric,           -- (bid depth - ask depth) / total, over the stored levels
  impact_bid_px numeric, impact_ask_px numeric  -- depth-weighted average of the stored levels
)

cb_trades_agg (
  ts,                              -- bucket end
  product_id text, bucket_secs int,  -- 60 from the WS stream; three buckets make the 3-min VWAP mark
  buy_vol numeric, sell_vol numeric, -- by aggressor side; see the note below
  trade_count int, vwap numeric,     -- vwap NULL when the bucket had no volume
  max_single_sz numeric,           -- sweep intensity input
  sweep_count int                  -- runs of >=2 same-side trades <=250ms apart
)

cb_features (
  ts, product_id text,
  -- Tier 1
  funding_rate_hourly numeric, funding_rate_est numeric,
  funding_source text, funding_annualized numeric,
  funding_zscore numeric,
  cumulative_funding numeric, expected_carry_n_hours numeric,
  futures_mark numeric, spot_mark numeric, basis numeric,
  trade_premium numeric, time_to_next_funding_secs int,
  -- Tier 2
  spread_bps numeric, tob_imbalance numeric, impact_imbalance numeric,
  trade_imbalance numeric, sweep_intensity numeric,
  mid_mark_dist numeric, slippage_est_bps numeric,
  -- Tier 3
  log_ret_1m numeric, ema_fast numeric, ema_slow numeric,
  atr numeric, realized_vol numeric, vwap numeric, momentum_slope numeric,
  -- Risk
  margin_ratio numeric, margin_ratio_at_target numeric,
  effective_leverage numeric,
  net_delta numeric, residual_delta numeric,
  contracts_held bigint, notional numeric,
  -- Pressure
  crowded_side text, pressure_level text,
  expected_pain numeric, exhaustion_flag bool
)

base_state (
  ts,
  spot_px numeric,                 -- ETH/USDC reference
  wallet_eth numeric, wallet_usdc numeric,
  gas_gwei numeric
)

cb_account_state (                 -- polled account/margin snapshot; risk reads margin_ratio from here
  ts,
  available_margin numeric,
  liquidation_threshold numeric,
  margin_ratio numeric,            -- available_margin / liquidation_threshold, as returned/derived
  cfm_usd_balance numeric,         -- futures account
  cbi_usd_balance numeric,         -- spot account
  futures_buying_power numeric,
  contracts_held bigint,           -- signed, from cfm/positions
  avg_entry_price numeric,
  unrealized_pnl numeric,
  intraday_margin_enabled bool     -- asserted false in v1
)
```

**Two definitions that would otherwise be buried in code.** `impact_bid_px` / `impact_ask_px` are the depth-weighted average price of the same top-N levels the row stores, not the price to fill a fixed notional. A fixed notional needs a size constant that means nothing to a reader, and any size this system trades — a whole position is a handful of contracts against a touch holding hundreds — is consumed by the best level alone, so the column would restate `best_bid` and carry no information. Measuring over the stored depth makes the value move when the book's mass moves and makes it reproducible from `bid_px`/`bid_depth` by anyone reading the row. A **sweep** is a run of two or more trades on the same aggressor side within 250 ms of each other: one taker clearing several resting orders. It is deliberately a property of timing and side rather than of size, because size alone cannot separate a large resting fill from an aggressive one.

**A series row is written only when something was observed.** Every producer in Parts 4–6 applies the same rule, because the alternative is worse than a hole: a row carried forward from stale state is indistinguishable from a fresh one, and it is what the basis, the premium and every downstream band get computed from. Concretely — `cb_venue_state` skips the quote half of a row once the ticker is more than three sample intervals stale, `cb_book_snapshots` stops being written once the level2 channel has gone that long without an update (a socket kept alive by heartbeats will otherwise tick forever against a frozen book), and `mid` is a midpoint or NULL, never the last trade price standing in for one. The perp's `maintenance_window` is the exception that proves the rule: it comes from the venue calendar and the `status` channel rather than from the quote, so it is still recorded when the ticker has gone stale — a halt is exactly when the ticker is most likely to have stopped too, and that is the moment the column exists for.

**`buy_vol + sell_vol` is not the bucket's total volume.** The venue publishes a third aggressor side, `UNKNOWN_ORDER_SIDE`, and those trades count toward `trade_count`, `vwap` and `max_single_sz` — their price and size are on the wire — while adding to neither side of the split. Anything deriving a trade imbalance must take the split as the *known* portion, not as the whole. Rejecting such trades outright, which is what ingest did until 2026-08-22, discards real volume and shows up only as a gap counter moving.

**A `cb_trades_agg` row with `trade_count = 0` is an observation, not a placeholder.** A minute in which nothing traded is recorded, so it stays distinguishable from a minute the ingest was not listening — the bucket spanning a reconnect is skipped and shows as a missing row, because a partial aggregate written under a whole bucket's key could never be corrected.

### 5.2 State tables

```sql
cb_products (product_id PK, contract_size numeric,   -- 0.10 ETH for ETP
             tick numeric, tick_value numeric,
             max_leverage_overnight numeric, max_leverage_intraday numeric,
             maker_fee_bps numeric, taker_fee_bps numeric,
             status text, updated_at)

decisions (id PK, ts, state text,
           target_spot numeric,           -- ETH, continuous leg
           target_contracts bigint,       -- whole contracts, quantized leg
           residual_delta numeric,
           reason_codes text[], confidence numeric,
           input_snapshot jsonb)          -- full FeatureSnapshot, reproducibility NFR

positions (id PK, opened_at, closed_at,
           venue text,                    -- 'paper' | 'sim' | 'live'
           spot_qty numeric, perp_contracts bigint,
           avg_spot_px numeric, avg_perp_px numeric,
           accrued_funding numeric,       -- total accrued since open
           settlement_pending_funding numeric,  -- accrued but not yet cash-adjusted
           fees numeric, slippage numeric,
           realized_pnl numeric, status text)

fills (id PK, ts, position_id FK, cl_ord_id text, venue text, leg text,
       side text, qty numeric, px numeric, fee numeric,
       exec_state text, venue_exec_id text, raw jsonb)

funding_events (id PK, ts, product_id text,
                kind text,                -- 'ACCRUAL' (hourly, computed) | 'SETTLEMENT' (cash adjustment observed)
                rate_hourly numeric,      -- ACCRUAL only
                funding_source text,      -- ACCRUAL only: 'venue' | 'computed'
                position_id FK NULL,      -- NULL for observed-only samples
                amount numeric,           -- signed: + = received
                settled_by FK NULL,       -- ACCRUAL -> the SETTLEMENT row that cleared it
                spot_mark numeric, contracts bigint)

risk_events (id PK, ts, kind text,        -- 'HARD_STOP_MARGIN_RATIO', 'FEED_STALE', ...
             detail jsonb, action_taken text, resolved_at)

fix_sessions (id PK, session_id text, started_at, ended_at,
              last_in_seq int, last_out_seq int, disconnects int)
```

Conventions: `positions.venue` separates paper/sim/live P&L buckets; `funding_events` doubles as the observed funding series (position_id NULL) and per-position ledger.

**Both sides of funding are rows.** `kind='ACCRUAL'` rows are what the system computed hourly; `kind='SETTLEMENT'` rows are cash adjustments actually observed on the account, twice daily. An accrual points at the settlement that cleared it via `settled_by` (NULL while pending), `positions.settlement_pending_funding` carries the unsettled sum, and the difference between matched accruals and their settlement is exactly what `carry_funding_reconciliation_error` measures. Storing only one side would make the reconciliation unfalsifiable.

`cb_account_state` is the polled truth behind the risk engine's margin ratio and the treasury's balance reconciliation — the system never derives a liquidation price of its own.

### 5.3 Constraints and indexes

The schema carries the rules that must hold regardless of which process wrote the row. Each one exists because the alternative is a silent wrong answer later.

**Read paths.** `(product_id, ts DESC)` on every product-scoped series table — the venue state cache, the feature engine's warm start and the replay all read "the latest rows for this product". `base_state` needs none beyond the hypertable's own time index; it has no product dimension. `decisions` and `risk_events` are indexed on `(ts DESC)`, `risk_events` again on unresolved rows only, `positions` on open positions only, and `fills` on `(position_id, ts DESC)` and `cl_ord_id`.

**Idempotency — every table, not just the backfilled ones.** Each table carries a unique key that is the identity of one of its rows, and every insert is written `ON CONFLICT … DO NOTHING`. This is what lets the writer re-send a batch whose commit status it could not determine ([ADR-0012](decisions/0012-idempotent-inserts-natural-keys.md)); it also makes the Part 5 backfills and Part 8 FIX resends replayable, which is what the first three keys were originally for.

| Table | Identity | Makes idempotent |
|---|---|---|
| `cb_venue_state` | `(product_id, ts)` | poll re-sends, writer retries |
| `cb_bars` | `(product_id, tf, ts)` | candle backfill (Part 5) |
| `cb_book_snapshots` | `(product_id, ts)` | snapshot re-sends |
| `cb_trades_agg` | `(product_id, bucket_secs, ts)` | trade backfill (Part 5) |
| `cb_features` | `(product_id, ts)` | tick re-computation, warm restart |
| `base_state`, `cb_account_state` | `(ts)` | poll re-sends |
| `cb_products` | `(product_id)` (upsert) | product refresh |
| `decisions` | `(ts)` | decision re-emission |
| `positions` | `(id)` — a client-minted ULID | see below |
| `fills` | `(venue, venue_exec_id)` | FIX resend after reconnect (Part 8) |
| `funding_events` | `(product_id, kind, ts, position_id)`, `NULLS NOT DISTINCT` | funding-history backfill (Part 5) |
| `risk_events` | `(ts, kind)` | event re-emission |
| `fix_sessions` | `(session_id, started_at)` | session re-record |

Three obligations come with that table, and none of them is enforceable by the schema alone:

- **`ts` is the sampling boundary, not `time.Now()`.** For the series tables and `cb_features`, producers align the timestamp to the poll or tick boundary. A nanosecond-resolution clock reading makes every row unique, and the constraint becomes decorative. Parts 4–6 own this.
- **`funding_events.ts` is the start of the funding hour** for an `ACCRUAL` — the rate is a property of the hour (venue doc §3), not of the moment it was computed. `position_id` is in the key because one hour legitimately produces both an observed-series row (`position_id NULL`) and a position-linked accrual; `NULLS NOT DISTINCT` is what makes the observed row collide with its own repeat.
- **`fills.venue_exec_id` is `NOT NULL`.** Every `Venue` implementation supplies one: FIX `ExecID` (tag 17) live and on sim, a minted id on paper.

`positions` is the one table with no natural key — a carry has no property that identifies it — so its identity is assigned rather than discovered: `id` is a client-minted ULID, the same convention as `ClOrdID`. That also removes the need to read a generated id back before `fills` and `funding_events` can reference it, which would have made a producer into a second writer.

**Closed vocabularies** are `CHECK` constraints, so a value outside the set is rejected at write time rather than found in a dashboard: `funding_source ∈ {venue, computed}` (on `cb_venue_state`, `cb_features`, `funding_events`), `decisions.state`, `fills.leg`/`side`/`exec_state`, `positions.venue`, `funding_events.kind`, and `cb_features.crowded_side`/`pressure_level`. `risk_events.kind` is deliberately *not* constrained — it is an append-only vocabulary that grows with the risk engine, and a rejected insert there would lose the record of the event it describes.

**Cross-column invariants.** `funding_events` enforces that a `SETTLEMENT` row carries no `rate_hourly`, no `funding_source` and no `settled_by`: a settlement is an observed cash movement, and a rate on one would make the accrual-versus-settlement reconciliation meaningless.

`decisions.id`, `fills.id`, `funding_events.id`, `risk_events.id` and `fix_sessions.id` are `bigint GENERATED ALWAYS AS IDENTITY` — nothing references them, so nothing needs to know them in advance. `positions.id` is a `text` ULID assigned by the caller, which is what closes the question previously left open against Part 13.

---

## 6. Prometheus metrics

All metrics prefixed per binary (`ingest_`, `carry_`, `simv_`). Labels kept low-cardinality (`product`, `stream`, `venue`, `leg`, `component`).

The four writer metrics (`rows_written_total`, `rows_conflicted_total`, `write_batch_seconds`, `write_queue_depth`) come from shared code in `internal/db` and take the prefix of whichever binary owns that writer, so `carry_rows_written_total` exists alongside the `ingest_` ones listed below. "Is this binary keeping up with its writes?" is a per-binary question. `rows_written_total` and `rows_conflicted_total` are deliberately separate: with `ON CONFLICT` on every insert, a re-sent batch is silent in every other signal, and a single counter adding the two together would report a retry storm as healthy throughput.

One prefix is deliberately outside this catalogue: the Part 3 learning exercise in `research/onramp/` exports `onramp_*` series and nothing scrapes it. It is not a service, it is not in the Compose stack, and its metrics are not alertable — the exception is recorded here so it does not read as drift.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `ingest_last_seen_timestamp_seconds` | gauge | stream | unix ts of last message per stream |
| `ingest_ws_reconnects_total` | counter | stream | reconnect count |
| `ingest_ws_gaps_total` | counter | stream | detected gaps |
| `ingest_rows_written_total` | counter | table | rows the database actually inserted (from the command tag, not rows submitted) |
| `ingest_rows_conflicted_total` | counter | table | rows the database already had: inserts that hit their natural key and did nothing |
| `ingest_write_batch_seconds` | histogram | — | batch insert latency |
| `ingest_write_queue_depth` | gauge | — | rows waiting on the writer's channel; a rising floor is backpressure |
| `carry_funding_rate` | gauge | product, source | current hourly funding; `source` = venue\|computed |
| `carry_funding_rate_est` | gauge | product | locally computed estimate (always present) |
| `carry_funding_reconciliation_error` | gauge | — | accrued estimate minus cash adjustments applied |
| `carry_funding_zscore` | gauge | product | rolling z |
| `carry_basis` | gauge | product | (futures_mark−spot_mark)/spot_mark |
| `carry_pressure_level` | gauge | product | 0=NORMAL 1=ELEVATED 2=EXTREME 3=FORCED |
| `carry_net_delta` | gauge | venue | signed ETH delta |
| `carry_residual_delta` | gauge | venue | delta not expressible in whole contracts |
| `carry_contracts_held` | gauge | venue | signed contract count |
| `carry_notional_usd` | gauge | venue | gross notional |
| `carry_margin_ratio` | gauge | — | available_margin / liquidation_threshold |
| `carry_maintenance_window` | gauge | — | 1 during Fri 17:00–18:00 ET break |
| `carry_accrued_funding_usd` | gauge | venue | funding accrued, open position |
| `carry_settlement_pending_funding_usd` | gauge | venue | accrued but not yet cash-settled |
| `carry_treasury_timeouts_total` | counter | transition | treasury transition exceeded its timeout |
| `carry_account_margin_ratio` | gauge | — | from `cb_account_state`, polled |
| `carry_pnl_usd` | gauge | venue, component=price\|funding\|fees\|slippage | P&L decomposition |
| `carry_decision_state` | gauge | — | enum-coded decision state |
| `carry_order_roundtrip_seconds` | histogram | venue | submit → terminal ExecReport |
| `carry_hard_stops_total` | counter | kind | hard-stop firings |
| `carry_kill_switch_engaged` | gauge | — | 0/1 |
| `carry_session_key_expiry_timestamp_seconds` | gauge | — | unix ts of session-key expiry |
| `fix_session_up` | gauge | session | 0/1 |
| `fix_msgs_total` | counter | session, msg_type, dir | message counts |
| `fix_resend_events_total` | counter | session | sequence recoveries |

### Alert rules (Alertmanager)

| Alert | Condition |
|---|---|
| `FixSessionDown` | `fix_session_up == 0` for 1m |
| `WsGap` | stream gap > 30s |
| `FeedStale` | `time() - ingest_last_seen_timestamp_seconds > 60` |
| `DeltaBreach` | `abs(carry_net_delta) * mark > tolerance_usd` for 2m |
| `HardStop` | `increase(carry_hard_stops_total[5m]) > 0` |
| `MarginRatioLow` | `carry_margin_ratio < floor` (floor from config) |
| `FundingReconciliationDrift` | `abs(carry_funding_reconciliation_error) > tolerance` for 1h |
| `TreasuryTransitionTimeout` | `increase(carry_treasury_timeouts_total[15m]) > 0` |
| `SessionKeyExpiring` | `carry_session_key_expiry_timestamp_seconds - time() < 7d` |

---

## 7. Configuration surface

`.env.example` (committed, synthetic placeholders) → `.env` (local working copy, gitignored, created by `make env`) + `.env.private` (gitignored, real values). Loaded once at startup; the kill switch is the only runtime-mutable flag.

| Variable | Scope | Example (synthetic) |
|---|---|---|
| `POSTGRES_USER`, `POSTGRES_PASSWORD`, `POSTGRES_DB` | deploy | `carry` / `carry` / `carry` — read by Compose and the provisioned Grafana datasource, not by the binaries |
| `DATABASE_URL` | all | postgres://… |
| `CB_API_URL`, `CB_WS_URL` | ingest, carry | `https://api.coinbase.com/api/v3/brokerage/` |
| `ASSET` | all | `ETH` |
| `PERP_PRODUCT_ID` | all | `ETP-20DEC30-CDE` |
| `SPOT_PRODUCT_ID` | all | `ETH-USD` |
| `CONTRACT_SIZE_ETH` | carry, migrate, research | `0.10` — `make migrate` seeds it into `cb_products` until Part 5 reads the real value from the products endpoint |
| `BASE_RPC_URL`, `ALCHEMY_API_KEY` | ingest, carry | — |
| `POLL_REST_SECS`, `POLL_BASE_SECS`, `BOOK_SNAP_SECS` | ingest | 5 / 30 / 10 — `POLL_REST_SECS` is also the `cb_venue_state` sampling boundary, shared by the WS sampler and the Part 5 poller because both must agree on where a boundary is ([ADR-0014](decisions/0014-one-sampler-owns-venue-state.md)) |
| `Z_ENTER`, `Z_EXIT`, `F_FLIP` | carry | 1.5 / 0.5 / 0 *(placeholders — real values private)* |
| `K_COST_MULT` | carry | 2 |
| `CARRY_HORIZON_HOURS` | carry | 168 — the `N` in `expected_carry_N_hours`; sets how long ENTER assumes the carry is held. Costs are a fixed round-trip toll while funding accrues per hour, so this parameter, not the notional cap, decides whether ENTER can ever be true *(see the break-even study, build plan R1)* |
| `BASIS_EXTREME` | carry | 0.007 |
| `DELTA_TOLERANCE_ETH` | carry | 0.05 *(≤ half a contract)* |
| `MAX_NOTIONAL_USD`, `MAX_LEVERAGE` | carry | 500 / 3 |
| `MARGIN_RATIO_FLOOR` | carry | 1.5 *(placeholder — real value private)* |
| `INTRADAY_MARGIN_OPT_IN` | carry | `false` *(must stay false in v1)* |
| `MAINTENANCE_BREAK` | carry, ingest | `Fri 17:00-18:00 America/New_York` |
| `FUNDING_RECON_TOLERANCE_USD` | carry | 0.50 |
| `TREASURY_TRANSFER_TIMEOUT` | carry | `5m` *(CBI→CFM auto-transfer visible)* |
| `TREASURY_SWEEP_TIMEOUT` | carry | `24h` *(scheduled sweep landed)* |
| `TREASURY_SETTLEMENT_TIMEOUT` | carry | `2h` *(past expected funding settlement)* |
| `TREASURY_BASE_TX_TIMEOUT` | carry | `10m` *(Base tx submitted→confirmed)* |
| `DAILY_LOSS_LIMIT_USD` | carry | — |
| `SPREAD_MAX_BPS`, `STALE_FEED_SECS` | carry | 5 / 60 |
| `PRESSURE_WEIGHTS` | carry | `0.5,0.3,0.2` *(placeholder)* |
| `FIX_SENDER`, `FIX_TARGET`, `FIX_HOST`, `FIX_PORT` | carry, sim-venue | — |
| `SIM_LATENCY_MS`, `SIM_SLIPPAGE_BPS`, `SIM_PARTIAL_THRESHOLD` | sim-venue | 20 / 2 / — |
| `CB_API_KEY_NAME`, `CB_API_PRIVATE_KEY` | **private** | CDP key for JWT auth |
| `CB_PORTFOLIO_ID` | **private** | futures portfolio |
| `WALLET_ADDRESS`, `SESSION_KEY`, `GAS_POLICY_ID` | **private** | Base leg |
| `SESSION_KEY_EXPIRY` | **private** | RFC3339; `baseVenue` refuses to start once past |
| `SESSION_KEY_ALLOWANCE_USD`, `SESSION_KEY_ROUTER` | **private** | scope the grant; asserted at preflight |
| `KILL_SWITCH` | carry | `false` |
| `INGEST_METRICS_ADDR`, `CARRY_METRICS_ADDR`, `SIM_METRICS_ADDR` | ingest / carry / sim-venue | `:9101` / `:9102` / `:9103` — one per binary so all three can also run side by side on a host |
| `LOG_LEVEL`, `LOG_FORMAT` | all | `info` / `json` (`text` when run outside a container) |

`DATABASE_URL`, `PERP_PRODUCT_ID` and `SPOT_PRODUCT_ID` are required; everything else has a documented default. `cmd/migrate` is the exception to the shared `Common` block: it reads only `DATABASE_URL`, `PERP_PRODUCT_ID`, `CONTRACT_SIZE_ETH` and the log settings, so a schema migration cannot fail on configuration it never uses. A load reports *every* problem it found at once rather than failing on the first, so a misconfigured deployment learns all of it in one restart.

Three values fail the load rather than being checked at trade time, because a configuration that violates them must never reach a venue:

- `INTRADAY_MARGIN_OPT_IN` true — intraday margin is never opted into in v1 (spec §9, architecture §12).
- `MAX_LEVERAGE` above 3 or non-positive — the overnight cap.
- `DELTA_TOLERANCE_ETH` above half of `CONTRACT_SIZE_ETH` — the spot leg can always close residual delta smaller than half a contract, so a wider tolerance would accept delta the system could have removed.

Credentials are typed as a redacting `Secret` in `internal/config`, reachable only through an explicit `Reveal()` at the point of use. Redaction covers `String()`, `GoString()` (`%#v`), `LogValue()` (slog attributes), `MarshalJSON` and `MarshalText` — the last two because slog resolves `LogValuer` only on the attribute value itself, so a `Secret` nested inside a struct handed to the JSON handler falls through to `encoding/json`, and JSON is the format containers log in. A `Secret` that redacted only under `fmt` and a top-level slog attribute would be redacted on every path except the one production uses; `TestSecretsAreRedacted` asserts all of them.

`DATABASE_URL` is **not** a `Secret` — it is a plain `string` on `Common`, and it carries the database password. Nothing logs it today, and pgx redacts it in its own connection errors, but a `%v` of a whole config struct would print it. Typing it as a `Secret` is a change to the shape every later part consumes, so it is recorded here as an open item rather than made silently.

---

## 8. Research interface (`research/`)

Python reads TimescaleDB directly (`polars`/`duckdb` over SQL). Contract:

- Backtester consumes `cb_bars`, `cb_venue_state`, `cb_book_snapshots`, `funding_events` chronologically; applies funding at hourly boundaries as `contracts × contract_size × mark × funding_rate` with twice-daily settlement, fees, slippage, mark-based risk, and integer-contract quantization.
- An optional notebook may read **public Hyperliquid data for cross-venue funding comparison only** — read-only, no trading, outside the production path and outside `cmd/`, `internal/`, and `deploy/`.
- Outputs a run report (total return, expectancy, max DD, Sharpe, profit factor, funding earned/paid per trade, P&L split, liquidation near-miss count, slippage vs quoted spread) and writes equity/decision series back to a `bt_runs` / `bt_results` table pair for Grafana.
- Acceptance rule (spec §6.10): **a strategy config is not accepted unless profitable after fees, slippage, and funding.**
