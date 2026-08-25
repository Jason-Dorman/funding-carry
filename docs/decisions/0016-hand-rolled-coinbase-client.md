# ADR-0016: The Advanced Trade client is hand-rolled, and the credential is isolated in one package

**Date:** 2026-08-23 · **Status:** accepted · **Build part:** P5 (closes spec §12 open item #1, required before P16)

## Context

There is no official Go SDK for Coinbase Advanced Trade. Spec §12 left the client an open decision — hand-rolled versus a community package — and the build plan requires it settled before Part 16, because Part 16 is where this code first sends an order.

The surface actually used is small. Market data needs four public GET endpoints. The account needs three authenticated GETs. Part 16 adds order submit, cancel and status. Authentication is a JWT per request, signed EdDSA over a CDP Ed25519 key.

## Decision

**Hand-roll it, and split it in two.**

- `internal/ingest` holds the **public market-data** client. It has no credential and cannot obtain one.
- `internal/coinbase` holds the **authenticated** client: JWT minting and the account endpoints, with order entry to follow in Part 16.

The split is the load-bearing half of this decision. Market data needs no authentication — every channel and public endpoint this system reads serves the perp unauthenticated ([venue doc §6](../venue-coinbase-perps.md)) — so the credential lives in exactly one package and the code that ingests candles, books and trades cannot reach it even by accident. A mistake in the market-data path stops at the package boundary.

JWT signing is about fifty lines: two base64url-encoded JSON objects, an `ed25519.Sign` over their concatenation, and a nonce. It uses `crypto/ed25519` from the standard library and nothing else.

## Alternatives considered

- **A community Advanced Trade package.** Several exist; none is official, and the ones surveyed wrap the whole API surface — spot, portfolios, converts, futures — where this system uses under a dozen endpoints. Adopting one means auditing code that handles a credential, on a dependency with no stability guarantee, to avoid writing a small amount of code that is easy to read. The trade goes the wrong way when the dependency's job is to hold the key.
- **A JWT library (`golang-jwt` or similar).** Larger than the thing it replaces, and it hides the detail that actually matters: the token is bound to one method and one path and expires in 90 seconds, so a captured token authorises a single call rather than the account. That property is worth being able to see.
- **One package for the whole venue API.** Cohesive by subject, and it puts the credential in the same package as the candle reader. The safety boundary was judged worth the small awkwardness of the venue's API spanning two packages, since each is cohesive by *purpose* — market-data ingestion, versus authenticated account access.

## Consequences

- **Venue drift is ours to absorb**, which Part 5 already demonstrated twice: the REST trades endpoint is `products/{id}/ticker`, not `market_trades` (the obvious path 404s), and `cfm/intraday/margin_setting` returns `setting` as a bare **string**, not the nested object the field name implies. The second failed silently on every poll — logged, but leaving `intraday_margin_enabled` NULL, which is to say leaving nobody checking the one setting v1 requires to be off. A wrapper might have absorbed the first; nothing would have caught the second but a test asserting on real behaviour.
- Both shapes are now accepted where a field has been observed to change form, because a field that has been one thing and could be another is where a silent NULL creeps back in.
- **Part 16 inherits this and the split.** Order entry goes in `internal/coinbase`, and the key that can place an order is a *different* key from the one that reads balances — view-only for Part 5, trade-scoped and IP-allowlisted for Part 16, created only once the kill switch exists.
- `go test -tags live ./internal/coinbase/` re-checks the whole auth chain against the venue, so "does the credential still work" stays a one-line answer. Every call in it is a GET.
- Revisit if the endpoint count grows past roughly a dozen, or if Coinbase ships an official Go SDK.
