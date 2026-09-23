# Manual Carry Playbook

**Source of truth:** [`basis-carry-build-spec.md`](basis-carry-build-spec.md) §1, §3, §10 (wallet track) · venue mechanics in [venue-coinbase-perps.md](venue-coinbase-perps.md)
**Companion docs:** [Build Plan](build-plan.md) (parallel wallet track) · [API Spec §5](api-spec.md#5-database-schema) (the schema this log mirrors)

This is the procedure for the **manual carries** — the wallet track that runs ahead of the code from week 0b through week 4 and never blocks on it. Manual carry #1 is *the reference sequence the code must reproduce*; every later component (paper engine, decision engine, live adapters, backtester) is validated against what is logged here. Log accordingly: the spreadsheet columns below deliberately mirror the `positions` / `fills` / `funding_events` tables so this data can be loaded and compared once the code exists.

**Wallet doctrine applies to every step** (spec §1): one named wallet, small size, real trades, readable history, nothing unrelated. Do not size up to look impressive. Leverage stays ≤ 3× on overnight margin, and intraday margin is never opted in.

Note on what is visible where: the **spot leg is on-chain** from the named wallet and is the auditable half of the story. The **perp leg is off-chain** at a CFTC-regulated venue, so the log below *is* its record — keep it accurate enough that a reviewer could reconcile it against account statements.

---

## Prerequisites (week 0a, once)

- [ ] ENS name registered; `.base.eth` name claimed; **both resolve to the same address**.
- [ ] Wallet funded on Base with small ETH (gas) + USDC (working capital).
- [ ] Coinbase futures account (CFM) applied for and approved; perps onboarding completed in Advanced Trade.
- [ ] Coinbase spot (CBI) account funded — cash auto-transfers to CFM when a futures order needs margin.
- [ ] `ETP-20DEC30-CDE` confirmed tradable in the account (place and cancel a far-off limit order).
- [ ] Intraday margin **not** opted in (leave the default; verify in Set Intraday Margin Setting).
- [ ] Record overnight margin per contract and the current fee tier; fill in the `TODO(verify)` items in [venue-coinbase-perps.md](venue-coinbase-perps.md).
- [ ] Log spreadsheet created from the template below.

## Sizing — contracts first

This is the step that differs most from a crypto-native perp venue. **Perp size is quantized: one contract = 0.10 ETH, integer contracts only.** So the perp leg leads and the spot leg follows:

1. Pick a contract count `N` small enough that total loss would be annoying, not painful. At ETH ≈ $4,000, one contract ≈ $400 notional — `N = 1` is a legitimate first carry.
2. **Perp leg:** short `N` contracts of `ETP-20DEC30-CDE` = `N × 0.10` ETH of short exposure.
3. **Spot leg:** buy exactly `N × 0.10` ETH on Base. The spot leg is continuous, so it can match the perp leg exactly — that is why it is the leg used to trim delta.
4. Keep effective leverage ≤ 3× on overnight margin, and confirm margin health leaves comfortable headroom at this size — see the box below for which number to read.

> **Which "margin ratio" — the app shows one, the API computes another, and they point opposite ways.**
> Coinbase's **Margin Ratio widget** is `maintenance margin / funds for margin` as a percentage: **0% flat, 100% = liquidation, higher is worse.** The API's developer-docs ratio is `available_margin / liquidation_threshold`: **higher is safer, 1.0 = liquidation.** They are not exact reciprocals; do not convert between them. Definitions and sources: [venue doc §4.1](venue-coinbase-perps.md#41-margin-ratio-names-two-opposite-quantities--read-this-before-using-either).
>
> **While carrying, record both, plus the API's `liquidation_buffer_percentage`** (the figure the system's hard stop will read — **0% = liquidation**). These fields only mean anything on an account holding a position, `cfm/balance_summary` returns current state and no history, and [Part 5A](build-plan.md#part-5a--balance-summary-field-completion) has not shipped — so **every carry run without these readings is a window that cannot be backfilled.** One of these snapshots is also what settles whether the buffer percentage arrives as `0.33` or `33`; the system's floor cannot be set until it does.

Do not try to express a notional target that isn't a whole number of contracts — round **down**, never up.

## Entry procedure (spot first, perp second)

**1. Pre-trade snapshot** — record before touching anything:

- Funding: current hourly rate and whether it's elevated versus recent history. Enter only if funding is **positive** and looks persistent, and expected carry over your intended hold clears round-trip costs with margin (spec rule: `expected_carry ≥ k × costs`, k ≥ 2 — estimate costs as both legs' fees + expected slippage + gas). If the venue doesn't display a rate, compute it from the formula in the venue doc §3 or take the system's estimate once Part 5 exists.
- Futures mark, spot mark, basis `(futures_mark − spot_mark)/spot_mark` — should be positive (future rich) and not extreme (> 0.7% = stand aside; you'd be entering into a dislocation).
- Spread on the perp book; ETH/USDC spot price on Base.
- Time until the Friday 5–6 pm ET maintenance break — don't open right into it.
- From week 3: what the engine's advisory signal says, and whether you're following it.

**2. Spot leg** — swap USDC → ETH on a Base DEX aggregator from the named wallet, targeting exactly `N × 0.10` ETH. Record: tx hash, USDC in, ETH out, effective price, gas paid, quoted-vs-effective slippage.

**3. Perp leg — immediately after.** Short `N` contracts on Coinbase, limit order at or near mid (don't cross a wide spread). Record: order id, fill price, fee, timestamp. If the perp leg can't fill within ~15 minutes at a sane price, **unwind the spot leg** rather than sitting on naked long delta — this mirrors the system's leg-risk rule.

**4. Entry verification** — net delta ≈ 0 (spot ETH ≈ `N × 0.10`), effective leverage ≤ 3×, margin health comfortable. Record the entry-complete snapshot.

- [ ] **BLOCKING — the margin triple.** Three numbers, **one instant**, all verbatim: the Coinbase app's **Margin Ratio widget** percentage; the API's **`liquidation_buffer_percentage`** string exactly as returned; and the **derived ratio** `available_margin / liquidation_threshold`. This carries the same status as the hard-stop checks — not a note to make if convenient. Taken together at one instant they are evidence; taken minutes apart they are three numbers nobody can line up afterwards, because the account moved between them.

Why it blocks: these fields mean nothing on an empty account, `cfm/balance_summary` returns current state and **no history**, and [Part 5A](build-plan.md#part-5a--balance-summary-field-completion) has not shipped. A carry run without the triple is an observation window no backfill can recover — and the **first** carry that captures it is very likely the only cheap chance to settle the scale question, since every later one costs a live position to reproduce.

## Hold procedure

Hold **≥ 24h, across several funding hours and at least one settlement**, so the log captures the accrual-vs-settlement distinction the code has to model.

- Funding accrues hourly but is **credited/debited as cash twice daily**. Log both: the hourly accrual you expect (`N × 0.10 × mark × rate`) and the actual cash adjustments when they land, with timestamps. The gap between them is exactly what `settlement_pending_funding` and `carry_funding_reconciliation_error` track in the system.
- No funding is published for the Friday maintenance hour — record it as a gap, not a zero.
- Daily check: funding still positive? basis blown out (> 0.7%)? margin health still comfortable — app widget well under 100%, `liquidation_buffer_percentage` well above 0%? **Log the full triple each time**, same instant as before.
- **Check directional consistency across consecutive triples**, and note any violation in the log: as the reported **buffer falls**, the **derived ratio must move toward its danger direction** (down, toward 1.0) and the **app widget must rise** (toward 100%). All three describe the same account approaching the same liquidation; they cannot disagree about which way that is.

  This is the cheap invariant that catches exactly the class of error this part just found — a quantity read with the wrong polarity looks perfectly plausible in isolation and betrays itself only against a second reading of the same thing. **One recorded triple across a real move validates it before any code enforces it**, which is the right order: Part 5A's reconciliation check asserts this, and an assertion written from observed rows is testable in a way one written from a doc is not. A violation means one of the three is being read backwards — record it and stop, rather than resolving it in favour of whichever number looks right. If funding flips negative and stays there, or basis goes extreme against the perp leg, exit — same rules the engine will use (spec §8).

## Exit procedure (perp first, spot second)

1. Snapshot the same pre-trade fields (funding, futures mark, spot mark, basis, spread).
2. **Close the perp short** (fast leg, and the only leg with liquidation risk; the brief residual is long spot). Record fill price + fee.
3. **Immediately swap ETH → USDC** on Base. Record tx hash, effective price, gas, slippage.
4. Confirm flat: no perp position, spot back to USDC. Note any funding that settles *after* the position closed.

## Post-trade

Compute and record the P&L decomposition — this is the number the paper engine and backtester must later reproduce:

```
pnl_total   = (usdc_out - usdc_in) + (futures account change)   ground truth, from balances
pnl_funding = Σ funding cash adjustments (incl. any settling after close)
pnl_fees    = perp fees (entry + exit) + DEX fees
pnl_gas     = gas on both swaps
pnl_price   = spot leg P&L + perp leg P&L                        (basis drift; should be small)
check:        pnl_funding - pnl_fees - pnl_gas + pnl_price ≈ pnl_total
```

Then write the **one-paragraph explanation** (spec wk-0b requirement): why you entered, what funding paid, what it cost, why you exited. This paragraph is the interview artifact.

## Log template

One workbook, three sheets (columns mirror the DB schema — keep names exactly):

**carries** (≈ `positions`): `carry_id, opened_at, closed_at, contracts, spot_qty, notional_usd, avg_spot_entry_px, avg_spot_exit_px, avg_perp_entry_px, avg_perp_exit_px, funding_accrued, funding_settled, fees, gas, slippage, pnl_price, pnl_total, entry_basis, exit_basis, entry_funding_rate, margin_ratio_at_entry, ui_margin_ratio_pct_at_entry, liquidation_buffer_pct_at_entry, margin_triple_ts, engine_signal (from wk 3), explanation`

> **The three `*_at_entry` margin columns and `margin_triple_ts` are required, not optional.** `margin_ratio_at_entry` is the derived API ratio (higher safer, 1.0 fatal); `ui_margin_ratio_pct_at_entry` is the Coinbase app widget (higher worse, 100% fatal); `liquidation_buffer_pct_at_entry` is the API string **verbatim, un-normalized, whatever scale it arrives in** — do not "tidy" it into a fraction or a percentage, because which one it already is, is the open question ([venue doc §4.1](venue-coinbase-perps.md#41-margin-ratio-names-two-opposite-quantities--read-this-before-using-either)). `margin_triple_ts` is the single instant all three were read at; three values without it are not a triple. A carry row is **incomplete** without all four.

Optionally add a **margin_log** sheet — `carry_id, ts, ui_margin_ratio_pct, liquidation_buffer_pct_raw, margin_ratio_derived` — one row per daily check, which turns the directional-consistency test above into a series rather than a pair of endpoints.

**fills** (≈ `fills`): `carry_id, ts, leg (spot|perp), side (buy|sell), qty (ETH for spot, contracts for perp), px, fee, tx_hash_or_order_id, quoted_px, slippage_bps`

**funding_log** (≈ `funding_events`): `carry_id, ts, rate_hourly, funding_source (venue|computed), amount_usd, settled_at, contracts, spot_mark`

## Cadence (from spec §10 — never goes backwards)

| Week | Wallet deliverable |
|---|---|
| 0b | Carry #1: full cycle, ≥ 24h, several funding hours and ≥ 1 settlement, explanation written |
| 1 | Carry #2 opened |
| 2 | Carry #2 closed |
| 3 | Carry #3, **timed by the engine's advisory ENTER/EXIT signals** (human executes; log signal vs action) |
| 5 | First automated cycle (code takes over; this playbook becomes the validation baseline) |

If a code milestone slips, the week's manual carry still happens.
