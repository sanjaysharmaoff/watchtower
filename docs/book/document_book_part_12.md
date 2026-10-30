# Part 12 — Ports and Adapters (Hexagonal Architecture)

This is the single most important part of the book for interviews. If you can explain this
part fluently you can explain the project.

## The idea in one sentence

**Your business logic defines interfaces for everything it needs from the outside world, and
the outside world implements them.**

Other names for the same idea: Hexagonal Architecture (Alistair Cockburn), Clean
Architecture (Robert Martin), Onion Architecture. "Ports and adapters" is the clearest:

- a **port** is an interface — a hole in the side of your application
- an **adapter** is an implementation that plugs into that hole

## Why: the dependency direction

The naive layering everyone writes first:

```
HTTP handler → business logic → Postgres
```

Business logic *imports* the database package. So the business logic cannot compile, run or
be tested without Postgres, and swapping Postgres means editing business logic. The
dependency points **outward**, toward infrastructure.

Ports and adapters inverts it:

```
HTTP handler ──▶ ports.AlertSink ◀── business logic ──▶ ports.IncidentStore ◀── Postgres adapter
```

Now **all arrows point inward**, toward the domain. Business logic depends only on
interfaces it declared itself. Postgres depends on the interface. Neither knows the other
exists.

That is **Dependency Inversion**: both high-level and low-level modules depend on an
abstraction, and the abstraction is owned by the high-level module.

## How Go makes this natural

Go's implicit interface satisfaction is the key language feature:

```go
// internal/ports/storage.go — declared by the application
type IncidentStore interface {
    SaveIncident(ctx context.Context, inc *domain.Incident) error
    GetIncident(ctx context.Context, id string) (*domain.Incident, error)
}
```

```go
// internal/adapters/storage/postgres/incidents.go
type Incidents struct{ pool *pgxpool.Pool }

func (s *Incidents) SaveIncident(ctx context.Context, inc *domain.Incident) error { ... }
func (s *Incidents) GetIncident(ctx context.Context, id string) (*domain.Incident, error) { ... }
```

Notice there is **no `implements` keyword**. The Postgres type satisfies the interface simply
by having the right methods. In Java or C# the implementation must name the interface, which
creates a compile-time dependency from the implementation to the abstraction. In Go the
interface can be declared where it is *consumed*, and the implementation need not even
import it.

This is why the Go community says **"accept interfaces, return structs"**, and why the
canonical proverb is *"the bigger the interface, the weaker the abstraction."*

## The three-layer rule in this project

Learn this; it is the answer to "how is your project structured?"

```
internal/domain      imports: standard library ONLY
internal/ports       imports: domain + standard library ONLY
everything else      may import domain and ports freely
internal/bootstrap   imports everything (it is the only place allowed to)
```

You can verify the first rule mechanically:

```bash
go list -deps ./internal/domain | Select-String "watchtower"
# → only the domain package itself
```

`domain` has zero project dependencies. `ports` depends only on `domain`. That is the
architecture, enforced by imports rather than by a document.

## The ports in this project

About 30 interfaces, grouped in six files that mirror the pipeline's phases:

**`ports/ingest.go`** — getting alerts in
`AlertDecoder`, `AlertSink`, `FeedbackSink`, `Gate`, `Correlator`, `ContextProvider`,
`LogProcessor`, `Dispatcher`

**`ports/rag.go`** — retrieval
`Embedder`, `VectorStore`, `Reranker`, `Cache`

**`ports/cognition.go`** — thinking and safety
`Redactor`, `PromptBuilder`, `Planner`, `Critic`, `CommandInspector`, `RiskClassifier`,
`Scorer`, `Narrator`

**`ports/storage.go`** — persistence
`IncidentStore`, `SummaryStore`, `DeadLetterQueue`, `PartitionManager`

**`ports/delivery.go`** — output
`Renderer`, `Notifier`, `Scheduler`, `Signer`

**`ports/platform.go`** — cross-cutting
`Logger`, `Clock`, `IDGen`

## Look how small they are

```go
type Gate interface {
    Allow(ctx context.Context, key string, window time.Duration) (bool, error)
}

type Embedder interface {
    Name() string
    Dimensions() int
    Embed(ctx context.Context, text string) ([]float32, error)
}

type Redactor interface {
    Name() string
    Redact(text string) string
}

type LogProcessor interface {
    Name() string
    Process(lines []domain.LogLine) []domain.LogLine
}
```

Mostly one real method. The consequences of that smallness are the whole payoff:

- Writing a new implementation is a few minutes' work, so people actually do it.
- Faking one in a test needs no mocking framework.
- Nothing can be "half implemented" — there is nothing to half-implement.
- The interface is impossible to misunderstand.

Compare with the interface a tutorial would write: a `Storage` interface with 15 methods
covering incidents, summaries, runbooks and DLQ. Then the in-memory implementation must
implement 15 methods to be useful for one test, and every new storage need edits an
interface that four types must satisfy.

## Driving vs driven adapters

Two directions, and the folder layout now shows the difference:

**Driven (outbound)** — the application calls them. Postgres, Loki, SMTP, OpenAI. The
application holds the interface; the adapter implements it.

**Driving (inbound)** — they call the application. The HTTP server. It *depends on* an
interface the application implements:

```go
// internal/adapters/inbound/httpapi/server.go
type Deps struct {
    Config   config.HTTPConfig
    Log      ports.Logger
    Clock    ports.Clock
    Decoder  ports.AlertDecoder
    Alerts   ports.AlertSink       // ← the service, seen as an interface
    Feedback ports.FeedbackSink
    Signer   ports.Signer
    ...
}
```

The HTTP layer has never heard of `services.Triage`. It knows `ports.AlertSink`, which is:

```go
type AlertSink interface {
    Submit(ctx context.Context, alerts []domain.Alert) (string, error)
}
```

So the same triage service can be driven by HTTP today, by an SQS consumer or a gRPC
endpoint tomorrow, with no change to the service. And the HTTP layer can be tested against
a five-line fake sink.

## Every port has at least two implementations

This is the test of whether an abstraction is real. An interface with one implementation is
usually speculative. Here:

| Port | Implementations |
| --- | --- |
| `AlertDecoder` | alertmanager, generic |
| `Gate` | memory, redis |
| `ContextProvider` | loki, k8s, demo, none |
| `LogProcessor` | dedupe, signal, compress, cap |
| `Embedder` | openai(-compatible), hashing(offline), + cached decorator |
| `VectorStore` | postgres/pgvector, memory |
| `Reranker` | heuristic, llm, none(passthrough) |
| `Cache` | memory, redis, none |
| `Planner` / `Critic` / `Narrator` | openai(-compatible), stub |
| `Redactor` | secrets, pii, custom |
| `CommandInspector` | denylist, flags, allowlist |
| `IncidentStore` / `SummaryStore` / `DLQ` / `PartitionManager` | postgres, memory |
| `Notifier` | smtp, stdout, memory, none |
| `Scheduler` | cron, none |

Every abstraction is load-bearing: it was **used** to build a second thing, not invented in
case someone might.

## What this bought, concretely

Four things you can point at, all of them real events:

**1. The system runs with zero infrastructure.** Default config is memory storage, memory
vectors, offline hashing embeddings, rule-based planner, stdout email. `go run
./cmd/watchtowerd` and the full pipeline works. That is not a mock mode with different code
paths — it is the *same* code with different adapters.

**2. Real infrastructure is a config change.** Postgres, pgvector, Loki, Kubernetes, real
models, SMTP: environment variables. No code edit, no rebuild, no `if production` branch.

**3. A 20-package folder restructure touched 6 files.** Because adapters import only
`domain`, `ports` and `wire`, moving them changed almost no import lines.

**4. Bugs stayed local.** The startup deadlock was in `wire.Env`; the fix was two lines in
one file, and nothing else could have been affected, because nothing else touches resource
construction.

## The costs — say these before you are asked

Honesty here reads as seniority:

- **Indirection.** "Where does the email actually get sent?" requires knowing that
  `deliver` is a bus subscriber holding a `ports.Notifier` which was built from a registry
  by name from config. A newcomer needs the map (this book) to navigate.
- **More files.** A 40-line direct implementation becomes an interface, an adapter, a
  registration and a config key.
- **It can be over-applied.** If a port only ever has one implementation and never will,
  the abstraction is cost without benefit. `ports.QueryBuilder`, `Migrator`, `Closer` and
  `HealthChecker` were originally declared and then **deleted** when it became clear nothing
  used them. Deleting your own unused abstraction is a good story to tell.
- **It is not free at small scale.** For a 500-line service this is overkill. It earns its
  keep when there are multiple deployment targets, multiple providers per concern, or a need
  to run without infrastructure.

## Interview answers to have ready

**"What architecture did you use and why?"**
Ports and adapters. The system integrates with eight external technologies, and I wanted to
be able to run the entire pipeline with none of them present — for development, for tests
and for a demo. So the application declares about 30 small interfaces and every external
technology is an adapter behind one. The dependency arrows all point inward at `domain` and
`ports`, which import nothing but the standard library.

**"How would you add Slack notifications?"**
One file. A type with `Name()` and `Notify(ctx, domain.Message)`, an `init()` that registers
it as `wire.Notifiers.Register("slack", ...)`, one blank import in `plugins.go`, then
`WT_NOTIFY_DRIVER=slack`. Nothing else changes — not the pipeline, not the projector that
sends mail, not the services.

**"How do you test this?"**
Every dependency is a small interface, so a test constructs the real pipeline with fake
adapters — no mocking framework, no network, no database. The in-memory adapters were built
for exactly that and are the default configuration, so the offline path is exercised
constantly rather than rotting.

**"Isn't this over-engineered?"**
For a small service, yes. It paid for itself here in four measurable ways: the whole system
runs with no dependencies, real infrastructure is config-only, a 20-package restructure
touched 6 files, and I deleted the abstractions that turned out to have no second
implementation. If a port never gets a second implementation, I regard that as a mistake and
remove it.

---

Next: **Part 13 — The Pipeline Pattern**, the second big architectural idea.
