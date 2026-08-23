# ADR-0014: One component samples `cb_venue_state`, fed by every producer

**Date:** 2026-08-20 · **Status:** accepted · **Build part:** P4 (binds P5)

## Context

`cb_venue_state` has two producers by design. Part 4's WebSocket ticker supplies the mid and the spread; Part 5's REST poller and funding estimator supply the marks, the funding rate with its provenance, the premium and open interest ([build plan Parts 4 and 5](../build-plan.md)).

Since [ADR-0012](0012-idempotent-inserts-natural-keys.md) the table is keyed `(product_id, ts)` and every insert is `ON CONFLICT … DO NOTHING`, and `ts` must be the sampling boundary rather than a clock reading. Those two rules together mean two independent producers writing their own rows on the same boundary would collide: the first row inserted wins, the second is silently dropped, and whichever columns it carried never appear. Nothing in the metrics or the logs would show it — a dropped insert is counted as a conflict, which is also what a healthy retry looks like.

## Decision

`internal/ingest.VenueState` is the only writer of `cb_venue_state`. Producers call `Observe`-style methods on it; it emits **one complete row per product per boundary** on its own ticker.

Two rules come with it:

- **Part 5 adds its observations to this component, not beside it.** The REST poller and the funding estimator gain methods on `VenueState`; they do not construct `db.VenueStateRow` themselves.
- **A column with nothing real behind it stays NULL.** Part 4 writes `mid`, `spread_bps` and `maintenance_window` and leaves marks, funding and premium unset. A premium computed against a mid standing in for a three-minute VWAP would be a number the decision engine acts on and nobody can reproduce.

A third rule falls out of the same reasoning: the sampler writes **no quote** for a product whose latest ticker message is older than three sample intervals. Carrying the last known mid forward would keep the series looking healthy through a dead feed, and a frozen price is what the basis, the premium and every downstream band are computed from. A missing row is a gap, which is visible; a repeated row is a lie, which is not. For the same reason `mid` is a midpoint or NULL — the ticker's last *trade* price is a print that carries forward unchanged for as long as nothing trades, so substituting it would put an arbitrarily old number into a column documented as a mid, with nothing on the row to mark it.

Freshness gates the quote, not the row. `maintenance_window` has a different source — the venue calendar and the `status` stream, which is a separate connection with its own health — so it is recorded even when the ticker has gone stale. Gating it on quote freshness would drop the one observation the column exists for at exactly the moment it matters, since a halt is when the ticker is most likely to have stopped too. A row is emitted when either half has something in it, and skipped when neither does. *(Added 2026-08-20 after the Part 4 adversarial review; the original rule gated the whole row.)*

A fourth, added at the same time: the sampler emits **one row per boundary**, tracking the last boundary it wrote. Two ticks landing in the same window would otherwise produce two rows for one key, and the second — the fresher observation — would be discarded by the database as a conflict, which is indistinguishable from a healthy retry.

## Alternatives considered

- **Each producer writes its own rows** — the collision above. Rejected because it fails silently and because the failure is indistinguishable from normal idempotent behaviour.
- **`ON CONFLICT … DO UPDATE` merging columns** — would work, but it makes the row's contents depend on the arrival order of two goroutines, so the same market would produce different rows on different runs. That breaks the reproducibility NFR, and it puts merge logic in SQL where nothing tests it.
- **Separate tables per producer, joined on read** — a join on every feature tick, and a schema change for a problem that a component boundary solves.

## Consequences

- One mutex, held only around a map of latest observations. Producers stay on their own goroutines; the writer stays the single writer.
- `cb_venue_state` gaps are now meaningful: a missing row means no fresh quote, which is exactly what the staleness rule in [architecture §8](../architecture.md#8-reliability-design) wants the feature engine to see.
- The sampling cadence is `POLL_REST_SECS`, shared between the WS sampler and the Part 5 poller, because both must agree on where a boundary is. If they are ever given separate intervals, the sampler is still the single writer, but the row is only complete when the two intervals coincide — which is a reason to keep them one setting.
- Revisit if a third producer needs a cadence the others cannot share, or if `cb_venue_state` is split by producer.
