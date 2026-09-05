-- Dropping the column loses the provenance of every price already recorded, and
-- there is nowhere else it is written down. The rows themselves are kept: a
-- price of unknown market is still a price, and unlike the backfilled funding
-- rows of migration 000005 these cannot be reconstructed — base_state is
-- point-in-time wallet and chain state (architecture section 7.1).
ALTER TABLE base_state DROP CONSTRAINT base_state_spot_px_has_a_source_check;
ALTER TABLE base_state DROP CONSTRAINT base_state_spot_px_source_check;
ALTER TABLE base_state DROP COLUMN spot_px_source;
