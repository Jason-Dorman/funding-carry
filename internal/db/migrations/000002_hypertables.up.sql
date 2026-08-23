-- Time-series tables (API spec section 5.1). Every one is a hypertable
-- partitioned on ts, with a one-day chunk interval: at the poll and tick rates
-- this project runs, a day is a few thousand rows per table, and a month of
-- self-recorded history — the minimum a funding z-score needs — stays around
-- thirty chunks rather than four.
--
-- Money and sizes are `numeric` with no declared precision, which in Postgres
-- means arbitrary precision. Nothing here is float, ever (spec section 11).
--
-- Columns that a poll or a tick may legitimately not have are nullable: an
-- absent mark or an unfilled rolling window must read as NULL, never as zero.

-- Venue snapshot: the marks, the funding rate and its provenance, the premium
-- and the spread, sampled by the WS handlers and the REST poller.
CREATE TABLE cb_venue_state (
    ts                  timestamptz NOT NULL,
    product_id          text        NOT NULL,
    futures_mark        numeric,
    spot_mark           numeric,
    mid                 numeric,
    funding_rate_hourly numeric,
    funding_rate_est    numeric,
    funding_source      text,
    funding_annualized  numeric,
    premium_proxy       numeric,
    spread_bps          numeric,
    open_interest       numeric,
    maintenance_window  boolean     NOT NULL DEFAULT false,
    -- funding_source is a closed set: either the venue published the rate or we
    -- computed it. A third value would silently break reconciliation.
    CONSTRAINT cb_venue_state_funding_source_check
        CHECK (funding_source IS NULL OR funding_source IN ('venue', 'computed'))
);
SELECT create_hypertable('cb_venue_state', by_range('ts', INTERVAL '1 day'));
CREATE INDEX cb_venue_state_product_ts_idx ON cb_venue_state (product_id, ts DESC);

-- Completed candles only. The unique key is what makes REST backfill idempotent:
-- re-running it inserts nothing new (build plan Part 5).
CREATE TABLE cb_bars (
    ts          timestamptz NOT NULL,   -- bar close time
    product_id  text        NOT NULL,
    tf          text        NOT NULL,   -- '5m' from the WS candles channel, '1m' from REST
    open        numeric     NOT NULL,
    high        numeric     NOT NULL,
    low         numeric     NOT NULL,
    close       numeric     NOT NULL,
    volume      numeric     NOT NULL,
    trade_count integer
);
SELECT create_hypertable('cb_bars', by_range('ts', INTERVAL '1 day'));
CREATE UNIQUE INDEX cb_bars_product_tf_ts_idx ON cb_bars (product_id, tf, ts DESC);

-- Periodic top-N book snapshot rather than every level2 update: the feature
-- engine needs imbalance and impact price, not a full book replay.
CREATE TABLE cb_book_snapshots (
    ts              timestamptz NOT NULL,
    product_id      text        NOT NULL,
    best_bid        numeric,
    best_ask        numeric,
    bid_px          numeric[],
    bid_depth       numeric[],
    ask_px          numeric[],
    ask_depth       numeric[],
    imbalance_top_n numeric,
    impact_bid_px   numeric,
    impact_ask_px   numeric
);
SELECT create_hypertable('cb_book_snapshots', by_range('ts', INTERVAL '1 day'));
CREATE INDEX cb_book_snapshots_product_ts_idx ON cb_book_snapshots (product_id, ts DESC);

-- Bucketed trade aggregates. These carry the VWAP the funding estimator marks
-- against, so the bucket length is part of the key, not just a column.
CREATE TABLE cb_trades_agg (
    ts            timestamptz NOT NULL,   -- bucket end
    product_id    text        NOT NULL,
    bucket_secs   integer     NOT NULL,
    buy_vol       numeric     NOT NULL,
    sell_vol      numeric     NOT NULL,
    trade_count   integer     NOT NULL,
    vwap          numeric,
    max_single_sz numeric,
    sweep_count   integer
);
SELECT create_hypertable('cb_trades_agg', by_range('ts', INTERVAL '1 day'));
CREATE UNIQUE INDEX cb_trades_agg_product_bucket_ts_idx
    ON cb_trades_agg (product_id, bucket_secs, ts DESC);

-- One row per decision tick: everything the decision engine looked at, so a
-- decision can be re-derived from storage rather than from memory.
CREATE TABLE cb_features (
    ts                       timestamptz NOT NULL,
    product_id               text        NOT NULL,
    -- Tier 1: funding and basis
    funding_rate_hourly      numeric,
    funding_rate_est         numeric,
    funding_source           text,
    funding_annualized       numeric,
    funding_zscore           numeric,
    cumulative_funding       numeric,
    expected_carry_n_hours   numeric,
    futures_mark             numeric,
    spot_mark                numeric,
    basis                    numeric,
    trade_premium            numeric,
    time_to_next_funding_secs integer,
    -- Tier 2: microstructure
    spread_bps               numeric,
    tob_imbalance            numeric,
    impact_imbalance         numeric,
    trade_imbalance          numeric,
    sweep_intensity          numeric,
    mid_mark_dist            numeric,
    slippage_est_bps         numeric,
    -- Tier 3: price
    log_ret_1m               numeric,
    ema_fast                 numeric,
    ema_slow                 numeric,
    atr                      numeric,
    realized_vol             numeric,
    vwap                     numeric,
    momentum_slope           numeric,
    -- Risk
    margin_ratio             numeric,
    margin_ratio_at_target   numeric,
    effective_leverage       numeric,
    net_delta                numeric,
    residual_delta           numeric,
    contracts_held           bigint,
    notional                 numeric,
    -- Pressure (appended by the pressure engine, Part 11)
    crowded_side             text,
    pressure_level           text,
    expected_pain            numeric,
    exhaustion_flag          boolean,
    CONSTRAINT cb_features_funding_source_check
        CHECK (funding_source IS NULL OR funding_source IN ('venue', 'computed')),
    CONSTRAINT cb_features_crowded_side_check
        CHECK (crowded_side IS NULL OR crowded_side IN ('LONG', 'SHORT', 'NONE')),
    -- The z-score bands from spec section 6.4. A level outside this set means the
    -- band mapping changed without the schema being updated with it.
    CONSTRAINT cb_features_pressure_level_check
        CHECK (pressure_level IS NULL
               OR pressure_level IN ('NORMAL', 'ELEVATED', 'EXTREME', 'FORCED'))
);
SELECT create_hypertable('cb_features', by_range('ts', INTERVAL '1 day'));
CREATE INDEX cb_features_product_ts_idx ON cb_features (product_id, ts DESC);

-- The Base side of the book: wallet inventory, reference price, gas.
CREATE TABLE base_state (
    ts          timestamptz NOT NULL,
    spot_px     numeric,
    wallet_eth  numeric,
    wallet_usdc numeric,
    gas_gwei    numeric
);
SELECT create_hypertable('base_state', by_range('ts', INTERVAL '1 day'));

-- Polled account and margin snapshot. The risk engine reads margin_ratio from
-- here rather than deriving a liquidation price of its own, and the treasury
-- reconciles the two cash balances against it.
CREATE TABLE cb_account_state (
    ts                      timestamptz NOT NULL,
    available_margin        numeric,
    liquidation_threshold   numeric,
    margin_ratio            numeric,
    cfm_usd_balance         numeric,
    cbi_usd_balance         numeric,
    futures_buying_power    numeric,
    contracts_held          bigint,
    avg_entry_price         numeric,
    unrealized_pnl          numeric,
    -- Asserted false in v1: intraday margin is never opted into, so the
    -- intraday-to-overnight transition can never trigger a call.
    intraday_margin_enabled boolean
);
SELECT create_hypertable('cb_account_state', by_range('ts', INTERVAL '1 day'));
