# Part 2 — Errors, Platform Ports, and All the Seams (Commits 6–10)

**Goal:** finish `domain`, then declare **every interface in the system** before writing a single
implementation. Declaring the seams first is what forces you to think about boundaries instead of
discovering them halfway through.

**The rule for `internal/ports`:** it imports `domain`, `context`, `io`, `time`. Nothing else,
ever.

---

## Commit 6 — `feat(domain): feedback records and sentinel errors`

**Files:** `internal/domain/feedback.go`, `internal/domain/errors.go`

**Build:**

`feedback.go`:
- `type Feedback struct` — `IncidentID`, `Signature string`, `Decision Decision`,
  `Reviewer`, `Title`, `RootCause string`, `Commands []Command`, `Notes string`,
  `ReceivedAt time.Time`. This is what a reviewer sends in.
- `type ApprovedSummary struct` — `ID`, `IncidentID`, `Signature`, `Title`, `RootCause`,
  `Commands`, `Decision`, `Reviewer`, `Notes`, `Services []string`, `Severity`,
  `Labels map[string]string`, `CreatedAt`. This is the durable record.
- `func (s ApprovedSummary) AsRunbook() Runbook` — input: a reviewed summary; output: retrieval
  material. The entire learning loop in one conversion.
- `type Digest struct` — `From`, `To time.Time`, `Total`, `Approved`, `Edited`, `Rejected int`,
  `Services []string`, `Entries []ApprovedSummary`, `Narrative string`.
- `type Message struct { To []string; Subject, Text, HTML string; Meta map[string]string }` —
  a rendered notification, transport-agnostic.
- `type DeadLetter struct { ID, Stage, Reason, RefID string; Payload []byte; At time.Time }`.

`errors.go`:
- Sentinels: `ErrDrop`, `ErrBlocked`, `ErrNotFound`, `ErrUnsupported`, `ErrUnavailable`.
- `func Drop(reason string, args ...any) error` — output: `fmt.Errorf("%w: %s", ErrDrop, ...)`.
- `func Block(reason string, args ...any) error` — same shape with `ErrBlocked`.
- `func IsDrop(err error) bool` and `func IsBlocked(err error) bool` using `errors.Is`.

**Verify:** `go build ./...` — and confirm the wrapping works:

```go
err := domain.Drop("duplicate %s", "abc")
// err.Error() == "dropped: duplicate abc"   &&   domain.IsDrop(err) == true
```

**Why:** `%w` **wraps** rather than formats, which is what lets `errors.Is` see through the added
context. That is the whole mechanism behind "a debounced duplicate is not an error": the pipeline
returns a rich message and the caller still classifies it correctly, with no string matching.

`DeadLetter.Payload []byte` holds the full JSON of whatever failed. That is what makes replay
possible rather than aspirational.

---

## Commit 7 — `feat(ports,platform): logger, clock and IDs`

**Files:** `internal/ports/platform.go`, `internal/platform/logging/slog.go`,
`internal/platform/clock/clock.go`

**Build:**

`ports/platform.go`:
- `type Logger interface { Debug(msg string, kv ...any); Info(...); Warn(...); Error(...); With(kv ...any) Logger }`
- `type Clock interface { Now() time.Time }`
- `type IDGen interface { New() string }`
- `type ClockFunc func() time.Time` with `func (f ClockFunc) Now() time.Time { return f() }`
- `type IDFunc func() string` with `func (f IDFunc) New() string { return f() }`

`platform/logging/slog.go`:
- `func New(level, format string) ports.Logger` — input: `debug|info|warn|error` and
  `text|json`; output: a `Logger` backed by `log/slog`. Implement all five methods on an
  unexported struct wrapping `*slog.Logger`; `With` returns a new wrapper.
- `func Discard() ports.Logger` for tests.

`platform/clock/clock.go`:
- `func System() ports.Clock` — returns `ports.ClockFunc(func() time.Time { return time.Now().UTC() })`.
  **UTC**, deliberately.
- `func Fixed(at time.Time) ports.Clock` for tests.
- `func UUID() ports.IDGen` — `github.com/google/uuid`. Run `go get github.com/google/uuid`.
- `func Sequence(prefix string) ports.IDGen` — predictable ids for tests.

**Verify:** `go build ./...`, and a throwaway `main` that logs one line at info.

**Why:** these three tiny interfaces buy a lot. `Logger` means **nothing else in the codebase ever
imports `log/slog`** — swapping to zap later is one file. `With` returning a `Logger` lets context
accumulate (`log.With("component","smtp")`) so every line from an adapter carries the field.

`Clock` matters more than it looks: debouncing, retention and expiring links are all
time-dependent, and testing them with `time.Now()` means `time.Sleep` in tests. With an injected
clock a test jumps an hour instantly.

`ClockFunc`/`IDFunc` are the `http.HandlerFunc` trick — a function type with a method — so a
closure can satisfy the interface.

---

## Commit 8 — `feat(ports): ingestion seams`

**Files:** `internal/ports/ingest.go`

**Build:**

```go
type AlertDecoder interface {
    Name() string
    Decode(r io.Reader) ([]domain.Alert, error)
}

type AlertSink interface {
    Submit(ctx context.Context, alerts []domain.Alert) (string, error)   // returns the incident id
}

type FeedbackSink interface {
    Submit(ctx context.Context, fb domain.Feedback) error
}

type Gate interface {
    Allow(ctx context.Context, key string, window time.Duration) (bool, error)
}

type Correlator interface {
    Correlate(ctx context.Context, inc *domain.Incident) error
}

type ContextProvider interface {
    Name() string
    Fetch(ctx context.Context, inc *domain.Incident) (domain.ContextFragment, error)
}

type LogProcessor interface {
    Name() string
    Process(lines []domain.LogLine) []domain.LogLine
}

type Dispatcher interface {
    Submit(job func(context.Context)) error
    Shutdown(ctx context.Context) error
}
```

**Verify:** `go build ./...`

**Why:** four deliberate signature choices worth understanding now, because they constrain
everything later.

`AlertDecoder.Decode` takes `io.Reader`, **not** `*http.Request` or `[]byte`. Taking a request
would tie the decoder to HTTP forever; taking a reader makes it streamable and testable with
`strings.NewReader`.

`Gate` is not called `Debouncer`. The capability is "may this key proceed", which is equally a
rate limiter or a once-only guard. Naming the capability rather than the current use case is why
one interface can serve three features.

`LogProcessor` has **no context and no error**. That signature is a statement: log processing is a
pure, local transformation. It cannot make a network call and it cannot fail, so composing four of
them is trivially safe.

`Dispatcher.Submit` returns an error so the caller can react to backpressure — that becomes an
HTTP `429` in commit 28.

---

## Commit 9 — `feat(ports): retrieval and storage seams`

**Files:** `internal/ports/rag.go`, `internal/ports/storage.go`

**Build:**

`rag.go`:
```go
type Embedder interface {
    Name() string
    Dimensions() int
    Embed(ctx context.Context, text string) ([]float32, error)
}

type VectorStore interface {
    Search(ctx context.Context, vec []float32, k int) ([]domain.RunbookMatch, error)
    Upsert(ctx context.Context, rb domain.Runbook) error
}

type Reranker interface {
    Name() string
    Rerank(ctx context.Context, query string, matches []domain.RunbookMatch, top int) ([]domain.RunbookMatch, error)
}

type Cache interface {
    Get(ctx context.Context, key string) ([]byte, bool, error)
    Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
    Close() error
}
```

`storage.go` — **four separate interfaces, not one `Storage`**:
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

**Verify:** `go build ./...`

**Why:** the storage split is the most instructive decision in the file, and a likely interview
question. The persist projector needs incidents and the DLQ. The digest needs only
`ListSummaries`. Maintenance needs only partitions. One 8-method `Storage` interface would hand
every consumer capabilities it has no business having, and would force the in-memory
implementation to implement all eight to be useful for one test.

`Cache` returns `(value, found, err)` because "missing" and "broken" need different handling.
`Embedder.Dimensions()` and `Name()` are not decoration — `Name()` becomes part of the cache key
in commit 67 so switching models cannot serve stale vectors.

`PartitionManager` looks Postgres-specific but is not: the memory store will implement it as a
ring trim. The abstraction is "manage retention", not "manage Postgres".

---

## Commit 10 — `feat(ports): cognition and delivery seams`

**Files:** `internal/ports/cognition.go`, `internal/ports/delivery.go`

**Build:**

`cognition.go`:
```go
type Redactor interface {
    Name() string
    Redact(text string) string                              // no error, no context
}

type PromptBuilder interface {
    Build(inc *domain.Incident) (domain.Prompt, error)
}

type Planner interface {
    Name() string
    Plan(ctx context.Context, p domain.Prompt) (domain.Plan, error)
}

type Critic interface {
    Name() string
    Review(ctx context.Context, p domain.Prompt, plan domain.Plan) (domain.Verdict, error)
}

type CommandInspector interface {
    Name() string
    Inspect(cmd domain.Command) error                       // domain.Block(...) refuses it
}

type RiskClassifier interface {
    Name() string
    Classify(cmd domain.Command) domain.RiskLevel
}

type Scorer interface {
    Score(inc *domain.Incident) float64
}

type Narrator interface {
    Name() string
    Narrate(ctx context.Context, p domain.Prompt) (string, error)
}
```

`delivery.go`:
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

type Scheduler interface {
    Schedule(name, spec string, job func(context.Context)) error
    Start()
    Stop(ctx context.Context) error
}

type Signer interface {
    Sign(payload string) string
    Verify(payload, signature string) bool
}
```

**Verify:** `go build ./... && go vet ./...`

**Why:** `Redactor.Redact` returns no error on purpose. A redactor that could fail would be a
security liability, because the caller would then have to decide what to do with un-redacted text.
There is no good answer to that question, so remove it.

`Planner` and `Critic` are **separate** interfaces so they can be different models or different
providers, and both return structured domain types — never raw text. Parsing is the adapter's job,
so the stage never sees a string that needs interpreting.

`RiskClassifier` exists because a model's self-declared risk cannot be trusted. You will see this
first-hand around commit 74.

`Renderer` and `Notifier` are split so one rendered message can feed several transports, and so
the plain-text path is exercised by the stdout notifier constantly instead of rotting.

`Signer.Verify` rather than exposing a comparison, so constant-time comparison cannot be forgotten
at a call site.

---

## End of Part 2

You now have every seam in the system declared, and not one implementation. Count them:

```bash
grep -c "^type .* interface" internal/ports/*.go
```

Roughly 30 interfaces, most with a single real method.

Next: **Part 3** — the generic pipeline engine and configuration.
