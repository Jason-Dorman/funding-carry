-- A third funding provenance: 'backfilled'.
--
-- The venue publishes no funding rate for this product (venue doc section 3,
-- verified 2026-08-20), so the local estimator is the primary source rather than
-- the fallback. That leaves a series that only begins when this system does,
-- and a funding z-score needs roughly thirty days of it before it means
-- anything — which would have made the signal unusable for a month.
--
-- It does not have to be. The estimator's inputs are a futures mark and a spot
-- mark, and the REST candles endpoint serves both back to the perp's launch
-- (2025-07-18). The history can therefore be reconstructed with the venue's own
-- formula rather than waited for.
--
-- What it cannot be is passed off as the same thing. A reconstructed hour is
-- built from one-minute candle closes; a recorded hour is built from three-
-- minute VWAPs of actual trades. Same formula, coarser inputs, and a different
-- error profile. Writing both under 'computed' would leave a mixed-provenance
-- series that nothing downstream could take apart — the z-score, the backtest
-- and the reconciliation would each silently average two different measurements
-- together. Provenance is therefore in the data, not in a comment.
ALTER TABLE cb_venue_state DROP CONSTRAINT cb_venue_state_funding_source_check;
ALTER TABLE cb_venue_state ADD CONSTRAINT cb_venue_state_funding_source_check
    CHECK (funding_source IS NULL OR funding_source IN ('venue', 'computed', 'backfilled'));

ALTER TABLE cb_features DROP CONSTRAINT cb_features_funding_source_check;
ALTER TABLE cb_features ADD CONSTRAINT cb_features_funding_source_check
    CHECK (funding_source IS NULL OR funding_source IN ('venue', 'computed', 'backfilled'));

ALTER TABLE funding_events DROP CONSTRAINT funding_events_funding_source_check;
ALTER TABLE funding_events ADD CONSTRAINT funding_events_funding_source_check
    CHECK (funding_source IS NULL OR funding_source IN ('venue', 'computed', 'backfilled'));
