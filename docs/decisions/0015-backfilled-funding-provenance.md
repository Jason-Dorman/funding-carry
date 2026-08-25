# ADR-0015: Funding history is reconstructed from candles, and marked `backfilled`

**Date:** 2026-08-23 · **Status:** accepted · **Build part:** P5

## Context

The funding series is the system's primary asset, and it has no *history* available from the venue.

> **Correction (2026-08-24, adversarial review).** This ADR was written believing the venue published no funding rate at all, because `future_product_details.perpetual_details.funding_rate` comes back empty for this contract. It does — at `future_product_details.funding_rate`, one level up, where it reads `0.000019` hourly with a `funding_time` and a `funding_interval` beside it. The system now records that rate with `funding_source='venue'` and keeps its own estimate in `funding_rate_est` as the cross-check.
>
> **The decision below is unaffected**, because the venue publishes only the *current* rate. There is still no history to fetch, so reconstruction from candles remains the only route to the thirty days a z-score needs. What changes is the framing: the estimator is the source of *history* and the check on the venue's number, not a substitute for a rate that does not exist.

That leaves a series which begins when this system does. A funding z-score needs roughly thirty days of history before it means anything, so the signal that gates every ENTER and EXIT would have been unusable for a month — and any outage inside that month would have restarted the clock.

Part 5's plan called for pulling `fundingHistory`. There is no such history to pull for this product.

There is, however, candle history. `GET market/products/{id}/candles` serves one-minute candles for both products back to the perp's launch (2025-07-18), unauthenticated. The estimator's only inputs are a futures mark and a spot mark, and both are derivable from candles.

## Decision

**Reconstruct the funding history with the venue's own formula, and mark every reconstructed row `funding_source = 'backfilled'`.**

Three rules follow from it:

- **The reconstruction is computed from the STORED bars, not from the download.** It reads `cb_bars`, fetches only the ranges missing from it, and computes from the two together. That makes the series *reproducible* — anyone can recompute it from the database and get the same number — and strictly more complete, because stored bars accumulate across runs while any single download is only as good as that one call. This was not the original design, and the difference was measurable: a single fetch returned 45,525 perp candles where the accumulated store held 46,655, and 5.5% of hours were computed from the short set. Against an independent reimplementation the stored-bar version agrees on 1,000 of 1,002 hours to 1e-9 relative; the fetch-only version agreed on 931 of 985.
- **The same code computes both series.** `RateForHour` is a pure function of its samples; the live path feeds it three-minute VWAPs of trades, the backfill feeds it marks built from one-minute candles. Nothing about the arithmetic differs, which is what makes the two comparable at all.
- **A window is marked only when both products have volume in it.** A premium computed against a price that was not observed is a fabricated basis, and the basis is the entire signal.
- **Reconstruction never beats recording.** Every insert is `ON CONFLICT … DO NOTHING` against the natural key, so a recorded row already present wins. The precedence falls out of [ADR-0012](0012-idempotent-inserts-natural-keys.md) rather than needing its own mechanism.

## Alternatives considered

- **Wait thirty days.** Honest, and it blocks Parts 10–20 on wall-clock time for a signal that can be recovered. It also makes every outage in that month expensive.
- **Reuse `computed` for both.** No migration, no contract change — and the two measurements become indistinguishable in the data. A live mark is a three-minute VWAP of actual trades; a reconstructed one is a volume-weighted close over three one-minute candles. Same formula, coarser inputs, different error profile. Merged under one label, the z-score, the backtest and the reconciliation would each average two different measurements together with nothing to warn them. This project keeps finding exactly that class of bug; deliberately creating one was not defensible.
- **A separate `funding_inputs` column.** More precise and more extensible, but a wider schema change than widening one `CHECK`, and the extra precision has no consumer yet.
- **Reconstruct into `cb_venue_state` as well.** Rejected: that table is the live 5-second series with a single writer ([ADR-0014](0014-one-sampler-owns-venue-state.md)), and `funding_events` is already documented as doubling as the observed funding series (`position_id NULL`). History belongs in the table designed for it.

## Consequences

- Migration `000005` widens the `funding_source` `CHECK` on `cb_venue_state`, `cb_features` and `funding_events` to `venue | computed | backfilled`. The down-migration deletes reconstructed rows before narrowing again — they are reproducible from candles, which is the point of marking them.
- **Every consumer of the funding series must decide what to do with `backfilled` rows.** They are not free: a z-score fitted across the boundary mixes two measurements. Weighting them, excluding them, or documenting that they are treated identically is a decision each of Parts 10, 12 and 20 has to make explicitly — the provenance column exists so that the decision is possible, not so that it is automatic.
- The reconstruction is only as good as its agreement with the live series, and that agreement is unmeasured until enough live history exists to compare. **Recomputing a recorded week from candles and diffing the two is acceptance work for the first part that depends on the z-score**, not something this ADR claims to have done.
- First observed run: 45 days, 110,294 one-minute bars, **981 funding hours reconstructed and 92 unmarkable**, averaging +8.80% annualised with a 24-hour standard deviation of 3.23 points. The unmarkable hours are overnight stretches where the perp did not trade for a whole three-minute window — they stay absent rather than being filled.
- The venue's own rate is recorded beside the estimate with `funding_source='venue'`, and the estimator keeps running as the reconciliation baseline. Revisit if the venue ever exposes funding *history*, which is the only thing that would make the reconstruction unnecessary.
- **A reconstructed series is only as good as the arithmetic that produced it, and that arithmetic changed once already.** The first 981 rows were computed with an hour-window off-by-one found by the 2026-08-24 review; they were deleted and regenerated. `funding_source='backfilled'` is what made that a one-line `DELETE` rather than an archaeology problem — which is the strongest argument for the marker that this ADR can offer.
