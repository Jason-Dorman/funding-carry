// Package ingest holds the market-data readers: the Coinbase WebSocket streams
// with their per-stream reconnect and gap detection, the REST poller and local
// funding-rate estimator, and the Base wallet poller.
//
// Feed errors degrade to staleness here; they never crash the binary.
//
// Built in Parts 4 to 6.
package ingest
