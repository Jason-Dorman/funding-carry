# Venue Facts — Coinbase US Perpetual-Style Futures (ETH)

**Single source of truth for perp-venue mechanics. Other docs link here; they do not restate numbers.**
**Last verified:** 2026-08-16 from the sources listed in §9; the WebSocket and public REST market surface re-verified against the live API 2026-08-20 (Part 4). Anything marked `TODO(verify)` was not confirmable from official docs at that date and must be checked against the live API in week 1 before it is relied on.

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
- **Computation:**
  - `Premium = TWAP( (futures_mark − spot_mark) / spot_mark / 24 , window = 1 hour, sample = 3 min )` — i.e. 20 three-minute samples averaged, then scaled by 1/24.
  - `Funding_t = 0.75 × Premium_t + 0.25 × Funding_{t−1}` (α = 0.75 smoothing).
  - `futures_mark` = 3-min VWAP of the future; if no trades, 3-min TWAP of futures mid; if no quotes, spot mark + previous (futures − spot) gap.
  - `spot_mark` = 3-min VWAP of spot; if no trades, 3-min TWAP of spot mid; if no quotes, futures mark + previous gap.
- **Payment amount per hour:** `contracts × 0.10 ETH × mark_price × funding_rate` (help-center example uses the BTC contract: 1 × 0.01 × $100,000 × 0.1% ≈ $1.00).
- **Settlement:** funding accrues hourly but is **debited/credited twice daily** as separate cash adjustments during the midday and end-of-day margin cycles (some FCMs may process once daily). Funding does not affect variation margin. `TODO(verify)` CFM's exact adjustment times and how they appear in the Advanced Trade account/fills feed.
- **Publication:** real-time and projected funding are published via CDE **FIX and SBE** market data (institutional). Historical funding is "available upon request." Coinbase's derivatives market-data page advertises funding-rate data. **Verified 2026-08-20:** the retail Advanced Trade product payload carries `future_product_details.perpetual_details.funding_rate` for `ETP-20DEC30-CDE` but returns it **empty**, with a null `funding_time`. None of the five market-data WebSocket channels carries a funding field either. The local estimator (§3, build plan Part 5) is therefore the primary source, not the fallback. Still open for Part 5: whether the *authenticated* endpoints or `fundingHistory` expose one.

**Design consequence (important):** the system must be able to **compute its own funding-rate estimate** from the published formula using Advanced Trade market data for `ETP-20DEC30-CDE` (futures mark) and `ETH-USD` spot (spot mark), and reconcile it against the funding actually applied to the account. This is required regardless of whether the API exposes an official rate, and it is a strong portfolio feature in its own right. Feature names: `funding_rate_hourly` (venue-published if available, else computed), `funding_rate_est` (always computed), `funding_annualized = funding_rate_hourly × 24 × 365`.

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

**Design consequence:** do not opt in to intraday leverage for v1. Size at ≤ 3× on overnight margin so the intraday/overnight change never triggers a margin call. Risk engine tracks `margin_ratio` directly from the balance-summary endpoint (or the `futures_balance_summary` WS channel) instead of computing a Hyperliquid-style liquidation price. `TODO(verify)` the actual overnight initial/maintenance margin per ETP contract from Get Current Margin Window in week 1.

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
| Funding rate (official) | **Verified 2026-08-20: the field exists and is empty.** `GET market/products/ETP-20DEC30-CDE` returns `future_product_details.perpetual_details.funding_rate: ""`, with `funding_time: null` and `max_leverage: ""` beside it; `open_interest` in the same block *is* populated. The retail API therefore publishes no usable rate for this product. Confirming this against the authenticated products endpoint, and against `fundingHistory`, is Part 5's. | Compute locally (§3); `funding_source = 'computed'`. |
| Balance / margin | `GET cfm/balance_summary` (fields incl. `futures_buying_power`, `total_usd_balance`, `cbi_usd_balance`, `cfm_usd_balance`, `available_margin`, `liquidation_threshold`) | Poll for margin ratio. |
| Positions | `GET cfm/positions`, `GET cfm/positions/{product_id}` (fields: `number_of_contracts`, `side`, `avg_entry_price`, `current_price`, `unrealized_pnl`, `daily_realized_pnl`, `expiration_time`) | |
| Margin window / intraday | `GET cfm/intraday/current_margin_window`, `GET/POST cfm/intraday/margin_setting` | Leave intraday off. |
| Sweeps | `POST/GET/DELETE cfm/sweeps` | Move idle margin back to spot. |
| Orders | `POST orders`, `GET orders/historical/{id}`, cancel/edit endpoints; `product_id=ETP-20DEC30-CDE`, size in contracts | Market and limit supported for US derivatives; other types `TODO(verify)`. |
| WebSocket | `wss://advanced-trade-ws.coinbase.com`. Market channels: `ticker`, `level2`, `market_trades`, `candles`, `status`, plus `heartbeats`; user channels: `user`, `futures_balance_summary` | **Verified 2026-08-20:** all five market channels accept `ETP-20DEC30-CDE` unauthenticated. Three details that cost a reconnect loop each if missed: `level2` is subscribed as `level2` and **arrives labelled `l2_data`**; `candles` is **5-minute only**, ignores a `granularity` argument, and sends a ~100-candle `snapshot` on subscribe followed by `update` messages carrying **exactly one** candle (the forming one) — so a closed candle is never re-sent and must be recognised by a newer start arriving, not by a newer start sharing its message; an unknown channel name is answered `{"type":"error","message":"authentication failure"}` followed by silence, which is misleading — it is not an auth problem. Book sides are `bid` / `offer`; the full perp book snapshot was **74 KB on the wire** (734 price levels). `market_trades` publishes a third aggressor side beside `BUY` and `SELL` — **`UNKNOWN_ORDER_SIDE`** — 330 trades over 22 hours, arriving in bursts and mostly on the replay that follows the Friday reopen. Such a trade carries a normal price and size; only its side is absent. `sequence_num` counts frames per connection, across channels. User channels remain unverified (Part 16). |
| Rate limits | returned in response headers | Log them; back off. |

Go client: no official Go SDK. Options are a thin hand-rolled REST+WS client (JWT via `golang-jwt` with ES256 over the CDP key) or a community package. Recommendation: hand-roll; the surface used is small and the auth code is worth understanding.

## 7. Onboarding checklist (milestone 0a)

1. Apply for a futures account: coinbase.com/futures or the Futures tab in Advanced Trade (CFM/NFA account, separate from spot).
2. Fund the CBI spot account; cash auto-transfers to CFM when a futures order needs margin.
3. Confirm `ETP-20DEC30-CDE` is tradable in the account (place and cancel a far-off limit order, or read the product via API).
4. Do **not** opt in to intraday margin.
5. Create a CDP API key with trade permission scoped to this account; store in `.env.private`.
6. Record: overnight margin per contract, current fee, and whether the API returns an official funding rate. Fill in the `TODO(verify)` items in this file and bump "last verified." *(The funding-rate and market-data items closed in Part 4, 2026-08-20; the margin and fee items need an account and stay open for Part 5.)*

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
