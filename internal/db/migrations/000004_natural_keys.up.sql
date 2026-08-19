-- Every insert becomes idempotent: each table gains the unique key that is the
-- real identity of one of its rows, and every producer inserts with
-- ON CONFLICT ... DO NOTHING.
--
-- This is what lets the writer re-send a batch whose commit status it cannot
-- determine. Without it the writer had to choose between duplicating rows and
-- abandoning them, and it chose to abandon — correct, but it bought correctness
-- with availability: a dropped connection became a restart, a restart became a
-- gap, and a gap in cb_venue_state, cb_features, cb_book_snapshots or
-- cb_trades_agg cannot be backfilled from anywhere, because those series are
-- computed here and exist nowhere else (spec section 0: self-recorded data is
-- the primary history). With a natural key the same failure is a repeat that
-- lands as a no-op.
--
-- The keys are row identity, not convenience. Where identity is a sampling
-- instant, the producer is obliged to align ts to the sampling boundary rather
-- than passing time.Now(): a nanosecond-resolution timestamp makes every row
-- unique and the constraint decorative. That obligation is recorded in
-- API spec section 5.3 and belongs to Parts 4-6.

-- ---------------------------------------------------------------------------
-- Series: identity is (what was sampled, when it was sampled)
-- ---------------------------------------------------------------------------

-- One venue sample per product per poll boundary.
CREATE UNIQUE INDEX cb_venue_state_identity_idx ON cb_venue_state (product_id, ts DESC);

-- One book snapshot per product per snapshot boundary. There is no level in the
-- key because a row here is the whole top-N snapshot — depths and prices are
-- numeric[] columns, not one row per level.
CREATE UNIQUE INDEX cb_book_snapshots_identity_idx ON cb_book_snapshots (product_id, ts DESC);

-- One feature row per product per decision tick.
CREATE UNIQUE INDEX cb_features_identity_idx ON cb_features (product_id, ts DESC);

-- One wallet sample per poll boundary. base_state has no address column because
-- the wallet doctrine is one named wallet (spec section 1); a second wallet would
-- have to be part of this key.
CREATE UNIQUE INDEX base_state_identity_idx ON base_state (ts DESC);

-- One account snapshot per poll boundary, for the same reason: one CFM account.
CREATE UNIQUE INDEX cb_account_state_identity_idx ON cb_account_state (ts DESC);

-- ---------------------------------------------------------------------------
-- State
-- ---------------------------------------------------------------------------

-- One decision per tick. v1 runs a single asset through a single decision loop,
-- so the instant identifies the decision; a second asset would have to add
-- product_id both here and to the table.
CREATE UNIQUE INDEX decisions_identity_idx ON decisions (ts DESC);

-- A risk event is identified by what happened and when. Two events of different
-- kinds at one instant stay distinct; two of the same kind at one instant are
-- the same event.
CREATE UNIQUE INDEX risk_events_identity_idx ON risk_events (ts DESC, kind);

-- A session starts once.
CREATE UNIQUE INDEX fix_sessions_identity_idx ON fix_sessions (session_id, started_at DESC);

-- The venue's own execution id is the identity of a fill, so it is now required
-- rather than optional, and unique per venue rather than unique-when-present.
-- Every Venue implementation must supply one: FIX ExecID (tag 17) is mandatory,
-- and the paper and sim engines mint their own (Parts 7, 14).
DROP INDEX fills_venue_exec_id_idx;
ALTER TABLE fills ALTER COLUMN venue_exec_id SET NOT NULL;
CREATE UNIQUE INDEX fills_identity_idx ON fills (venue, venue_exec_id);

-- Funding identity is the hour, the product, the side of the ledger, and whose
-- position it belongs to. position_id has to be in the key: the same funding
-- hour legitimately produces both an observed-series row (position_id NULL) and
-- a position-linked accrual, and they are different rows. NULLS NOT DISTINCT is
-- what makes the observed row collide with its own repeat rather than with
-- nothing (Postgres 15+).
--
-- For an ACCRUAL, ts is the start of the funding hour, not the moment the row
-- was computed — the rate is a property of the hour (venue doc section 3).
DROP INDEX funding_events_observed_idx;
CREATE UNIQUE INDEX funding_events_identity_idx
    ON funding_events (product_id, kind, ts DESC, position_id) NULLS NOT DISTINCT;

-- ---------------------------------------------------------------------------
-- positions: the one table with no natural key
-- ---------------------------------------------------------------------------
--
-- A carry has no property that identifies it. Its open time is not identity —
-- it is a fact about it — and everything else (venue, size, price) can repeat.
-- The honest reading is that the identity of a position is assigned, not
-- discovered, so it is assigned by the component that opens it: id becomes a
-- client-minted ULID, the same convention ClOrdID already uses (API spec
-- section 3.2).
--
-- This also settles the open question left against Part 13. A database-generated
-- id has to be read back before fills and funding events can reference it, and
-- reading it back means a round trip from a producer — a second writer, which
-- the one-writer rule forbids. A ULID is known before the insert, so the whole
-- persistence path stays on the batch channel.
--
-- The tables are empty, so this is a type change rather than a data migration.
ALTER TABLE fills DROP CONSTRAINT fills_position_id_fkey;
ALTER TABLE funding_events DROP CONSTRAINT funding_events_position_id_fkey;

ALTER TABLE positions ALTER COLUMN id DROP IDENTITY;
ALTER TABLE positions ALTER COLUMN id TYPE text USING id::text;
ALTER TABLE fills ALTER COLUMN position_id TYPE text USING position_id::text;
ALTER TABLE funding_events ALTER COLUMN position_id TYPE text USING position_id::text;

ALTER TABLE fills ADD CONSTRAINT fills_position_id_fkey
    FOREIGN KEY (position_id) REFERENCES positions (id);
ALTER TABLE funding_events ADD CONSTRAINT funding_events_position_id_fkey
    FOREIGN KEY (position_id) REFERENCES positions (id);
