# ADR-0018: `base_state.spot_px` is read from the Base pool itself, with the Coinbase mid as the fallback

**Date:** 2026-09-04 · **Status:** accepted · **Build part:** P6

## Context

The Part 6 deliverable asks for "ETH/USDC reference px (DEX aggregator quote; Coinbase spot as configured fallback)" and never names an aggregator. [api-spec §2.1](../api-spec.md#21-read-path-internalingest-base-poller) left the same line open, pointing at [spec §12](../basis-carry-build-spec.md#12-open-items) item 2 — which is a different question: item 2 decides where the automated leg **executes** in Part 17, and is analysed in the [Base spot venue memo](memo-base-spot-venue.md). Part 6 has to decide where a **reference price** is read, now, without pre-empting that.

Every mainstream aggregator quote API (0x, 1inch) now requires a provisioned API key.

## Decision

**Read the price from the pool, on chain, over the same RPC connection as the balances.** `slot0()` on the configured Uniswap v3 WETH/USDC pool gives `sqrtPriceX96`; squaring it, dividing by 2^192 and shifting by the difference of the two tokens' decimals gives ETH priced in USDC. The pool, WETH and USDC addresses are configuration (`BASE_SPOT_POOL`, `BASE_WETH_CONTRACT`, `BASE_USDC_CONTRACT`), because all three are chain-scoped and Base Sepolia needs different ones.

**The fallback is the Coinbase spot mid the ingest binary already holds**, from the `ETH-USD` ticker stream. It costs no request, cannot itself fail over the network, and is subject to the same staleness rule the venue-state sampler applies to its own rows — so a fallback that has gone stale leaves `spot_px` NULL rather than writing a frozen price.

**The pool is verified, not trusted.** On the first successful poll the poller reads `token0()` and `token1()` and checks them against the configured WETH and USDC, and reads each token's `decimals()` rather than assuming 18 and 6. A pool holding some other pair answers `slot0` perfectly happily, and the number it returns would land in `spot_px` looking exactly like a price — in the test that induces it, a wrong pair produced `400000000.0000000000000001`. That is the one failure on this path that is silent unless it is checked for, so a mismatch is permanent (it can only be a wrong address), is logged at `ERROR`, and sends `spot_px` to the fallback rather than to a number.

## Alternatives considered

- **An aggregator quote endpoint (0x / 1inch / Odos).** The right answer for an *executable* price: it accounts for routing and for size-dependent slippage, which is what the Part 17 leg will actually pay. Rejected for Part 6 because `base_state.spot_px` is documented as a reference, not a quote; because it needs a provisioned key; and because it adds a second external dependency, with its own rate limits and downtime, to a poll loop whose other three columns need only the node. Part 14's paper engine already models slippage separately (`base_state.spot_px` + configured DEX slippage + gas), so nothing downstream is waiting on an executable number.
- **Coinbase spot alone.** Simplest, and it ships without any of the above. Rejected because `base_state.spot_px` would then not be a Base price at all: the column exists to mark the leg that trades on Base, and marking it against a centralised venue's book would hide exactly the divergence the spot leg pays.
- **A Chainlink ETH/USD feed on Base.** Also keyless and also on-chain, and it is an aggregate of centralised prices — closer to Coinbase's number than to the pool the swap will actually cross. It would be the better choice for a robustness-weighted mark and the worse one for a leg-execution reference.
- **Hard-coding the pool's token ordering and decimals.** WETH sorts below USDC by address on Base, so `token0` is WETH and the decimals are 18 and 6. All true, all cheap to assert, and all silently wrong on another chain or against the bridged USDbC sitting beside native USDC on this one.

## Consequences

- **Every row records which source it holds**, in `spot_px_source` (`'dex'` | `'coinbase'`), added by migration `000006` on PO direction the same day this ADR was written. It is the same shape `funding_source` has for exactly the same reason ([ADR-0015](0015-backfilled-funding-provenance.md)): two measurements of different things in one column, which nothing downstream could take apart afterwards. A `CHECK` pairs it with `spot_px` in **both** directions — a price with no source and a source with no price are both rejected — because a column that is merely usually set is one nothing can rely on. The pairing is added `NOT VALID`: it binds every write from that migration onwards without requiring rows that predate the column to satisfy it. Those rows have a price and genuinely no market to name, and retro-labelling them would be inventing the provenance this column exists to stop being invented. It was written validating first, on the reasoning that `base_state` was empty; an adversarial review found that `migrate down 1` then `up` therefore fails and leaves the schema dirty, reproduced against a database that by then held 174 priced rows. `ingest_base_spot_px_source_total{source}` carries the same fact for alerting; the row is for anything reading the series back.
- The fallback is expected to be rare: an endpoint that answers `eth_getBalance` answers `slot0`. It exists for the pool being paused or migrated, not for network trouble.
- **This does not decide [spec §12](../basis-carry-build-spec.md#12-open-items) item 2.** Where the automated leg executes is still open, still Part 17's, and still the memo's question. If it resolves to Route B (Coinbase spot + withdraw), this ADR is worth revisiting — a leg that no longer crosses a Base pool has less reason to be marked against one.
