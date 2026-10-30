# Part 17 — Code Walkthrough: the `ports` Package

Thirty interfaces in six files. This part is a reference: what each one is for, why it has the
shape it has, and who implements it. Skim it now, come back when an interviewer names one.

**Rule for this package:** imports `domain`, `context`, `io`, `time` — nothing else.

## `ingest.go` — getting alerts in

```go
type AlertDecoder interface {
    Name() string
    Decode(r io.Reader) ([]domain.Alert, error)
}
```
Turns a request body into normalized alerts. Takes `io.Reader`, not `[]byte` or
`*http.Request`: streaming-friendly, and testable with `strings.NewReader`. Taking
`*http.Request` would tie the decoder to HTTP forever.
*Implementations:* `alertmanager`, `generic`.

```go
type AlertSink interface {
    Submit(ctx context.Context, alerts []domain.Alert) (string, error)
}
```
The inbound seam. Returns the new incident id so the HTTP layer can echo it back for
correlation. Implemented by `services.Triage`; consumed by the HTTP handler, which has never
heard of `services`.

```go
type FeedbackSink interface {
    Submit(ctx context.Context, fb domain.Feedback) error
}
```
The other inbound seam. Implemented by `services.Feedback`.

```go
type Gate interface {
    Allow(ctx context.Context, key string, window time.Duration) (bool, error)
}
```
Keyed admission control. Note it is **not** called `Debouncer`: the concept is "may this key
proceed", which is equally a rate limiter or a once-only guard. Naming the capability rather
than the current use case is why the same interface could serve three features.
*Implementations:* `memory`, `redis`.

```go
type Correlator interface {
    Correlate(ctx context.Context, inc *domain.Incident) error
}
```
Mutates the incident in place — merging related alerts, setting the correlation key. Returning
an error is advisory: the stage logs it as a note and continues.
*Implementations:* `window`, `none`.

```go
type ContextProvider interface {
    Name() string
    Fetch(ctx context.Context, inc *domain.Incident) (domain.ContextFragment, error)
}
```
The most extensible port in the system. Logs, cluster events, traces, deployment history —
anything that adds evidence. `Name()` exists so failures can be attributed in the trace and so
one provider's output is identifiable among many.
*Implementations:* `loki`, `k8s`, `demo`, `none`. Would also be: ELK, Datadog, Tempo, a
deployment-history API.

```go
type LogProcessor interface {
    Name() string
    Process(lines []domain.LogLine) []domain.LogLine
}
```
`[]LogLine → []LogLine`, no context, no error. That signature is a design statement: log
processing is a **pure, local transformation**. It cannot make a network call, cannot fail, and
therefore cannot become a source of latency or partial failure. Composing four of them is
trivially safe.
*Implementations:* `dedupe`, `signal`, `compress`, `cap`.

```go
type Dispatcher interface {
    Submit(job func(context.Context)) error
    Shutdown(ctx context.Context) error
}
```
Run work off the request path. `Submit` takes a closure and returns an error so the caller can
react to backpressure (the HTTP layer turns it into `429`).
*Implementation:* `worker.Pool`.

## `rag.go` — retrieval

```go
type Embedder interface {
    Name() string
    Dimensions() int
    Embed(ctx context.Context, text string) ([]float32, error)
}
```
`Dimensions()` is not decoration — it lets the system reason about vector width, and `Name()`
is part of the cache key so switching models cannot serve stale vectors. Single text rather
than a batch: simpler to decorate, and this system embeds one query at a time. Batch would be
the right change if throughput ever mattered.
*Implementations:* `openai`(-compatible), `hashing` (offline), plus the `cached` decorator.

```go
type VectorStore interface {
    Search(ctx context.Context, vec []float32, k int) ([]domain.RunbookMatch, error)
    Upsert(ctx context.Context, rb domain.Runbook) error
}
```
Two methods: read and write. `Search` takes a vector, not text — embedding is a separate
concern, which is what allows the cache decorator to sit between them.
*Implementations:* `postgres` (pgvector), `memory` (brute-force cosine).

```go
type Reranker interface {
    Name() string
    Rerank(ctx context.Context, query string, matches []domain.RunbookMatch, top int) ([]domain.RunbookMatch, error)
}
```
Takes candidates, returns fewer and better ones. Same interface for a free heuristic and for a
model call, which is exactly the point: cost/quality is a config decision.
*Implementations:* `heuristic`, `llm`, `none`.

```go
type Cache interface {
    Get(ctx context.Context, key string) ([]byte, bool, error)
    Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
    Close() error
}
```
Opaque bytes, not `any` and not a generic. Deliberately trivial to implement; typed access is
layered on by whoever uses it. The `(value, found, err)` triple distinguishes "missing" from
"broken", which the caller genuinely needs to treat differently.
*Implementations:* `memory`, `redis`, `none`.

## `cognition.go` — thinking and safety

```go
type Redactor interface {
    Name() string
    Redact(text string) string
}
```
`string → string`. No error, no context. A redactor that could fail would be a security
liability, because the caller would then have to decide what to do with un-redacted text.
Composing them is `for _, r := range redactors { s = r.Redact(s) }`.
*Implementations:* `secrets`, `pii`, `custom`.

```go
type PromptBuilder interface {
    Build(inc *domain.Incident) (domain.Prompt, error)
}
```
All model-facing wording behind one seam, so prompt iteration never touches pipeline code.
*Implementation:* `sre`.

```go
type Planner interface {
    Name() string
    Plan(ctx context.Context, p domain.Prompt) (domain.Plan, error)
}

type Critic interface {
    Name() string
    Review(ctx context.Context, p domain.Prompt, plan domain.Plan) (domain.Verdict, error)
}
```
Two roles, two interfaces, so they can be different providers or different models. Both return
*structured* domain types, never raw text — parsing is the adapter's job, so the stage never
sees a string that needs interpreting. `Name()` is recorded on the plan and verdict for
auditability ("which model said this?").
*Implementations:* `openai`, `stub`.

```go
type CommandInspector interface {
    Name() string
    Inspect(cmd domain.Command) error
}
```
One policy rule. Returning a `domain.Block(...)` error refuses the command; `nil` allows it.
`Name()` goes into the reason shown to the reviewer, so they know *which* rule fired.
*Implementations:* `denylist`, `flags`, `allowlist`.

```go
type RiskClassifier interface {
    Name() string
    Classify(cmd domain.Command) domain.RiskLevel
}
```
Added after the real run showed the model labelling `kubectl delete pod` as `read`. Its doc
comment states the reason:

> This exists because a model's self-declared risk level cannot be trusted: small models
> routinely label a pod deletion as "read". The reviewer sees this verdict, not the model's
> claim.

*Implementation:* `heuristic`.

```go
type Scorer interface {
    Score(inc *domain.Incident) float64
}
```
Takes the whole incident, because confidence depends on evidence depth, retrieval support,
critic outcome and plan shape together. Returns a plain float — no error, because a scoring
function that fails is a scoring function that should have a default.
*Implementation:* `weighted`.

```go
type Narrator interface {
    Name() string
    Narrate(ctx context.Context, p domain.Prompt) (string, error)
}
```
Free-form prose, for the daily digest. Separate from `Planner` because the output contract is
completely different: prose versus strict JSON. Merging them would mean one interface with two
incompatible contracts.
*Implementations:* `openai`, `stub`.

## `storage.go` — persistence

```go
type IncidentStore interface {
    SaveIncident(ctx context.Context, inc *domain.Incident) error
    GetIncident(ctx context.Context, id string) (*domain.Incident, error)
}

type SummaryStore interface {
    SaveSummary(ctx context.Context, s domain.ApprovedSummary) error
    ListSummaries(ctx context.Context, from, to time.Time) ([]domain.ApprovedSummary, error)
}

type DeadLetterQueue interface {
    Park(ctx context.Context, dl domain.DeadLetter) error
}

type PartitionManager interface {
    EnsurePartitions(ctx context.Context, daysAhead int) error
    DropPartitionsOlderThan(ctx context.Context, days int) error
}
```

**Four interfaces, not one `Storage` interface.** This is the most instructive design decision
in the file, and a great answer to "how do you decide interface boundaries?":

- The persist projector needs `IncidentStore` and `DeadLetterQueue`. It cannot touch summaries.
- The digest service needs only `SummaryStore.ListSummaries`.
- The maintenance service needs only `PartitionManager`.
- A single 8-method `Storage` interface would hand every consumer capabilities it has no
  business having, and would force the in-memory implementation to implement everything to be
  useful for one test.

Interface Segregation Principle, applied for real reasons rather than as a slogan.

`PartitionManager` is interesting because it looks Postgres-specific but is not: the in-memory
store implements `EnsurePartitions` as a no-op and `DropPartitionsOlderThan` as a ring trim,
with a comment explaining that the maintenance service should not have to ask which storage it
is talking to. The abstraction is "manage retention", not "manage Postgres partitions".

## `delivery.go` — output

```go
type Renderer interface {
    Name() string
    RenderIncident(inc *domain.Incident) (domain.Message, error)
    RenderDigest(d domain.Digest) (domain.Message, error)
}

type Notifier interface {
    Name() string
    Notify(ctx context.Context, msg domain.Message) error
}
```
Render and send are **separate**: one renderer can feed several notifiers, and stdout in
development uses the same rendered message as SMTP in production, which means the plain-text
part is exercised constantly instead of rotting.

```go
type Scheduler interface {
    Schedule(name, spec string, job func(context.Context)) error
    Start()
    Stop(ctx context.Context) error
}
```
`Stop(ctx)` rather than `Stop()`, so shutdown is bounded and in-flight jobs can be cancelled.

```go
type Signer interface {
    Sign(payload string) string
    Verify(payload, signature string) bool
}
```
Small enough to be obviously correct. `Verify` rather than exposing a comparison to the caller,
so constant-time comparison cannot be forgotten at a call site.

## `platform.go` — cross-cutting

```go
type Logger interface {
    Debug(msg string, kv ...any)
    Info(msg string, kv ...any)
    Warn(msg string, kv ...any)
    Error(msg string, kv ...any)
    With(kv ...any) Logger
}
```
Structured logging: a message plus key/value pairs, never `fmt.Sprintf` into the message.
`With` returns a `Logger` so context accumulates (`log.With("component", "smtp")`), and every
line from that adapter carries the field. **Nothing in the codebase imports `log/slog` except
the one adapter that implements this** — so switching to zap or zerolog is one file.

```go
type Clock interface { Now() time.Time }
type IDGen interface { New() string }

type ClockFunc func() time.Time
func (f ClockFunc) Now() time.Time { return f() }

type IDFunc func() string
func (f IDFunc) New() string { return f() }
```

Two one-method interfaces that make time and identity injectable. Why it matters: debouncing,
retention and expiring links are all time-dependent, and testing them with `time.Now()` means
`time.Sleep` in tests. With a `Clock`, a test jumps forward instantly.

The `ClockFunc`/`IDFunc` adapters are the same trick as `http.HandlerFunc`: a function type
with a method, so you can satisfy the interface with a closure:

```go
func System() ports.Clock { return ports.ClockFunc(func() time.Time { return time.Now().UTC() }) }
func Fixed(at time.Time) ports.Clock { return ports.ClockFunc(func() time.Time { return at }) }
```

Note `System()` returns **UTC**. Every stored timestamp is comparable, and nobody has to
remember which zone a row is in.

## Four interfaces that were deleted

`QueryBuilder`, `Migrator`, `Closer` and `HealthChecker` were declared early and removed once
it was clear nothing used them. That is worth mentioning unprompted:

> I deleted four interfaces I had speculatively defined. An interface with no second
> implementation and no consumer is cost without benefit — it is a promise the code does not
> keep. Abstractions should be extracted when the second case appears, not predicted.

---

Next: **Part 18 — the pipeline stages, first half.**
