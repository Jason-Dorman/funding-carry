#!/usr/bin/env python3
"""Snapshot the pre-2026-08-24 recording to Parquet before the record is reset.

This data is quarantined, not deleted, for two reasons. It is still a valid
shape for exercising the research pipeline — the columns, types and cadences are
the real ones — and the defects in it are themselves the evidence that the
recording fixes worked, which is worth keeping next to the fixes.

It must never be loaded as history. See README.md in this directory for the
three known defect classes and which tables carry which.

Usage:  python3 research/quarantine/export.py [outdir]
"""
import decimal
import pathlib
import subprocess
import sys

import pyarrow as pa
import pyarrow.parquet as pq

REPO = pathlib.Path(__file__).resolve().parents[2]
COMPOSE = ["docker", "compose", "--project-directory", str(REPO),
           "-f", str(REPO / "deploy/docker-compose.yml")]

# Everything self-recorded. cb_products and cb_bars are excluded deliberately:
# products is one metadata row, and bars are re-fetchable from the venue, so
# neither is worth quarantining.
TABLES = ["cb_venue_state", "cb_book_snapshots", "cb_trades_agg",
          "cb_account_state", "funding_events"]


def psql(sql):
    out = subprocess.run(
        COMPOSE + ["exec", "-T", "timescaledb", "psql", "-U", "carry", "-d", "carry",
                   "-tAF", "\x1f", "-c", sql],
        capture_output=True, text=True, check=True)
    return [l.split("\x1f") for l in out.stdout.split("\n") if l.strip()]


def columns(table):
    rows = psql(
        "SELECT column_name, data_type FROM information_schema.columns "
        f"WHERE table_name = '{table}' ORDER BY ordinal_position")
    return [(c[0], c[1]) for c in rows]


def convert(value, pgtype):
    if value == "":
        return None
    if pgtype == "numeric":
        # Kept as a string. A numeric that has survived decimal-to-numeric round
        # trips exactly should not be handed to a float on the way out — the
        # whole no-float-money rule would be undone at the export boundary.
        return str(decimal.Decimal(value))
    if pgtype == "boolean":
        return value == "t"
    if pgtype in ("integer", "bigint"):
        return int(value)
    return value


def export(table, outdir):
    cols = columns(table)
    if not cols:
        print(f"  {table}: no such table, skipped")
        return 0
    names = [c for c, _ in cols]
    rows = psql(f"SELECT {', '.join(names)} FROM {table} ORDER BY 1")
    if not rows:
        print(f"  {table}: empty, skipped")
        return 0

    data = {name: [] for name in names}
    for r in rows:
        for (name, pgtype), raw in zip(cols, r):
            data[name].append(convert(raw, pgtype))

    # Every column is written as a string except the plainly integral ones:
    # this is an archive, and preserving exactly what was recorded matters more
    # than convenient dtypes.
    fields = []
    for name, pgtype in cols:
        fields.append(pa.field(name, pa.int64() if pgtype in ("integer", "bigint")
                               else pa.bool_() if pgtype == "boolean"
                               else pa.string()))
    table_out = pa.Table.from_pydict(
        {n: pa.array(v, type=f.type) for (n, v), f in zip(data.items(), fields)},
        schema=pa.schema(fields))

    path = outdir / f"{table}.parquet"
    pq.write_table(table_out, path, compression="zstd")
    print(f"  {table}: {len(rows):,} rows -> {path.name} ({path.stat().st_size:,} bytes)")
    return len(rows)


def main():
    outdir = pathlib.Path(sys.argv[1]) if len(sys.argv) > 1 else REPO / "research/quarantine/data"
    outdir.mkdir(parents=True, exist_ok=True)
    print(f"exporting to {outdir}")
    total = sum(export(t, outdir) for t in TABLES)
    print(f"{total:,} rows quarantined")
    return 0


if __name__ == "__main__":
    sys.exit(main())
