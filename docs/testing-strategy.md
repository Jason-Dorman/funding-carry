# Testing Strategy

**Companion docs:** [Build Plan](build-plan.md) (per-part acceptance criteria) · [Engineering Principles](engineering-principles.md) (testing mindset) · [Architecture](architecture.md)

Conventions defined once so every build part tests the same way. Per-part acceptance criteria in the build plan say *what* to prove; this doc says *how*.

## Principles

1. **Acceptance is demonstrated, not asserted.** Each part's summary shows the command run and the observed evidence (test output, induced alert, DB rows). "It should work" is not done.
2. **Determinism is a feature under test.** Same inputs → same decisions is an NFR (reproducibility); flaky tests are bugs, not annoyances.
3. **Fakes over mocks.** Hand-written fakes implementing the real interface (fake WS server, fake `Venue`, seeded DB) — no mocking frameworks. If a component is hard to fake, its interface is wrong; fix the interface.
4. **Tests document behavior** — a reviewer should be able to read the rule tests for spec §8 and reconstruct the rule table.

## Go conventions

- Tests live beside code (`*_test.go`); fixtures in `testdata/` per package.
- **Table-driven tests** for anything rule- or band-shaped: decision rules (one case per reason code, per build-plan Part 12), z-score bands, pressure levels, hard stops (one per stop kind), exec state machine transitions (including out-of-order and duplicate ExecReports).
- **Golden files** for feature computation: fixed input series in `testdata/`, expected outputs committed. Regenerate with `go test -update`, and the regenerated diff is reviewed like code — a golden change without an explanation is a red flag.
- **No wall clock, no real randomness in logic.** Time enters through an injected clock interface; randomness (sim jitter) through a seeded source. Tests never `time.Sleep` to synchronize — use channels/sync primitives.
- **`goleak`** on every test involving goroutines (ingest streams, writer, FIX sessions, router).
- **Money is `decimal` in tests too** — expected values written as strings, never float literals.
- Race detector always on: `go test -race` is the default in `make test`.

## Integration tests

- Build tag `//go:build integration`, run via `make test-integration` against the Compose TimescaleDB (real hypertables, real migrations — the schema is a contract under test). `golangci-lint` lints with the tag set, so these files are not a blind spot.
- **The suite creates and drops its own database** (`carry_integration`) beside the one `TEST_DATABASE_URL` points at, and never writes to that one. Self-recorded market history is the system's primary asset ([ADR-0002](decisions/0002-one-timescaledb-instance.md)); a test run must not be able to delete weeks of it. It connects over the published host port `15432`, which is deliberately not `5432` so a Postgres already on the developer's machine cannot be mistaken for the stack's.
- FIX integration: real quickfixgo initiator ↔ acceptor over localhost, including the restart/resend scenario (Part 8 acceptance) — kill the initiator process mid-fill, restart, assert no lost ExecReport.
- Deterministic sim: `sim-venue` under a fixed seed produces byte-identical fill sequences.

## Python (`research/`)

- `pytest`; same golden-fixture discipline for backtest math.
- **Parity tests are first-class:** backtester vs paper engine over the same recorded period → same decisions and P&L decomposition within documented tolerance (Part 20 acceptance). Manual-carry logs ([playbook](manual-carry-playbook.md)) are a third parity source: replaying a manual carry's period must produce a P&L decomposition consistent with the hand-logged one.

## Chaos matrix (Part 21, but written against from Part 4 on)

Every reliability claim in [architecture §8](architecture.md#8-reliability-design) gets a fault-injection test. The matrix — fault → expected behavior → where tested:

| Fault | Expected behavior |
|---|---|
| WS drop / flap mid-decision | reconnect + gap counted; risk sees staleness; no crash |
| DB outage | writer backpressure then fatal (invariant), services restart clean |
| FIX disconnect mid-partial | resend recovery, no lost/duplicated fills |
| Venue reject storm | orders → REJECTED terminal, no stuck state, alert |
| Cancel/replace race | state machine converges, CumQty monotonic |
| Second-leg failure on entry | first leg unwound within timeout, risk event |
| Crash with open position | state rebuilt from DB equals pre-crash state |
| Clock skew at funding boundary | funding applied exactly once per hour |
| Kill switch mid-anything | cancel all, flatten, submissions refused until reset |

## Non-negotiables

- Hard stops and the kill switch are **never stubbed out** to make another test pass — fake the venue instead (CLAUDE.md safety rail).
- A part is not done with skipped or `t.Skip`ped tests unless the skip is documented in the part summary with a reason and a follow-up.
- CI/`make test` gate: `lint` + unit (race) green always; integration green before a part is called complete.
