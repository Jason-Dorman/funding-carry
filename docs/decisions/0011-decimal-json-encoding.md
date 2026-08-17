# ADR-0011: Decimals in `jsonb` columns are JSON strings, not JSON numbers

**Date:** 2026-08-17 · **Status:** accepted · **Source:** build plan Part 2 (carried forward from Part 1)

## Context

Every decision persists its full input snapshot to `decisions.input_snapshot` as `jsonb`, and the reproducibility requirement says a decision must be re-derivable from it. `fills.raw` and `risk_events.detail` are the same shape.

`shopspring/decimal` decides how it serializes through a mutable package global, `MarshalJSONWithoutQuotes`. Its default writes a quoted string; flipped, it writes a bare JSON number. Either way Postgres stores the value exactly — `jsonb` keeps a number as `numeric` — so **no test inside the Go system can tell the difference**, and no linter flags it.

The loss happens on the way out. Python's `json` module, `jq`, and any JavaScript reading a Grafana panel parse a JSON number into an IEEE-754 double. A funding rate of `0.0000123456789012345678` comes back as something else, and the backtester's parity check against the paper engine (Part 20) starts failing for a reason nothing points at. This is the second of the two precision boundaries `internal/guard` cannot see, carried forward from Part 1.

## Decision

Decimals inside a `jsonb` column are JSON **strings**. Three mechanisms, because a convention is not enough for a failure this quiet:

1. `internal/config`'s `init` pins `decimal.MarshalJSONWithoutQuotes = false`, next to the `DivisionPrecision` pin and for the same reason — it is a mutable global any dependency could change.
2. `internal/db.EncodeSnapshot` is the only way a snapshot is built, and it **refuses to encode** if the global is ever true rather than assuming someone else pinned it. `internal/db` does not import `internal/config`.
3. The integration suite round-trips a snapshot through a real `jsonb` column and asserts the stored document contains the quoted string, with a value wider than a `float64` can hold.

## Alternatives considered

- **Bare JSON numbers.** Slightly more natural to query with Postgres' JSON operators, and correct inside Postgres — but it exports a precision loss to every consumer outside Go, which is exactly the audience the snapshot exists for.
- **A hand-written marshaller per snapshot type.** More code to keep in step with the feature set, and the failure mode returns the moment someone adds a field.
- **Relying on the `internal/config` pin alone.** `internal/db` is usable without `config`, and the tests that matter would then be proving something about the test binary rather than about the service.

## Consequences

Consumers read `input_snapshot->>'funding_rate'` and parse the string with their own decimal type — `decimal.Decimal` in Go, `Decimal`/`polars` string cast in Python. Numeric comparison inside SQL needs an explicit cast (`(input_snapshot->>'funding_rate')::numeric`), which is a small, visible cost paid at query time instead of an invisible one paid at write time.

Trailing zeros are still normalized away by `decimal.String()` (`0.10` serializes as `"0.1"`); the value is unchanged, and money-facing output uses `StringFixed(n)` per [API spec §3.5](../api-spec.md#35-decimal-rules).
