-- TimescaleDB supplies the hypertables every time-series table in 000002 is
-- built on. The official image creates the extension in its default database,
-- but the schema must not depend on that: applied against any Postgres that has
-- the extension installed, this is where it gets enabled.
CREATE EXTENSION IF NOT EXISTS timescaledb;
