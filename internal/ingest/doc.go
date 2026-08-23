// Package ingest holds the market-data readers: the Coinbase WebSocket streams
// with their per-stream reconnect and gap detection, the REST poller and local
// funding-rate estimator, and the Base wallet poller.
//
// The WebSocket half (Part 4) is five streams — ticker, level2, market_trades,
// candles and status — each with its own connection and its own goroutine, and
// each also subscribed to heartbeats. That heartbeat subscription is the
// structural idea worth knowing before reading anything else here: it guarantees
// a frame a second on every socket, which makes one read deadline a dependable
// liveness check across channels with wildly different natural rates, and gives
// each stream's goroutine a regular pulse to run its periodic work on. The book
// snapshot, the trade-bucket close and the silence check all run there, so this
// package needs no timer beside the read loop and no lock around state the read
// loop owns.
//
// Rows go to the writer through the Sink interface declared here rather than to
// a database handle: internal/ingest is the consumer, so it names the contract
// (architecture section 12). The one exception to "each handler owns its own
// state" is VenueState, which several producers feed and which therefore carries
// a mutex — see ADR-0014 for why cb_venue_state has exactly one writer.
//
// Feed errors degrade to staleness here; they never crash the binary. A stream
// logs, counts and retries every failure it meets, and what a consumer sees
// instead of an error is ingest_last_seen_timestamp_seconds ceasing to advance.
//
// The REST poller and funding estimator arrive in Part 5, the Base poller in
// Part 6.
package ingest
