# Architecture — Basis Carry System

**Source of truth for requirements:** [`basis-carry-build-spec.md`](basis-carry-build-spec.md) (v3.1)
**Companion docs:** [PRD](prd.md) · [API Spec](api-spec.md) · [Build Plan](build-plan.md) · [Venue Facts](venue-coinbase-perps.md)

This document describes *how* the system is built: processes, concurrency, data flow, state machines, storage, deployment, and reliability design. Requirements and rationale live in the PRD; wire-level and schema-level contracts live in the API spec.

---

## 1. System context

The system runs a delta-neutral funding carry: **long spot ETH on Base, short the ETH perpetual-style future on Coinbase**, collecting funding while it is positive. A funding-pressure engine times entries, exits, and hold-offs. Everything the system observes is self-recorded, because retail history depth is limited and the venue publishes the current funding rate but no history of it — **our own database is the primary history**, and what can be recovered is recovered by the [backfill pattern](#71-historical-recovery--the-backfill-pattern) rather than waited for. Venue mechanics live in [venue-coinbase-perps.md](venue-coinbase-perps.md); this doc does not restate them.

```mermaid
graph TD
    subgraph External
        CB["Coinbase Advanced Trade<br/>WS + REST, market data and orders"]
        BASE["Base L2<br/>JSON-RPC via Alchemy"]
        WALLET["Named wallet<br/>ENS + .base.eth resolve to one address"]
    end

    subgraph "This system"
        SYS["ingest, carry, sim-venue<br/>TimescaleDB, Prometheus, Grafana"]
    end

    OPERATOR["Operator - Jason"]
    REVIEWER["Reviewer / recruiter<br/>audits wallet + public repo"]

    CB <--> SYS
    BASE <--> SYS
    SYS -.->|"live path signs as"| WALLET
    OPERATOR -->|"config, kill switch, manual carries"| SYS
    OPERATOR -->|"manual trades wk 0b-4"| WALLET
    REVIEWER -->|"looks up ENS name"| WALLET
```

Two consumers matter and they see different surfaces:

- The **operator** sees Grafana, alerts, and the decision log.
- A **reviewer** sees the public repo and the on-chain history of the named wallet. The wallet is a deliverable, not a side effect (spec §1).

---

## 2. Process model

Three Go binaries plus infrastructure containers, one `go.mod`, orchestrated by Docker Compose.

| Binary | Role | Talks to |
|---|---|---|
| `cmd/ingest` | Market data in → TimescaleDB | Coinbase Advanced Trade WS + REST, Base RPC, TimescaleDB, Prometheus |
| `cmd/carry` | Features → pressure → decision → risk → execution | TimescaleDB, sim-venue (FIX), paper engine (in-proc), live venues (week 5), Prometheus |
| `cmd/sim-venue` | FIX 4.4 acceptor simulating an exchange | `carry` (FIX), TimescaleDB, Prometheus |

```mermaid
graph TD
    subgraph "cmd/ingest"
        ING["stream goroutines to writer"]
    end
    subgraph "cmd/carry"
        FE[feature engine] --> FPE[pressure engine] --> DEC[decision engine] --> RISK[risk engine] --> EXEC[execution router]
    end
    subgraph "cmd/sim-venue"
        SIM[quickfixgo acceptor<br/>fill model]
    end

    CBWS["Coinbase Advanced Trade WS"] --> ING
    CBREST["Coinbase Advanced Trade REST"] --> ING
    BASERPC[Base RPC / Alchemy] --> ING
    ING --> TS[(TimescaleDB)]
    TS --> FE
    EXEC -->|FIX 4.4 initiator| SIM
    EXEC -->|in-process| PAPER[paper engine]
    EXEC -.->|week 5 live| CBLIVE["Coinbase Advanced Trade<br/>authenticated REST"]
    EXEC -.->|week 5 live| ALCH[Alchemy Smart Wallet<br/>ERC-4337 UserOps]
    SIM --> TS
    PAPER --> TS
    RISK --> TS
    DEC --> TS
    TS --> BT[backtester / replay<br/>Python]
    ING --> PROM[Prometheus]
    EXEC --> PROM
    RISK --> PROM
    PROM --> GRAF[Grafana]
    PROM --> AM[Alertmanager]
```

**Why `carry` reads from TimescaleDB rather than subscribing to feeds directly:** it gives live trading and replay the *same read path*. The backtester replays DB rows chronologically; the live engine polls the same tables for the latest rows. One code path to reason about, and every decision's inputs are already persisted (reproducibility NFR, spec §6.5).

### Repository layout

```
cmd/ingest/          cmd/carry/          cmd/sim-venue/
internal/ingest/     internal/venue/     internal/features/
internal/pressure/   internal/carry/     internal/risk/
internal/exec/       internal/fix/       internal/metrics/
internal/treasury/   internal/db/        internal/config/
research/            deploy/             docs/
```

`internal/db` is the shared persistence layer: the pgx pool (with the shopspring codec registered so `numeric` is a `decimal` on the wire), the embedded SQL migrations, the typed row structs that enumerate every table the system writes to, and the batch writer. `cmd/migrate` is a fourth, one-shot binary that applies those migrations and seeds the product row; it is not a service and is not part of `make up`. `internal/config` loads each binary's typed configuration from the environment ([API spec §7](api-spec.md#7-configuration-surface)) and is the one place that knows how a credential is redacted; `internal/metrics` owns each binary's Prometheus registry and its `/metrics` and `/healthz` endpoints. Everything else matches spec §6.11.

---

## 3. Concurrency model (`ingest`)

The one-writer pattern is the load-bearing idea: many readers, typed channels, exactly one goroutine that touches the database per binary.

```mermaid
graph LR
    subgraph "stream goroutines (one WS connection each)"
        G1["ticker<br/>perp + spot"]
        G2["level2 -> l2_data<br/>perp"]
        G3["market_trades<br/>perp + spot"]
        G4["candles 5m<br/>perp + spot"]
        G5["status<br/>perp"]
    end
    subgraph "poller goroutines"
        G6["REST<br/>funding, marks, positions, margin, meta"]
        G7["Base<br/>balances, spot px, gas"]
    end
    G1 --> VS["venue-state sampler<br/>one writer of cb_venue_state"]
    G5 --> VS
    G6 --> VS
    VS --> CH["writer channel<br/>bounded, typed rows"]
    G2 --> CH
    G3 --> CH
    G4 --> CH
    G6 --> CH
    G7 --> CH
    CH --> W[writer goroutine<br/>batch INSERT via pgx]
    W --> DB[(TimescaleDB)]
    G1 --> M[Prometheus:<br/>last_seen per stream<br/>reconnect + gap counters]
    G2 --> M
    G3 --> M
    G4 --> M
    G5 --> M
    G6 --> M
```

Rules, enforced in review:

1. **One writer per binary.** Stream goroutines never call the DB.

   **Reading is a narrow, named exception.** The rule exists to keep *writes* serialized through one goroutine, so insert order, batching and failure handling have exactly one home; a read is none of those. `internal/db.Reader` exists for one caller — the backfill, which has to know what it already holds before deciding what to download, and would otherwise re-fetch forty-five days to discover that forty-five days are present. It also buys something less obvious: a reconstruction computed from *stored* bars is a function of the database, so anyone can recompute it and get the same answer, where one computed from a particular download is a function of what that call happened to return. For the series the whole signal rests on, reproducibility is worth more than the simplicity of not reading. They publish typed structs to channels; the writer batches inserts (interval- and size-triggered flush) and sends each flush as a single pgx batch, which Postgres runs in one implicit transaction — so a flush lands whole or not at all, in one round trip (demonstrated in `TestBatchIsAtomic`, not assumed). The channel is bounded: when it fills, producers block, which is the backpressure that keeps an unreachable database from turning into unbounded memory.

   **A failed flush is re-sent, and re-sending is safe by construction.** Every insert carries `ON CONFLICT … DO NOTHING` against the natural key of its table ([ADR-0012](decisions/0012-idempotent-inserts-natural-keys.md)), so a batch that turns out to have already committed lands the second time as a no-op. That matters because a send can fail in a state where the outcome is unknowable — every statement acknowledged, the connection lost before the commit's acknowledgement — and the driver cannot tell that from "never sent". Without the keys the writer had to abandon those batches, which bought correctness with availability at the worst moment: a dropped connection became a restart, a restart became a gap, and a gap in the self-recorded series (`cb_venue_state`, `cb_features`, `cb_book_snapshots`, `cb_trades_agg`) is unrecoverable, because those series are computed here and exist nowhere else to backfill from.

   What the retry policy still decides is whether another attempt *could* succeed, not whether it is safe. A server error the database will raise again — a check violation, a missing column — fails at once instead of spending the flush timeout; everything else is retried. `rows_written_total` counts what the command tag reports rather than what was submitted, with `rows_conflicted_total` beside it, so a retry landing on work already done is visible rather than reading as throughput.

2. **`context` cancellation flows down the stack.** Root context → per-stream contexts. Graceful shutdown = cancel root, flush channels, close writer, log out FIX, close pool — in that order. The writer's final flush runs on a context cancellation cannot reach, so SIGTERM still persists the last batch.
3. **Each stream owns its connection and its reconnect loop**, with exponential backoff and jitter over the top half of the window — a delay drawn from the whole window would retry a venue that is refusing connections about as often as it retried politely, which is what the schedule exists to prevent. Reconnects and detected gaps increment per-stream counters; a `last_seen` timestamp gauge per stream feeds the staleness flag that the risk engine consumes.

   **A reconnect is a reset, not a resume.** The venue carries no subscription across a socket and restarts its sequence numbering, so every stream resubscribes and drops everything derived from the old connection. The order book is the case that matters: one carried across a drop is missing every update that happened while the socket was down, and nothing in the resulting row could reveal it. The same reasoning skips the trade bucket that was open across the drop — a partial aggregate written under a whole bucket's key can never be corrected, because `ON CONFLICT … DO NOTHING` drops the correction. What is *not* reset is knowledge of what has already been persisted: candle bookkeeping survives, so the window the venue replays on resubscribe refills the hole rather than rewriting it.

   **Every connection also subscribes to `heartbeats`.** One frame a second on every socket, whatever the market is doing, is what makes a single read deadline a dependable liveness check across streams with completely different natural rates — and it gives each stream's own goroutine a regular pulse, so the periodic work (book snapshot, trade-bucket close, silence check) needs no timer beside the read loop and no lock around state the read loop owns. Heartbeats never advance `last_seen`: a staleness gauge that they did advance would report every dead feed as healthy.
4. **Errors on a feed are wrapped and surfaced, not swallowed** — but a feed error degrades to `stale`, it never crashes the binary. Only invariant violations (e.g. writer cannot reach DB after retries) are fatal.

The `carry` binary is simpler: a ticker-driven loop (decision tick) plus the FIX session goroutines that quickfixgo manages, plus one DB writer for decisions/fills/events.

---

## 4. The decision tick

The core loop of `cmd/carry`, run on every funding tick (hourly boundary), on a fast evaluation ticker, and on risk events:

```mermaid
sequenceDiagram
    participant TS as TimescaleDB
    participant VS as venue state cache
    participant FE as feature engine
    participant FPE as pressure engine
    participant DEC as decision engine
    participant RISK as risk engine
    participant EXEC as execution router
    participant V as Venue (fix / paper / live)

    loop every tick
        VS->>TS: refresh latest venue rows
        FE->>VS: read state
        FE->>FE: compute Tier 1-3 + risk features
        FE->>TS: persist cb_features row
        FPE->>FE: features
        FPE->>FPE: crowded_side, pressure_level,<br/>expected_pain, exhaustion_flag
        DEC->>FPE: pressure outputs
        DEC->>DEC: apply rules (spec sec 8)
        DEC->>TS: persist decision + full input snapshot
        DEC->>RISK: TargetPosition
        RISK->>RISK: pre-trade checks + hard stops
        alt checks pass
            RISK->>EXEC: approved delta orders
            EXEC->>V: Submit(Order)
            V-->>EXEC: ExecReport stream
            EXEC->>TS: persist fills
            EXEC->>RISK: position updates
        else hard stop
            RISK->>EXEC: flatten
            RISK->>TS: risk_event
            RISK->>RISK: alert + BLOCKED state
        end
    end
```

Two invariants:

- **Every decision row carries its full input snapshot** (JSON). Any historical decision can be re-derived and argued about.
- **The risk engine is the only component that can emit orders.** The decision engine emits *intent* (`TargetPosition`); risk converts intent into approved order deltas or blocks it.

---

## 5. Decision engine state machine

States and transitions per spec §8. Thresholds (`z_enter`, `z_exit`, `f_flip`, `k`, bands) come from config; the public repo ships synthetic placeholders.

```mermaid
stateDiagram-v2
    [*] --> FLAT
    FLAT --> ENTER: funding positive AND z >= z_enter<br/>AND carry >= k x costs AND basis positive but under extreme<br/>AND margin ratio OK AND one whole contract affordable<br/>AND not in maintenance window AND not blocked
    ENTER --> HOLD: both legs filled
    HOLD --> REBALANCE: abs residual delta > tolerance<br/>trimmed on the spot leg
    REBALANCE --> HOLD: delta restored
    HOLD --> EXIT: funding below f_flip OR z <= z_exit<br/>OR basis extreme against perp<br/>OR any hard stop
    EXIT --> FLAT: both legs closed
    FLAT --> BLOCKED: feed stale OR spread > max<br/>OR cascade flag OR exhaustion at EXTREME/FORCED<br/>OR daily loss limit
    HOLD --> BLOCKED: same conditions -<br/>block new entries, hard stops still active
    BLOCKED --> FLAT: condition clears while flat
    BLOCKED --> HOLD: condition clears with position on
```

`BLOCKED` gates *new risk*, it does not disable hard stops: a stale feed while a position is on still allows (and may force) a flatten.

---

## 6. Execution architecture

### 6.1 The Venue interface

Defined at the consumer (`internal/carry`), implemented by each path:

```go
type Venue interface {
    Submit(ctx context.Context, o Order) (Ack, error)
    Cancel(ctx context.Context, id OrderID) error
    ExecReports() <-chan ExecReport
}
```

| Implementation | Package | Transport | Fills |
|---|---|---|---|
| `fixVenue` | `internal/fix` | FIX 4.4 to `sim-venue` | sim fill model (slippage, latency, partials) |
| `paperVenue` | `internal/exec` | in-process | book-depth fill model, both legs, funding accrual |
| `cbVenue` (week 5) | `internal/exec` | JWT-signed Advanced Trade REST + WS user channel | real |
| `baseVenue` (week 5) | `internal/exec` | ERC-4337 UserOps via Alchemy | real (DEX aggregator swap) |

Every implementation feeds the same `ExecReport` channel and the same order state machine, so the router, risk engine, and P&L accounting are identical across paper, sim, and live.

### 6.2 Order lifecycle

```mermaid
stateDiagram-v2
    [*] --> NEW: Submit acked
    NEW --> PARTIAL: partial fill
    PARTIAL --> PARTIAL: more fills
    NEW --> FILLED: full fill
    PARTIAL --> FILLED: final fill
    NEW --> CANCELED: cancel acked
    PARTIAL --> CANCELED: cancel acked - remainder
    NEW --> REJECTED: venue reject
    FILLED --> [*]
    CANCELED --> [*]
    REJECTED --> [*]
```

### 6.3 FIX session (initiator in `carry`, acceptor in `sim-venue`)

FIX 4.4, 30s heartbeat, file-backed sequence numbers so sessions survive restart, standard resend/gap-fill handling (details and tag dictionary in the [API spec](api-spec.md#4-fix-44-specification)).

```mermaid
sequenceDiagram
    participant C as carry (initiator)
    participant S as sim-venue (acceptor)

    C->>S: Logon (A) seq from store
    S->>C: Logon (A)
    Note over C,S: heartbeats every 30s
    C->>S: NewOrderSingle (D)
    S->>C: ExecutionReport (8) - NEW
    S->>C: ExecutionReport (8) - PARTIAL
    S->>C: ExecutionReport (8) - FILLED
    Note over C: carry restarts
    C->>S: Logon (A) with next expected seq
    S->>C: ResendRequest (2) if gap detected
    C->>S: gap fill / resend
    Note over C,S: session resumes, no lost ExecReports
```

### 6.4 Live carry entry (week 5)

The automated version of the manual carry sequence from spec week 0b — same steps, same wallet:

```mermaid
sequenceDiagram
    participant RISK as risk engine
    participant EXEC as execution router
    participant BV as baseVenue
    participant DEX as Base DEX aggregator
    participant CV as cbVenue
    participant CB as Coinbase

    RISK->>EXEC: approved TargetPosition - ENTER
    EXEC->>BV: Submit spot buy - ETH
    BV->>DEX: UserOp: swap USDC to ETH<br/>session key, Gas Manager
    DEX-->>BV: swap receipt
    BV-->>EXEC: ExecReport FILLED - spot leg
    EXEC->>CV: Submit perp short - integer contracts
    CV->>CB: JWT-signed order, Advanced Trade REST
    CB-->>CV: fill
    CV-->>EXEC: ExecReport FILLED - perp leg
    EXEC->>RISK: net delta approx 0 confirmed
    Note over RISK: if leg 2 fails, immediate<br/>unwind of leg 1 (leg risk rule)
```

**Leg risk rule:** the spot leg fills first (it is slower and lumpier); if the perp leg cannot be placed within a timeout, the spot leg is unwound immediately rather than carrying naked delta. Kill switch halts both venues.

**Quantization rule:** the perp leg moves in whole contracts of 0.10 ETH, so it is sized first (floor toward zero) and the spot leg is sized to match it, leaving a residual under half a contract. Delta is trimmed on the spot leg only — the perp leg cannot express fractions. No entry is attempted unless at least one whole contract is affordable within the notional cap.

---

## 7. Data architecture

TimescaleDB (Postgres + hypertables). Full column-level schema in the [API spec](api-spec.md#5-database-schema). Hypertables hold time series; plain tables hold state.

```mermaid
erDiagram
    cb_products ||--o{ cb_venue_state : product_id
    cb_products ||--o{ cb_bars : product_id
    cb_products ||--o{ cb_book_snapshots : product_id
    cb_products ||--o{ cb_trades_agg : product_id
    cb_products ||--o{ cb_features : product_id
    cb_features ||--o{ decisions : "input snapshot"
    cb_account_state ||--o{ positions : "margin + contracts"
    decisions ||--o{ positions : "drives"
    positions ||--o{ fills : "composed of"
    positions ||--o{ funding_events : "accrues"
    decisions ||--o{ risk_events : "may trigger"
    fix_sessions ||--o{ fills : "carried over"

    cb_venue_state {
        timestamptz ts
        text product_id
        numeric futures_mark
        numeric spot_mark
        numeric funding_rate_hourly
        numeric spread_bps
        numeric open_interest
    }
    cb_features {
        timestamptz ts
        text product_id
        numeric tier1_core
        numeric tier2_micro
        numeric tier3_price
        numeric risk_features
    }
    decisions {
        timestamptz ts
        text state
        numeric target_spot
        numeric target_perp
        text reason_codes
        jsonb input_snapshot
    }
    base_state {
        timestamptz ts
        numeric spot_px
        numeric wallet_eth
        numeric wallet_usdc
        numeric gas_gwei
    }
    cb_account_state {
        timestamptz ts
        numeric available_margin
        numeric liquidation_threshold
        numeric margin_ratio
        numeric cfm_usd_balance
        numeric cbi_usd_balance
        bigint contracts_held
    }
    funding_events {
        timestamptz ts
        text kind
        numeric rate_hourly
        numeric amount
        bigint settled_by
    }
```

Flow of truth:

- `ingest` writes raw observations (`cb_venue_state`, `cb_bars`, `cb_book_snapshots`, `cb_trades_agg`, `base_state`, `cb_account_state`).
- `carry` writes derived and stateful rows (`cb_features`, `decisions`, `positions`, `fills`, `funding_events`, `risk_events`).
- The Python backtester **reads the same tables** and replays them chronologically; `make replay` runs 30 days end-to-end and refreshes Grafana.

---

### 7.1 Historical recovery — the backfill pattern

Every backfill in this system follows the same shape, and it is written here rather than in one part's section because Parts 6, 19 and 20 each have a plausible reason to want one.

**1. Decide what is recoverable at all, and say so.** Two kinds of series live in this database, and the difference is not a detail:

| | Recoverable | Why |
|---|---|---|
| `cb_bars`, `cb_trades_agg` | Yes | The venue re-serves them. Candles reach back to the contract's launch |
| `funding_events` | Derivable | The venue publishes only the *current* rate, but the estimator's inputs are candles ([ADR-0015](decisions/0015-backfilled-funding-provenance.md)) |
| `cb_venue_state`, `cb_book_snapshots`, `cb_features` | **No** | Self-recorded. The book at a past instant exists nowhere else |
| `cb_account_state`, `base_state` | **No** | Point-in-time account and wallet state, only from when polling started |

A part that wants a backfill must first place its series in that table. "No" is a legitimate answer and means downtime is permanent loss — which is why the stack is meant to stay up.

**2. Run on every start, and be gap-driven.** Not "on first run": read what is stored, download only the ranges missing from it, and do nothing when nothing is missing. A start with complete history costs one query (~1s observed, against ~70s for an unconditional download of the same window), which is what makes it safe to leave enabled rather than something an operator has to remember. A container down for an hour then recovers that hour by itself.

**3. Judge coverage on a dense series, never a sparse one.** Absence only means "not downloaded" where a row is expected every interval. `ETH-USD` trades every minute, so a missing minute is a real hole; the perp is sparse and a missing minute usually means nobody traded. Ingest judges coverage on spot and applies the answer to both products. Getting this backwards makes a backfill either re-download forever or never notice a gap.

**4. Compute derived series from what is STORED, not from what was just downloaded.** This is the one that is easy to get wrong and expensive to detect. A single download is only as complete as that one call — measured at 45,525 perp candles against the 46,655 the store had accumulated — so a derivation from it silently inherits its holes. Reading the store instead makes the result **reproducible**: anyone can recompute from the database and get the same number. For the funding series that took independent-reimplementation agreement from 931/985 hours to 1,000/1,002.

**5. Mark provenance when the inputs differ.** A value derived from coarser inputs is not the same measurement as one recorded live, even when the formula is identical. `funding_source = 'backfilled'` exists so that no consumer has to guess, and so a series computed with arithmetic later found to be wrong can be deleted and regenerated with one statement — which has already been needed twice.

**6. Idempotency is the whole safety net.** Every insert is `ON CONFLICT … DO NOTHING` against the natural key ([ADR-0012](decisions/0012-idempotent-inserts-natural-keys.md)), so a re-run costs conflicts rather than duplicates, and precedence settles the right way round on its own: a recorded row is already there when a reconstruction tries, so recording always beats reconstruction.

Reading the database is otherwise reserved to `carry`; `internal/db.Reader` is the named exception that makes steps 2 and 4 possible, and it is read-only (§3, rule 1).

---

## 8. Reliability design

| Failure | Detection | Response |
|---|---|---|
| WS disconnect | read error / heartbeat miss | per-stream reconnect with backoff + jitter; gap counter; resubscribe |
| Data gap | sequence/timestamp discontinuity per stream | gap metric; backfill via REST where history allows |
| Stale feed | `last_seen` age > threshold | `BLOCKED` for new entries; alert at 60s; hard-stop evaluation continues on last good data |
| FIX session drop | quickfixgo session state | auto re-logon, sequence recovery from disk store; alert |
| Hard stop breach (notional, leverage, margin-ratio floor, basis blowout, funding flip, cascade) | risk engine, every tick | flatten via fastest venue path, `risk_event` row, alert; manual reset required |
| Leg failure on entry | ExecReport timeout on second leg | unwind first leg immediately |
| Daily loss limit | realized+unrealized P&L vs limit | flatten, `BLOCKED` until UTC day roll |
| Operator kill switch | config flag / signal | cancel all open orders, flatten, halt submission on both live venues |
| Process crash | Compose restart policy | on restart: reload positions from DB, resume FIX with persisted seq nums, re-derive state — DB is the source of truth, no in-memory-only state |
| Maintenance window (Fri 17:00–18:00 ET) | venue calendar + `status` channel | no new orders, no rebalances; no funding published for that hour, so the funding series records a gap rather than a zero |
| Funding reconciliation drift | computed accrual vs cash adjustments applied twice daily | `funding_reconciliation_error` metric; alert on drift beyond tolerance; computed rate flagged `funding_source = "computed"` |

Graceful shutdown order: stop accepting decisions → cancel open orders → flush channels → persist state → FIX logout → close DB pool.

**Cancellation stops producers, not writes in flight.** The root context is how a
binary is told to stop *starting* work. Any I/O already in progress that the
shutdown order depends on — the writer's flush, an order cancel, a FIX logout —
must run on `context.WithoutCancel` with its own timeout, never on the root
context. Handing the root context to a call that is already in flight means
`SIGTERM` aborts it, the caller reports failure, and the drain-and-flush steps
above never execute: shutdown corrupts precisely the state it exists to protect.
The timeout is per-call, so a stuck dependency still cannot hang the process.
This rule was written after `internal/db.Writer` violated it (see the Part 3
entry in the [build plan changelog](build-plan.md#changelog--decision-record));
every later part that performs I/O during shutdown is bound by it.

**The writer outlives the cancellation that stops its producers.** The rule above
is about one call; this one is about the component's whole lifetime, and Part 4
found it the hard way. `Writer.shutdown` drains the rows producers have already
handed over, and that drain is only correct once the producers have stopped —
otherwise a row accepted after the drain has passed sits in a channel whose only
reader has gone, with its producer told `nil`. So a binary must **not** hand the
writer its root context. The writer is started on `context.WithoutCancel` and
stopped by `Close`, which is called after every producer goroutine has returned.
Ordering then falls out of the wiring instead of depending on a race:

```
cancel root -> producers return -> writer.Close() -> drain -> final flush -> pool close
```

Two things enforce it rather than describing it. `Submit` returns
`ErrWriterStopped` from the moment shutdown *begins*, not from the moment it
finishes, so a producer that is still running is refused rather than silently
dropped (`TestSubmitIsRejectedOnceShutdownBegins`). And a writer that dies
fatally must cancel its producers, because a producer only learns the writer is
gone by being told so from a `Submit` — and not every producer submits. In
`cmd/ingest` the ticker and status streams hand their observations to the
venue-state sampler and never call `Submit` at all; without that cancellation
they read a socket forever after a fatal write, the process never exits, and the
Compose restart policy the table above relies on never fires
(`TestPipelineExitsWhenTheWriterDies`).

---

## 9. Observability

- **Prometheus** (catalog in [API spec §6](api-spec.md#6-prometheus-metrics)): funding rate/z, basis, net delta, accrued funding, P&L split by component, order round-trip latency, FIX session state, WS gap counts, feed staleness, margin ratio, funding reconciliation error.
- **Grafana**, four panels: (1) funding & basis, (2) position & delta, (3) P&L decomposition, (4) system health. Headline card: *"Who is paying whom, how much, and is the crowd getting exhausted?"*
- **Alertmanager:** FIX session down, WS gap > 30s, delta breach, hard-stop trigger, margin ratio < floor, feed stale > 60s, funding reconciliation drift.

---

## 10. Deployment

```mermaid
graph TD
    subgraph "docker compose"
        ING[ingest]
        CAR[carry]
        SIM[sim-venue]
        TSDB[(timescaledb)]
        PROM[prometheus]
        GRAF[grafana]
        AM[alertmanager]
    end
    ING --> TSDB
    CAR --> TSDB
    SIM --> TSDB
    CAR <-->|FIX 4.4| SIM
    PROM --> ING
    PROM --> CAR
    PROM --> SIM
    GRAF --> PROM
    GRAF --> TSDB
    AM --> PROM
    CAR -.->|live, host network egress| NET["Coinbase + Alchemy"]
    ING -.-> NET
```

`make up` brings the stack up; `make replay` runs the 30-day backtest; `make test` runs the Go and Python suites. Config is env-based: `.env` (public defaults) + gitignored `.env.private` (real thresholds, credentials, session keys).

---

## 11. Public / private split

Per spec §9 — *architecture is shown, edge is not*:

- **Public:** all service code, sim-venue, paper engine, observability stack, replay harness, these docs, synthetic threshold placeholders.
- **Private (`.env.private`, never committed):** fitted thresholds and weights, live credentials, wallet session keys, treasury config, live P&L.

---

## 12. Key design decisions

| Decision | Rationale |
|---|---|
| Go services, Python research | quickfixgo is the only production-grade open FIX engine in a modern language; goroutines map onto multi-stream ingestion; Python owns notebooks/backtest (spec §2) |
| `carry` reads DB, not feeds | one read path for live and replay; inputs persisted by construction |
| One writer goroutine per binary | serializes DB access, makes batching trivial, is an explainable interview pattern |
| Risk engine is sole order emitter | intent (decision) and authority (risk) separated; hard stops cannot be bypassed |
| Consumer-defined `Venue` interface | paper → sim → live swap without touching the state machine |
| Self-recorded data as primary history | retail history depth limited and funding may be unpublished; z-scores need >= 30 days of trusted history |
| Spot leg first, perp second, unwind on leg failure | perp is fast/reliable; spot (DEX) is the slow leg; never hold naked delta |
| ETH only, leverage ≤ 3× overnight, no intraday opt-in, small notional | v1 scope control; the intraday/overnight margin transition can never trigger a call; the wallet must stay a *readable* résumé line |
| Perp sized in whole contracts, delta trimmed on spot | contract size is 0.10 ETH and integer-only; spot on Base is the continuous leg |
| Funding rate computed locally and reconciled | the retail API may not publish one for this product; reconciliation against applied cash adjustments is the correctness check |
