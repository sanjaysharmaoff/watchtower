# Part 15 — The Registry, the Composition Root and the Resource Pool

Ports define the holes. Adapters fill them. Something has to decide *which* adapter fills
*which* hole, and build them in the right order. That is this part — and it is where the
"plug and play" claim is actually implemented.

## The problem: the giant switch statement

The obvious way to pick an implementation from config:

```go
var store ports.IncidentStore
switch cfg.Store.Driver {
case "postgres":
    store = postgres.NewIncidents(pool)
case "memory":
    store = memstore.New(...)
default:
    return fmt.Errorf("unknown store driver %q", cfg.Store.Driver)
}
```

Multiply that by 15 ports. Now every new adapter means editing this file, which means it
imports every adapter package, which means a merge conflict for every team adding one, and a
compile-time dependency on all of them.

## The solution: self-registering adapters

Each adapter registers itself into a typed registry at package init time.

```go
// internal/adapters/storage/postgres/incidents.go
func init() {
    wire.IncidentStores.Register("postgres", func(e *wire.Env) (ports.IncidentStore, error) {
        pool, err := Pool(e)
        if err != nil { return nil, err }
        return &Incidents{pool: pool}, nil
    })
}
```

```go
// internal/adapters/storage/memstore/memstore.go
func init() {
    wire.IncidentStores.Register("memory", func(e *wire.Env) (ports.IncidentStore, error) {
        return shared(e), nil
    })
}
```

And the composition root asks by name:

```go
incidentStore, err := wire.IncidentStores.Build(env, cfg.Store.Driver)
```

No switch. No imports of adapter packages in the wiring logic. Adding a driver is one file
plus one blank import.

## The generic registry

```go
// internal/wire/registry.go
type Factory[T any] func(e *Env) (T, error)

type Registry[T any] struct {
    kind  string
    mu    sync.RWMutex
    items map[string]Factory[T]
}

func (r *Registry[T]) Register(name string, f Factory[T]) {
    r.mu.Lock()
    defer r.mu.Unlock()
    if _, dup := r.items[name]; dup {
        panic(fmt.Sprintf("wire: duplicate %s driver %q", r.kind, name))
    }
    r.items[name] = f
}

func (r *Registry[T]) Build(e *Env, name string) (T, error) {
    r.mu.RLock()
    f, ok := r.items[strings.ToLower(strings.TrimSpace(name))]
    r.mu.RUnlock()
    if !ok {
        var zero T
        return zero, fmt.Errorf("wire: unknown %s driver %q (available: %s)",
            r.kind, name, strings.Join(r.Names(), ", "))
    }
    return f(e)
}
```

Four details worth pointing at in an interview:

**Generic over the port type.** `Registry[ports.IncidentStore]` cannot accidentally be given a
`Notifier` factory. Before generics this would be `map[string]func(...) interface{}` with a
type assertion at every call site.

**`panic` on duplicate registration.** Harsh on purpose. Two adapters claiming the name
`postgres` is always a programming error, it can only happen at startup, and silently letting
the last one win would be a nightmare to debug. Panicking during `init` is the right time to
fail.

**The error message lists what *is* available.** `unknown notifier driver "slak" (available:
memory, none, smtp, stdout)` — a typo in an env var becomes a one-line fix instead of a
support ticket. Small thing; it is the difference between a tool people can use and one they
cannot.

**Case and whitespace normalised** on lookup, because these values come from environment
variables typed by humans.

There is also `BuildAll` for ports where several implementations run together:

```go
func (r *Registry[T]) BuildAll(e *Env, names []string) ([]T, error) {
    out := make([]T, 0, len(names))
    for _, n := range names {
        if n == "" || strings.EqualFold(n, "none") { continue }
        v, err := r.Build(e, n)
        if err != nil { return nil, err }
        out = append(out, v)
    }
    return out, nil
}
```

That is what turns `WT_ENRICH_PROVIDERS=loki,k8s` into a `[]ports.ContextProvider`, and
`WT_SANITIZE_CHAIN=dedupe,signal,compress,cap` into an ordered processor chain. **The
sanitizer's order is an environment variable.** Skipping `"none"` lets an operator disable
something without removing it from a list.

## One registry per port

```go
// internal/wire/registries.go
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

That block is a **table of contents for the entire system's variability**. Every axis along
which this project can be reconfigured is one line here.

## Blank imports: making `init()` run

Go only runs a package's `init()` if the package is imported. Nothing references
`adapters/storage/postgres` by name, so it must be imported for its side effect:

```go
// internal/bootstrap/plugins.go
import (
    _ "github.com/sanjaysharmaoff/watchtower/internal/adapters/cognition/guard"
    _ "github.com/sanjaysharmaoff/watchtower/internal/adapters/cognition/llm/openai"
    _ "github.com/sanjaysharmaoff/watchtower/internal/adapters/cognition/llm/stub"
    _ "github.com/sanjaysharmaoff/watchtower/internal/adapters/cognition/prompt"
    _ "github.com/sanjaysharmaoff/watchtower/internal/adapters/cognition/score"
    _ "github.com/sanjaysharmaoff/watchtower/internal/adapters/delivery/notify"
    _ "github.com/sanjaysharmaoff/watchtower/internal/adapters/delivery/render"
    _ "github.com/sanjaysharmaoff/watchtower/internal/adapters/delivery/scheduler"
    _ "github.com/sanjaysharmaoff/watchtower/internal/adapters/enrich/demo"
    _ "github.com/sanjaysharmaoff/watchtower/internal/adapters/enrich/k8s"
    _ "github.com/sanjaysharmaoff/watchtower/internal/adapters/enrich/loki"
    _ "github.com/sanjaysharmaoff/watchtower/internal/adapters/enrich/sanitize"
    _ "github.com/sanjaysharmaoff/watchtower/internal/adapters/ingest/correlate"
    _ "github.com/sanjaysharmaoff/watchtower/internal/adapters/ingest/decode"
    _ "github.com/sanjaysharmaoff/watchtower/internal/adapters/ingest/gate"
    _ "github.com/sanjaysharmaoff/watchtower/internal/adapters/rag/cache"
    _ "github.com/sanjaysharmaoff/watchtower/internal/adapters/rag/embed/hashing"
    _ "github.com/sanjaysharmaoff/watchtower/internal/adapters/rag/rerank"
    _ "github.com/sanjaysharmaoff/watchtower/internal/adapters/rag/vector/memory"
    _ "github.com/sanjaysharmaoff/watchtower/internal/adapters/storage/memstore"
    _ "github.com/sanjaysharmaoff/watchtower/internal/adapters/storage/postgres"
)
```

The `_` means "import for side effects only". This file **is** the plugin manifest: the
complete list of implementations compiled into the binary. Want a build without the OpenAI
adapter? Delete one line.

If you have seen `database/sql` with `_ "github.com/lib/pq"`, this is the same pattern — the
standard library's own driver registry. Saying that out loud is a good way to show the design
is idiomatic rather than invented.

Two packages are *not* blank imports because their types are used directly:
`rag/embed/cached` (the decorator is applied in code) and `ingest/worker` (the pool is a
concrete type, and `httpapi` checks for `worker.ErrQueueFull`).

## The Env: what every factory receives

```go
type Env struct {
    Cfg   config.Config
    Log   ports.Logger
    Clock ports.Clock
    IDs   ports.IDGen
    HTTP  *http.Client

    mu        sync.Mutex
    resources map[string]any

    closeMu sync.Mutex
    closers []func() error
}
```

Config, platform services, a shared HTTP client with sane connection pooling, plus the
resource pool. A factory signature is `func(e *Env) (T, error)` and never grows, no matter
what an adapter needs.

The **shared `*http.Client`** matters: `MaxIdleConns: 64`, `IdleConnTimeout: 90s`. Adapters
that use it (OpenAI, Loki) reuse TCP connections and TLS sessions instead of each keeping its
own pool. The Kubernetes adapter deliberately builds its *own* client, because it needs a
custom TLS root CA — a good example of a defensible exception.

## The resource pool

Four Postgres adapters need one connection pool. Two Redis adapters need one client. Nobody
should own it, and it must be built lazily — a config with `STORE_DRIVER=memory` should never
connect to Postgres.

```go
func Resource[T any](e *Env, key string, build func() (T, error)) (T, error) {
    if existing, ok := e.lookup(key); ok {
        typed, ok := existing.(T)
        if !ok {
            var zero T
            return zero, fmt.Errorf("resource %q already registered as %T", key, existing)
        }
        return typed, nil
    }

    value, err := build()
    if err != nil {
        var zero T
        return zero, err
    }

    e.mu.Lock()
    defer e.mu.Unlock()
    if existing, ok := e.resources[key]; ok {
        if typed, ok := existing.(T); ok { return typed, nil }
    }
    e.resources[key] = value
    return value, nil
}
```

Usage — note that *every* Postgres adapter calls this and only the first one actually
connects:

```go
func Pool(e *wire.Env) (*pgxpool.Pool, error) {
    return wire.Resource(e, "postgres.pool", func() (*pgxpool.Pool, error) {
        cfg, err := pgxpool.ParseConfig(e.Cfg.Store.DSN)
        ...
        pool, err := pgxpool.NewWithConfig(ctx, cfg)
        ...
        e.OnClose(func() error { pool.Close(); return nil })
        e.Log.Info("postgres connected", "max_conns", cfg.MaxConns)
        if e.Cfg.Store.Migrate {
            if err := migrate(ctx, pool, e.Cfg.RAG.VectorDims, e.Log); err != nil {
                pool.Close()
                return nil, err
            }
        }
        return pool, nil
    })
}
```

This is lazy singleton-per-key, with generics providing type safety and a clear error if two
callers disagree about the type behind a key.

### The deadlock this code had — remember this story

The first version held the mutex for the whole function:

```go
func Resource[T any](e *Env, key string, build func() (T, error)) (T, error) {
    e.mu.Lock()
    defer e.mu.Unlock()          // held while build() runs
    ...
    value, err := build()        // build() calls e.OnClose(...) → e.mu.Lock() → DEADLOCK
```

`OnClose` used the same mutex. `sync.Mutex` in Go is **not reentrant**, so the goroutine
deadlocked against itself. The whole service hung after printing one log line — and only when
using Postgres or Redis, because the in-memory adapters never register a closer. Every local
run passed.

The fix, two parts:

1. Separate locks, with the reason written in the struct:

```go
// Two independent locks on purpose. A resource factory is expected to register
// its own shutdown hook while it is being built, so guarding the resource map
// and the closer list with one mutex would deadlock the moment a factory calls
// OnClose. sync.Mutex is not reentrant.
```

2. Release the map lock while `build()` runs, then double-check on insert.

Use this in interviews when asked about a hard bug. It has everything: a language detail
(non-reentrant mutexes), a testing gap (the offline path did not exercise it), a symptom that
looked like a hang rather than an error, and a fix that also removed a second latent issue
(nested resource construction).

## Ordered shutdown

```go
func (e *Env) OnClose(fn func() error) {
    e.closeMu.Lock()
    defer e.closeMu.Unlock()
    e.closers = append(e.closers, fn)
}

func (e *Env) Close() error {
    ...
    for i := len(closers) - 1; i >= 0; i-- {     // reverse order: LIFO
        if err := closers[i](); err != nil && firstErr == nil { firstErr = err }
    }
    return firstErr
}
```

Reverse registration order, like a stack of `defer`s: things built last are torn down first,
so a dependency is never closed while something still needs it.

## The composition root

`internal/bootstrap/app.go` is the only file that knows the whole picture. It reads top to
bottom in phases, each clearly labelled:

```
--- storage ---------------  incident store, summary store, DLQ, partition manager
--- retrieval -------------  embedder, vector store, reranker, + cache decorator
--- cognition -------------  prompt builder, planner, critic, narrator, scorer
--- guardrails ------------  redactors, inspectors, risk classifier
--- ingestion -------------  decoder, gate, correlator, providers, log processors
--- delivery --------------  renderer, notifier
--- event bus and projectors
--- the pipeline ----------  the stage list
--- services --------------  worker pool, triage, feedback, digest, maintenance
--- inbound edge ----------  the HTTP server
--- schedules -------------  cron jobs
```

Two things it does that are worth stealing for your own projects.

**The decorator is applied here, in code, not in config:**

```go
if cfg.RAG.CacheDriver != "none" {
    embedCache, err := wire.Caches.Build(env, cfg.RAG.CacheDriver)
    if err != nil { return err }
    env.OnClose(embedCache.Close)
    // Decorator, not a config flag inside the embedder: the OpenAI adapter never
    // learns that caching exists.
    embedder = cached.Wrap(embedder, embedCache, cfg.RAG.CacheTTL, log)
}
```

**It logs what it built**, so there is never any doubt which adapters are live:

```
msg="watchtower starting" version=compose drivers="env=dev store=postgres vector=postgres
  embedder=openai rerank=llm llm=openai notify=smtp gate=redis cache=redis enrich=loki"
msg="pipeline assembled" stages="admit -> debounce -> correlate -> enrich -> sanitize ->
  input-guard -> publish:incident.sanitized -> retrieve -> input-guard:retrieved -> prompt ->
  plan -> output-guard -> score -> publish:incident.triaged"
```

Two log lines that answer "what is this instance actually doing?" — worth copying into every
service you ever build.

## Fail fast

Every `Build` error returns immediately, so a misconfiguration is a startup failure, never a
3am surprise:

```
watchtower: startup failed: invalid config: WT_HTTP_AUTH_TOKEN is required when ENV=prod:
the /alerts and /runbook endpoints would otherwise be unauthenticated; WT_APPROVAL_SECRET
must be set when ENV=prod so approval links cannot be forged
```

And if construction fails partway, the deferred `env.Close()` releases whatever was already
built:

```go
app := &App{cfg: cfg, log: log, env: env}
if err := app.build(); err != nil {
    _ = env.Close()
    return nil, err
}
```

## Interview answers to have ready

**"How does an implementation get selected?"**
Adapters self-register into a typed registry in `init()`, keyed by a driver name. The
composition root resolves `config → registry → instance`. No switch statement, no adapter
imports in the wiring logic. It is the same pattern as `database/sql` drivers.

**"How do two adapters share a database connection?"**
A memoized resource pool on the build environment, keyed by a string. Every Postgres adapter
asks for `"postgres.pool"`; the first call builds it and registers a shutdown hook, the rest
get the same handle. It is lazy, so a memory-only configuration never opens a connection.

**"What is the hardest bug you hit?"**
A self-deadlock in that resource pool. The mutex was held while the factory ran, and factories
register their own shutdown hook, which took the same non-reentrant mutex. The service hung
after one log line — but only with Postgres or Redis, because the in-memory adapters never
register a closer, so every local test passed. Two fixes: separate locks for the map and the
closer list, and release the lock while building.

---

That completes the architecture. Next: **Part 16 — the `domain` package**, where the code
walkthrough begins.
