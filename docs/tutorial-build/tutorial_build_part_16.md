# Part 16 — PostgreSQL, Partitions and pgvector (Commits 76–80)

**Goal:** real storage. One connection pool shared by five adapters, a daily-partitioned incident
table, and vector search in the same database.

Setup for this part:
```bash
docker run -d --name wt-pg -p 5432:5432 \
  -e POSTGRES_USER=watchtower -e POSTGRES_PASSWORD=watchtower -e POSTGRES_DB=watchtower \
  pgvector/pgvector:pg16
go get github.com/jackc/pgx/v5
```

---

## Commit 76 — `feat(postgres): connection pool and schema`

**Files:** `internal/adapters/storage/postgres/db.go`

**Build:**

```go
func Pool(e *wire.Env) (*pgxpool.Pool, error)
```
Wrapped in `wire.Resource(e, "postgres.pool", func() (*pgxpool.Pool, error) { ... })`:
1. error when the DSN is empty
2. `pgxpool.ParseConfig(dsn)`; set `MaxConns` from config, `MaxConnLifetime = 30 * time.Minute`,
   `MaxConnIdleTime = 5 * time.Minute`
3. `pgxpool.NewWithConfig` with a 10-second context, then `pool.Ping`
4. **`e.OnClose(func() error { pool.Close(); return nil })`**
5. log `"postgres connected"` with the max connection count
6. when `cfg.Store.Migrate`, call `migrate(...)`; on failure close the pool and return the error

```go
func migrate(ctx context.Context, pool *pgxpool.Pool, dims int, log ports.Logger) error
```
Execute a slice of `IF NOT EXISTS` statements:
- `raw_anomalies` — `id TEXT`, `created_at TIMESTAMPTZ`, `signature TEXT`, `correlation_key TEXT`,
  `severity TEXT`, `services TEXT[]`, `alert_count INT`, `log_count INT`,
  `confidence DOUBLE PRECISION`, `payload JSONB`, `PRIMARY KEY (created_at, id)`,
  **`PARTITION BY RANGE (created_at)`**
- an index on `(signature, created_at DESC)`
- `approved_summaries` — real columns, `commands JSONB`, `labels JSONB`, indexes on `created_at DESC`
  and `incident_id`
- `dead_letter_queue` — `id` primary key, `stage`, `reason`, `ref_id`, `payload JSONB`, `at`, plus an
  index on `at DESC`

Then, separately: `CREATE EXTENSION IF NOT EXISTS vector`. **When it fails, log a warning and return
nil** — a database without pgvector should still serve incidents. When it succeeds, create
`historical_runbooks` with `embedding vector(%d)` (using `dims`), an index on `signature`, and
`USING hnsw (embedding vector_cosine_ops)`. Log `"postgres schema ready"` with the dimension count.

**Verify:**

```bash
WT_STORE_DRIVER=postgres \
WT_POSTGRES_DSN='postgres://watchtower:watchtower@localhost:5432/watchtower?sslmode=disable' \
./bin/watchtowerd
```
Expect `postgres connected` then `postgres schema ready vector_dims=1536`. Then:
```bash
docker exec -it wt-pg psql -U watchtower -d watchtower -c "\dt"
```

**⚠️ If it hangs after one log line**, you have the commit-17 deadlock: `wire.Resource` is holding
its mutex while `Pool` calls `e.OnClose`. Two separate mutexes is the fix. This is the exact symptom
the original build produced, and only ever with Postgres or Redis — the in-memory adapters never
register a closer.

**Why:** the pool is memoized under one key, so the four adapters in commits 77–80 all share it
without any of them owning it, and it is **lazy** — a memory-only configuration never connects.

Schema in code with every statement `IF NOT EXISTS` means a fresh environment boots with no
out-of-band step. State the tradeoff in the commit message: with many replicas racing you would want
a migration tool with advisory locks, which is why it is behind `WT_STORE_AUTO_MIGRATE`.

Treating a missing `vector` extension as a warning rather than fatal is graceful degradation: you
lose retrieval, not the service.

---

## Commit 77 — `feat(postgres): incident store`

**Files:** `internal/adapters/storage/postgres/incidents.go`

**Build:**

```go
type Incidents struct { pool *pgxpool.Pool }

func (s *Incidents) SaveIncident(ctx context.Context, inc *domain.Incident) error
func (s *Incidents) GetIncident(ctx context.Context, id string) (*domain.Incident, error)
```

`SaveIncident` — `json.Marshal` the whole incident into `payload`, then:
```sql
INSERT INTO raw_anomalies
    (id, created_at, signature, correlation_key, severity, services, alert_count, log_count, confidence, payload)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
ON CONFLICT (created_at, id) DO UPDATE SET
    payload    = EXCLUDED.payload,
    confidence = EXCLUDED.confidence,
    log_count  = EXCLUDED.log_count
```

`GetIncident` — `SELECT payload FROM raw_anomalies WHERE id = $1 ORDER BY created_at DESC LIMIT 1`,
mapping `pgx.ErrNoRows` to `domain.ErrNotFound`, then unmarshalling.

Register `"postgres"` in `wire.IncidentStores`, obtaining the pool via `Pool(e)`.

**Verify:** run with Postgres, post an alert, then:
```bash
docker exec -it wt-pg psql -U watchtower -d watchtower \
  -c "select id, severity, alert_count, log_count, confidence from raw_anomalies;"
```
You should see **one** row even though the incident is published twice (sanitized and triaged).

**Why:** the **upsert** is load-bearing. The persist projector writes at `incident.sanitized` and
again at `incident.triaged`; without `ON CONFLICT ... DO UPDATE` the second write would fail on a
duplicate key. `EXCLUDED` is the row that was proposed.

The **hybrid column/JSONB pattern**: columns for what you filter and sort on, JSONB for the full
document. You get indexed queries without designing separate tables for alerts, logs, events,
matches, steps and trace — and the incident's shape can evolve without a migration.

`PRIMARY KEY (created_at, id)` is required because Postgres needs the partition key in any unique
constraint, which is also why `GetIncident` cannot assume `id` alone is unique.

**A performance note worth recording in the commit message:** that `GetIncident` query has no
`created_at` predicate, so it cannot prune partitions and touches all of them. Fine at eight
partitions, not at three hundred. The fix is to carry the incident's date in the (already signed)
approval link and add `AND created_at >= $2`.

---

## Commit 78 — `feat(postgres): summary store and dead letter queue`

**Files:** `internal/adapters/storage/postgres/summaries.go`

**Build:**

```go
type Summaries struct { pool *pgxpool.Pool }
func (s *Summaries) SaveSummary(ctx context.Context, sum domain.ApprovedSummary) error
func (s *Summaries) ListSummaries(ctx context.Context, from, to time.Time) ([]domain.ApprovedSummary, error)
```
`SaveSummary` — marshal `Commands` and `orEmpty(Labels)` to JSON; insert with
`ON CONFLICT (id) DO UPDATE` refreshing decision, commands, root cause, notes and reviewer.

`ListSummaries` — `WHERE created_at >= $1 AND created_at < $2 ORDER BY created_at DESC`, wrapping
every nullable column in **`COALESCE`** (`signature`, `root_cause`, `reviewer`, `notes`,
`services` → `'{}'`, `severity`). Unmarshal `commands` per row.

```go
type DLQ struct { pool *pgxpool.Pool; log ports.Logger }
func (d *DLQ) Park(ctx context.Context, dl domain.DeadLetter) error
```
`INSERT ... ON CONFLICT (id) DO NOTHING`. Pass `nil` for an empty payload so the column stays NULL.
On failure:
```go
// Last resort: if even the DLQ write fails, the log line is the record.
d.log.Error("dead letter write failed", "stage", dl.Stage, "ref", dl.RefID, "reason", dl.Reason, "err", err.Error())
```

Register `"postgres"` in `wire.SummaryStores` and `wire.DeadLetterQueues`.

**Verify:** with Postgres, approve an incident, then:
```bash
docker exec -it wt-pg psql -U watchtower -d watchtower \
  -c "select incident_id, decision, reviewer from approved_summaries;" \
  -c "select count(*) from dead_letter_queue;"
```
The DLQ count should be **0**.

**Why:** `COALESCE` everywhere because scanning SQL `NULL` into a Go `string` fails, and the
alternative — `sql.NullString` on eight fields — is much noisier.

The half-open range `>= from AND < to` means consecutive digest windows can never double-count a row.

`DO NOTHING` rather than `DO UPDATE` for the DLQ: for a failure record, first write wins and a retry
should be a no-op.

Every failure path needs a terminus. Thinking one level past "and what if *that* fails" is the point
of the last-resort log line.

---

## Commit 79 — `feat(postgres): partition manager`

**Files:** `internal/adapters/storage/postgres/partitions.go`

**Build:**

```go
type Partitions struct { pool *pgxpool.Pool; clock ports.Clock; log ports.Logger }

func (p *Partitions) EnsurePartitions(ctx context.Context, daysAhead int) error
func (p *Partitions) createFor(ctx context.Context, day time.Time) error
func (p *Partitions) DropPartitionsOlderThan(ctx context.Context, days int) error
func partitionName(day time.Time) string                  // "raw_anomalies_20260730"
func dayFromPartition(name string) (time.Time, bool)
```

`EnsurePartitions` — truncate today to a day boundary in UTC and loop `offset := -1` through
`daysAhead`. **Yesterday is included** so a late-arriving alert with an older timestamp still lands
somewhere.

`createFor`:
```sql
CREATE TABLE IF NOT EXISTS raw_anomalies_20260730 PARTITION OF raw_anomalies
    FOR VALUES FROM ('2026-07-30 00:00:00+00') TO ('2026-07-31 00:00:00+00')
```

`DropPartitionsOlderThan` — list the children from the catalog:
```sql
SELECT c.relname
FROM pg_class c
JOIN pg_inherits i ON i.inhrelid = c.oid
JOIN pg_class parent ON parent.oid = i.inhparent
WHERE parent.relname = 'raw_anomalies'
```
Close the rows **before** issuing DDL, parse the date out of each name, and `DROP TABLE IF EXISTS`
those entirely before the cutoff. Log each drop and a summary.

Register `"postgres"` in `wire.PartitionManagers`.

**Verify:**

```bash
docker exec -it wt-pg psql -U watchtower -d watchtower \
  -c "select tablename from pg_tables where tablename like 'raw_anomalies%' order by 1;"
```
Expect the parent plus yesterday through +7 days. (Nothing calls `EnsurePartitions` yet — commit 88
adds the service, commit 89 calls it at startup. Call it from a scratch main for now.)

**Why:** this is the payoff of partitioning. Retention is **`DROP TABLE`** — an instant metadata
operation — instead of a mass `DELETE` that takes hours, floods the WAL and leaves dead tuples for
autovacuum.

The rule that bites everyone: **a partitioned table can only accept a row if a partition covering
that timestamp exists.** No partition, insert fails. That is why partitions are created ahead of
time and why something has to own the job.

Deliberately **no DEFAULT partition**: once it holds a row for a given day, creating that day's real
partition fails, because Postgres would have to move existing rows. Failing loudly into the DLQ is
better than a scheme that silently degrades. Put that in a comment.

---

## Commit 80 — `feat(postgres): pgvector store and the SQL mirror`

**Files:** `internal/adapters/storage/postgres/vector.go`, `migrations/001_init.sql`,
`internal/bootstrap/plugins.go` (edit), `internal/config/config.go` (edit)

**Build:**

```go
type Vectors struct { pool *pgxpool.Pool }
func (v *Vectors) Upsert(ctx context.Context, rb domain.Runbook) error
func (v *Vectors) Search(ctx context.Context, vec []float32, k int) ([]domain.RunbookMatch, error)
func encodeVector(vec []float32) string
```

`Upsert` — error on an empty embedding; marshal commands; insert with `$9::vector` and
`ON CONFLICT (id) DO UPDATE` refreshing title, cause, commands, tags, content and embedding. Store
`rb.Text()` in `content`.

`Search`:
```sql
SELECT id, title, COALESCE(signature,''), COALESCE(root_cause,''), commands,
       COALESCE(tags,'{}'), COALESCE(reviewer,''), content, created_at,
       1 - (embedding <=> $1::vector) AS similarity
FROM historical_runbooks
WHERE embedding IS NOT NULL
ORDER BY embedding <=> $1::vector
LIMIT $2
```
Scan `content` into `rb.SourceText`; clamp a negative similarity to 0; set `Stage: "pgvector"`.

`encodeVector` — build `[0.023,-0.018,...]` with
`strconv.FormatFloat(float64(f), 'f', -1, 32)`.

Add a **validation rule** to `config.Validate()`:
```go
if c.RAG.VectorDriver == "postgres" && c.RAG.EmbedderDriver == "stub" && c.RAG.VectorDims != 256 {
    errs = append(errs, "the stub embedder produces 256-dimension vectors: set WT_VECTOR_DIMS=256 or use the openai embedder with pgvector")
}
```

Write `migrations/001_init.sql` mirroring the DDL from commit 76, with comments explaining the
dimension choice and **why there is no DEFAULT partition**.

Register `"postgres"` in `wire.VectorStores`; add the blank import for `storage/postgres`.

**Verify — the full real-storage path:**

```bash
WT_STORE_DRIVER=postgres WT_POSTGRES_DSN='...' \
WT_VECTOR_DRIVER=postgres WT_VECTOR_DIMS=256 ./bin/watchtowerd
```
(256 because you are still on the hashing embedder — and if you forget, validation now tells you.)

Post an alert, approve it, then:
```bash
docker exec -it wt-pg psql -U watchtower -d watchtower \
  -c "select id, title, vector_dims(embedding) from historical_runbooks;"
```
Then post the similar alert and confirm the trace retrieves it — this time out of pgvector.

**Why:** `ORDER BY embedding <=> $1::vector LIMIT k` **is** k-nearest-neighbour search. `<=>` is
cosine *distance*, so similarity is `1 - distance`. The HNSW index makes it approximate and roughly
logarithmic instead of exact and linear, and the index's operator class must match the operator in
the query or it will not be used.

`vector(768)` fixes the dimension **in the schema**, which is why the validation rule exists. Catching
a mismatch at boot with a sentence naming the fix beats catching it at the first insert with a driver
error.

---

## End of Part 16

Real storage: partitioned incidents, reviewed summaries, a dead letter queue and vector search — one
connection pool, five adapters, and the memory drivers still work unchanged.

```bash
go build ./... && go vet ./... && gofmt -l .
```

Next: **Part 17** — Redis, Loki and Kubernetes.
