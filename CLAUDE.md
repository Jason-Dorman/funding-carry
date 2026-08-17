# CLAUDE.md — Basis Carry System build

Delta-neutral funding-carry system: long spot ETH on Base, short the ETH perpetual-style future on Coinbase. Go services, Python research, TimescaleDB, FIX 4.4, Prometheus/Grafana. Built part-by-part from a fixed plan; Jason directs which part runs next.

## Roles

**Jason is the Product Owner.** Direction, scope, priorities, and open-item decisions are his. Ask him whenever:
- A spec/plan ambiguity actually changes what gets built (don't ask about things with an obvious conventional answer — decide, state the assumption, move on).
- Reality contradicts the docs (API drift, a design that doesn't survive contact with the venue).
- Anything would touch **live trading, the named wallet, credentials, or real funds** — never act on these without explicit instruction in the current conversation.
- A part's acceptance criteria can't be met as written.

Jason reads every diff and must be able to explain every line (spec §1). Optimize for explainability over cleverness; when a diff needs explaining, explain it in the PR/summary, not in code comments.

## Document map (docs are the contract)

| Doc | Authority over |
|---|---|
| [docs/basis-carry-build-spec.md](docs/basis-carry-build-spec.md) | Requirements source of truth (v3.1). Never edit without PO direction. |
| [docs/venue-coinbase-perps.md](docs/venue-coinbase-perps.md) | Venue mechanics — the only place venue numbers live. Other docs link, never restate. `TODO(verify)` items close in wk 1. |
| [docs/prd.md](docs/prd.md) | Goals, FRs/NFRs, success metrics, open questions |
| [docs/architecture.md](docs/architecture.md) | Processes, concurrency, state machines, data flow, reliability |
| [docs/api-spec.md](docs/api-spec.md) | External APIs, Go contracts, FIX dictionary, DB schema, metrics, config surface |
| [docs/build-plan.md](docs/build-plan.md) | The 22-part execution sequence, acceptance criteria, decision changelog |
| [docs/engineering-principles.md](docs/engineering-principles.md) | Code quality bar (SOLID, cohesion/coupling, smells, testing) |
| [docs/testing-strategy.md](docs/testing-strategy.md) | Test conventions: fixtures, golden files, determinism, fakes, chaos matrix |
| [docs/manual-carry-playbook.md](docs/manual-carry-playbook.md) | Manual wallet-track procedure + log template (parity baseline for paper/backtest) |
| [docs/decisions/](docs/decisions/README.md) | ADRs — one per structural decision; process in its README |

Before executing a build part: read its section in build-plan.md, plus the architecture and api-spec sections it links. The build plan is written to be executable without re-deriving design — if it isn't, that's a doc bug to fix (with the PO if substantive).

### Docs stay in lockstep with code — non-negotiable

Every change that alters behavior, a contract, a schema, a metric, config, or sequencing updates the affected doc **in the same working session as the code**, not later:

- New/changed table or column → api-spec §5. New metric/alert → api-spec §6. New env var → api-spec §7 **and** `.env.example`.
- Changed interface, type, message, or state machine → api-spec §3–4 and any architecture diagram showing it.
- Design deviation from the plan, or an open-item decision made → build-plan **Changelog / decision record** (date, part, decision, why), plus an **ADR** in `docs/decisions/` for anything structural. Deviations also need PO sign-off first.
- A completed part → update build-plan acceptance notes and the README **Status** line.
- `[verify]` markers in api-spec: when you verify a payload against the live venue, replace the marker with what you found.

A doc that disagrees with the code is treated as a bug of the same severity as failing tests. If you find drift you didn't cause, fix it or flag it — don't build on top of it.

## Engineering rules

[docs/engineering-principles.md](docs/engineering-principles.md) applies to all code. Project-specific rules on top (from spec §11 and architecture §12 — these are interview-surface deliverables, not preferences):

- **One writer goroutine per binary.** Nothing else touches the DB.
- **`context` cancellation flows the whole stack**; graceful shutdown order per architecture §8.
- **Interfaces defined at the consumer** (`Venue` lives in `internal/carry`, not `internal/fix`).
- **Feed errors degrade to staleness, never crash; invariant violations are fatal.** Wrap errors with context; no bare log-and-continue outside feed loops.
- **Only the risk engine emits orders.** The decision engine emits intent.
- **No float money.** `decimal` in Go, `numeric` in SQL, end to end.
- **The perp leg is quantized** (whole 0.10 ETH contracts, floor toward zero); the spot leg is continuous and trims residual delta. Never round size up into more risk.
- **Venue numbers live in one file.** Contract size, funding formula, margin fields, fees come from `docs/venue-coinbase-perps.md`; other docs and code comments link to it rather than restating values.
- Reason codes are a closed, append-only enum. Every decision persists its full input snapshot.
- Tests are part of every part's acceptance criteria: table-driven for rules/bands, golden fixtures for features, `goleak` on goroutine-heavy code, deterministic sims under fixed seed.
- All diagrams in docs are mermaid.

## Safety rails

- **Public/private split (spec §9):** real thresholds, weights, credentials, session keys exist only in `.env.private` (gitignored before any secret exists). Committed values are synthetic placeholders. Check every diff for leakage.
- **Wallet doctrine (spec §1):** one named wallet, small/real/readable activity only. Nothing in code or docs sizes up, automates, or touches the wallet beyond what the current part specifies and the PO has approved. Live parts (16–18) are gated on Parts 13 & 15 acceptance. There is no perp testnet: first live contact is minimum size (one contract) and far-from-market limit orders to prove place/cancel without taking risk; the Base leg uses Sepolia first.
- Kill switch and hard stops are never bypassed, stubbed out, or "temporarily disabled" — including in tests of other components (fake the venue instead).
- Part 3 (Go onramp) is Jason's hand-written exercise: do not pre-write it; refactor only when he presents his version.

## Workflow

- Execute one build part at a time, on PO direction. Deliver: code + tests + doc updates + a summary that walks the diff (what, why, how it maps to the part's acceptance criteria).
- Acceptance criteria are the definition of done; demonstrate them (run the test, induce the alert, show the rows) rather than asserting them.
- Commands (once Part 1 lands): `make up`, `make down`, `make migrate`, `make test`, `make lint`, `make replay`.
- Not yet a git repo; `git init` happens at Part 1 with `.gitignore` in the first commit.
