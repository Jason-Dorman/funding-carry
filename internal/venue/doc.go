// Package venue is carry's read model: the latest venue and wallet state,
// refreshed from TimescaleDB on a ticker, with per-source staleness flags.
//
// Live trading and replay read through this same cache, which is what makes a
// backtest and a live decision share one code path (architecture section 2).
//
// Built in Part 9.
package venue
