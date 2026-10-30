# Part 18 — Cron, Digest, Maintenance, Shutdown (Commits 86–90)

**Goal:** the last features. Scheduled jobs, the daily executive summary, storage lifecycle, and a
shutdown that loses nothing. Commit 90 is feature-complete against the original design.

```bash
go get github.com/robfig/cron/v3
```

---

## Commit 86 — `feat(scheduler): cron wrapper`

**Files:** `internal/adapters/delivery/scheduler/cron.go`

**Build:**

```go
type Cron struct {
    c   *cron.Cron
    log ports.Logger

    mu      sync.Mutex
    running sync.WaitGroup
    cancels []context.CancelFunc
}

func NewCron(loc *time.Location, log ports.Logger) *Cron
func (s *Cron) Schedule(name, spec string, job func(context.Context)) error
func (s *Cron) Start()
func (s *Cron) Stop(ctx context.Context) error
```

`Schedule`:
- **empty spec → log `"job disabled, empty schedule"` and return nil**
- otherwise `s.c.AddFunc(spec, func() { ... })` where the closure:
  1. `defer recover()` logging `"scheduled job panicked"` with the job name
  2. builds a `context.WithTimeout(context.Background(), 30*time.Minute)` and records the cancel
     func under the mutex
  3. `s.running.Add(1)`, deferring `Done()` and `cancel()`
  4. logs `"job started"`, runs the job, logs `"job finished"` with the duration
- wrap an `AddFunc` error with the job name and the spec, then log `"job scheduled"`

`Stop` — `s.c.Stop()` and wait on the returned context or `ctx`; then wait for `running` in a
goroutine and select against `ctx`, **cancelling every recorded cancel func** if the deadline hits.

Also `type Noop struct{}` implementing all three as no-ops, registered as `"none"`.

Register `"cron"` in `wire.Schedulers`, loading the location from `cfg.Cron.Timezone` via
`time.LoadLocation` and **falling back to UTC with a warning** on an unknown zone.

**Verify:** build; scratch-schedule `*/1 * * * *` with a job that logs, `Start()`, and wait a minute.

**Why:** the wrapper adds three things the raw library does not: **panic recovery per job** (so one
broken job cannot kill the process), **a bounded context per run** with cancellation on shutdown, and
structured start/finish/duration logging.

Timezone matters: "08:00" is meaningless without one, and using the server's local zone is how you
get reports at random hours after a datacentre migration. Defaulting to UTC and warning on a bad
value is the safe combination.

Empty spec disabling a job means `WT_CRON_DIGEST_SPEC=` turns off the report without needing a
separate boolean.

---

## Commit 87 — `feat(services): the daily digest`

**Files:** `internal/services/digest.go`

**Build:**

```go
type Digest struct {
    summaries ports.SummaryStore
    narrator  ports.Narrator
    renderer  ports.Renderer
    notifier  ports.Notifier
    bus       bus.Bus
    clock     ports.Clock
    window    time.Duration
    log       ports.Logger
}
func NewDigest(...) *Digest                      // window defaults to 24h when non-positive

func (d *Digest) Run(ctx context.Context) error
func build(from, to time.Time, entries []domain.ApprovedSummary) domain.Digest
func fallbackNarrative(d domain.Digest) string
```

`Run`:
1. `to := clock.Now()`, `from := to.Add(-d.window)`
2. `d.summaries.ListSummaries(ctx, from, to)` — **return the error**, this one is fatal to the job
3. `digest := build(from, to, entries)`
4. `d.narrator.Narrate(ctx, prompt.Digest(digest))` — **on error warn and use
   `fallbackNarrative`**
5. render, then notify — both errors are returned
6. publish `bus.TopicDigestReady` and log `"daily pulse sent"` with the counts

`build` — count by decision into `Approved`/`Edited`/`Rejected`, tally services into a map, then sort
`digest.Services` by **frequency descending with alphabetical as the tie-break** so output is
deterministic.

`fallbackNarrative` — "No incidents were reviewed in this window." or a one-line summary of the
counts and the affected services.

**Verify:** build.

**Why:** it reports on **`approved_summaries`, not `raw_anomalies`** — reviewed incidents only. So the
report reflects what humans confirmed, not what a model guessed. That is a product decision worth
stating in the commit message.

If the model fails, it **still sends** a data-only digest. A report that arrives with less prose beats
a report that does not arrive.

Note the asymmetry in error handling within one function: the query failing is fatal (there is nothing
to report), the narrator failing is not (there is something to report, just less prettily). Being
deliberate about that per call is what error handling actually is.

---

## Commit 88 — `feat(services): maintenance`

**Files:** `internal/services/maintenance.go`

**Build:**

```go
type Maintenance struct {
    partitions ports.PartitionManager
    ahead      int
    retention  int
    log        ports.Logger
}
func NewMaintenance(p ports.PartitionManager, ahead, retention int, log ports.Logger) *Maintenance

func (m *Maintenance) Run(ctx context.Context) error
```
**Attempt both halves even when the first fails**, collecting errors:
```go
var errs []error
if err := m.partitions.EnsurePartitions(ctx, m.ahead); err != nil {
    errs = append(errs, fmt.Errorf("ensure partitions: %w", err))
} else {
    m.log.Info("partitions ensured", "days_ahead", m.ahead)
}
if err := m.partitions.DropPartitionsOlderThan(ctx, m.retention); err != nil {
    errs = append(errs, fmt.Errorf("drop old partitions: %w", err))
}
if len(errs) > 0 { return errors.Join(errs...) }
```

**Verify:** build.

**Why:** a missing future partition and stale old data are **independent problems**; failing the first
should not skip the second. `errors.Join` reports both, so one run tells you everything that is wrong
instead of one thing at a time.

This service works against the interface, so it is a no-op-plus-ring-trim on the memory driver and
real DDL on Postgres — the caller never asks which.

---

## Commit 89 — `feat(bootstrap): schedules and one-shot jobs`

**Files:** `internal/bootstrap/app.go` (edit), `cmd/watchtowerd/main.go` (edit),
`internal/bootstrap/plugins.go` (edit)

**Build:**

In `build()`:
- construct `narrator` (`cfg.LLM.Provider`), `partitions`
  (`wire.PartitionManagers.Build(env, cfg.Store.Driver)`)
- `a.Digest = services.NewDigest(summaryStore, narrator, renderer, notifier, a.broker, env.Clock, cfg.Cron.DigestWindow, log)`
- `a.Maintenance = services.NewMaintenance(partitions, cfg.Store.PartitionAhead, cfg.Store.RetentionDays, log)`
- pick the scheduler driver: `"cron"` when `cfg.Cron.Enabled`, else `"none"`
- schedule two jobs, each logging its own error rather than propagating:
  ```go
  a.sched.Schedule("partition-manager", cfg.Cron.PartitionSpec, func(ctx context.Context) {
      if err := a.Maintenance.Run(ctx); err != nil { log.Error("maintenance failed", "err", err.Error()) }
  })
  a.sched.Schedule("daily-pulse", cfg.Cron.DigestSpec, func(ctx context.Context) {
      if err := a.Digest.Run(ctx); err != nil { log.Error("daily pulse failed", "err", err.Error()) }
  })
  ```
- add the readiness probe and pass it into `httpapi.Deps.Ready`:
  ```go
  func (a *App) ready(ctx context.Context) error {
      if a.Maintenance == nil { return errors.New("not initialized") }
      ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
      defer cancel()
      return a.Maintenance.Run(ctx)
  }
  ```

In `Run(ctx)`, before serving: `a.sched.Start()`, then run maintenance once with a 15-second context
so **today's partition exists before the first write**, warning rather than failing if it errors.

```go
func (a *App) RunOnce(ctx context.Context, job string) error
```
Start the broker, defer closing it and the env, then switch: `"digest"`/`"pulse"` →
`a.Digest.Run(ctx)`; `"maintenance"`/`"partitions"` → `a.Maintenance.Run(ctx)`; default an error
naming the valid values.

In `main.go` add `--job` and route through `RunOnce` when it is set:
```go
run := app.Run
if *job != "" {
    run = func(ctx context.Context) error { return app.RunOnce(ctx, *job) }
}
```

Add the blank import for `delivery/scheduler`.

**Verify:**

```bash
go build -o bin/watchtowerd ./cmd/watchtowerd

./bin/watchtowerd --job maintenance     # creates partitions, exits 0
./bin/watchtowerd --job digest          # sends a digest (empty is fine), exits 0

./bin/watchtowerd                        # expect two "job scheduled" lines and "partitions ensured"
curl http://localhost:8080/readyz        # {"status":"ready"}
```
With Postgres, confirm the partitions exist. Then stop Postgres and hit `/readyz` again — it must
return `503` with the reason.

**Why:** supporting **both** internal cron and one-shot execution is one `switch` and it answers "how
would this run in production?". Internal cron is convenient for a single deployment; in Kubernetes the
platform already has a scheduler with retries and alerting, and three replicas running internal cron
would send three daily reports.

Running maintenance at **startup** as well as on schedule means a fresh deployment can accept writes
immediately rather than failing until midnight.

The readiness probe running real maintenance is deliberate: it exercises connectivity **and** DDL. If
partitions cannot be created, writes will fail, so the instance should not take traffic. It is
idempotent, so probing constantly is safe.

---

## Commit 90 — `feat(bootstrap): graceful shutdown` 🎉

**Files:** `internal/bootstrap/app.go` (edit)

**Build:**

```go
func (a *App) Shutdown() error
```
One context bounded by `cfg.HTTP.ShutdownGrace` (defaulting to 10s), then **in this order**,
collecting every error with `errors.Join`:

1. `a.server.Shutdown(ctx)` — stop accepting, let in-flight requests finish
2. `a.sched.Stop(ctx)` — no new cron jobs
3. `a.pool.Shutdown(ctx)` — **drain queued incidents**
4. `a.broker.Close(ctx)` — flush pending events so the last email is actually sent
5. `a.env.Close()` — close Postgres and Redis, LIFO

Finish with `a.log.Info("watchtower stopped")`.

Make sure `Run` calls `Shutdown()` when `ctx.Done()` fires, and that `main` uses
`signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)`.

**Verify:**

```bash
./bin/watchtowerd &
# post several alerts quickly, then immediately:
kill -TERM %1
```
The log must show the shutdown sequence, and the queued incidents must **finish** — including their
emails — before `watchtower stopped`. Confirm nothing landed in the DLQ.

**Why:** the order is the whole answer, and it is a common interview question.

Stopping HTTP first means no new work arrives. Draining the pool **before** closing connections is
essential — reverse them and a draining job fails on a closed pool, which would put perfectly good
incidents in the dead letter queue during every deploy. Flushing the bus before closing resources
means the last email is sent rather than dropped. And resources close last, in reverse construction
order.

Collecting errors rather than returning on the first means one stubborn component cannot skip the rest
of the shutdown.

Honouring **SIGTERM** is what makes this work in Kubernetes, which sends SIGTERM and then SIGKILL
after the grace period.

---

## End of Part 18

**Feature-complete** against the original design: ingestion, debouncing, correlation, enrichment,
sanitizing, redaction, retrieval, reranking, planning, criticism, guardrails, scoring, delivery,
feedback, learning, partitioning, the daily pulse, and clean shutdown.

```bash
go build ./... && go vet ./... && gofmt -l .
git log --oneline | wc -l          # 90
```

Next: **Part 19** — Docker, Compose, and the full observability stack in one command.
