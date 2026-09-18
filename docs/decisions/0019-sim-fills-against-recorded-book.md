# ADR-0019: sim-venue fills against the recorded book; orders rest, and no market means no fill

**Date:** 2026-09-15 · **Status:** accepted · **Build part:** P7 (binds P14, P20)

## Context

The simulator has to decide three things the [build plan](../build-plan.md#part-7--sim-venue-fix-acceptor) named but did not answer, and all three change what a fill *means* downstream: whether an order that does not cross can ever fill later, what happens when there is no usable market data, and where the prices come from at all. The paper engine (Part 14) and the replay (Part 20) are both measured against the same question later — the acceptance rule in [spec §6.10](../basis-carry-build-spec.md) is "profitable after fees, slippage and funding", and every one of those numbers comes out of a fill model.

The constraint that shapes the answer is that this system's own data is the point. It records `cb_book_snapshots` every `BOOK_SNAP_SECS` and keeps them; a simulator with a private notion of the market would produce numbers nobody can re-derive, in a repository whose thesis is that the reasoning is inspectable.

Put to the PO as three gaps on 2026-09-15 and decided there rather than guessed.

## Decision

**Fills are priced against the last recorded top-of-book for the configured product** — the same `cb_book_snapshots` rows the feature engine and the replay read, re-read every `SIM_BOOK_POLL_SECS`, taking the newest snapshot that carries both sides of the touch.

Three rules follow:

- **A non-crossing order rests** and fills when the market comes to it, re-evaluated on each book read. An order that could not fill is deferred to the next read rather than re-asked immediately — nothing can change its answer until the book does, and re-asking sooner is a spin.
- **No fresh market, no order.** An order arriving when the newest snapshot is absent or older than `SIM_BOOK_MAX_AGE_SECS` is rejected with `no market data`; a resting order simply stops filling while the book is stale. The simulator never fabricates a price.
- **A fill never prints through the order's own limit.** Slippage is always applied against the order — up for a buy, down for a sell — and then capped at the limit, because a limit order filling worse than its limit is an execution the real venue could not have produced.

## Alternatives considered

- **Decide an order's fate once, at entry.** Simpler: no evaluation loop, no book poll, one config knob fewer. Rejected because the simulator could then never demonstrate a resting order being hit — which is exactly the shape of the first live safety probe in Part 16, a far-from-market limit order proving place and cancel.
- **Reject any order that is not immediately marketable.** Simplest of all, and it removes the pre-fill cancel case the part's acceptance criteria require.
- **Fall back to the order's limit price when no book is available.** It would keep the demo filling on an empty database. Rejected on the same grounds as [ADR-0015](0015-backfilled-funding-provenance.md) and `spot_px_source`: it manufactures a price the market never showed, and nothing on the resulting `fills` row would say so.
- **A private synthetic book** (random walk, or a fixed spread around a mark). Cheap to write and impossible to check: a P&L decomposition computed from it could not be reconciled against anything.

## Consequences

- A simulated run is reproducible from the database. Given the `cb_book_snapshots` rows and the seed, the fills follow — and the two-basis-point arithmetic on any row can be redone by hand, which is how the Part 7 acceptance was actually checked.
- The simulator inherits ingest's failures honestly. A dead feed does not produce fills at stale prices; it produces rejections, with `simv_book_age_seconds` saying why.
- **Fill prices are not on the venue's tick grid.** The touch moved by a basis-point slippage lands between ticks — a 2400.00 ask at 2 bps prints 2400.48, where the real product moves in $0.50 steps. This is a deliberate consequence of pricing slippage as a rate rather than as a number of ticks, and it means a sim fill is a modelled *expected cost*, not a price the venue could have printed. Quantizing it would have to round adversely, which makes the smallest possible slippage a whole tick (about 2.08 bps at this price) and takes the knob away. Left as it is, recorded here rather than discovered later; Part 14's paper engine is where tick and queue realism belong. *(Raised by the Part 7 adversarial review and refuted as a defect — the code matches this ADR and api-spec §4.3 — but the limitation was undocumented, which is the half that was worth fixing.)*
- It cannot model depth. Size affects a fill only through the partial-slice threshold, not through the book's levels — the snapshot's `bid_depth`/`ask_depth` arrays are ignored. For the sizes this system trades (a whole position is a handful of contracts against a touch holding hundreds) the touch is the fill, so modelling depth would add a mechanism with nothing behind it. **Part 14's paper engine is where a depth-based model belongs**, because it is the one that must match live execution.
- The resting book is in memory. The FIX session survives a sim-venue restart; orders working at the time do not.
- Revisit if the paper engine and the simulator are ever required to agree fill-for-fill, or if a strategy arrives whose orders are large enough that the touch is no longer the fill.
