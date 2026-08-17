-- State tables (API spec section 5.2): plain Postgres tables, not hypertables.
-- They are keyed by identity rather than by time, and two of them carry foreign
-- keys, which hypertables cannot be the target of.

-- Product metadata, refreshed from the products endpoint by the REST poller.
-- Seeded at migrate time with the configured perp product id and contract size
-- and nothing else: the remaining columns stay NULL until Part 5 fills them from
-- the venue, so a placeholder can never be mistaken for a verified number.
CREATE TABLE cb_products (
    product_id             text PRIMARY KEY,
    contract_size          numeric,
    tick                   numeric,
    tick_value             numeric,
    max_leverage_overnight numeric,
    max_leverage_intraday  numeric,
    maker_fee_bps          numeric,
    taker_fee_bps          numeric,
    status                 text,
    updated_at             timestamptz NOT NULL DEFAULT now()
);

-- One row per carry, bucketed by venue so paper, sim and live P&L never mix.
CREATE TABLE positions (
    id                         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    opened_at                  timestamptz NOT NULL,
    closed_at                  timestamptz,
    venue                      text        NOT NULL,
    spot_qty                   numeric     NOT NULL,
    perp_contracts             bigint      NOT NULL,
    avg_spot_px                numeric,
    avg_perp_px                numeric,
    accrued_funding            numeric     NOT NULL DEFAULT 0,
    -- Accrued but not yet cash-adjusted. Funding accrues hourly and settles
    -- twice daily, so this is non-zero for most of a position's life.
    settlement_pending_funding numeric     NOT NULL DEFAULT 0,
    fees                       numeric     NOT NULL DEFAULT 0,
    slippage                   numeric     NOT NULL DEFAULT 0,
    realized_pnl               numeric,
    status                     text        NOT NULL,
    CONSTRAINT positions_venue_check CHECK (venue IN ('paper', 'sim', 'live'))
);
CREATE INDEX positions_open_idx ON positions (venue, opened_at DESC) WHERE closed_at IS NULL;

-- Every emitted TargetPosition, persisted before it is acted on. input_snapshot
-- holds the whole FeatureSnapshot so the decision can be re-derived later; its
-- encoding is a contract, not a detail (API spec section 3.5).
CREATE TABLE decisions (
    id               bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    ts               timestamptz NOT NULL,
    state            text        NOT NULL,
    target_spot      numeric,
    target_contracts bigint,
    residual_delta   numeric,
    reason_codes     text[]      NOT NULL,
    confidence       numeric,
    input_snapshot   jsonb       NOT NULL,
    CONSTRAINT decisions_state_check
        CHECK (state IN ('ENTER', 'HOLD', 'REBALANCE', 'EXIT', 'BLOCKED'))
);
CREATE INDEX decisions_ts_idx ON decisions (ts DESC);

-- Fills from every venue implementation. The unique index on the venue's own
-- execution id is what makes a FIX resend idempotent: replaying an
-- ExecutionReport after a reconnect inserts nothing new (build plan Part 8).
CREATE TABLE fills (
    id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    ts            timestamptz NOT NULL,
    position_id   bigint REFERENCES positions (id),
    cl_ord_id     text        NOT NULL,
    venue         text        NOT NULL,
    leg           text        NOT NULL,
    side          text        NOT NULL,
    qty           numeric     NOT NULL,
    px            numeric     NOT NULL,
    fee           numeric,
    exec_state    text        NOT NULL,
    venue_exec_id text,
    raw           jsonb,
    CONSTRAINT fills_leg_check CHECK (leg IN ('spot', 'perp')),
    CONSTRAINT fills_side_check CHECK (side IN ('buy', 'sell')),
    CONSTRAINT fills_exec_state_check
        CHECK (exec_state IN ('NEW', 'PARTIAL', 'FILLED', 'CANCELED', 'REJECTED'))
);
CREATE UNIQUE INDEX fills_venue_exec_id_idx ON fills (venue, venue_exec_id)
    WHERE venue_exec_id IS NOT NULL;
CREATE INDEX fills_position_idx ON fills (position_id, ts DESC);
CREATE INDEX fills_cl_ord_id_idx ON fills (cl_ord_id);

-- Both sides of funding are rows. ACCRUAL is what the system computed for an
-- hour; SETTLEMENT is a cash adjustment actually observed on the account, twice
-- daily. An accrual points at the settlement that cleared it, and the difference
-- between matched pairs is exactly what carry_funding_reconciliation_error
-- measures — storing one side only would make that unfalsifiable.
CREATE TABLE funding_events (
    id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    ts             timestamptz NOT NULL,
    product_id     text        NOT NULL,
    kind           text        NOT NULL,
    rate_hourly    numeric,
    funding_source text,
    position_id    bigint REFERENCES positions (id),
    amount         numeric     NOT NULL,   -- signed: positive = received
    settled_by     bigint REFERENCES funding_events (id),
    spot_mark      numeric,
    contracts      bigint,
    CONSTRAINT funding_events_kind_check CHECK (kind IN ('ACCRUAL', 'SETTLEMENT')),
    CONSTRAINT funding_events_funding_source_check
        CHECK (funding_source IS NULL OR funding_source IN ('venue', 'computed')),
    -- A settlement is an observed cash movement: it has no rate, no rate
    -- provenance, and nothing above it to point at.
    CONSTRAINT funding_events_accrual_only_fields
        CHECK (kind = 'ACCRUAL'
               OR (rate_hourly IS NULL AND funding_source IS NULL AND settled_by IS NULL))
);
-- The observed funding series (position_id NULL) is one row per product, kind
-- and hour, which is what makes the Part 5 history backfill idempotent.
CREATE UNIQUE INDEX funding_events_observed_idx
    ON funding_events (product_id, kind, ts DESC) WHERE position_id IS NULL;
CREATE INDEX funding_events_position_idx ON funding_events (position_id, ts DESC);
CREATE INDEX funding_events_pending_idx
    ON funding_events (product_id, ts) WHERE kind = 'ACCRUAL' AND settled_by IS NULL;

-- Hard stops, staleness, reconciliation divergence, kill-switch engagements.
-- kind is deliberately unconstrained: it is an append-only vocabulary that grows
-- with the risk engine, and a rejected insert here would lose the record of the
-- very event it describes.
CREATE TABLE risk_events (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    ts           timestamptz NOT NULL,
    kind         text        NOT NULL,
    detail       jsonb,
    action_taken text,
    resolved_at  timestamptz
);
CREATE INDEX risk_events_ts_idx ON risk_events (ts DESC);
CREATE INDEX risk_events_unresolved_idx ON risk_events (ts DESC) WHERE resolved_at IS NULL;

-- FIX session history. quickfixgo owns the authoritative sequence numbers in its
-- file store; these rows are the operational record of what happened to the
-- session, for the dashboard and the resend demo.
CREATE TABLE fix_sessions (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    session_id   text        NOT NULL,
    started_at   timestamptz NOT NULL,
    ended_at     timestamptz,
    last_in_seq  integer,
    last_out_seq integer,
    disconnects  integer     NOT NULL DEFAULT 0
);
CREATE INDEX fix_sessions_session_idx ON fix_sessions (session_id, started_at DESC);
