# ADR-0005: The risk engine is the only component that emits orders

**Date:** 2026-08-16 · **Status:** accepted · **Source:** spec §6.5–6.6; [architecture §4](../architecture.md#4-the-decision-tick)

## Context

The decision engine decides *what the position should be*; something must decide whether acting on that is safe (leverage, margin ratio, staleness, maintenance windows, loss limits) and translate it into leg orders. If multiple components can reach the execution router, hard stops become advisory.

## Decision

The decision engine emits intent only (`TargetPosition`). The risk engine converts intent into approved order deltas — or blocks it — and is the sole caller of the execution router. Hard stops and the kill switch therefore sit on the only path any order can take; there is no code path that bypasses them, including flattens (which the risk engine itself originates).

## Alternatives considered

- **Decision engine orders directly, risk as a veto callback** — every new order source must remember to consult risk; one forgotten call is a naked position. Structural guarantees beat discipline.
- **Risk checks inside each venue adapter** — four copies of the rules, drifting independently.

## Consequences

Risk state (positions, delta, P&L) lives where orders originate, so pre-trade checks always see current truth. The seam is also the test point: hard-stop tests inject state into one component and assert on its order output. Cost: the risk engine is the widest module in the system — held to extra scrutiny under the single-responsibility bar (its one job: "keep the book inside policy").
