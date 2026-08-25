# Quarantined recording — 2026-08-21 to 2026-08-24

**This is not history. Do not load it as history.**

74,938 rows recorded during Parts 4 and 5, snapshotted to Parquet on **2026-08-24** immediately before the record was reset on PO direction. It is kept for two reasons: the shape is real, so it exercises the research pipeline with genuine columns, types and cadences; and the defects in it are the evidence that the recording fixes worked, which belongs next to the fixes rather than in a deleted volume.

The fresh record starts **2026-08-24**. Spec §12 open item 4 — ≥ 30 days self-recorded before z-scores are trusted — is measured from that date, not from anything here.

## Why it was discarded

Self-recorded data is the primary history ([spec §6.1](../../docs/basis-carry-build-spec.md)), and this stretch carries three defect classes. None of it is *corrupt*; it is incomplete in known, bounded ways. But a z-score and a backtest built on it would carry an asterisk into every downstream number, and two days was the cheapest the reset would ever be.

| # | Defect | Tables affected | Detail |
|---|---|---|---|
| 1 | **Missing sampling boundaries** | `cb_venue_state` | ~6.8% of 5-second boundaries absent — 1,002 of 14,823 intervals, always exactly one at a time. A free-running `time.Ticker` holds its period but not its phase, and the boundary was derived by truncating the tick's arrival time; with the phase near an edge, a millisecond of jitter made a tick land just *below* its intended boundary and be discarded as a duplicate. Fixed 2026-08-22 by waiting for the boundary rather than for a period |
| 2 | **Understated trade volume** | `cb_trades_agg` | The venue publishes a third aggressor side, `UNKNOWN_ORDER_SIDE`; 330 trades over 22 hours were rejected outright, discarding their price and size along with their side. `trade_count`, `vwap` and `max_single_sz` are low for the buckets overlapping five bursts. Fixed 2026-08-22 — such trades now count everywhere except the buy/sell split |
| 3 | **Mixed and partly wrong funding provenance** | `funding_events` | Rows here were computed under at least three different arithmetic regimes: before the hour-window off-by-one fix, before the unbounded-decimal-scale fix, and from an incomplete candle set. The final regeneration is sound, but this file is a mixture. The live series also never produced a `computed` row before the reset — every row is `backfilled` |

Two further caveats on the whole snapshot: `cb_venue_state` rows before 2026-08-24 carry the local estimate in `funding_rate_hourly` with `funding_source='computed'`, because the venue's published rate was not being read (it was at `future_product_details.funding_rate`, one level above the empty field the code looked at). And `cb_book_snapshots` is the one table here with no known defect — it ran 7,478 consecutive 10-second intervals with no gap outside the market close.

## What it is still good for

- **Pipeline shape.** Column names, types, null patterns and cadences are the production ones. A `polars`/`duckdb` reader written against this will work against the real tables.
- **Regression fixtures.** Defects 1 and 2 are visible in the data. The boundary-loss signature — a delta distribution of 13,821 five-second gaps against 1,002 ten-second ones — is a ready-made test that an audit query can actually detect the fault.
- **Evidence.** The fixes for all three are recorded in the [build-plan changelog](../../docs/build-plan.md#changelog--decision-record) with the numbers; this is the data those numbers describe.

## Format

One Parquet file per table, zstd-compressed, written by [`export.py`](export.py) in this directory.

Every `numeric` column is stored as a **string**, deliberately. These values survived `decimal` → `numeric` round trips exactly ([API spec §3.5](../../docs/api-spec.md#35-decimal-rules)); handing them to a float on the way out would undo at the export boundary the one rule the whole money path is built on. Read them with a decimal type, never a float — the same obligation the `jsonb` snapshots carry.

```python
import polars as pl
df = pl.read_parquet("research/quarantine/data/cb_venue_state.parquet")
df = df.with_columns(pl.col("mid").cast(pl.Decimal(scale=16)))
```
