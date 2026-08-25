# Venue Facts — Coinbase US Perpetual-Style Futures (ETH)

**Single source of truth for perp-venue mechanics. Other docs link here; they do not restate numbers.**
**Self-recorded history begins 2026-08-24.** The recording from 2026-08-21 to 2026-08-24 was reset on PO direction: it carried three known defect classes (missing sampling boundaries, understated trade volume, mixed funding provenance) and self-recorded data is the primary history, so an asterisked foundation was not worth carrying into every downstream number. It is quarantined, not deleted — [`research/quarantine/`](../research/quarantine/README.md). **Spec §12 open item 4 — ≥ 30 days self-recorded before z-scores are trusted — is measured from 2026-08-24.**

**Last verified:** 2026-08-16 from the sources listed in §9; the WebSocket and public REST market surface re-verified against the live API 2026-08-20 (Part 4); the funding-rate publication, its hourly step cadence and its one-hour phase offset verified 2026-08-24 against 10 h of live polling (Part 5). Anything marked `TODO(verify)` was not confirmable from official docs at that date and must be checked against the live API in week 1 before it is relied on.

---

## 1. Product identity

| Item | Value | Source |
|---|---|---|
| Product name | nano Ether Perpetual-Style Futures (ticker root **ETP**) | S1, S3 |
| Advanced Trade product ID | `ETP-20DEC30-CDE` | S3 (Coinbase product page URL and Advanced Trade futures URL) |
| Exchange | Coinbase Derivatives Exchange (CDE), CFTC-regulated DCM | S1, S2 |
| Clearing | Nodal Clear (CFTC-regulated DCO) | S4, S5 |
| Retail broker | Coinbase Financial Markets (CFM), NFA-member FCM; accessed through Coinbase Advanced Trade | S6, S7 |
| Also listed | nano BTC (BIP, 0.01 BTC), nano SOL (SLP, 5 SOL), nano XRP (XPP, 500 XRP) | S1 |

**Expiry:** the current help-center contract spec page says the contract has no expiration date and no final settlement date; only one contract is listed at a time. Older launch materials described a 5-year horizon (hence `20DEC30` in the product ID). Treat as effectively perpetual; do not build roll logic. `TODO(verify)` whether the product ID will ever change; store `product_id` in config, not code.

## 2. Contract specification

| Item | Value | Source |
|---|---|---|
| Contract size | **0.10 ETH** per contract | S1 (help center); S3 |
| Tick size | $0.50 (index points) | S1 |
| Tick value | $0.05 per contract per tick | S1 |
| Settlement | cash-settled in USD | S3 |
| Min order | 1 contract (integer contracts only) | S1 (implied by fixed size); `TODO(verify)` no fractional contracts via API |
| Trading hours | 24/7 except a maintenance break **Fridays 5:00–6:00 pm ET** | S1, S3 |

Note: MarketsWiki lists 1/100 ETH; the Coinbase help center and product page both say 1/10 ETH. Use **0.10 ETH**. (One FOW article at launch also said 0.10 ETH.)

**Implication for delta:** at ETH ≈ $4,000 one contract ≈ $400 notional. Perp size is quantized to 0.10 ETH; the Base spot leg is the fine-grained leg used to trim residual delta. Risk engine `delta_tolerance` should be ≤ 0.05 ETH (half a contract).

## 3. Funding

Everything here is from the official funding-rate help page (S4).

- **Sign convention:** positive funding → longs pay shorts (standard).
- **Frequency:** funding rate is calculated **hourly**. If the market is closed for the entire hour (Friday maintenance), no rate is published for that hour.
- **Publication cadence and phase (verified 2026-08-24 over 10 h of 5-second polling):** the published rate is a **step function that changes exactly at the top of the hour** and holds constant across it, so an hourly average of the live series is exact rather than a smoothing; consecutive hours may repeat a value unchanged. The value visible *during* hour *H* describes the hour that just ended. It is therefore **one hour behind** a locally computed rate stamped with the hour whose windows produced it: `funding_events.ts = H` lines up with the venue's value published at *H+1*, not at *H*. Aligning that way across the first 9 live hours halved both the mean gap (1.88 → 1.10 percentage points annualised) and its scatter (sd 2.89 → 1.50 pp). **Compare like with like before reading a difference as `carry_funding_reconciliation_error`.**
- **Computation:**
  - `Premium = TWAP( (futures_mark − spot_mark) / spot_mark / 24 , window = 1 hour, sample = 3 min )` — i.e. 20 three-minute samples averaged, then scaled by 1/24.
  - `Funding_t = 0.75 × Premium_t + 0.25 × Funding_{t−1}` (α = 0.75 smoothing).
  - `futures_mark` = 3-min VWAP of the future; if no trades, 3-min TWAP of futures mid; if no quotes, spot mark + previous (futures − spot) gap.
  - `spot_mark` = 3-min VWAP of spot; if no trades, 3-min TWAP of spot mid; if no quotes, futures mark + previous gap.
- **Payment amount per hour:** `contracts × 0.10 ETH × mark_price × funding_rate` (help-center example uses the BTC contract: 1 × 0.01 × $100,000 × 0.1% ≈ $1.00).
- **Settlement:** funding accrues hourly but is **debited/credited twice daily** as separate cash adjustments during the midday and end-of-day margin cycles (some FCMs may process once daily). Funding does not affect variation margin. `TODO(verify)` CFM's exact adjustment times and how they appear in the Advanced Trade account/fills feed.
- **Publication: the retail API DOES publish an hourly funding rate.** Corrected 2026-08-24. `GET market/products/ETP-20DEC30-CDE` returns, at the **`future_product_details`** level:

  | Field | Observed |
  |---|---|
  | `funding_rate` | `0.000019` (hourly; ×24×365 ≈ **16.6% annualised**) |
  | `funding_time` | `2026-08-24T03:00:00Z` — the next funding boundary |
  | `funding_interval` | `3600s`, confirming the hourly cadence |

  **The trap that cost two build parts:** `future_product_details.perpetual_details` carries fields of the *same names* — `funding_rate` and `funding_time` — and they are **permanently empty** for this contract. Reading the inner pair and concluding the venue publishes nothing is a mistake this project made and recorded as a verified fact until an adversarial review caught it. Read the outer fields; keep the inner as a fallback only.

  Real-time and projected funding are also published via CDE **FIX and SBE** market data (institutional), and historical funding is "available upon request" — but the REST field above is enough for the live rate. What it does **not** provide is *history*: only the current rate, so the reconstruction from candles ([ADR-0015](decisions/0015-backfilled-funding-provenance.md)) remains the only route to a back-series.

**Design consequence (important):** the system computes its own funding-rate estimate from the published formula using Advanced Trade market data for `ETP-20DEC30-CDE` (futures mark) and `ETH-USD` spot (spot mark), *and* records the venue's published rate beside it. The estimate is not redundant now that the venue's rate is known to exist — it is what makes the venue's number checkable, it is the only way to obtain history, and their difference is exactly what `carry_funding_reconciliation_error` measures. `funding_rate_hourly` takes the venue's rate with `funding_source='venue'`; `funding_rate_est` is always the local computation. Feature names: `funding_rate_hourly` (venue-published if available, else computed), `funding_rate_est` (always computed), `funding_annualized = funding_rate_hourly × 24 × 365`.

Interval mapping vs the old spec: `funding_interval = 1h`. `cumulative_funding` sums hourly rates over hours held. `expected_carry_N_hours = notional × Σ expected hourly rates`. Basis features use **spot mark**, not "oracle" or "index": `basis = (futures_mark − spot_mark) / spot_mark`.

## 4. Margin, leverage, liquidation

| Item | Value | Source |
|---|---|---|
| Max leverage | up to **10× intraday**; lower overnight | S2, S7 |
| Intraday window | 8:00 am–4:00 pm ET, only if opted in via Set Intraday Margin Setting | S7 |
| Margin ratio | `margin_ratio = available_margin / liquidation_threshold`, fields from Get Futures Balance Summary | S7 |
| Liquidation | auto-liquidation if margin health insufficient; both intraday and overnight health returned when opted in | S7 |
| Cash treatment | cash deposits land in the CBI spot account; auto-transferred to the CFM futures account for margin; sweeps back via Schedule Futures Sweep | S7 |
| Fund segregation | futures margin held at CFM (CFTC customer protections); spot at CBI (not CFTC-protected) | S7 |

**Observed at the Friday close (2026-08-21, 17:00 ET / 21:00 UTC).** The transition is visible in the market data a few seconds ahead of the calendar: the perp's quoted spread widened from ~4 bps to **65.6 bps** at 20:59:55 as makers pulled, the ticker then froze, and within thirty seconds the venue was publishing ticker messages with **empty `best_bid`/`best_ask`** — the message keeps arriving, the quote inside it is gone. Anything deriving a mid must treat that as absent rather than falling back to the last trade price, which continues to be populated. The `status` channel kept reporting `online` throughout, so the calendar, not the status channel, is what identifies this window.

**Design consequence:** do not opt in to intraday leverage for v1. Size at ≤ 3× on overnight margin so the intraday/overnight change never triggers a margin call. Risk engine tracks `margin_ratio` directly from the balance-summary endpoint (or the `futures_balance_summary` WS channel) instead of computing a Hyperliquid-style liquidation price. **Verified 2026-08-24** from the public product payload, which publishes both rates directly: `overnight_margin_rate` = `{long 0.24525, short 0.33475}` and `intraday_margin_rate` = `{long 0.100062, short 0.0998894}`. The short rate is the one this system sizes against. The same payload carries `index_price` and `settlement_price`.

## 5. Fees and taxes

- Advanced Trade futures guide states futures use the Advanced Trade fee structure; an older "introductory beta" note says 0.05% (lowest tier). `TODO(verify)` current CFM per-contract or bps fee for non-professional users via the Fees / Transaction Summary endpoint (`product_type=FUTURE`, `product_venue=FCM`). CDE also has its own fee schedule by participant type.
- Tax: product pages state 60/40 (Section 1256) treatment for perpetual-style futures. Note only; not advice. Spot ETH on Base is not 1256.

## 6. API surface (retail, Advanced Trade)

Auth: CDP API key + JWT (`Authorization: Bearer <token>`); same scheme for REST and WS **where auth is needed at all**. Verified 2026-08-20: the public market REST endpoints (`market/products/...`) and all five market-data WebSocket channels serve `ETP-20DEC30-CDE` with no credential, so `cmd/ingest` holds none. The account, sweep and order endpoints do require it. Base URL `https://api.coinbase.com/api/v3/brokerage/`.

| Need | Endpoint / channel | Notes |
|---|---|---|
| Product metadata | `GET products` filtered `product_type=FUTURE`, `contract_expiry_type=PERPETUAL`, venue `FCM`; `GET products/{product_id}` | Public variants exist without auth. **Verified 2026-08-20** on `market/products/ETP-20DEC30-CDE`: `future_product_details.contract_size` is `0.1`, `venue` `cde`, `price_increment` `0.5`, `base_increment` `1` (whole contracts), `product_venue` `FCM`, and `fcm_trading_session_details` carries `session_state` and a `maintenance` field. Note `contract_expiry_type` reads `EXPIRING` with a `2030-12-20` expiry, not `PERPETUAL` — a filter on `PERPETUAL` would not return this product. |
| Candles, ticker, book, trades | `products/{id}/candles`, `best_bid_ask`, `product_book`, `market_trades` (public variants available) | Used for futures mark computation and Tier-2 microstructure. **Verified 2026-08-20:** `market/products/{id}/candles?granularity=ONE_MINUTE` does serve one-minute candles unauthenticated — it is where 1m bars come from, since the WS channel serves 5m only. |
| Spot reference | same endpoints on `ETH-USD` | Used for spot mark. |
| Funding rate (official) | **Verified 2026-08-24: the retail API publishes it — see §3.** The rate is at `future_product_details.funding_rate`; the same-named field one level in, at `future_product_details.perpetual_details.funding_rate`, is permanently empty for this contract. The entry that stood here until 2026-08-24 read only the inner field and concluded the venue published nothing — it was wrong, and §3 records what the mistake cost. | Record the venue's rate with `funding_source='venue'`; keep computing locally in parallel (§3), which is the only route to history. |
| Balance / margin | `GET cfm/balance_summary` (fields incl. `futures_buying_power`, `total_usd_balance`, `cbi_usd_balance`, `cfm_usd_balance`, `available_margin`, `liquidation_threshold`) | Poll for margin ratio. **Verified 2026-08-23** with a view-only Ed25519 CDP key: returns 200 and a single `balance_summary` object. |
| Positions | `GET cfm/positions`, `GET cfm/positions/{product_id}` (fields: `number_of_contracts`, `side`, `avg_entry_price`, `current_price`, `unrealized_pnl`, `daily_realized_pnl`, `expiration_time`) | **Verified 2026-08-23:** 200, a single `positions` key. |
| Margin window / intraday | `GET cfm/intraday/current_margin_window`, `GET/POST cfm/intraday/margin_setting` | Leave intraday off. **Verified 2026-08-23:** `margin_setting` returns 200 and a single `setting` key. |
| Sweeps | `POST/GET/DELETE cfm/sweeps` | Move idle margin back to spot. |
| Orders | `POST orders`, `GET orders/historical/{id}`, cancel/edit endpoints; `product_id=ETP-20DEC30-CDE`, size in contracts | Market and limit supported for US derivatives; other types `TODO(verify)`. |
| WebSocket | `wss://advanced-trade-ws.coinbase.com`. Market channels: `ticker`, `level2`, `market_trades`, `candles`, `status`, plus `heartbeats`; user channels: `user`, `futures_balance_summary` | **Verified 2026-08-20:** all five market channels accept `ETP-20DEC30-CDE` unauthenticated. Three details that cost a reconnect loop each if missed: `level2` is subscribed as `level2` and **arrives labelled `l2_data`**; `candles` is **5-minute only**, ignores a `granularity` argument, and sends a ~100-candle `snapshot` on subscribe followed by `update` messages carrying **exactly one** candle (the forming one) — so a closed candle is never re-sent and must be recognised by a newer start arriving, not by a newer start sharing its message; an unknown channel name is answered `{"type":"error","message":"authentication failure"}` followed by silence, which is misleading — it is not an auth problem. Book sides are `bid` / `offer`; the full perp book snapshot was **74 KB on the wire** (734 price levels). `market_trades` publishes a third aggressor side beside `BUY` and `SELL` — **`UNKNOWN_ORDER_SIDE`** — 330 trades over 22 hours, arriving in bursts and mostly on the replay that follows the Friday reopen. Such a trade carries a normal price and size; only its side is absent. `sequence_num` counts frames per connection, across channels. User channels remain unverified (Part 16). |
| Rate limits | returned in response headers | Log them; back off. |

**None of the `cfm/*` endpoints takes a portfolio parameter, and none needs one** (verified 2026-08-23). They are scoped to the futures account the key belongs to. `GET portfolios` on this account lists exactly one, of type `DEFAULT` — portfolios are a spot-side concept here. `CB_PORTFOLIO_ID` is therefore **not required for Part 5**; it stays in the config surface for the order-entry work in Part 16, where a portfolio may need naming explicitly.

**Auth verified end to end 2026-08-23.** A CDP **Secret** API key, Ed25519, signed **EdDSA**, `kid` and `sub` set to the bare key-id UUID the portal now issues (not the legacy `organizations/.../apiKeys/...` name), and a `uri` claim of `GET api.coinbase.com/api/v3/brokerage/<path>` — host and path, no scheme, no query. `internal/coinbase` mints these; `go test -tags live ./internal/coinbase/` re-checks the whole chain against the venue.

Go client: no official Go SDK. **Decided 2026-08-24 — hand-rolled** ([ADR-0016](decisions/0016-hand-rolled-coinbase-client.md)): the surface used is under a dozen endpoints, and a wrapper large enough to cover the whole API would have to be audited for the same reason the credential does. JWT is **EdDSA over Ed25519** using `crypto/ed25519` from the standard library — not `golang-jwt`/ES256, which the CDP portal marks as the legacy path.

## 7. Onboarding checklist (milestone 0a)

1. Apply for a futures account: coinbase.com/futures or the Futures tab in Advanced Trade (CFM/NFA account, separate from spot).
2. Fund the CBI spot account; cash auto-transfers to CFM when a futures order needs margin.
3. Confirm `ETP-20DEC30-CDE` is tradable in the account (place and cancel a far-off limit order, or read the product via API).
4. Do **not** opt in to intraday margin.
5. Create a CDP API key at `portal.cdp.coinbase.com` and store it in `.env.private` (gitignored). Two choices that are easy to get wrong: it must be a **Secret API Key**, not a Client API Key — a client key is a public identifier for browser-side SDKs and cannot sign a request, so it will not authenticate `cfm/*` at all — and where a signature algorithm is offered, **Ed25519 (EdDSA)** — the portal marks ECDSA as being for legacy SDKs, and a new key has no legacy to be compatible with. Scope it **view-only** for the read-only work in Part 5; the trade-scoped key belongs to Part 16, alongside the kill switch, so a key that can send an order does not exist before something can stop it. The private key is displayed once.
6. Record: overnight margin per contract, current fee, and whether the API returns an official funding rate. Fill in the `TODO(verify)` items in this file and bump "last verified." *(Market-data items closed in Part 4, 2026-08-20. The funding-rate item was closed **wrongly** on that date — see the §9 row — and genuinely closed 2026-08-24. Margin and fee remain open: a view-only CDP key reaches `cfm/balance_summary` and `cfm/positions` but gets **401 on `transaction_summary`**, so the fee tier is still unread, and `overnight_margin_rate` is fetched but not yet persisted — there is no column for it.)*

## 8. Differences from Hyperliquid that touch the code (summary for CHANGE-001)

| Topic | Hyperliquid (old) | Coinbase US perps (new) |
|---|---|---|
| Size | any size ≥ min | integer contracts × 0.10 ETH |
| Funding cadence | hourly, from 8h premium sampled 5s, 4%/hr cap | hourly, from 1h TWAP of 3-min futures-vs-spot premium ÷ 24, 75/25 EMA; no documented cap |
| Funding settlement | hourly, on-chain | accrues hourly, cash-adjusted twice daily off-chain |
| Reference price | oracle | spot mark (Coinbase spot VWAP/TWAP) |
| Margin driver | mark price, liquidation price | margin ratio from balance summary; intraday vs overnight windows |
| Max leverage | per-asset, high | 10× intraday (opt-in), lower overnight |
| Hours | 24/7 | 24/7 except Fri 5–6 pm ET |
| Funding data | public WS/REST | FIX/SBE institutional; retail REST returns the field empty (verified 2026-08-20); compute locally |
| Order entry | signed L1 actions | Advanced Trade REST (JWT); FIX only via FCM/institutional |
| On-chain trace of perp | yes (HL L1) | none |

## 9. Sources

- S1 — Coinbase Help, "US Perpetual-Style Futures Contract Specifications": https://help.coinbase.com/en/derivatives/perpetual-style-futures/contract-specifications
- S2 — Coinbase Learn, "US Perpetual-Style Futures 101": https://www.coinbase.com/learn/futures/us-perpetual-style-futures-101
- S3 — Coinbase product page, nano Ether Perpetual-Style Futures: https://www.coinbase.com/futures/etp-20dec30-cde ; Advanced Trade URL https://coinbase.com/advanced-trade/futures/ETP-20DEC30-CDE
- S4 — Coinbase Help, "US Perpetual-Style Futures Funding Rate Mechanism": https://help.coinbase.com/en/derivatives/perpetual-style-futures/funding-rate
- S5 — Coinbase Help, "US Perpetual-Style Futures Market Access": https://help.coinbase.com/en/derivatives/perpetual-style-futures/market-access
- S6 — Coinbase blog, "Coming July 21: US Perpetual-Style Futures": https://www.coinbase.com/blog/coming-july-21-us-perpetual-style-futures
- S7 — Coinbase Developer Docs, "Advanced Trade US Derivatives" guide: https://docs.cdp.coinbase.com/coinbase-app/advanced-trade-apis/guides/futures ; Futures endpoints reference: https://docs.cdp.coinbase.com/api-reference/advanced-trade-api/rest-api/futures/get-futures-balance-summary
- S8 — Coinbase Developer Docs, "Advanced Trade Perpetual Futures" (INTX, non-US — for contrast only): https://docs.cdp.coinbase.com/coinbase-business/advanced-trade-apis/guides/perpetual
- S9 — coinbase/coinbase-advanced-py README (WS channels incl. `futures_balance_summary`): https://github.com/coinbase/coinbase-advanced-py
