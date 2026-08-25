# Decision memo — the automated Base spot venue

**For:** Jason (PO) · **Date:** 2026-08-24 · **Closes:** [spec §12](../basis-carry-build-spec.md) open item 2 · **Gates:** Part 17

The spot leg has to be bought and sold somewhere. Two routes, and they differ more in what they leave behind than in what they cost.

---

## The number that decides it

At v1's notional cap the round-trip **cost dominates the carry**, on either route. That reframes the question from "which is cheaper" to "which is survivable".

A $500 clip held for `CARRY_HORIZON_HOURS` (168h) at 10% annualised — near the 8.91% average of the 45-day reconstructed series — earns:

```
$500 × 0.10 × 7/365  =  $0.96
```

Against that, the spot leg's round-trip cost:

| | Route A — DEX aggregator | Route B — Coinbase spot + withdraw |
|---|---|---|
| Spot fee, round trip | ~0 (aggregator spread only) | **~$6.00** — 0.60% taker × 2 at the lowest published tier |
| Slippage at $500 | ~5–15 bps ≈ **$0.50–1.50** | negligible (deep book) |
| Gas | **$0.006** round trip — Base measured at 0.0060 gwei, ~$0.003/swap | withdrawal fee, small but non-zero |
| Latency to a hedged position | seconds (one tx) | **minutes to hours** — withdrawal to Base is not instant |
| **Round trip vs one week of carry** | **~0.5–1.5×** | **~6×** |

**Route B's fee alone is six times a week's carry.** The perp leg's fees are identical on both routes and don't differentiate.

*Confidence:* Base gas is measured today. The 0.60% is Coinbase's **published** lowest-tier taker fee — I could not verify it against your account, because the view-only key returns **401 on `transaction_summary`**. If your tier is better, B improves proportionally; it would need to be under ~0.10% to draw level, which is several volume tiers up.

---

## The wallet column, weighed explicitly

[ADR-0008](0008-wallet-first-milestones.md) and spec §1 put wallet history *above* code milestones in the ordering rule. That is not a tiebreaker here, it is the second decisive input.

- **Route A** puts the entire spot leg on-chain from the named wallet: every entry and exit is a visible swap, and the wallet tells the whole story of the strategy.
- **Route B** moves the buy off-chain. Only the *withdrawal* lands on Base, so the on-chain record thins to periodic transfers — the wallet shows funds arriving, not a carry being run. The auditable half of the story ([manual-carry playbook](../manual-carry-playbook.md): "the spot leg is on-chain and is the auditable half") largely disappears.

Route B does not merely score lower on this axis; it removes the thing the ordering rule exists to accumulate.

---

## Honest case for Route B

It is materially simpler, and the simplification is not trivial: **Part 17's entire session-key apparatus disappears** — ERC-4337 UserOperations, a bundler, gas sponsorship, a router-scoped allowance, an expiry preflight that must fail loudly ([api-spec §2.2](../api-spec.md#22-write-path-baseVenue-week-5)). That is the highest-risk code in the build and the only place a key can move funds autonomously. Route B replaces it with an authenticated REST order on a venue we already talk to.

If the goal were purely to ship a working carry with the least chance of losing money to a bug, Route B wins.

---

## Recommendation: **Route A, the DEX aggregator**

Economics and doctrine point the same way, which is unusual enough to act on. Route B costs six times a week's carry at the size v1 actually trades, and it deletes the on-chain record that spec §1 ranks above shipping speed. Route A's cost is real but roughly an order of magnitude smaller, and its risk is *engineering* risk — which is bounded by the session-key scoping, the allowance cap and the expiry preflight that Part 17 already specifies.

Two conditions I would attach:

1. **Keep Coinbase spot as the configured fallback**, which [api-spec §2.1](../api-spec.md#21-read-path-internalingest-base-poller) already contemplates for the reference price. If the aggregator route fails at submit time, a manual Route-B entry is better than an unhedged spot leg.
2. **Treat the cost/carry ratio as a first-class signal, not a footnote.** At $500 the round trip is comparable to a week's carry on the *better* route. That is the same arithmetic already flagged against `CARRY_HORIZON_HOURS` in api-spec §7 — costs are a fixed toll while funding accrues per hour, so the horizon, not the notional cap, decides whether ENTER can ever be true. The break-even study (build plan R1) should be run before Part 17, not after.

## What would change my mind

- Your actual spot fee tier being **under ~0.10%** — which the 401 above means neither of us can currently see. Worth checking in the UI while you're spot-checking `cb_account_state`.
- A materially larger notional cap, which would shrink both costs relative to carry and make B's simplicity worth more.
- Aggregator slippage at $500 measuring far worse than 15 bps on a live quote. I could not obtain one for this memo: 0x's keyless endpoint now returns `no Route matched`, and the alternatives want an API key. **Worth measuring before Part 17 commits**, and it is a half-hour job once a key exists.
