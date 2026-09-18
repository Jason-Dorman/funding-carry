// Package fix holds both ends of the FIX 4.4 session: the acceptor used by
// sim-venue and the initiator used by carry, over quickfixgo.
//
// The acceptor (Part 7) is an exchange simulator. It answers NewOrderSingle and
// OrderCancelRequest, and it prices fills against the last top-of-book ingest
// recorded in TimescaleDB — the same rows the feature engine and the replay
// read, so a simulated run is reproducible from the database rather than from
// whatever a private model happened to do. Latency, slippage and partial fills
// are configured, and the jitter is seeded, because a simulator nobody can
// reproduce produces numbers nobody can check.
//
// Sequence numbers are file-backed and ResetOnLogon is off, because surviving a
// restart with resend recovery intact is the point of having a real FIX session
// rather than a REST client.
//
// The initiator half is Part 8.
package fix
