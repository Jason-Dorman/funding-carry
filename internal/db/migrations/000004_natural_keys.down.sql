ALTER TABLE fills DROP CONSTRAINT fills_position_id_fkey;
ALTER TABLE funding_events DROP CONSTRAINT funding_events_position_id_fkey;

ALTER TABLE funding_events ALTER COLUMN position_id TYPE bigint USING position_id::bigint;
ALTER TABLE fills ALTER COLUMN position_id TYPE bigint USING position_id::bigint;
ALTER TABLE positions ALTER COLUMN id TYPE bigint USING id::bigint;
ALTER TABLE positions ALTER COLUMN id ADD GENERATED ALWAYS AS IDENTITY;

ALTER TABLE fills ADD CONSTRAINT fills_position_id_fkey
    FOREIGN KEY (position_id) REFERENCES positions (id);
ALTER TABLE funding_events ADD CONSTRAINT funding_events_position_id_fkey
    FOREIGN KEY (position_id) REFERENCES positions (id);

DROP INDEX funding_events_identity_idx;
CREATE UNIQUE INDEX funding_events_observed_idx
    ON funding_events (product_id, kind, ts DESC) WHERE position_id IS NULL;

DROP INDEX fills_identity_idx;
ALTER TABLE fills ALTER COLUMN venue_exec_id DROP NOT NULL;
CREATE UNIQUE INDEX fills_venue_exec_id_idx ON fills (venue, venue_exec_id)
    WHERE venue_exec_id IS NOT NULL;

DROP INDEX fix_sessions_identity_idx;
DROP INDEX risk_events_identity_idx;
DROP INDEX decisions_identity_idx;
DROP INDEX cb_account_state_identity_idx;
DROP INDEX base_state_identity_idx;
DROP INDEX cb_features_identity_idx;
DROP INDEX cb_book_snapshots_identity_idx;
DROP INDEX cb_venue_state_identity_idx;
