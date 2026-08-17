# ADR-0006: Spot-first entry, perp-first exit, timeout unwind on leg failure

**Date:** 2026-08-16 · **Status:** accepted · **Source:** [architecture §6.4](../architecture.md#64-live-carry-entry-v15); exercised manually per the [carry playbook](../manual-carry-playbook.md)

## Context

A carry entry is two trades on two venues; between them the book has naked delta. The legs differ in character: the Coinbase perp is fast with near-certain fills but trades only in whole 0.10 ETH contracts; the Base spot swap is slower, gas-dependent, and lumpier, but is continuous. The dangerous residual states also differ: a naked perp short can be liquidated; naked long spot cannot.

## Decision

**Sizing precedes sequencing:** the quantized perp leg fixes the size (whole contracts, floored), and the continuous spot leg is sized to match it. **Entry:** spot leg first (the uncertain leg), perp short second. If the perp leg cannot be placed within a timeout, unwind the spot leg immediately. **Exit:** perp close first (fast, removes the liquidatable leg), spot swap immediately after. The brief residual exposure is therefore always *long spot* — the leg with no liquidation surface. Manual carries follow the same sequence, which is how the rule was validated before automation.

## Alternatives considered

- **Perp first on entry** — if the spot swap then fails or slips badly, the book holds a naked liquidatable short while retrying a slow leg.
- **Simultaneous fire-and-manage** — minimizes average exposure time but maximizes the states the router must reconcile; not worth it at hourly-carry horizons and tiny size.

## Consequences

Entry latency is bounded by the slow leg plus one timeout — acceptable for this strategy. The router needs exactly one compensation behavior (unwind first leg), which is drilled in Part 15 acceptance. Delta alerts still guard the residual window.
