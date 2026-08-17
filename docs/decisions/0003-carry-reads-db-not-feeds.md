# ADR-0003: `carry` reads market state from TimescaleDB, not live feeds

**Date:** 2026-08-16 · **Status:** accepted · **Source:** [architecture §2](../architecture.md#2-process-model); design decision made while drafting the architecture doc (implied but not stated by the spec's §5 diagram)

## Context

`carry` needs current market state every decision tick. It could subscribe to the venue's feeds directly (lower latency) or read the rows `ingest` has already persisted. The system is an hourly-funding carry, not a latency-sensitive strategy, and it carries a hard NFR: every decision must be reproducible from persisted inputs.

## Decision

`carry` refreshes its venue-state cache from TimescaleDB on a ticker. It has no market-data network connections of its own; live trading and replay share one read path.

## Alternatives considered

- **Direct WS subscription in `carry`** — seconds-fresher data, but two feed-handling implementations to maintain, and decisions could act on data that was never persisted, breaking reproducibility by construction.
- **In-process bus from ingest to carry** — couples the binaries' lifecycles and still diverges from the replay path.

## Consequences

Decision inputs are persisted before they can be acted on. The backtester exercises the same read model. Cost: decision data is one poll-interval stale — acceptable at funding-tick horizons, and staleness is itself measured and gated by the risk engine. Revisit if a v2 mode ever needs sub-second reaction.
