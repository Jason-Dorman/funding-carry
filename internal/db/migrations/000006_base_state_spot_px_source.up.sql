-- Provenance for base_state.spot_px, the same shape funding_source already has
-- on the three tables that carry a rate (migration 000005, ADR-0015).
--
-- The column has two sources and they are different markets. 'dex' is the mid
-- of the configured Base pool, read on chain at the row's block; 'coinbase' is
-- the spot mid the ingest binary already holds from the ETH-USD ticker,
-- standing in when the pool cannot be read (ADR-0018). A row that cannot say
-- which one it holds is a price whose market is unknowable after the fact, and
-- the spot leg is marked against this column.
--
-- The paired NULL check is the invariant that makes the column trustworthy
-- rather than decorative: a price with no source and a source with no price are
-- both nonsense, and without it the column would drift into being set sometimes.
--
-- It is added NOT VALID, which is the whole difference between a migration that
-- can be applied once and one that can be applied to a database that already has
-- history. NOT VALID enforces the check on every INSERT and UPDATE from now on —
-- which is the guarantee the column exists for — while not requiring rows that
-- predate the column to satisfy it. Those rows have a price and genuinely have
-- no market to name; retro-labelling them would be inventing provenance, which
-- is the exact failure the column was added to prevent.
--
-- The first draft added it validating, on the reasoning that base_state was
-- empty. It was empty for about ten minutes. An adversarial review found that
-- `migrate down 1` followed by `migrate up` then fails — the down drops the
-- column, which is where the provenance lived, and the re-applied up rejects
-- every row whose price survived it — leaving the schema marked dirty. That was
-- reproduced against the running database, which by then held 174 priced rows.
--
-- The vocabulary check below does not need NOT VALID: pre-existing rows have a
-- NULL source, which it already permits.
ALTER TABLE base_state ADD COLUMN spot_px_source text;

ALTER TABLE base_state ADD CONSTRAINT base_state_spot_px_source_check
    CHECK (spot_px_source IS NULL OR spot_px_source IN ('dex', 'coinbase'));

ALTER TABLE base_state ADD CONSTRAINT base_state_spot_px_has_a_source_check
    CHECK ((spot_px IS NULL) = (spot_px_source IS NULL)) NOT VALID;
