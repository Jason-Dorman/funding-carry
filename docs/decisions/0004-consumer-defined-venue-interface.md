# ADR-0004: `Venue` interface defined at the consumer; one exec state machine for all paths

**Date:** 2026-08-16 · **Status:** accepted · **Source:** spec §6.7

## Context

Orders flow to four backends across the build's life: FIX sim-venue, paper engine, live Coinbase perps, live Base smart wallet. The decision/risk core must not care which, and the live cutover (Part 15 → 16/17) must be a wiring change, not a rewrite.

## Decision

`type Venue interface { Submit; Cancel; ExecReports() <-chan ExecReport }` is declared in `internal/carry` — the consumer — not in `internal/fix` or any implementation package. Every implementation feeds the same `ExecReport` state machine (`NEW → PARTIAL* → FILLED | CANCELED | REJECTED`) in `internal/exec`, with CumQty-monotonic sequencing and idempotent duplicates.

## Alternatives considered

- **Interface in the fix package** — inverts the dependency; the core would import an implementation detail (violates DIP, and quickfixgo types would leak upward).
- **Per-venue report handling** — four subtly different fill semantics reaching the risk engine; paper results stop predicting live behavior.

## Consequences

Paper → sim → live is config, not code. Fakes for testing are trivial (implement three methods). Every venue must translate its native semantics into the shared report model — that translation is where each adapter's complexity is quarantined.
