# Part 8 — Correlation, Redaction, and Persistence (Commits 36–40)

**Goal:** several alerts become one incident, secrets get scrubbed, and incidents start landing in
storage through the event bus instead of by direct call.

---

## Commit 36 — `feat(correlate): windowed correlator`

**Files:** `internal/adapters/ingest/correlate/window.go`

**Build:**

```go
type Window struct {
    clock  ports.Clock
    keys   []string
    window time.Duration

    mu     sync.Mutex
    groups map[string]*group
}

type group struct {
    alerts   []domain.Alert
    lastSeen time.Time
}

func NewWindow(c ports.Clock, keys []string, window time.Duration) *Window
func (w *Window) Correlate(_ context.Context, inc *domain.Incident) error
func (w *Window) keyOf(inc *domain.Incident) string
func (w *Window) prune(now time.Time)
```

`keyOf` — input: the incident; output: `cluster=prod-eu-1,namespace=checkout,service=checkout-api`
built from the configured `keys` via `primary.Label(k)`, skipping empties. When no configured label
is present, fall back to `"target=" + primary.Target()`.

`Correlate` — the algorithm is **bidirectional**:
1. compute the key, take the mutex, `prune(now)`
2. get or create the group
3. **pull**: `added := inc.MergeAlerts(g.alerts)` — historical alerts join this incident
4. **push**: `g.alerts = dedupe(append(g.alerts, inc.Alerts...))` — this incident's alerts join the
   window
5. set `g.lastSeen = now`
6. `inc.CorrelationKey = key`, `inc.SetMeta("correlation_key", key)`,
   `inc.SetMeta("correlated_alerts", itoa(len(inc.Alerts)))`
7. when `added > 0`: `inc.Note("correlated %d recent alert(s) on %s", added, key)`
8. `inc.Recompute()`, then **assign `inc.CorrelationKey = key` again**

Helper `dedupe(in []domain.Alert) []domain.Alert` — unique by `Identity()`, then
`sort.SliceStable` by `Severity.Rank()` descending.

`prune` — delete groups whose `lastSeen` is older than the window.

Register `"window"` (reading `cfg.Ingest.CorrelationKeys` and `CorrelationWindow`) and a `"none"`
no-op correlator.

**Verify:** build. In a scratch main, correlate incident A (one alert, namespace `checkout`), then
incident B (a *different* alert, same namespace) — B should end up with **two** alerts and a note
saying so.

**Why:** the second `inc.CorrelationKey = key` assignment is not a mistake — `Recompute()` defaults
`CorrelationKey` to the signature when empty, and would otherwise clobber it. Worth a comment.

Alertmanager already groups, so why this: Alertmanager groups by exact label equality on a fixed key
set and **cannot reason across groups over time**. This merges alerts that arrived in *separate*
webhooks minutes apart.

Known limitation to note in the commit message: this window is **per-process**, so with several
replicas each has a partial view. The fix is a Redis-backed correlator, and `ports.Correlator` is
already the seam that allows it.

---

## Commit 37 — `chore(bootstrap): wire correlation in`

**Files:** `internal/bootstrap/app.go`, `internal/bootstrap/plugins.go`

**Build:**

```go
correlator, err := wire.Correlators.Build(env, "window")
```
and insert `stages.Correlate(correlator)` between `Debounce` and `Enrich`. Add the blank import for
`adapters/ingest/correlate`.

**Verify:**

```bash
./bin/watchtowerd &
curl -X POST localhost:8080/alerts -H "Content-Type: application/json" \
  --data-binary @examples/alerts/alertmanager-oom.json
```
Now create a second fixture, `examples/alerts/alertmanager-oom-repeat.json`, with a **different**
`alertname` (say `ContainerOOMKilledAgain`) in the same namespace and service. Post it. The second
incident should log `correlated 2 recent alert(s)` and its title should end with `(+2 related)`.

Note that the different alert name produces a **different signature**, which is why the debouncer
lets it through — signature is per failure *kind*.

**Why:** small commit, but it is the first time you see the "five pages become one incident"
behaviour that justifies the whole project. Worth its own commit so the diff is legible.

---

## Commit 38 — `feat(guard): input redactors`

**Files:** `internal/adapters/cognition/guard/redact.go`

**Build:**

```go
type Rule struct { Label string; Re *regexp.Regexp }

type Pattern struct { label string; rules []Rule }
func NewPattern(label string, rules []Rule) Pattern
func NewRaw(label string, patterns []string) (Pattern, error)     // compiles user-supplied regexes
func (p Pattern) Name() string
func (p Pattern) Redact(text string) string                       // replaces with "[REDACTED:<LABEL>]"
```

`secretPatterns` — at minimum these labels and shapes:
- `AWS_KEY` — `\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`
- `AWS_SECRET` — `(?i)\baws_?secret_?access_?key\b\s*[:=]\s*\S+`
- `PRIVATE_KEY` — `(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`
- `JWT` — `\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b`
- `BEARER` — `(?i)\b(?:bearer|token)\s+[A-Za-z0-9._\-]{12,}`
- `GITHUB_PAT` — `\bgh[pousr]_[A-Za-z0-9]{20,}\b`
- `SLACK` — `\bxox[abprs]-[A-Za-z0-9-]{10,}\b`
- `OPENAI_KEY` — `\bsk-[A-Za-z0-9_\-]{20,}\b`
- `DSN_PASSWORD` — `(?i)\b([a-z0-9+.\-]+)://([^:@\s/]+):([^@\s/]+)@`
- `ASSIGNMENT` — `(?i)\b(password|passwd|secret|api[_-]?key|access[_-]?key|auth[_-]?token|client[_-]?secret)\b\s*[:=]\s*("[^"]*"|'[^']*'|\S+)`
- `BASIC_AUTH` — `(?i)\bauthorization\s*[:=]\s*\S+`

`piiPatterns` — `EMAIL`, `CARD`, `SSN`, `PHONE`. **Deliberately no IP rule**, with the comment:
```go
// Note: IP addresses are deliberately not redacted by default. They are usually
// the most useful token in a network incident. Add them via
// WT_INPUT_REDACT_PATTERNS if your policy requires it.
```

Register `"secrets"`, `"pii"`, and `"custom"` (the last built from
`cfg.Guard.CustomRedact` via `NewRaw`).

**Verify:** build, then scratch-check a handful of inputs:
```
postgres://svc:s3cr3tp4ss@db.internal:5432/checkout → [REDACTED:DSN_PASSWORD]db.internal:...
password: hunter2hunter2                             → [REDACTED:ASSIGNMENT]
sk-abcdefghijklmnopqrstuvwxyz012345                  → [REDACTED:OPENAI_KEY]
oncall@example.com                                   → [REDACTED:EMAIL]
```

**Why:** `(?s)` makes `.` match newlines, which you need for multi-line PEM blocks. The `JWT` rule
keys off `eyJ`, which is `{"` base64-encoded.

Replacing with a **labelled marker** rather than blanking means the reviewer can see that something
was removed and what kind — much better than staring at an unexplained gap.

The IP omission is the interesting part: it is a security-versus-utility judgement, stated
explicitly, with an escape hatch. Interviewers would rather hear you reason about a tradeoff than
apply a default.

---

## Commit 39 — `feat(stages): the input guardrail`

**Files:** `internal/stages/guard.go`

**Build:**

```go
func InputGuard(redactors []ports.Redactor) Stage                       // delegates to the named version
func InputGuardNamed(name string, redactors []ports.Redactor) Stage
```

Return early when there are no redactors. Build a local
`clean := func(s string) string { for _, r := range redactors { s = r.Redact(s) }; return s }`, then
scrub **every text field**:

- for each alert (`for i := range inc.Alerts`, taking `a := &inc.Alerts[i]`): `Summary`,
  `Description`, and every value in `Annotations` and `Labels`
- every `inc.Logs[i].Message`
- every `inc.Events[i].Message`
- every retrieved runbook's `RootCause`, and each command's `Run` and `Explain`

Finish with `inc.SetMeta("redactors", strings.Join(names, "+"))`.

**Verify:** build. You will prove it in commit 40.

**Why:** `for i := range` and `&inc.Alerts[i]` are mandatory — ranging by value copies the struct
and mutates the copy, which is the classic Go mistake and here it would be a silent security
failure.

The field list must be **exhaustive**, not "the ones likely to matter". Missing one is a leak.

`InputGuardNamed` exists so the same guard can appear twice in a chain with distinguishable log
lines. You will use the second pass in commit 65, over material retrieved from a shared database.
Redaction is idempotent, so a second pass has no side effects.

---

## Commit 40 — `feat(projectors): persistence through the bus`

**Files:** `internal/projectors/persist.go`, `internal/stages/publish.go`,
`internal/bootstrap/app.go` (edit)

**Build:**

`stages/publish.go` — add (you already added `Tap` in commit 30):
```go
func Publish(b bus.Bus, topic string) Stage
```
Four lines: `b.Publish(ctx, topic, inc)` and return the incident unchanged. Name the stage
`"publish:" + topic`.

`projectors/persist.go`:
```go
type Persist struct {
    store ports.IncidentStore
    dlq   ports.DeadLetterQueue
    ids   ports.IDGen
    clock ports.Clock
    log   ports.Logger
}

func NewPersist(store ports.IncidentStore, dlq ports.DeadLetterQueue, ids ports.IDGen,
    clock ports.Clock, log ports.Logger) *Persist

func (p *Persist) Register(broker bus.Broker)
func (p *Persist) handle(ctx context.Context, e bus.Event) error
func (p *Persist) park(ctx context.Context, stage, reason string, inc *domain.Incident)
```
`Register` subscribes the same handler to **three** topics: `TopicIncidentSanitized`,
`TopicIncidentTriaged`, `TopicIncidentFailed`, labelling each `"persist:" + topic`.

`handle` — type-assert `e.Payload.(*domain.Incident)`, returning nil when it is something else.
Save it; on error `park(...)` and return the error.

Also add a `type Audit struct` with `NewAudit(log)` and `Register` subscribing to `"*"`, logging
topic and time at debug.

Wire in `app.go`, replacing the temporary `Tap` from commit 30:
```go
redactors, err := wire.Redactors.BuildAll(env, cfg.Guard.Redactors)
...
a.broker = bus.NewAsync(...)
projectors.NewPersist(incidentStore, dlq, env.IDs, env.Clock, log).Register(a.broker)
projectors.NewAudit(log).Register(a.broker)
```
and the pipeline becomes:
```
admit -> debounce -> correlate -> enrich -> sanitize -> input-guard -> publish:incident.sanitized
```
Keep a temporary notifier `Tap` at the end for now so you still see output; the renderer arrives in
Part 11.

**Verify:**

```bash
./bin/watchtowerd &
curl -X POST localhost:8080/alerts -H "Content-Type: application/json" \
  --data-binary @examples/alerts/alertmanager-oom.json
```
Expect `incident persisted projector=persist topic=incident.sanitized` at debug level
(`WT_LOG_LEVEL=debug`). Add a temporary debug print of `store.DeadLetters()` or of the saved
incident's `Meta` and confirm `redactors=secrets+pii+custom`, and that the fake password from your
fixture is now `[REDACTED:DSN_PASSWORD]` in the stored copy.

**Why — the ordering is the whole point.** Note where `input-guard` sits: **before**
`publish:incident.sanitized`. In the original build of this project it sat *after* persistence and
after the embedding call, which meant credentials from a log line were written to Postgres and could
be sent to a model provider. That was found by querying the stored payload for the fake password on
the real stack. Putting the guard first, here, is not a stylistic choice.

Why persist at `sanitized` rather than only at the end: the expensive, failure-prone part
(retrieval, models) has not happened yet. If it all falls over, **the evidence is already safe**.
The later `triaged` write updates the same row, which is why the SQL in commit 77 must be an upsert.

---

## End of Part 8

Correlation, redaction and durable persistence are in, and the pipeline is seven stages long.

```bash
go build ./... && go vet ./... && gofmt -l .
```

Next: **Part 9** — prompts and the offline planner/critic loop. The system starts producing plans.
