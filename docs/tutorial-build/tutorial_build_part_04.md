# Part 4 — Environment, Registries, and the Event Bus (Commits 16–20)

**Goal:** the machinery that turns a config string into a live adapter, plus the in-process
publish/subscribe hop. After this part, adapters have somewhere to plug in.

---

## Commit 16 — `feat(wire): build environment with ordered shutdown`

**Files:** `internal/wire/env.go`

**Build:**

```go
type Env struct {
    Cfg   config.Config
    Log   ports.Logger
    Clock ports.Clock
    IDs   ports.IDGen
    HTTP  *http.Client

    mu        sync.Mutex
    resources map[string]any

    closeMu sync.Mutex          // ← a SECOND, separate mutex. Commit 17 explains why.
    closers []func() error
}
```

- `func NewEnv(cfg config.Config, log ports.Logger, clock ports.Clock, ids ports.IDGen) *Env` —
  builds the struct and a shared `*http.Client` with `Timeout: 60s` and a `Transport` setting
  `MaxIdleConns: 64`, `MaxIdleConnsPerHost: 16`, `IdleConnTimeout: 90s`.
- `func (e *Env) OnClose(fn func() error)` — appends under **`closeMu`**.
- `func (e *Env) Close() error` — copies the closers under `closeMu`, clears the slice, then runs
  them **in reverse registration order**, returning the first error but running all of them.

**Verify:** `go build ./...`

**Why:** this struct is what every adapter factory receives, which is why the factory signature
`func(e *Env) (T, error)` never has to grow no matter what an adapter needs.

The **shared HTTP client** matters: adapters that use it reuse TCP connections and TLS sessions
instead of each keeping a private pool.

**Reverse-order shutdown** is the `defer` discipline applied to resources: things built last are
torn down first, so a dependency is never closed while something still needs it.

The two mutexes look like over-engineering right now. Commit 17 is where you find out they are not.

---

## Commit 17 — `feat(wire): memoized resource pool`

**Files:** `internal/wire/env.go` (extend)

**Build:**

```go
func Resource[T any](e *Env, key string, build func() (T, error)) (T, error)
```

- input: a string key and a constructor; output: the memoized value for that key.
- **Look up under `mu`, then release the lock, then call `build()`.** Do not hold `mu` while
  building.
- On insert, take `mu` again and double-check — if another goroutine won the race, return theirs.
- If the key exists but holds a different type, return
  `fmt.Errorf("resource %q already registered as %T", key, existing)`.
- Add a small `func (e *Env) lookup(key string) (any, bool)` helper that takes `mu` briefly.

Add the comment that explains the two-mutex design, in the struct from commit 16:

```go
// Two independent locks on purpose. A resource factory is expected to register
// its own shutdown hook while it is being built, so guarding the resource map
// and the closer list with one mutex would deadlock the moment a factory calls
// OnClose. sync.Mutex is not reentrant.
```

**Verify:** build, then prove memoization in a scratch main:

```go
n := 0
for i := 0; i < 3; i++ {
    v, _ := wire.Resource(env, "thing", func() (int, error) { n++; return 42, nil })
    fmt.Println(v)      // 42, 42, 42
}
fmt.Println(n)          // 1  ← built exactly once
```

**⚠️ The trap this avoids.** The original version of this function held `mu` for its whole body,
`defer`-unlocking at the end. Every factory calls `e.OnClose(...)` to register cleanup, and
`OnClose` took the same mutex. Go mutexes are **not reentrant**, so the goroutine deadlocked
against itself. The symptom was the service hanging after printing exactly one log line — and
**only** when using Postgres or Redis, because the in-memory adapters never register a closer, so
every offline test passed. Writing it correctly now, with the comment, is cheaper than debugging
it at commit 76.

**Why:** four Postgres adapters need one connection pool; two Redis adapters need one client.
Nobody should own it, and it must be lazy — a memory-only configuration must never open a
connection. A memoized pool keyed by string gives you exactly that.

---

## Commit 18 — `feat(wire): generic driver registry`

**Files:** `internal/wire/registry.go`

**Build:**

```go
type Factory[T any] func(e *Env) (T, error)

type Registry[T any] struct {
    kind  string
    mu    sync.RWMutex
    items map[string]Factory[T]
}
```

- `func NewRegistry[T any](kind string) *Registry[T]`
- `func (r *Registry[T]) Register(name string, f Factory[T])` — **`panic` on a duplicate name**,
  with the message `"wire: duplicate <kind> driver <name>"`.
- `func (r *Registry[T]) Build(e *Env, name string) (T, error)` — lowercase and trim the name
  before lookup; on a miss return
  `"wire: unknown <kind> driver %q (available: <sorted names>)"`.
- `func (r *Registry[T]) BuildAll(e *Env, names []string) ([]T, error)` — builds several in order,
  **skipping empty strings and `"none"`**.
- `func (r *Registry[T]) Has(name string) bool`, `func (r *Registry[T]) Names() []string`
  (sorted).

**Verify:** build, then in a scratch main register two fakes and confirm a typo produces the
"available:" list.

**Why:** the alternative is a `switch` on every driver name in the wiring code, which means that
file imports every adapter package and every new adapter is a merge conflict.

**Panic on duplicate registration** is correct here: two adapters claiming `postgres` is always a
programming error, it can only happen during `init()`, and silently letting the last one win would
be miserable to debug.

**The error listing valid names** turns a typo in an environment variable into a one-line fix
instead of a support conversation.

`BuildAll` skipping `"none"` lets an operator disable one entry in a list without editing the list.
It is what turns `WT_ENRICH_PROVIDERS=loki,k8s` into a slice and
`WT_SANITIZE_CHAIN=dedupe,signal,compress,cap` into an **ordered** chain — the log-cleaning
algorithm becomes an environment variable.

---

## Commit 19 — `feat(wire): one registry per port`

**Files:** `internal/wire/registries.go`

**Build:** one package-level variable per port:

```go
var (
    AlertDecoders    = NewRegistry[ports.AlertDecoder]("alert decoder")
    Gates            = NewRegistry[ports.Gate]("gate")
    Correlators      = NewRegistry[ports.Correlator]("correlator")
    ContextProviders = NewRegistry[ports.ContextProvider]("context provider")
    LogProcessors    = NewRegistry[ports.LogProcessor]("log processor")

    Embedders    = NewRegistry[ports.Embedder]("embedder")
    VectorStores = NewRegistry[ports.VectorStore]("vector store")
    Rerankers    = NewRegistry[ports.Reranker]("reranker")
    Caches       = NewRegistry[ports.Cache]("cache")

    Redactors       = NewRegistry[ports.Redactor]("redactor")
    RiskClassifiers = NewRegistry[ports.RiskClassifier]("risk classifier")
    PromptBuilders  = NewRegistry[ports.PromptBuilder]("prompt builder")
    Planners        = NewRegistry[ports.Planner]("planner")
    Critics         = NewRegistry[ports.Critic]("critic")
    Inspectors      = NewRegistry[ports.CommandInspector]("command inspector")
    Scorers         = NewRegistry[ports.Scorer]("scorer")
    Narrators       = NewRegistry[ports.Narrator]("narrator")

    IncidentStores    = NewRegistry[ports.IncidentStore]("incident store")
    SummaryStores     = NewRegistry[ports.SummaryStore]("summary store")
    DeadLetterQueues  = NewRegistry[ports.DeadLetterQueue]("dead letter queue")
    PartitionManagers = NewRegistry[ports.PartitionManager]("partition manager")

    Renderers  = NewRegistry[ports.Renderer]("renderer")
    Notifiers  = NewRegistry[ports.Notifier]("notifier")
    Schedulers = NewRegistry[ports.Scheduler]("scheduler")
)
```

**Verify:** `go build ./...`

**Why:** this one file is a **table of contents for everything about the system that can vary**.
Every axis of reconfiguration is one line here. When someone asks "what can you swap?", you show
them this file.

---

## Commit 20 — `feat(bus): in-process publish/subscribe`

**Files:** `internal/bus/bus.go`

**Build:**

Topic constants: `TopicIncidentReceived`, `TopicIncidentSanitized`, `TopicIncidentTriaged`,
`TopicIncidentDropped`, `TopicIncidentFailed`, `TopicFeedbackAccepted`, `TopicRunbookLearned`,
`TopicDigestReady` — values `"incident.received"` etc. **Past-tense facts, not commands.**

```go
type Event struct { Topic string; At time.Time; Payload any }

type Handler interface {
    Name() string
    Handle(ctx context.Context, e Event) error
}

type HandlerFunc struct {
    Label string
    Fn    func(ctx context.Context, e Event) error
}

type Bus interface {                      // publish only — producers get this
    Publish(ctx context.Context, topic string, payload any)
}

type Broker interface {                   // publish + subscribe — the composition root gets this
    Bus
    Subscribe(topic string, h Handler)
}
```

`type Async struct` — a buffered `chan Event`, a worker count, `subs map[string][]Handler` under
an `RWMutex`, a `done chan struct{}`, a `sync.WaitGroup`, and `sync.Once` for start and stop.

- `func NewAsync(log ports.Logger, clock ports.Clock, buffer, workers int) *Async` — defaults
  256 and 2 when non-positive.
- `func (b *Async) Subscribe(topic string, h Handler)`
- `func (b *Async) Publish(_ context.Context, topic string, payload any)` — **non-blocking**:
  `select` on the send with a `default` that logs `Warn("event dropped, buffer full", ...)`.
- `func (b *Async) Start()` — under `sync.Once`, launches N goroutines running a `loop()` that
  selects on `done` and the queue.
- `dispatch(e Event)` — copies the handler list for `e.Topic` **plus** the `"*"` wildcard list
  under `RLock`, then runs each handler in its own closure with its own `recover` and a 2-minute
  context. A handler error is logged with the handler name; a panic is logged, not propagated.
- `func (b *Async) Close(ctx context.Context) error` — under `sync.Once`, tries to drain the queue
  bounded by both a 2-second timer and `ctx`, then closes `done` and waits on the WaitGroup.
- `type Discard struct{}` with a no-op `Publish`, for tests.

**Verify:** build, then in a scratch main subscribe two handlers to one topic, publish, `Start()`,
sleep briefly, and confirm both ran. Then make one handler panic and confirm the other still runs.

**Why:** the pipeline will announce `incident.sanitized` and `incident.triaged` and nothing else.
It will never import a store, a renderer or a mailer. Consumers subscribe.

**Publishers never block**, and that is a deliberate tradeoff: blocking a publisher blocks the
pipeline, which blocks a worker, which eventually rejects webhooks. Losing an audit event is better
than stalling ingestion — and the warning makes it visible. If events were business-critical you
would use a persistent queue, and `Bus` is exactly the seam that allows that later.

**Per-handler recover and timeout** means handlers are independent by construction: a panicking
email renderer cannot stop the persistence handler.

Two interfaces, `Bus` and `Broker`, so producers are handed the narrowest thing they need. A
producer has no business knowing who is listening.

---

## End of Part 4

Infrastructure for composition is done: a build environment with lazy shared resources, typed
registries for every port, and an event bus.

```bash
go build ./... && go vet ./...
```

Next: **Part 5** — the first stages and the first real adapters.
