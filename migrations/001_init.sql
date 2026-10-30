-- WatchTower schema.
--
-- The service applies this itself when WT_STORE_AUTO_MIGRATE=true (the default).
-- Apply it by hand and set that flag to false if your database is managed
-- separately or the application role has no DDL rights.
--
-- Note on dimensions: vector(1536) matches text-embedding-3-small. Use 3072 for
-- text-embedding-3-large, or 256 for the built-in offline hashing embedder, and
-- set WT_VECTOR_DIMS to the same number.

BEGIN;

-- ---------------------------------------------------------------------------
-- Raw incident records, partitioned by day.
-- Only sanitized logs land here: deduplicated, filtered and capped.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS raw_anomalies (
    id              TEXT        NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL,
    signature       TEXT        NOT NULL,
    correlation_key TEXT,
    severity        TEXT,
    services        TEXT[],
    alert_count     INT         NOT NULL DEFAULT 0,
    log_count       INT         NOT NULL DEFAULT 0,
    confidence      DOUBLE PRECISION,
    payload         JSONB       NOT NULL,
    PRIMARY KEY (created_at, id)
) PARTITION BY RANGE (created_at);

CREATE INDEX IF NOT EXISTS raw_anomalies_signature_idx
    ON raw_anomalies (signature, created_at DESC);

-- The partition manager creates one child table per day (yesterday through
-- WT_STORE_PARTITION_AHEAD) at startup and again at 00:00, then drops those past
-- WT_STORE_RETENTION_DAYS. Create them by hand like this if you disable it:
--
--   CREATE TABLE raw_anomalies_20260730 PARTITION OF raw_anomalies
--       FOR VALUES FROM ('2026-07-30 00:00:00+00') TO ('2026-07-31 00:00:00+00');
--
-- A DEFAULT partition is deliberately not created: once it holds a row for a given
-- day, creating that day's real partition fails.

-- ---------------------------------------------------------------------------
-- Reviewed outcomes: what an SRE actually decided.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS approved_summaries (
    id          TEXT PRIMARY KEY,
    incident_id TEXT NOT NULL,
    signature   TEXT,
    title       TEXT NOT NULL,
    root_cause  TEXT,
    commands    JSONB NOT NULL DEFAULT '[]'::jsonb,
    decision    TEXT NOT NULL,
    reviewer    TEXT,
    notes       TEXT,
    services    TEXT[],
    severity    TEXT,
    labels      JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS approved_summaries_created_idx  ON approved_summaries (created_at DESC);
CREATE INDEX IF NOT EXISTS approved_summaries_incident_idx ON approved_summaries (incident_id);

-- ---------------------------------------------------------------------------
-- Anything that failed to persist or process. Never silently dropped.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS dead_letter_queue (
    id      TEXT PRIMARY KEY,
    stage   TEXT NOT NULL,
    reason  TEXT NOT NULL,
    ref_id  TEXT,
    payload JSONB,
    at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS dead_letter_queue_at_idx ON dead_letter_queue (at DESC);

COMMIT;

-- ---------------------------------------------------------------------------
-- Retrieval corpus. Requires the pgvector extension; skip this block entirely if
-- you run WT_VECTOR_DRIVER=memory.
-- ---------------------------------------------------------------------------
CREATE EXTENSION IF NOT EXISTS vector;

CREATE TABLE IF NOT EXISTS historical_runbooks (
    id         TEXT PRIMARY KEY,
    title      TEXT NOT NULL,
    signature  TEXT,
    root_cause TEXT,
    commands   JSONB NOT NULL DEFAULT '[]'::jsonb,
    tags       TEXT[],
    reviewer   TEXT,
    content    TEXT NOT NULL,
    embedding  vector(1536),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS historical_runbooks_signature_idx ON historical_runbooks (signature);
CREATE INDEX IF NOT EXISTS historical_runbooks_embedding_idx
    ON historical_runbooks USING hnsw (embedding vector_cosine_ops);
