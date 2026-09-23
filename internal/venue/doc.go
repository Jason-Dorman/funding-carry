// Package venue is carry's read model: the latest venue and wallet state,
// refreshed from TimescaleDB on a ticker, with per-source staleness flags.
//
// The cache has no network connection of its own: everything it knows, ingest
// wrote first, so every decision's inputs are persisted before they can be
// acted on (architecture section 2, ADR-0003). That is what a live decision
// and a backtest share — the same persisted tables, read with the same NULL
// semantics — and it is worth stating precisely, because they do not share
// this code: the backtester is Python (spec section 6.10), and Store reads the
// newest row with no as-of parameter, so nothing here can be driven through
// history. An earlier version of this comment claimed one code path; the Part 9
// adversarial review caught it.
//
// State is a value and Latest returns a copy. Staleness is judged per source
// against STALE_FEED_SECS and published as typed flags (Freshness, FeedOK) for
// the risk engine, and as gauges for the dashboard. A read that fails keeps the
// last good row and lets it age, so a database outage degrades to staleness the
// same way a stopped feed does — the cache never crashes and never freezes.
//
// Built in Part 9.
package venue
