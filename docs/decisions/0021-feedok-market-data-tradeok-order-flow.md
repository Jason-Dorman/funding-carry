# ADR-0021: `FeedOK` is market-data freshness; order flow gates on `TradeOK`

**Date:** 2026-09-22 · **Status:** accepted · **Build part:** P9 (binds P13) · **Source:** PO decision 2026-09-22, on a gap the Part 9 plan did not fill; the condition attached to it is the PO's

## Context

Spec §8 lists "feed not stale" among the conditions for ENTER and among the reasons for BLOCKED, and [architecture §8](../architecture.md#8-reliability-design) gives staleness a row. Neither says **which sources** a freshness flag covers, and Part 9 is the part that has to answer, because it builds the flag.

The venue state cache has four independently fed sources ([api-spec §3.7](../api-spec.md#37-venue-state-cache-internalvenue)): the perp and spot rows of `cb_venue_state`, written by ingest's WebSocket sampler and REST poller; `cb_account_state`, written only when a CDP credential is configured; and `base_state`, written only when `BASE_RPC_URL` and `WALLET_ADDRESS` are configured.

That last fact is what forces the decision. The public stack — the one a reader clones, and the one CI runs — has no credential and no wallet address, so those two sources have no rows at all, ever. A single boolean over all four would be false on that stack permanently.

## Decision

Two predicates, and the distinction is load-bearing.

- **`FeedOK() = !Perp.Stale && !Spot.Stale`** — market-data freshness. This is the input to spec §8's "feed not stale", and it is what `carry_feed_ok` exports.
- **`TradeOK() = FeedOK() && !Account.Stale && !Wallet.Stale`** — every source fresh. **Anything that gates order flow gates on this**, never on `FeedOK` alone. Part 13's pre-trade checks consume it; until Part 13 there is nothing in `carry` that gates order flow, so the predicate exists ahead of its consumer deliberately, so that the consumer has one name to reach for.

`FeedOK` at 1 is therefore **not** a "safe to act" signal, and the `carry_feed_ok` help string says so in as many words, so the qualification travels with the series rather than living only in this file.

## Alternatives considered

- **One boolean over all four sources.** Rejected: permanently false on the public stack. That is not conservative, it is dead — a flag that is always false says nothing, and it trains every reader to ignore it. It would also hide a distinction the risk engine needs: a stale margin ratio should block a *size* check, not be reported as a dead market-data feed.
- **`FeedOK` over all four, with the polled sources excused when unconfigured.** Rejected: it makes the flag's meaning depend on configuration, so the same reading means different things on two deployments, and the excusing logic would have to be duplicated by anything reading the gauge.
- **No composite at all; every consumer combines the four `Freshness` values itself.** Rejected for the reason the cache derives `Stale` once: a rule applied in four places is a rule that will differ in one of them. The per-source flags remain exposed for consumers that need them individually, which is what makes the account and wallet cases expressible.

## Consequences

- On the public stack `TradeOK` is always false. That is the correct answer there: nothing on that stack may trade.
- **Part 13 carries the wiring as a named deliverable** — the pre-trade checks gate on `TradeOK`, and a dashboard or alert built on `carry_feed_ok == 1` must not be able to let a stale wallet trade through. The failure this guards against is specific: someone builds that gate six months from now and a stale wallet trades through it.
- `carry_feed_ok`'s help string carries the conjunction in valid PromQL, so it can be pasted where it is read. The Part 9 adversarial review found the first version unquoted and therefore unparseable.
- The scope is revisable at Part 13 without touching the cache: both predicates are pure functions of `Staleness`.
- Recording this as an ADR was itself a review finding. The Part 9 changelog originally judged it a rule inside [ADR-0003](0003-carry-reads-db-not-feeds.md)'s structure; ADR-0003 decides *where* `carry` reads from and says nothing about which sources a freshness boolean covers. The other three Part 9 decisions — the refresh cadence, the failed-read behaviour, and the rows being the write types — do sit inside existing structure and have no ADR of their own.
