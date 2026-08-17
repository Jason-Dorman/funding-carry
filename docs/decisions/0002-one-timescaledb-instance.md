# ADR-0002: One TimescaleDB instance for time series and state

**Date:** 2026-08-16 · **Status:** accepted · **Source:** spec §0, §7

## Context

Source specs proposed Postgres in one and TimescaleDB + Postgres in the other. Retail history depth at the perp venue is limited and the funding rate may not be published to our API tier at all, so self-recorded series are the primary history — the database is not a cache, it is the asset.

## Decision

A single TimescaleDB instance (it *is* Postgres): hypertables for time series (`cb_venue_state`, `cb_bars`, `cb_book_snapshots`, `cb_trades_agg`, `cb_features`, `base_state`), plain tables for state (`decisions`, `positions`, `fills`, `funding_events`, `risk_events`, `fix_sessions`, `cb_products`).

## Alternatives considered

- **Two databases (series vs state)** — operational overhead and cross-DB consistency problems for a solo operator, with no gain at this scale.
- **Plain Postgres** — loses hypertable partitioning/compression for the always-growing series tables, which the 30-day replay reads end-to-end.

## Consequences

One connection string, one backup, one thing to operate. Everything — live engine, backtester, Grafana — reads one source of truth, which is what makes ADR-0003 possible.
