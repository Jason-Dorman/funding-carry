// Package coinbase is the authenticated half of the Coinbase Advanced Trade
// API: JWT minting from the CDP key, and the account endpoints that require it.
//
// It is a separate package from the market-data client in internal/ingest for a
// reason that is about safety rather than tidiness. Market data needs no
// credential — every channel and every public REST endpoint this system reads
// serves the perp unauthenticated (venue doc section 6) — so the credential
// lives in exactly one package, and the code that ingests candles and books
// cannot reach it even by accident. The blast radius of a mistake in the
// market-data path stops at the package boundary.
//
// Everything here is read-only in v1. Order entry arrives with cbVenue in Part
// 16, behind the kill switch, and the key that can place an order is a
// different key from the one that reads balances.
//
// Built in Part 5 (account polling) and Part 16 (order entry).
package coinbase
