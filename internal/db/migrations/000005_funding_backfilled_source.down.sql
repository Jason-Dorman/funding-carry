-- Narrowing the vocabulary again would reject rows the widened one accepted, so
-- the reconstructed rows are removed first. They are reproducible from the
-- candles endpoint, which is the whole point of them being marked.
DELETE FROM cb_venue_state WHERE funding_source = 'backfilled';
DELETE FROM cb_features WHERE funding_source = 'backfilled';
DELETE FROM funding_events WHERE funding_source = 'backfilled';

ALTER TABLE cb_venue_state DROP CONSTRAINT cb_venue_state_funding_source_check;
ALTER TABLE cb_venue_state ADD CONSTRAINT cb_venue_state_funding_source_check
    CHECK (funding_source IS NULL OR funding_source IN ('venue', 'computed'));

ALTER TABLE cb_features DROP CONSTRAINT cb_features_funding_source_check;
ALTER TABLE cb_features ADD CONSTRAINT cb_features_funding_source_check
    CHECK (funding_source IS NULL OR funding_source IN ('venue', 'computed'));

ALTER TABLE funding_events DROP CONSTRAINT funding_events_funding_source_check;
ALTER TABLE funding_events ADD CONSTRAINT funding_events_funding_source_check
    CHECK (funding_source IS NULL OR funding_source IN ('venue', 'computed'));
