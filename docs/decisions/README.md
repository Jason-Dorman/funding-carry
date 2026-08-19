# Architecture Decision Records

One short file per decision that shaped the system. ADRs make the build's judgment calls visible, discussable, and defensible — which is the project's thesis ("architecture is the product") and its interview surface.

## Process

- New ADR whenever: a build-plan open item is decided, a design deviates from the plan/spec (PO sign-off first), or a non-obvious technical choice is made during a part.
- Copy [`0000-template.md`](0000-template.md), next sequential number, kebab-case title. Keep it under a page.
- Status: `accepted` | `superseded by ADR-XXXX`. Never delete or rewrite an accepted ADR — supersede it.
- The [build-plan changelog](../build-plan.md#changelog--decision-record) stays the chronological index; it links here for anything bigger than a one-liner.

## Index

| ADR | Decision | Status |
|---|---|---|
| [0001](0001-go-services-python-research.md) | Go for services, Python for research; Rust deferred | accepted |
| [0002](0002-one-timescaledb-instance.md) | One TimescaleDB instance for series + state | accepted |
| [0003](0003-carry-reads-db-not-feeds.md) | `carry` reads market state from TimescaleDB, not live feeds | accepted |
| [0004](0004-consumer-defined-venue-interface.md) | `Venue` interface defined in the consumer; one exec state machine for all paths | accepted |
| [0005](0005-risk-engine-sole-order-emitter.md) | Risk engine is the only component that emits orders | accepted |
| [0006](0006-leg-sequencing-spot-first-entry.md) | Spot-first entry, perp-first exit, timeout unwind | accepted |
| [0007](0007-carry-core-directional-deferred.md) | Delta-neutral carry is the core; directional fade deferred to v2 | accepted |
| [0008](0008-wallet-first-milestones.md) | Wallet history outranks code milestones | accepted |
| [0009](0009-perp-venue-coinbase.md) | Perp venue is Coinbase US perpetual-style futures, not Hyperliquid | accepted |
| [0010](0010-embedded-migrations.md) | Migrations embedded in the binaries, applied by a one-shot `cmd/migrate` | accepted |
| [0011](0011-decimal-json-encoding.md) | Decimals in `jsonb` columns are JSON strings, not JSON numbers | accepted |
| [0012](0012-idempotent-inserts-natural-keys.md) | Every insert is idempotent, keyed on the row's natural identity; `positions.id` is a client-minted ULID | accepted |

Pending (from spec §12 — will become ADRs when decided): Advanced Trade Go client (hand-rolled vs community, wk 1/Part 5), automated Base spot venue (DEX aggregator vs Coinbase spot + withdraw, wk 4), FIX beyond sim-venue (wk 7/Part 21), z-score minimum history, Rust port.
