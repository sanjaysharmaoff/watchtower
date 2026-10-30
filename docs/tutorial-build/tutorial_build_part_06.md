# Part 6 — Worker Pool, HTTP Edge, and a Running Binary (Commits 26–30)

**Goal:** by commit 30 you `curl` an alert into a running service and an email appears on stdout.
This is the milestone part.

---

## Commit 26 — `feat(worker): bounded dispatcher`

**Files:** `internal/adapters/ingest/worker/pool.go`

**Build:**

```go
var ErrQueueFull = errors.New("worker queue is full")
var ErrClosed    = errors.New("worker pool is closed")

type Pool struct {
    queue      chan func(context.Context)
    workers    int
    jobTimeout time.Duration
    log        ports.Logger

    wg       sync.WaitGroup
    closing  atomic.Bool
    closed   chan struct{}
    stopOnce sync.Once

    accepted, rejected, failed, done atomic.Int64
}

func New(workers, queueSize int, jobTimeout time.Duration, log ports.Logger) *Pool
```
- defaults: `workers` 4, `queueSize = workers * 32` when non-positive; starts the goroutines and
  logs `"worker pool started"`.
- `func (p *Pool) Submit(job func(context.Context)) error` — **non-blocking**: return `ErrClosed`
  when closing; `select` on the send with a `default` returning `ErrQueueFull`. Increment counters.
- `loop(id int)` — selects on `p.closed` and the queue; **on close, drain whatever is already
  queued** with an inner `select`/`default` loop before returning.
- `run(job)` — `defer recover()` logging a panic; build
  `ctx := context.Background()` and apply `jobTimeout` if positive; call the job.
- `func (p *Pool) Shutdown(ctx context.Context) error` — under `stopOnce` set closing and close
  `done`; then wait on the WaitGroup in a goroutine and select against `ctx`.
- `func (p *Pool) Stats() map[string]int64` — accepted, completed, rejected, panics, queued.

**Verify:** build; in a scratch main submit 3 jobs to a 2-worker pool, `Shutdown`, and confirm all
3 ran. Then submit more than `queueSize` at once and confirm you get `ErrQueueFull`.

**Why:** **`context.Background()`, not the request context** — this is subtle and important. The
HTTP handler returns `202` immediately, which cancels the request context. If the job inherited it,
every job would be cancelled the instant the response was written.

**Non-blocking submit** is the difference between backpressure and collapse: a full queue becomes
an immediate honest `429` instead of a hanging request that makes Alertmanager time out and retry,
adding load during overload.

---

## Commit 27 — `feat(httpapi): server, middleware and auth`

**Files:** `internal/adapters/inbound/httpapi/server.go`

**Build:**

```go
type Deps struct {
    Config   config.HTTPConfig
    Log      ports.Logger
    Clock    ports.Clock
    Decoder  ports.AlertDecoder
    Alerts   ports.AlertSink
    Feedback ports.FeedbackSink
    Signer   ports.Signer
    Version  string
    Ready    func(ctx context.Context) error
    Stats    func() map[string]int64
}

type Server struct { deps Deps; srv *http.Server; log ports.Logger }
func New(d Deps) *Server
func (s *Server) Start() error
func (s *Server) Shutdown(ctx context.Context) error
```

`New` registers Go 1.22 method-aware routes:
```go
mux.HandleFunc("POST /alerts", s.handleAlerts)
mux.HandleFunc("POST /runbook/approve", s.handleFeedbackJSON)
mux.HandleFunc("GET /runbook/approve", s.handleFeedbackLink)
mux.HandleFunc("GET /healthz", s.handleHealth)
mux.HandleFunc("GET /readyz", s.handleReady)
mux.HandleFunc("GET /status", s.handleStatus)
```
then `handler := s.withRecover(s.withLogging(s.withAuth(mux)))`, and an `http.Server` with
`ReadHeaderTimeout: 5s` plus the configured read/write/idle timeouts.

Middleware:
- `withAuth` — skip when the token is empty, when the path is `/healthz` or `/readyz`, or when it
  is `GET /runbook/approve`. Read the token from `Authorization: Bearer` or `X-WatchTower-Token`,
  compare with `constantTimeEqual`, log a warning with the client IP on failure, return `401`.
- `withLogging` — wrap the writer in a `statusRecorder` to capture the status; log method, path,
  status, duration, remote.
- `withRecover` — log the panic, return `500`.

Helpers: `writeJSON(w, status, payload)` setting `Content-Type` and
`X-Content-Type-Options: nosniff`; `clientIP(r)` honouring the first `X-Forwarded-For` entry;
`constantTimeEqual(a, b string) bool` — length check then XOR accumulate.

Add the comment explaining the one exemption:
```go
// GET /runbook/approve is deliberately exempt: it is clicked from an email client
// that cannot set headers, and it carries its own HMAC signature and expiry, which
// is what authenticates it.
```

> The three feedback/health handlers do not exist yet — commit 28 adds them. Write them as stubs
> if you want a compiling intermediate state.

**Verify:** build.

**Why:** `ReadHeaderTimeout` is the one people omit, and it is the Slowloris defence. Go's default
`http.Server` has **no** timeouts at all.

Middleware order matters: recover outermost so it catches panics from the auth middleware too;
logging next so rejected requests are still logged.

`constantTimeEqual` rather than `==` so response timing cannot leak the token byte by byte.

---

## Commit 28 — `feat(httpapi): alerts, health and status handlers`

**Files:** `internal/adapters/inbound/httpapi/handlers.go`

**Build:**

```go
func (s *Server) handleAlerts(w http.ResponseWriter, r *http.Request)
```
1. wrap the body in `http.MaxBytesReader(w, r.Body, s.maxBody())` and `defer Close()`
2. `s.deps.Decoder.Decode(body)` — on error log a warning and return `400` with the message
3. `id, err := s.deps.Alerts.Submit(r.Context(), alerts)`, then switch:
   - `errors.Is(err, worker.ErrQueueFull)` → set `Retry-After: 30`, return `429` with
     `{"error":"pipeline saturated, retry shortly","incident_id":id}`
   - other error → `500`
   - success → **`202`** with `{"incident_id":..., "alerts":N, "status":"queued"}`

```go
func (s *Server) handleHealth(w, r)   // 200 {"status":"ok","version":...}   — no dependencies
func (s *Server) handleReady(w, r)    // calls deps.Ready(ctx); 503 with the error when it fails
func (s *Server) handleStatus(w, r)   // version, time, and deps.Stats() when present
func (s *Server) maxBody() int64      // config value or 2<<20
```

Also add `writeHTML(w, status, title, detail string)` — a minimal accessible page with
`html.EscapeString` on both strings and `lang="en"`. You need it in commit 57.

Leave `handleFeedbackJSON` and `handleFeedbackLink` as `501 Not Implemented` for now with a `TODO`
naming commits 56 and 57.

**Verify:** build.

**Why:** **`202 Accepted`, not `200`** — the work is queued, not completed, and the status code
should say so.

Three outcomes mapped to three status codes, with `Retry-After` on the backpressure path, is what
makes the sender able to behave correctly. Returning `200` on a full queue would be a lie that
silently drops alerts.

`/healthz` must stay dependency-free: if it consulted the database, a brief blip would get healthy
pods killed. `/readyz` is where dependency checks belong.

---

## Commit 29 — `feat(services): the triage service`

**Files:** `internal/services/triage.go`

**Build:**

```go
type Triage struct {
    pipe  *pipeline.Pipeline[*domain.Incident]
    disp  ports.Dispatcher
    bus   bus.Bus
    dlq   ports.DeadLetterQueue
    ids   ports.IDGen
    clock ports.Clock
    log   ports.Logger
}

func NewTriage(pipe *pipeline.Pipeline[*domain.Incident], disp ports.Dispatcher, b bus.Bus,
    dlq ports.DeadLetterQueue, ids ports.IDGen, clock ports.Clock, log ports.Logger) *Triage
```

- `func (t *Triage) Submit(ctx context.Context, alerts []domain.Alert) (string, error)` —
  input: normalized alerts; output: the new incident id.
  1. error on an empty slice
  2. `inc := domain.NewIncident(t.ids.New(), t.clock.Now(), alerts)`
  3. publish `bus.TopicIncidentReceived`
  4. `t.disp.Submit(func(jobCtx context.Context) { t.Process(jobCtx, inc) })`
  5. **on submit failure, `t.park(ctx, "dispatch", err.Error(), inc)`** and return
     `(inc.ID, err)` — a saturated pipeline must lose nothing
  6. log `"incident queued"` and return `(inc.ID, nil)`

- `func (t *Triage) Process(ctx context.Context, inc *domain.Incident)` — **exported**. Runs the
  pipeline and switches on the outcome:
  - `err == nil` → `Info("incident triaged", "confidence", ..., "attempts", ...)`
  - `domain.IsDrop(err)` → note it, publish `TopicIncidentDropped`, log at **info**
  - `errors.Is(err, context.DeadlineExceeded)` or `Canceled` → note, publish
    `TopicIncidentFailed`, park with stage `"pipeline-timeout"`, log at error
  - default → note, publish `TopicIncidentFailed`, park with stage `"pipeline"`, log at error

- `func (t *Triage) park(ctx context.Context, stage, reason string, inc *domain.Incident)` —
  `json.Marshal` the incident into `domain.DeadLetter{ID: t.ids.New(), Stage: stage, Reason: reason,
  RefID: inc.ID, Payload: ..., At: t.clock.Now()}` and call `t.dlq.Park`. Log an error if even that
  fails.

**Verify:** build.

**Why:** `Submit` returns as soon as the work is queued, which is what keeps Alertmanager from
timing out and retrying.

`Process` is exported deliberately — add the comment *"Exported so a replay tool or a test can
drive it directly without a queue."* That single decision is what makes a DLQ replay tool a small
addition later rather than a refactor.

Four outcomes, four behaviours, one `error` return type. This is the payoff of the sentinel errors
from commit 6.

---

## Commit 30 — `feat(bootstrap): composition root and entrypoint` 🎉

**Files:** `internal/bootstrap/app.go`, `internal/bootstrap/plugins.go`,
`cmd/watchtowerd/main.go`

**Build:**

`plugins.go` — blank imports for their registration side effects only:
```go
import (
    _ ".../internal/adapters/delivery/notify"
    _ ".../internal/adapters/ingest/decode"
    _ ".../internal/adapters/ingest/gate"
    _ ".../internal/adapters/storage/memstore"
)
```

`app.go`:
```go
var Version = "dev"                      // injected at link time later

type App struct {
    cfg    config.Config
    log    ports.Logger
    env    *wire.Env
    broker *bus.Async
    pool   *worker.Pool
    server *httpapi.Server
    Triage *services.Triage
}

func New(cfg config.Config) (*App, error)
func (a *App) build() error
func (a *App) Run(ctx context.Context) error
func (a *App) Shutdown() error
```

`New` — `cfg.Validate()`, build the logger, `wire.NewEnv(...)`, log
`"watchtower starting"` with `cfg.Summary()`, then `build()`; on failure `env.Close()`.

`build()` in labelled phases (leave gaps for what does not exist yet):
1. storage: `wire.IncidentStores.Build(env, cfg.Store.Driver)`, plus DLQ
2. delivery: `wire.Notifiers.Build(env, cfg.Notify.Driver)`
3. ingestion: `wire.AlertDecoders.Build(env, cfg.Ingest.Decoder)`,
   `wire.Gates.Build(env, cfg.Ingest.GateDriver)`
4. bus: `bus.NewAsync(log, env.Clock, cfg.Runtime.BusBuffer, cfg.Runtime.BusWorkers)`
5. the pipeline — for now just two stages:
   ```go
   pipe := pipeline.New[*domain.Incident]("triage",
       stages.Admit(stages.AdmitOptions{
           DropResolved: cfg.Ingest.DropResolved,
           MinSeverity:  domain.ParseSeverity(cfg.Ingest.MinSeverity),
       }),
       stages.Debounce(gate, cfg.Ingest.DebounceWindow, nil),
   ).Use(
       pipeline.WithRecover[*domain.Incident](log),
       pipeline.WithLogging[*domain.Incident](log),
       pipeline.WithTimeout[*domain.Incident](cfg.Runtime.StageTimeout),
   )
   log.Info("pipeline assembled", "stages", pipe.String())
   ```
6. a temporary inline stage that renders and prints, so you can see output before the renderer
   exists — use `stages.Tap` (add it to `stages/publish.go` now: it takes a name and a
   `func(ctx, inc)` and returns the incident unchanged) calling `notifier.Notify` with a
   hand-built `domain.Message{Subject: inc.Title(), Text: ...}`
7. services: `worker.New(...)`, `services.NewTriage(...)`
8. the HTTP server via `httpapi.New(httpapi.Deps{...})` with `Stats: a.pool.Stats`

`Run` — `broker.Start()`, launch `server.Start()` in a goroutine, `select` on that error channel
and `ctx.Done()`, calling `Shutdown()` on cancellation.

`Shutdown` — in order: server, pool, broker, `env.Close()`, each bounded by
`cfg.HTTP.ShutdownGrace`, collecting errors with `errors.Join`.

`main.go` — flags `--env-file` (default `.env`) and `--version`; `config.LoadDotEnv`;
`bootstrap.New(config.Load())`; `signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)`;
`app.Run(ctx)`; exit 1 with a message on any error.

**Verify — this is the milestone:**

```bash
go build -o bin/watchtowerd ./cmd/watchtowerd
./bin/watchtowerd

# in another shell
curl -X POST http://localhost:8080/alerts -H "Content-Type: application/json" \
  --data-binary @examples/alerts/alertmanager-oom.json
# → {"alerts":2,"incident_id":"...","status":"queued"}

curl http://localhost:8080/healthz     # {"status":"ok","version":"dev"}
```

You should see in the log: `incident queued`, `stage ok stage=admit`,
`stage ok stage=debounce`, then your printed message. **Send the same payload again** — the second
one should log `stage dropped payload stage=debounce reason="dropped: duplicate signature ..."`.

**Why:** you now have a working service with a real pipeline, and everything from here is adding
stages and adapters to a system that already runs. That is deliberate: the remaining 70 commits
each end with something you can observe, rather than a big-bang integration at the end.

---

## End of Part 6

**Milestone reached.** An alert in, a debounced duplicate, an email on stdout.

```bash
git log --oneline | head -30
```

Next: **Part 7** — enrichment and the sanitizer chain, so the incident carries evidence.
