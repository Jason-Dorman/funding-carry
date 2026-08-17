# ADR-0007: Delta-neutral carry is the core; directional funding-fade deferred to v2

**Date:** 2026-08-16 · **Status:** accepted · **Source:** spec §0 (reconciliation), §3

## Context

The four source specs described two projects: a directional funding mean-reversion signal system and a delta-neutral carry MVP. Running both from day one doubles risk surface and scope for a solo builder, and the directional strategy needs exactly the features, pressure engine, and risk machinery the carry build produces anyway.

## Decision

Delta-neutral carry (long spot on Base / short the ETH perpetual-style future on Coinbase) is the only position-taking strategy in v1. The funding-pressure engine — the directional docs' core insight, *you are trading incentives and crowding, not price* — is repurposed as the carry book's entry/exit/hold-off timing. Directional fade returns in v2 as an overlay: same features, same risk engine, tighter buffers, separate P&L bucket. Also dropped for v1: reverse carry (borrowing on Base adds counterparty/liquidation surface), multi-asset ranking (ETH only — it's the leg holdable as spot on Base), maker-rebate, multi-venue arb, options, LLM classifier.

## Alternatives considered

- **Directional first** — higher variance P&L from a wallet meant to read as disciplined; no natural on-chain spot leg, weakening the wallet story.
- **Both at once** — two strategies' failure modes before one system is hardened.

## Consequences

One trade to explain end-to-end, whose P&L decomposition (funding − costs) is auditable against the chain. The v2 overlay lands on finished machinery. Cost: v1 earns nothing when funding is negative — by design, it stands aside.
