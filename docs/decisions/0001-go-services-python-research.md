# ADR-0001: Go for services, Python for research; Rust deferred

**Date:** 2026-08-16 · **Status:** accepted · **Source:** spec §0, §2

## Context

The reconciled source specs disagreed: one was Python-shaped (FastAPI), one was Go (quickfixgo). A solo builder needs one systems language, and the FIX requirement narrows the field: quickfixgo is the only production-grade open FIX engine in a modern language. Target employers (Coinbase and similar crypto infrastructure) are Go shops.

## Decision

Go for every long-running service (`ingest`, `carry`, `sim-venue`). Python for research, threshold fitting, and the backtester, reading the same TimescaleDB. No second systems language in this repo.

## Alternatives considered

- **Python services** — no credible FIX engine; weaker fit for multi-stream concurrency; weaker signal for the target roles.
- **Rust** — stronger signal for prop-shop trading roles specifically, but slower to ship solo and quickfix-rs is not comparable. Deferred: if targets narrow to prop-shop trading systems, port the execution hot path as a *separate* artifact.

## Consequences

Goroutines/channels map directly onto ingestion-feeding-a-state-machine; the Go patterns themselves (one writer, context cancellation, consumer interfaces) become interview material. Research/backtest parity must cross a language boundary — mitigated by both sides reading the same DB and config (see [testing strategy](../testing-strategy.md) parity tests).
