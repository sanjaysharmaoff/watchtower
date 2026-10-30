# Part 8 — PostgreSQL: JSONB, Partitioning, and the Schema

Postgres is where incidents, reviewed outcomes, learned runbooks and failures live. Three
features matter here: JSONB, table partitioning, and (in Part 9) pgvector.

## Why Postgres and not something else

The honest answer, and a good interview answer:

> I needed relational queries for the digest, document storage for the incident payload,
> and vector similarity search for retrieval. Postgres does all three: JSONB for
> documents, normal SQL for reporting, pgvector for embeddings. Using Mongo plus a
> dedicated vector database would have meant three data stores, three failure modes and
> three backup strategies for a system that handles a modest number of writes.

## The four tables

```sql
raw_anomalies        -- every incident, partitioned by day, sanitized logs only
approved_summaries   -- what the SRE decided
historical_runbooks  -- the retrieval corpus, with embeddings (pgvector)
dead_letter_queue    -- anything that failed to persist or process
```

## JSONB

Postgres has two JSON types. `json` stores text verbatim; `jsonb` parses into a binary
form. Use `jsonb` — it is smaller, faster to read, and indexable.

`raw_anomalies` stores the whole incident as JSONB plus a few extracted columns:

```sql
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
```

This is the **hybrid pattern**: columns for what you filter and sort on, JSONB for the
full document. You get indexed queries on severity and signature without designing tables
for alerts, logs, events, matches, plan steps and trace notes separately — and the shape
of `domain.Incident` can evolve without a migration.

The tradeoff, which you should volunteer: no schema enforcement inside the blob, and
querying nested fields is more verbose. For an append-mostly audit record, that is the
right trade. For `approved_summaries`, which the digest queries and reports on, real
columns are used instead.

### Querying JSONB

The operators you need:

```sql
payload -> 'plan'                  -- returns jsonb
payload ->> 'id'                    -- returns text
payload -> 'plan' ->> 'root_cause'  -- navigate then extract as text
payload::text LIKE '%s3cr3tp4ss%'   -- crude full-document search
jsonb_array_length(payload -> 'logs')
jsonb_pretty(payload)
jsonb_array_elements(payload -> 'events')  -- expand an array into rows
```

The verification query used to prove the redaction fix combines several:

```sql
SELECT (payload::text LIKE '%s3cr3tp4ss%') AS plaintext_secret_at_rest,
       (SELECT count(*) FROM jsonb_array_elements(payload->'events') e
        WHERE e->>'type' = 'Warning')      AS k8s_warnings
FROM raw_anomalies WHERE id = '...';
```

Result: `plaintext_secret_at_rest = false`, `k8s_warnings = 3`. That is how a claim
becomes evidence.

## Partitioning: the problem it solves

An incident table grows forever. After a year you have millions of rows, indexes that no
longer fit in memory, and a `DELETE FROM raw_anomalies WHERE created_at < ...` that takes
hours, generates enormous WAL, and leaves dead tuples for autovacuum to chew through.

**Declarative partitioning** splits one logical table into physical child tables.
`PARTITION BY RANGE (created_at)` with one child per day means:

- Inserts go to today's child table, which is small, so its indexes stay hot.
- Queries with a time filter touch only relevant children (**partition pruning**).
- **Deleting old data is `DROP TABLE`** — an instant metadata operation, no row scanning,
  no vacuum, no WAL flood.

That last point is the whole reason to do it.

### The child tables

```sql
CREATE TABLE raw_anomalies_20260730 PARTITION OF raw_anomalies
    FOR VALUES FROM ('2026-07-30 00:00:00+00') TO ('2026-07-31 00:00:00+00');
```

Verified from the running system:

```
raw_anomalies
raw_anomalies_20260728
raw_anomalies_20260729
raw_anomalies_20260730
raw_anomalies_20260731
... through raw_anomalies_20260806
```

### The rule that bites everyone

**A partitioned table can only accept a row if a partition covering that timestamp
exists.** No partition means the insert fails outright. So partitions must be created
*ahead of time*, and something must own that job.

In WatchTower that is `ports.PartitionManager`:

```go
type PartitionManager interface {
    EnsurePartitions(ctx context.Context, daysAhead int) error
    DropPartitionsOlderThan(ctx context.Context, days int) error
}
```

`EnsurePartitions` runs at startup and again at 00:00 via cron, creating yesterday through
`WT_STORE_PARTITION_AHEAD` days (default 7). Yesterday is included so a late-arriving alert
with an older timestamp still lands somewhere. Seven days of headroom means the system
survives a week of the cron being broken.

`DropPartitionsOlderThan` finds child tables via the catalog and drops the expired ones:

```sql
SELECT c.relname
FROM pg_class c
JOIN pg_inherits i ON i.inhrelid = c.oid
JOIN pg_class parent ON parent.oid = i.inhparent
WHERE parent.relname = 'raw_anomalies'
```

then parses the date out of each name and drops those entirely before the cutoff.

### Why there is no DEFAULT partition

A DEFAULT partition catches rows that fit nowhere. It looks like a safety net and is a
trap: once it holds a row for a given day, creating that day's real partition **fails**,
because Postgres would have to move existing rows. The migration file says so explicitly:

```sql
-- A DEFAULT partition is deliberately not created: once it holds a row for a given
-- day, creating that day's real partition fails.
```

Failing loudly into the dead letter queue is better than a partition scheme that silently
degrades. Good detail to have ready — it shows you read the docs rather than copying a
tutorial.

## Composite primary key

```sql
PRIMARY KEY (created_at, id)
```

Postgres requires the partition key to be part of any unique constraint, because
uniqueness is enforced per-partition. Hence `(created_at, id)` rather than just `id`. That
is also why `GetIncident` looks like this:

```sql
SELECT payload FROM raw_anomalies WHERE id = $1 ORDER BY created_at DESC LIMIT 1
```

It cannot assume `id` alone is unique across partitions.

## Idempotent writes

```sql
INSERT INTO raw_anomalies (...) VALUES (...)
ON CONFLICT (created_at, id) DO UPDATE SET
    payload    = EXCLUDED.payload,
    confidence = EXCLUDED.confidence,
    log_count  = EXCLUDED.log_count
```

`ON CONFLICT ... DO UPDATE` is Postgres's **upsert**. It matters here because the same
incident is written twice: once after sanitizing (evidence saved early, so it survives a
model failure) and again after triage (with the plan and confidence). The second write
updates rather than exploding on a duplicate key. `EXCLUDED` is the row that was proposed.

The dead letter queue uses `ON CONFLICT (id) DO NOTHING` instead — for a failure record,
first write wins and a retry should be a no-op.

## Connection pooling with pgx

`pgx` is the modern Postgres driver for Go. This project uses `pgxpool`:

```go
cfg.MaxConns       = int32(e.Cfg.Store.MaxConns)   // default 10
cfg.MaxConnLifetime = 30 * time.Minute
cfg.MaxConnIdleTime = 5 * time.Minute
```

Why a pool: opening a TCP connection and authenticating costs milliseconds, and Postgres
allocates a backend process per connection. A pool amortises that and caps concurrency so
your app cannot exhaust the database's connection limit. `MaxConnLifetime` recycles
connections periodically, which avoids problems with long-lived connections behind
load balancers and proxies like PgBouncer.

**One pool, four adapters.** The incident store, summary store, DLQ, partition manager and
pgvector store all share it, through the memoized resource pool in `wire.Env`
(Part 15). Each asks for `"postgres.pool"` and gets the same handle. None of them knows
the others exist.

## Migrations

The schema is applied at startup, idempotently, when `WT_STORE_AUTO_MIGRATE=true`:

```go
stmts := []string{
    `CREATE TABLE IF NOT EXISTS raw_anomalies (...) PARTITION BY RANGE (created_at)`,
    `CREATE INDEX IF NOT EXISTS raw_anomalies_signature_idx ON raw_anomalies (signature, created_at DESC)`,
    ...
}
for _, stmt := range stmts {
    if _, err := pool.Exec(ctx, stmt); err != nil {
        return fmt.Errorf("postgres: migrate: %w", err)
    }
}
```

Every statement is `IF NOT EXISTS`, so running it a hundred times is harmless. The same
DDL is mirrored in `migrations/001_init.sql` for teams whose database role has no DDL
rights — set `WT_STORE_AUTO_MIGRATE=false` and apply it yourself.

State the tradeoff before you are asked: auto-migration at startup is convenient and fine
for a single instance, but with many replicas racing you would want a migration tool with
advisory locks and versioning (golang-migrate, goose, atlas). It is deliberately a
config flag so the strategy can change without touching code.

## Useful psql

```bash
docker compose exec -T postgres psql -U watchtower -d watchtower -c "\dt"     # tables
docker compose exec -T postgres psql -U watchtower -d watchtower \
  -c "select id, severity, confidence, created_at from raw_anomalies order by created_at desc limit 5;"
docker compose exec -T postgres psql -U watchtower -d watchtower \
  -c "select jsonb_pretty(payload) from raw_anomalies order by created_at desc limit 1;"
docker compose exec -T postgres psql -U watchtower -d watchtower \
  -c "select count(*) from dead_letter_queue;"      -- should be 0
```

`-A -t` (unaligned, tuples-only) makes output easy to script against.

## Interview answers to have ready

**"Why partition?"**
Retention becomes `DROP TABLE` instead of a mass `DELETE`. Constant-time, no vacuum
pressure, no WAL flood. Inserts also stay fast because today's indexes are small.

**"What breaks if the partition job does not run?"**
Inserts for uncovered days fail. Mitigations: partitions are created seven days ahead by
default, `EnsurePartitions` runs at startup as well as on cron, the readiness probe
actually exercises it (so a broken instance reports not-ready), and any failed write lands
in the dead letter queue with the full payload for replay.

**"Why JSONB instead of normalised tables?"**
The incident is an append-mostly audit document with nested, evolving structure — alerts,
logs, events, matches, plan steps, trace. Normalising it would mean six tables and a
migration every time the domain changes. I extract the columns I filter on and keep the
rest as JSONB. Where I do report — `approved_summaries` — I used real columns.

---

Next: **Part 9 — Vectors, Embeddings, pgvector and RAG**, the part interviewers ask about
most.
