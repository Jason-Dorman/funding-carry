# research/

Python side of the system. Reads TimescaleDB directly (`polars` / `duckdb` over
SQL) — the same tables `carry` writes, which is what makes replay and live
trading share one set of inputs.

Contents as the build progresses:

| Path | What | Part |
|---|---|---|
| [`onramp/`](onramp/README.md) | Go learning exercise: fake feeds, fan-in channel, one pgx writer, `/metrics` — with a walkthrough of every construct in it | 3 |
| `notebooks/` | Carry break-even study (research task R1), funding comparisons | before 13 |
| `backtest/` | Chronological replay, run report, `bt_runs` / `bt_results` output | 20 |

Contract and acceptance rule: [`docs/api-spec.md` §8](../docs/api-spec.md#8-research-interface-research).
Test conventions, including the parity tests against the paper engine and the
manual carry logs: [`docs/testing-strategy.md`](../docs/testing-strategy.md).
