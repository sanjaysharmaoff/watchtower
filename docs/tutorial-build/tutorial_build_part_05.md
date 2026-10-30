# Part 5 — First Stages and First Adapters (Commits 21–25)

**Goal:** the first two pipeline stages and four adapters. By the end of this part every piece
needed for a running service exists except the HTTP layer and the wiring.

---

## Commit 21 — `feat(stages): stage helpers and the admit filter`

**Files:** `internal/stages/stages.go`, `internal/stages/admit.go`

**Build:**

`stages.go`:
```go
type Stage = pipeline.Stage[*domain.Incident]      // type ALIAS, note the =
type Chain = pipeline.Pipeline[*domain.Incident]

func mutator(name string, fn func(ctx context.Context, inc *domain.Incident) error) Stage
```
`mutator` wraps `pipeline.Of`: return `domain.Drop("nil incident")` when `inc == nil`, run `fn`,
return `(inc, err)`.

`admit.go`:
```go
type AdmitOptions struct {
    DropResolved bool
    MinSeverity  domain.Severity
}

func Admit(opts AdmitOptions) Stage
```
Behaviour, in order:
1. no alerts → `domain.Drop("no alerts in payload")`
2. if `DropResolved`: filter out resolved alerts **in place** using
   `firing := inc.Alerts[:0]`; if nothing is left → `domain.Drop("all alerts resolved")`;
   otherwise call `inc.Recompute()`
3. if `MinSeverity.Rank() > 0` and the incident ranks lower →
   `domain.Drop("severity %s below floor %s", ...)`

Also add a tiny `func itoa(n int) string` helper in the package — you will use it for `Meta`
values.

**Verify:** build, plus a scratch check that a resolved-only incident is dropped and that
`domain.IsDrop(err)` is true.

**Why:** `inc.Alerts[:0]` is the zero-allocation filter idiom — it reuses the backing array,
safe because the write index never overtakes the read index.

`Recompute()` after filtering is not optional: removing alerts can change severity, services and
the signature, and you are about to debounce on that signature.

`Rank() > 0 &&` means `WT_MIN_SEVERITY=unknown` disables the floor rather than rejecting
everything. Config meaning "no floor" must not mean "reject all".

This stage runs first because it is the cheapest thing in the system: no I/O, microseconds. The
whole pipeline is ordered by cost.

---

## Commit 22 — `feat(gate,stages): in-memory gate and the debouncer`

**Files:** `internal/adapters/ingest/gate/memory.go`, `internal/stages/admit.go` (extend)

**Build:**

`gate/memory.go`:
```go
type Memory struct {
    clock ports.Clock
    mu    sync.Mutex
    seen  map[string]time.Time
    last  time.Time
}

func NewMemory(c ports.Clock) *Memory
func (m *Memory) Allow(_ context.Context, key string, window time.Duration) (bool, error)
func (m *Memory) Size() int
```
`Allow` — input: a key and a window; output: `true` if the key has **not** been seen inside the
window, recording it if so.
- `window <= 0` → `(true, nil)` immediately
- prune at most once per window (`if now.Sub(m.last) > window`), deleting entries older than
  `until + window`
- if `seen[key]` is still in the future → `(false, nil)`
- otherwise set `seen[key] = now.Add(window)` and return `(true, nil)`

Register it:
```go
func init() {
    wire.Gates.Register("memory", func(e *wire.Env) (ports.Gate, error) {
        return NewMemory(e.Clock), nil
    })
}
```

`stages/admit.go` — add:
```go
func Debounce(gate ports.Gate, window time.Duration, keyOf func(*domain.Incident) string) Stage
```
- `keyOf == nil` defaults to `func(inc) string { return inc.Signature }`
- **on a gate error: record `inc.Note("debounce gate error, failing open: %v", err)` and return
  `nil`** — fail open
- `!allowed` → `domain.Drop("duplicate signature %s within %s", key, window)`
- otherwise `inc.SetMeta("debounce_key", key)`

Also add `func Correlate(c ports.Correlator) Stage` in the same file: call
`c.Correlate(ctx, inc)`, and on error only `inc.Note("correlation skipped: %v", err)`. (The
correlator adapter arrives in commit 36; the stage compiles now.)

**Verify:** build. In a scratch main, call `Allow` twice with the same key and a one-hour window —
`true` then `false`. Advance a `clock.Fixed` past the window and it should be `true` again.

**Why:** the entire decision lives behind `ports.Gate`, so the Redis version in commit 82 changes
nothing here.

**Amortised pruning** avoids both a background goroutine and a per-call sweep. A map that only
grows is a leak; a cleanup goroutine is another lifecycle to manage.

**Fail open** is the decision to be able to defend: broken Redis means possibly duplicate emails,
whereas failing closed means **no** emails during an infrastructure outage — exactly when they
matter most. Write the reason in a comment, as above.

---

## Commit 23 — `feat(decode): Alertmanager and generic decoders`

**Files:** `internal/adapters/ingest/decode/alertmanager.go`

**Build:**

An unexported `amPayload` struct matching the Alertmanager v4 webhook: `version`, `groupKey`,
`status`, `receiver`, `externalURL`, `commonLabels`, `commonAnnotations`, and
`alerts[]` with `status`, `labels`, `annotations`, `startsAt`, `endsAt`, `fingerprint`,
`generatorURL`.

```go
type Alertmanager struct{}
func (Alertmanager) Name() string
func (d Alertmanager) Decode(r io.Reader) ([]domain.Alert, error)
```
Input: the webhook JSON. Output: one `domain.Alert` per entry in `alerts[]`. Rules:
- error when `alerts` is empty
- `labels = merge(commonLabels, alert.labels)` — **per-alert wins**; same for annotations
- `Name = pick(labels, "alertname", "alert", "rule")`
- `Severity = domain.ParseSeverity(pick(labels, "severity", "priority"))`
- `Service = pick(labels, "service", "app", "job", "deployment", "container")`
- `Namespace = pick(labels, "namespace", "ns")`, `Cluster = pick(labels, "cluster", "env", "environment")`
- `Summary = pick(anns, "summary", "title", "message")`, `Description = pick(anns, "description", "detail")`,
  `RunbookURL = pick(anns, "runbook_url", "runbook")`
- alert-level `status`, falling back to the group's
- `EndsAt = zeroIfEpoch(a.EndsAt)` — Alertmanager sends `0001-01-01T00:00:00Z` for firing alerts
- `Source = "alertmanager"`

```go
type Generic struct{}
func (Generic) Decode(r io.Reader) ([]domain.Alert, error)
```
Accepts a single `domain.Alert` object, a JSON array of them, or `{"alerts":[...]}`. Normalise:
default `Severity` to unknown, `Source` to `"generic"`, `StartsAt` to now, and fill `Name` from
labels when empty.

Helpers: `merge(base, over map[string]string) map[string]string`,
`pick(m map[string]string, keys ...string) string`, `zeroIfEpoch(t time.Time) time.Time`.

Register both: `wire.AlertDecoders.Register("alertmanager", ...)` and `"generic"`.

Create `examples/alerts/alertmanager-oom.json` — a realistic two-alert payload with
`commonLabels` for cluster/namespace/service, a critical `ContainerOOMKilled` and a warning
`PodRestartRateHigh`. **Put a fake database password and an email address in the description**;
you will use it to prove redaction in commit 39.

**Verify:** build, then decode the fixture in a scratch main and print the two alerts. Confirm
`Target()` returns `prod-eu-1/checkout/checkout-api`.

**Why:** this is the **anti-corruption layer**. Messy external vocabulary is translated once, at
the edge, into one clean internal type. Nothing downstream ever sees Alertmanager's shape.

`pick(..., "service", "app", "job", ...)` — first non-empty wins — is how you absorb the fact that
every team labels the same concept differently, instead of forcing them to relabel.

Adding Grafana or Datadog support later is one more `AlertDecoder`. Nothing else changes.

---

## Commit 24 — `feat(memstore): in-memory storage for four ports`

**Files:** `internal/adapters/storage/memstore/memstore.go`

**Build:**

```go
type Store struct {
    limit int
    log   ports.Logger

    mu        sync.RWMutex
    incidents map[string]*domain.Incident
    order     []string
    summaries []domain.ApprovedSummary
    dead      []domain.DeadLetter
}

func New(limit int, log ports.Logger) *Store
```

One type implementing all four storage ports:
- `SaveIncident` — insert or replace; append the id to `order` when new; **trim the ring** while
  `len(order) > limit`, deleting the oldest from the map.
- `GetIncident` — return `domain.ErrNotFound` when absent.
- `SaveSummary` — replace by matching `ID`, else append; trim to `limit`.
- `ListSummaries(from, to)` — half-open window `>= from && < to`, sorted newest first.
- `Park` — append, trim, and log a warning.
- `DeadLetters() []domain.DeadLetter` — a copy, for assertions.
- `EnsurePartitions` — **no-op**, with a comment saying the maintenance service should not have to
  know which storage it is talking to.
- `DropPartitionsOlderThan(days)` — trim incidents older than the cutoff.

Registration: register the **same instance** for all four ports via a helper

```go
func shared(e *wire.Env) *Store {
    s, err := wire.Resource(e, "memstore", func() (*Store, error) {
        return New(e.Cfg.Store.MemoryLimit, e.Log.With("component", "memstore")), nil
    })
    if err != nil { return New(e.Cfg.Store.MemoryLimit, e.Log) }
    return s
}
```
then `wire.IncidentStores.Register("memory", ...)`, `SummaryStores`, `DeadLetterQueues`,
`PartitionManagers`. Also add a `LogOnly` DLQ registered as `"log"` that only writes a log line.

**Verify:** build, then in a scratch main save 600 incidents with `limit=500` and confirm the map
size stays at 500.

**Why:** `shared()` is the first real use of the resource pool — four registrations returning one
instance, so a summary written through one interface is visible through another, exactly like a
real database.

**Bounded by construction.** A dev process that runs for a week must not leak. Every in-memory
adapter in this project has a ceiling.

The no-op partition methods are the proof that `PartitionManager` is a real abstraction and not a
Postgres detail leaking through a port.

---

## Commit 25 — `feat(notify): stdout, discard and collector notifiers`

**Files:** `internal/adapters/delivery/notify/stdout.go`

**Build:**

```go
type Stdout struct { log ports.Logger; mu sync.Mutex }
func (*Stdout) Name() string
func (s *Stdout) Notify(_ context.Context, msg domain.Message) error
```
Prints a separator line, `msg.Subject`, a divider, then `msg.Text` (or `"(no text part)"`), then a
closing separator. Guard with the mutex so concurrent workers do not interleave output.

```go
type Discard struct{}                       // Notify does nothing
type Collector struct { mu sync.Mutex; sent []domain.Message }
func NewCollector() *Collector
func (c *Collector) Sent() []domain.Message // a copy
```

Register `"stdout"`, `"none"`, `"memory"`.

**Verify:** build, then in a scratch main notify a message through `Stdout` and see it printed.

**Why:** `stdout` is the **default** notifier, which is what makes a fresh clone show the full
pipeline output with no mail configuration. That matters for adoption: the first thing a reviewer
does is run it.

`Collector` makes asserting on delivery in a test two lines and no mock framework — which is the
payoff of `Notifier` being a two-method interface.

---

## End of Part 5

You have: two stages, a gate, two decoders, storage for four ports, and three notifiers. Nothing
runs yet — one more part.

```bash
go build ./... && go vet ./...
```

Next: **Part 6** — the worker pool, the HTTP edge, the triage service, and the composition root.
At commit 30 you get a running service.
