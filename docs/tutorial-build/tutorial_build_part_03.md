# Part 3 — The Pipeline Engine and Configuration (Commits 11–15)

**Goal:** the generic stage runner, its middleware, and the whole configuration surface. Still no
adapters — but by commit 15 the skeleton of the application exists.

---

## Commit 11 — `feat(pipeline): generic stage runner`

**Files:** `internal/pipeline/pipeline.go`

**Build:**

```go
type Stage[T any] interface {
    Name() string
    Process(ctx context.Context, in T) (T, error)
}

type Func[T any] func(ctx context.Context, in T) (T, error)
```

- an unexported `funcStage[T]` struct holding a name and a `Func[T]`, satisfying `Stage[T]`
- `func Of[T any](name string, fn Func[T]) Stage[T]` — lifts a function into a named stage
- `type Middleware[T any] func(Stage[T]) Stage[T]`
- `type Pipeline[T any] struct { name string; stages []Stage[T]; mws []Middleware[T] }`
- `func New[T any](name string, stages ...Stage[T]) *Pipeline[T]` — drops nil stages via
  `compact`
- `func (p *Pipeline[T]) Use(mws ...Middleware[T]) *Pipeline[T]` — chainable
- `func (p *Pipeline[T]) Append(stages ...Stage[T]) *Pipeline[T]`
- `func (p *Pipeline[T]) Names() []string` and `func (p *Pipeline[T]) String() string`
  (joined with `" -> "`)
- `func (p *Pipeline[T]) Run(ctx context.Context, in T) (T, error)`:
  - for each stage: check `ctx.Err()` first and return early if cancelled
  - wrap the stage with the middleware **in reverse order** so the first registered is outermost
  - on error, `return cur, err` — the payload **as mutated so far**, not a zero value
- `func compact[T any](in []Stage[T]) []Stage[T]` — drops nils

**Verify:** `go build ./...`, plus a scratch check:

```go
p := pipeline.New[int]("demo",
    pipeline.Of("double", func(_ context.Context, n int) (int, error) { return n * 2, nil }),
    pipeline.Of("inc",    func(_ context.Context, n int) (int, error) { return n + 1, nil }),
)
out, err := p.Run(context.Background(), 5)   // 11, nil
fmt.Println(p.String())                      // "double -> inc"
```

**Why:** three decisions to be able to defend.

**Generic over the payload**, so `Pipeline[*domain.Incident]` will not compile if you insert a
stage of the wrong type. The pre-generics version used `interface{}` plus a type assertion in
every stage — 14 copies of the same boilerplate, all of them runtime checks.

**`return cur, err`, not a zero value.** A failed run returns the payload with its trace, its logs
and its partial work intact. That is what makes the dead-letter payload useful in commit 29.

**`ctx.Err()` before each stage**, so cancellation stops at the next boundary rather than starting
a fresh model call that will be thrown away.

---

## Commit 12 — `feat(pipeline): logging, recover, timeout middleware`

**Files:** `internal/pipeline/middleware.go`

**Build:**

- `func WithLogging[T any](log ports.Logger) Middleware[T]` — times the stage, then logs:
  - success → `Debug("stage ok", "stage", name, "took", ...)`
  - **`domain.IsDrop(err)` → `Info("stage dropped payload", "stage", name, "reason", ...)`**
  - anything else → `Error("stage failed", ...)`
- `func WithRecover[T any](log ports.Logger) Middleware[T]` — converts a panic into an error.
  Use **named return values** `(out T, err error)` so the deferred function can set both; on
  panic set `out = in` and `err = fmt.Errorf("stage %s panicked: %v", ...)`.
- `func WithTimeout[T any](d time.Duration) Middleware[T]` — derives a per-stage context.
  **`if d <= 0 { return next }`** so zero disables it.
- `type Observer func(stage string, took time.Duration, err error)` and
  `func WithObserver[T any](obs Observer) Middleware[T]` — nil-safe.

**Verify:** build, then a scratch pipeline with a stage that panics — the run should return an
error rather than crashing the program.

**Why:** the drop/error distinction is the important line. A debounced duplicate is **the system
working correctly**; logging it at ERROR would train everyone to ignore errors and would page
someone at 3am for a success.

`WithObserver` is your metrics extension point. A Prometheus histogram of stage duration goes here
later, and the pipeline package still will not import Prometheus.

---

## Commit 13 — `feat(config): environment reader`

**Files:** `internal/config/env.go`

**Build:**

- `const prefix = "WT_"`
- `func LoadDotEnv(path string) error` — reads `KEY=VALUE` lines, skips blanks and `#` comments,
  strips surrounding quotes, and **only sets a variable that is not already present**. A missing
  file is **not** an error.
- `func lookup(key string) (string, bool)` — tries `WT_<key>` first, then bare `<key>`, treating
  whitespace-only as absent.
- Typed helpers, each taking a default: `str`, `integer`, `decimal`, `boolean`,
  `duration` (accepts `30s`/`5m`/`24h`, and a bare number as seconds), `list` (comma-separated,
  trimmed, empties dropped).

**Verify:** build, then confirm precedence by hand — a real environment variable must beat the
`.env` file.

**Why:** the "do not overwrite what is already set" rule is what makes `.env` safe in production:
a real environment variable always wins. The `WT_` prefix avoids collisions while the bare
fallback means `OPENAI_API_KEY` works as-is.

**This is the only package in the entire project that calls `os.Getenv`.** Everything else
receives values through a struct, which is why adapters stay usable from tests with no
environment at all.

---

## Commit 14 — `feat(config): the configuration struct and defaults`

**Files:** `internal/config/config.go`

**Build:**

`type Config struct` with nested sections: `Env string`, `Log`, `HTTP`, `Worker`, `Ingest`,
`Enrich` (containing `Loki` and `K8s`), `Clean`, `RAG`, `LLM`, `Guard`, `Store`, `Notify`,
`Cron`, `Runtime`.

`func Load() Config` populating everything from the helpers. The defaults that matter — they must
describe a system that **runs with no infrastructure at all**:

```
Env                 dev
HTTP.Addr           :8080
Worker.Workers      8          Worker.QueueSize     512      Worker.JobTimeout 3m
Ingest.Decoder      alertmanager
Ingest.GateDriver   memory     Ingest.DebounceWindow 1h      Ingest.MinSeverity warning
Enrich.Providers    [demo]     Enrich.Timeout 8s             Enrich.LogLimit 100
Clean.Processors    [dedupe signal compress cap]             Clean.MaxLines 60
RAG.EmbedderDriver  stub       RAG.VectorDriver memory       RAG.RerankDriver heuristic
RAG.CacheDriver     memory     RAG.TopK 10                   RAG.TopN 2
RAG.VectorDims      1536       RAG.MinScore 0.2
LLM.Provider        stub       LLM.PlannerModel gpt-4o-mini  LLM.MaxAttempts 2
Guard.Redactors     [secrets pii custom]                     Guard.Mode strip
Guard.Inspectors    [denylist flags allowlist]
Store.Driver        memory     Store.RetentionDays 30        Store.PartitionAhead 7
Notify.Driver       stdout     Notify.Renderer html
Cron.Enabled        true       Cron.PartitionSpec "0 0 * * *"  Cron.DigestSpec "0 8 * * *"
Runtime.BusBuffer   512        Runtime.StageTimeout 60s
```

**Verify:** build, then print `Load()` from a scratch main and eyeball the defaults.

**Why:** the default configuration is a **product decision**, not a placeholder. `go run` with no
setup must exercise the entire pipeline, because that is what a reviewer of your repository will
do, and because it means the offline path is used constantly instead of rotting.

Every driver field is a *name* that will be looked up in a registry in commit 18. That indirection
is the whole plug-and-play mechanism.

---

## Commit 15 — `feat(config): validation and a startup summary`

**Files:** `internal/config/config.go` (extend)

**Build:**

- `func (c Config) Validate() error` — collects **all** problems and joins them into one error.
  At minimum:
  - empty `HTTP.Addr`; `Worker.Workers <= 0`
  - `RAG.TopN > RAG.TopK`
  - `Store.Driver == "postgres"` with an empty DSN
  - `RAG.VectorDriver == "postgres"` with an empty DSN
  - `LLM.Provider == "openai"` or `RAG.EmbedderDriver == "openai"` with an empty API key
  - `Notify.Driver == "smtp"` with an empty host
  - `Env == "prod"` with an empty `HTTP.AuthToken` — message must say **why**:
    *"the /alerts and /runbook endpoints would otherwise be unauthenticated"*
  - `Env == "prod"` with the default `ApprovalSecret` — *"so approval links cannot be forged"*
- `func (c Config) Summary() string` — one line naming every selected driver:
  `env=dev store=memory vector=memory embedder=stub rerank=heuristic llm=stub notify=stdout ...`

**Verify:**

```bash
WT_ENV=prod go run ./cmd/watchtowerd      # once main exists, must refuse to start
```

**Why:** collecting all errors and returning them together means one restart to fix a
misconfiguration instead of five. Explaining *why* in the message turns a rejection into
guidance.

The two `prod` rules are the interesting part: **the service will not let you deploy it
insecurely by accident.** Validation is a security control, not a formality.

`Summary()` gets logged at startup, so there is never any doubt about which adapters are live in
a given environment.

---

## End of Part 3

You have a generic pipeline with middleware, and a fully specified configuration surface.

```bash
go build ./... && go vet ./... && gofmt -l .
```

Next: **Part 4** — the build environment, the plugin registries, and the event bus. After that,
adapters can finally exist.
