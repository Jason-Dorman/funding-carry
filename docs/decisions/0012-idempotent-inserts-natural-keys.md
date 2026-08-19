# ADR-0012: Every insert is idempotent, keyed on the row's natural identity

**Date:** 2026-08-19 · **Status:** accepted · **Source:** PO direction following the Parts 1–3 adversarial review

## Context

The writer sends a flush as one pgx batch, which Postgres runs in a single implicit transaction — atomic, one round trip ([`TestBatchIsAtomic`](../../internal/db/integration_test.go)). But a send can fail in a state where the commit's outcome is unknowable: every statement acknowledged, and the connection lost before the acknowledgement of the commit itself. The driver cannot distinguish that from "never sent".

The writer's first answer was to refuse to re-send anything in doubt. That is correct — re-sending a batch that did commit would duplicate every row in it, and nine of the fourteen tables had no unique constraint to absorb it — but it bought correctness with availability, and the bill arrived at the worst moment. A dropped connection became a fatal, a fatal became a restart, and a restart became a gap.

The gap is the part that does not survive scrutiny. "Part 5 backfills it" is only true for candles and whatever history Coinbase exposes. `cb_venue_state`, `cb_features`, `cb_book_snapshots` and `cb_trades_agg` are **computed here and exist nowhere else** — the local funding estimate above all, which is the system's answer to a venue that may publish no funding rate at all. Spec §0 calls self-recorded data the primary history. A gap there means the funding estimator is blind for that window, and the decision engine enters or holds a carry on stale features.

## Decision

**Every table carries a unique key that is the real identity of one of its rows, and every insert uses `ON CONFLICT … DO NOTHING`.** The in-doubt branch of the retry policy therefore flips from "abandon" to "re-send": a repeat of a batch that already committed lands as a no-op.

Identity is what makes a row *that* row, not whatever combination happens to be unique:

| Table | Identity |
|---|---|
| `cb_venue_state`, `cb_features`, `cb_book_snapshots` | `(product_id, ts)` — one sample per product per sampling boundary |
| `cb_bars` | `(product_id, tf, ts)` |
| `cb_trades_agg` | `(product_id, bucket_secs, ts)` |
| `base_state`, `cb_account_state` | `(ts)` — one wallet, one account (spec §1) |
| `cb_products` | `(product_id)`, upsert |
| `decisions` | `(ts)` — one decision loop, one asset in v1 |
| `fills` | `(venue, venue_exec_id)` — the venue's own execution id, now required |
| `funding_events` | `(product_id, kind, ts, position_id)` `NULLS NOT DISTINCT` — `ts` is the funding *hour* for an accrual |
| `risk_events` | `(ts, kind)` |
| `fix_sessions` | `(session_id, started_at)` |
| `positions` | **assigned, not discovered** — see below |

Two consequences of that table are worth stating on their own:

- **`ts` must be the sampling boundary, not `time.Now()`.** A nanosecond-resolution timestamp makes every row unique and the constraint decorative. This is an obligation on Parts 4–6, recorded in [API spec §5.3](../api-spec.md#53-constraints-and-indexes).
- **`fills.venue_exec_id` is now `NOT NULL`.** Every `Venue` implementation mints one: FIX `ExecID` (tag 17) is mandatory, and the paper and sim engines assign their own.

### `positions` has no natural key, and that is the finding

A carry has no property that identifies it. Its open time is a fact about it, not its identity; venue, size and price all repeat. Rather than manufacture a key from a hash of its columns — which would be a synthetic key wearing a natural key's clothes — the honest reading is that a position's identity is **assigned by whatever opens it**. `positions.id` becomes a client-minted ULID, the convention `ClOrdID` already uses ([API spec §3.2](../api-spec.md#32-core-types)).

This also settles the open question left against Part 13. A database-generated id has to be read back before `fills` and `funding_events` can reference it, and a producer reading it back is a second writer — which the one-writer rule forbids. A ULID is known before the insert, so the whole persistence path stays on the batch channel.

## Alternatives considered

- **Keep the abandon-on-doubt policy.** Correct, and what shipped first. Rejected for the reason above: it trades an invisible failure for a visible one, but the visible one lands on exactly the series that cannot be reconstructed.
- **A content hash as the key** for tables without an obvious one. It makes a re-send idempotent and nothing else: two genuinely distinct rows that happen to carry identical values collapse into one, silently, and the schema stops describing what a row *is*.
- **An idempotency token per flush**, stored and checked. A second table, a second write, and a garbage-collection problem, to reproduce what a unique key already does.
- **Read back to settle the doubt** — query whether a representative row landed. Requires a natural key to query by, so it presupposes the thing it replaces.

## Consequences

The writer rides out a dropped connection instead of restarting, so a TimescaleDB bounce no longer costs a hole in the funding series. Duplicate risk goes to zero by construction rather than to "zero as long as nobody makes the retry more aggressive".

The retry policy survives, and is still worth having: idempotency answers *whether a repeat is safe*, not *whether it can succeed*. A check violation or a bad column fails identically on every attempt, and still fails at once rather than spending the flush timeout.

Two mechanisms hold the assumption up, because it is the kind that rots quietly: `TestEveryRowTypeIsIdempotent` fails if a row type is added without a conflict clause, and `TestEveryInsertIsIdempotent` inserts all fourteen rows twice against the real schema — a clause naming a key the database does not have is a runtime error, not a compile error.

`rows_written_total` now counts what the command tag reports rather than what was submitted, with `rows_conflicted_total` alongside it. With `ON CONFLICT` everywhere, a counter that added the two together would report a retry storm as healthy throughput — which is precisely the failure this ADR makes possible.
