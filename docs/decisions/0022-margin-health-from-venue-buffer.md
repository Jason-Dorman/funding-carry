# ADR-0022: Margin health is the venue's `liquidation_buffer_percentage`, not a ratio the system divides

**Date:** 2026-09-22 · **Status:** accepted · **Build part:** P5A (ingestion), P13 (control)

## Context

**This is not a record of implementing the wrong formula.** The formula implemented matches Coinbase's developer guide verbatim. The hazard is upstream of us: **the venue overloads one term across two of its own surfaces, with opposite polarity**, and we resolved the overload toward the surface an operator cannot see ([venue doc §4.1](../venue-coinbase-perps.md#41-margin-ratio-names-two-opposite-quantities--read-this-before-using-either)):

- the **developer guide's** `available_margin / liquidation_threshold` — **higher is safer, 1.0 is liquidation**; and
- the **help centre's and the app's Margin Ratio widget** — `maintenance_margin_requirement / total_funds_for_margin` as a percentage, **higher is more dangerous, 100% is liquidation**.

Both are correct. Both are "the margin ratio" in Coinbase's own words. We named our column `margin_ratio`, our gauge `carry_margin_ratio` and our alert `MarginRatioLow` after the first — which is the definition **nobody is looking at during an incident**. The failure mode is not a miscalculation; it is an operator reading `carry_margin_ratio = 1.2` on a dashboard beside a Coinbase app showing `Margin Ratio 83%` and having to derive, under time pressure, that these are different quantities rather than a discrepancy to reconcile. Getting a bad number is recoverable. Getting two good numbers you believe are the same number is not.

Spec §4 additionally asserted the venue "returns" the ratio; it does not — it returns the two operands and `internal/coinbase` divides them, so the principle was being stated and broken in one sentence.

The more consequential half is separate from the naming question. The venue reports margin health **directly** — `liquidation_buffer_percentage`, *"if your liquidation buffer percentage reaches 0%, your futures positions and/or open orders will be liquidated"* — alongside `liquidation_buffer_amount` and per-window `maintenance_margin`. None is parsed; none has a column. Spec §4's own principle is "margin health is **read, not derived**", and the one field the venue explicitly ties to the liquidation trigger was the field being ignored in favour of a local division. The per-window measures matter independently: `available_margin` and `liquidation_threshold` are blended across margin windows, so the spec's ≤ 3×-**on-overnight-margin** rail was **not enforceable as written** against the data being collected.

## Decision

1. **`liquidation_buffer_percentage` is the authoritative hard-stop input.** The config value is `LIQUIDATION_BUFFER_FLOOR_PCT`, replacing `MARGIN_RATIO_FLOOR`.
2. **The raw venue value is persisted unreinterpreted**, beside its source timestamp, its provenance, and a separately stored normalized percentage. Storing the raw string means a scale mistake is correctable from history rather than baked into it.
3. **`available_margin / liquidation_threshold` is retained only as a derived reconciliation metric.** It is never a fallback control value. If the two disagree about safety, that disagreement is a signal to surface, not one to resolve silently in favour of the one still available.
4. **Missing or stale venue margin data blocks new entries** and emits a typed `MARGIN_DATA_UNAVAILABLE` risk event. It is never replaced by the derived ratio.
5. **No threshold is set until the percentage scale is reconciled against the Coinbase UI** on a live account holding a position. The field is a string and the docs never state whether 33% arrives as `0.33` or `33`; a floor set against the wrong scale is wrong by 100× in the direction that does not fire.
6. **Ingestion and control are separated.** [Part 5A](../build-plan.md#part-5a--balance-summary-field-completion) owns capturing the fields; Part 13 consumes them and owns the control logic. Part 5A runs first, so history accumulates across the manual carries before any risk rule depends on it.

## The generalizable rule

**When a venue overloads a term across its surfaces, name our artifacts after the operator-visible definition, and define the other one beside it.**

The reasoning is about incident cost, not correctness. Our column, metric, alert and log field are read by a human comparing them against the venue's own UI while deciding whether to intervene. A name that matches what the machine documents but contradicts what the screen shows converts every such comparison into a translation step, and translation steps fail under exactly the conditions that produce incidents. Where the operator-visible figure cannot be the control value — as here, since the app widget's inputs are not returned by the API — the control value gets a name that **cannot be confused with either** (`liquidation_buffer_pct`, not a third thing called a ratio), and the collision is documented in one place that both other names point at.

This generalizes past margin: any venue term that appears in both an API reference and a customer-facing UI is a candidate for this check, and the check is cheap — read the help centre page for the same concept, not just the developer docs. Reading only the developer docs is what produced this, and it is the same failure shape as the `perpetual_details.funding_rate` trap recorded in [venue doc §3](../venue-coinbase-perps.md): one authoritative-looking source, read in isolation, agreeing with itself.

## Alternatives considered

- **Keep `available_margin / liquidation_threshold`, rename for direction.** Smallest change, no migration. Rejected: it still derives what the venue reports, and the derived value is blended across margin windows, so it cannot express the overnight-margin constraint spec §4 sizes against.
- **Adopt the app-widget convention (100% = liquidation).** Best parity with what the PO sees. Rejected: it inverts an existing hard stop — the highest-risk possible sign error — and depends on `maintenance_margin` and a "total funds for margin" figure the API does not return under that name.
- **Convert arithmetically between the two ratios.** Rejected: they are not exact reciprocals. `available_margin` and "funds for margin" are separately defined, and `liquidation_threshold` is not documented as the maintenance margin requirement.

## Consequences

**Easier.** The hard stop reads a number the venue computed, with a documented fatal value (0%) and no division — which is what [ADR-0009](0009-perp-venue-coinbase.md)'s "margin health is read, not derived" always meant. Storing both margin windows side by side makes the ≤ 3×-overnight rail checkable against the venue's own overnight maintenance margin rather than against a blended figure.

**Harder.** Three numbers now describe one thing (venue buffer %, derived ratio, app-widget ratio), and a reader must know which is which — hence §4.1 being the only place any of them is defined. The rename touches config, schema, a metric, an alert and the spec's formula table.

**Fails closed by construction.** An unset or unreconciled `LIQUIDATION_BUFFER_FLOOR_PCT` must **block entry**, never disable the check — "we have not decided the safe line" and "there is no safe line to enforce" must not be the same state. The loader precedent cuts the wrong way here and is recorded as a trap in [Part 5A](../build-plan.md#part-5a--balance-summary-field-completion): every neighbouring risk limit uses the silently-defaulting `l.Decimal(key, default)`, so idiom-matching the surrounding code produces exactly the failure this ADR exists to prevent.

**Revisit if:** the scale reconciliation shows `liquidation_buffer_percentage` absent or unpopulated on a real position, in which case the derived ratio becomes the control value by necessity and this ADR is superseded rather than quietly ignored.
