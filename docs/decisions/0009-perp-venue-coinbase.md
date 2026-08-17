# ADR-0009: Perp venue is Coinbase US perpetual-style futures, not Hyperliquid

**Date:** 2026-08-16 · **Status:** accepted · **Source:** CHANGE-001 · **Supersedes the venue choice in** ADR-0002, 0003, 0004, 0006, 0007 (each amended in place)

## Context

The original spec put the perp leg on Hyperliquid. Two facts made that untenable once examined:

1. **The operator is a U.S. person.** Hyperliquid's terms exclude U.S. users and the app is geoblocked; trading it from the U.S. via API is a terms violation, and it would leave an on-chain trace sitting next to the named ENS wallet — the exact wallet built to be audited by a compliance-aware reviewer at a regulated exchange. That directly contradicts the project's stated purpose.
2. **Paper must validate the venue that goes live.** Paper trading Hyperliquid and then going live on anything else would make the week-5 cutover a second integration and the 30-day replay meaningless for the live book.

Coinbase now lists US perpetual-style futures on its own CFTC-regulated DCM, reachable through the same Advanced Trade API the project already needs for spot reference prices.

## Decision

The perp leg trades **Coinbase US perpetual-style futures** — nano Ether (`ETP-20DEC30-CDE`) on Coinbase Derivatives Exchange, cleared by Nodal Clear, accessed retail through Coinbase Financial Markets via Advanced Trade. It is the only perp venue for market data, paper trading, and live execution.

Hyperliquid is removed from the production path entirely. Nothing in `cmd/`, `internal/`, `deploy/`, or the config schema references it. It survives only as an optional **read-only public-data source inside `research/`** for a cross-venue funding comparison notebook — no trading, ever.

The spot leg is unchanged: long ETH on Base in the Alchemy Smart Wallet resolved from the ENS / .base.eth name.

## Alternatives considered

- **Stay on Hyperliquid** — terms violation for a U.S. person, geoblocked, and self-defeating for a wallet whose whole purpose is to survive compliance-aware review.
- **Dual-venue (paper on one, live on the other)** — doubles the funding-model, basis-definition, and adapter surface for a solo build, and breaks the "paper validates what goes live" property that makes the replay harness meaningful.
- **Offshore venue via entity/VPN** — not considered; the project's value depends on the activity being defensible, not merely executable.

## Consequences

**What got easier.** One perp venue, and it is the target employer's own product — building against its contract sizing and funding-settlement quirks is a stronger interview artifact than a crypto-native venue. The `Venue` interface (ADR-0004) absorbed the swap as designed: it is one adapter, not a rewrite. Section 1256 tax treatment may apply to the futures leg (note, not advice).

**What got harder, and is now load-bearing in the design:**

- **Contract quantization.** Size moves in whole 0.10 ETH contracts. The perp leg is sized first (floored toward zero) and the continuous Base spot leg trims residual delta to under half a contract. This changed `TargetPosition`, the risk engine, the §8 sizing formula, and the manual playbook's sizing step.
- **Funding may not be published to our API tier.** The system computes its own hourly rate from the venue's documented formula (3-minute marks → 1-hour premium TWAP ÷ 24 → 75/25 smoothing) and reconciles it against the cash adjustments actually applied. This is a requirement, not an optimization — and it is a better portfolio story than reading a field.
- **Funding accrues hourly but settles as cash twice daily**, so accrued-vs-settled is a tracked distinction (`settlement_pending_funding`, `funding_events.settled_at`).
- **No oracle.** Basis is `(futures_mark − spot_mark)/spot_mark` against Coinbase's own spot construction.
- **Margin health is read, not derived** — `margin_ratio = available_margin / liquidation_threshold` replaces liquidation-price estimation and the 15%-buffer hard stop.
- **Trading pauses Fridays 5–6 pm ET**, a guard the decision and risk engines must honor and a gap the funding series must record.
- **The perp leg leaves no on-chain trace.** On-chain history now comes from the Base spot leg and wallet operations only; the wallet-first rule (ADR-0008) still holds, but the carry story is completed by the trade log rather than being fully on-chain.

Revisit only if Hyperliquid becomes lawfully available to U.S. persons, in which case it is a second adapter behind the same interface — never a replacement for the regulated leg.
