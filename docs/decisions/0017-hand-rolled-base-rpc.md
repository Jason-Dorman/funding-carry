# ADR-0017: The Base read path is hand-rolled JSON-RPC, not `go-ethereum`

**Date:** 2026-09-04 · **Status:** accepted · **Build part:** P6 (amends [spec §3](../basis-carry-build-spec.md#3-stack-and-libraries))

## Context

[Spec §3](../basis-carry-build-spec.md#3-stack-and-libraries) names `ethereum/go-ethereum` (`ethclient`, `abigen`) for Base. That line was written before the Base surface was enumerated. Part 6's read path turns out to be **four JSON-RPC methods** — `eth_blockNumber`, `eth_getBalance`, `eth_gasPrice`, `eth_call` — and **five contract calls**, all of which take at most one address argument and return fixed 32-byte words: `balanceOf`, `decimals`, `token0`, `token1`, `slot0`. There are no dynamic ABI types anywhere in it, nothing is signed, and no key is held.

The same question was already answered once for the other venue: [ADR-0016](0016-hand-rolled-coinbase-client.md) hand-rolled the Coinbase client rather than adopt a wrapper for a dozen endpoints.

## Decision

**Hand-roll it.** `internal/ingest/ethrpc.go` is a JSON-RPC client — request, response, error classification — plus a parsed `Address` type, a selector derived by keccak-256 from its signature, address-argument encoding and fixed-word decoding. It is about 250 lines including its comments and holds no credential.

Two details are load-bearing rather than incidental:

- **`Address` is a parsed type, never a string.** Every read this system makes answers a wrong address with a plausible answer rather than an error: `eth_getBalance` on an address that does not exist returns 0, and `balanceOf` on a contract that is not a token returns nothing. A typo in `WALLET_ADDRESS` would otherwise poll happily forever and record a wallet holding nothing. The parse is the only place that mistake can be caught, so there is a type whose only way in is a parse, and `cmd/ingest` fails the startup rather than degrading.
- **Method ids are computed, not pasted.** `0x70a08231` is a famous constant and pasting it would work, but deriving it from `balanceOf(address)` makes the well-known value a *test* (`TestSelectorsMatchThePublishedMethodIds`) rather than five magic numbers a reviewer has to take on faith. This is the one thing that costs a dependency: `golang.org/x/crypto/sha3`, for keccak-256, which the standard library's `crypto/sha3` deliberately does not expose.

Part 17 makes its own choice when it needs signing. Nothing here binds it.

## Alternatives considered

- **`ethclient` + `abigen`.** Follows the spec as written and would be the obvious answer for a system that used a meaningful fraction of it. It brings roughly a hundred indirect modules into `go.mod` — a full consensus client's dependency tree — for four RPC methods, and `abigen`'s generated bindings would add a build step and a checked-in generated file to read `balanceOf`. The reason ADR-0016 gives applies unchanged: the trade goes the wrong way when the surface is this small.
- **Pasting the five selectors as constants.** Removes the `x/crypto` dependency entirely and would be correct. Rejected because a wrong pasted selector is silent to a reader — it looks exactly like a right one — and the four lines of keccak turn the check into something the test suite performs.
- **Deferring the whole client to Part 17,** and having Part 6 read balances through some other route. There is no other route; the read path is JSON-RPC either way.

## Consequences

- **RPC drift is ours to absorb**, as venue drift already is. The surface is small enough that this is a bounded liability, and `RPCError` distinguishes a refusal worth retrying (429, 5xx, `-32005`, `-32603`) from one that will be refused again.
- **The endpoint URL is the credential.** An Alchemy key lives in `BASE_RPC_URL`'s path, so the URL is never logged and **both** `*url.Error` paths deliberately avoid `%w`: the transport failure, and the request build. The second was missed on the first pass and found by four independent reviewers on the same line — an endpoint `net/url` rejects (a control character from a CRLF-edited env file, a space in the host, a missing scheme) fails at `http.NewRequestWithContext`, and the wrapped error put the key into the poller's first `WARN`. `TestATransportFailureDoesNotLeakTheURL` could not reach it, because it uses a well-formed URL on a dead port; `TestAMalformedEndpointDoesNotLeakTheURLEither` covers the other path, and was run against the unfixed code to watch the key appear.
- **Part 17 inherits a client it can extend or replace.** ERC-4337 is `eth_sendUserOperation` and `eth_getUserOperationReceipt` over the same transport ([api-spec §2.2](../api-spec.md#22-write-path-baseVenue-week-5)), so the read client extends naturally; signing is where `go-ethereum`'s `crypto` package would first earn its place, and that is a decision to make there with the evidence in hand.
- Revisit if the Base surface grows dynamic ABI types (arrays, strings, structs returned by value), which is where hand-decoding stops being cheaper than a generated binding.
