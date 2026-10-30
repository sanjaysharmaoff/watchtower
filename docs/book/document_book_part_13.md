# Part 13 — The Pipeline Pattern, Generics and Middleware

Ports and adapters answers "how do we talk to the outside world". The pipeline answers "how
is our own logic organised". It is the second thing to explain on a whiteboard.

## The problem: one giant function

The naive implementation of triage is one function:

```go
func Triage(alerts []Alert) error {
    // 1. drop resolved                    // 2. check duplicates
    // 3. group related                    // 4. fetch logs
    // 5. fetch k8s events                 // 6. deduplicate logs
    // 7. filter                           // 8. compress
    // 9. redact secrets                   // 10. save to db
    // 11. embed                           // 12. search vectors
    // 13. rerank                          // 14. build prompt
    // 15. call planner                    // 16. call critic
    // 17. maybe retry                     // 18. lint commands
    // 19. score                           // 20. render + send email
}
```

400 lines. Untestable in pieces. Reordering means moving code and hoping. Adding a step
means reading all 400 lines first. Every step can see every local variable, so accidental
coupling is inevitable — step 17 starts depending on a variable step 4 happened to leave
around.

## The solution: a chain of independent steps

```go
// internal/pipeline/pipeline.go
type Stage[T any] interface {
    Name() string
    Process(ctx context.Context, in T) (T, error)
}
```

A stage takes a payload, returns a payload, and **knows nothing about its neighbours**.
Order is data, not code:

```go
pipe := pipeline.New[*domain.Incident]("triage",
    stages.Admit(...),
    stages.Debounce(...),
    stages.Correlate(...),
    stages.Enrich(...),
    stages.Sanitize(...),
    stages.InputGuard(...),
    stages.Publish(broker, bus.TopicIncidentSanitized),
    stages.Retrieve(...),
    stages.InputGuardNamed("input-guard:retrieved", ...),
    stages.BuildPrompt(...),
    stages.Plan(...),
    stages.OutputGuard(...),
    stages.Score(...),
    stages.Publish(broker, bus.TopicIncidentTriaged),
)
```

**Reordering the system is reordering a slice literal.** That sentence is worth saying out
loud in an interview — and it was proven when a real security defect required moving
redaction earlier (Part 23): a two-line edit to this list, no stage modified.

## Why generics

`Stage[T any]` is generic over the payload type. Two reasons.

**Type safety without `interface{}`.** A pre-generics Go pipeline used `interface{}` and
type-asserted in every stage:

```go
func (s myStage) Process(ctx context.Context, in interface{}) (interface{}, error) {
    inc, ok := in.(*Incident)     // runtime check, on every stage, forever
    if !ok { return nil, errors.New("wrong type") }
```

With generics, `Pipeline[*domain.Incident]` will not compile if you insert a stage of the
wrong payload type. The mistake becomes impossible instead of caught at runtime.

**Reusability.** The package has no idea what an incident is — it imports only `context`,
`strings`, and (for the middleware file) `domain` and `ports`. A future feature with a
different payload gets the same runner for free.

## The full runner

```go
type Pipeline[T any] struct {
    name   string
    stages []Stage[T]
    mws    []Middleware[T]
}

func (p *Pipeline[T]) Run(ctx context.Context, in T) (T, error) {
    cur := in
    for _, stage := range p.stages {
        if err := ctx.Err(); err != nil {
            return cur, err
        }
        wrapped := stage
        for i := len(p.mws) - 1; i >= 0; i-- {
            wrapped = p.mws[i](wrapped)
        }
        out, err := wrapped.Process(ctx, cur)
        if err != nil {
            return cur, err            // note: the partially-processed payload comes back
        }
        cur = out
    }
    return cur, nil
}
```

Three details worth explaining:

1. **`ctx.Err()` before each stage.** On cancellation or timeout the pipeline stops at the
   next boundary rather than starting a fresh model call that will be thrown away.
2. **`return cur, err`, not `return zero, err`.** The caller gets the payload *as mutated so
   far*, so a failed run still carries its trace, its logs and its partial work — which is
   what makes the dead-letter payload useful for debugging.
3. **Middleware applied in reverse.** So the first registered middleware ends up outermost.

## Function stages

Not every stage needs a type:

```go
func Of[T any](name string, fn Func[T]) Stage[T] { return funcStage[T]{name: name, fn: fn} }
```

Which is how `Publish` is a one-liner:

```go
func Publish(b bus.Bus, topic string) Stage {
    return pipeline.Of("publish:"+topic, func(ctx context.Context, inc *domain.Incident) (*domain.Incident, error) {
        b.Publish(ctx, topic, inc)
        return inc, nil
    })
}
```

And the `stages` package adds one more convenience for the common "read the envelope, enrich
it in place" shape:

```go
func mutator(name string, fn func(ctx context.Context, inc *domain.Incident) error) Stage {
    return pipeline.Of(name, func(ctx context.Context, inc *domain.Incident) (*domain.Incident, error) {
        if inc == nil {
            return nil, domain.Drop("nil incident")
        }
        if err := fn(ctx, inc); err != nil {
            return inc, err
        }
        return inc, nil
    })
}
```

That is why every stage constructor in the project is a handful of lines: the boilerplate
lives in one place.

## `compact`: nil stages are allowed

```go
func compact[T any](in []Stage[T]) []Stage[T] {
    out := make([]Stage[T], 0, len(in))
    for _, s := range in {
        if s != nil { out = append(out, s) }
    }
    return out
}
```

So the composition root can write a conditional stage inline without an `if` block breaking
up the readable list of the pipeline.

## Middleware: cross-cutting concerns, once

Logging, timeouts and panic recovery are needed by every stage and belong to none of them.

```go
type Middleware[T any] func(Stage[T]) Stage[T]
```

A middleware takes a stage and returns a wrapped stage. This is the decorator pattern, and
it is exactly how `net/http` middleware works.

### Logging, with the drop/error distinction

```go
func WithLogging[T any](log ports.Logger) Middleware[T] {
    return func(next Stage[T]) Stage[T] {
        return Of(next.Name(), func(ctx context.Context, in T) (T, error) {
            start := time.Now()
            out, err := next.Process(ctx, in)
            took := time.Since(start).Round(time.Millisecond)
            switch {
            case err == nil:
                log.Debug("stage ok", "stage", next.Name(), "took", took.String())
            case domain.IsDrop(err):
                log.Info("stage dropped payload", "stage", next.Name(), "reason", err.Error())
            default:
                log.Error("stage failed", "stage", next.Name(), "took", took.String(), "err", err.Error())
            }
            return out, err
        })
    }
}
```

The important line is `case domain.IsDrop(err)`. A debounced duplicate is **not an error** —
it is the system working correctly. Logging it at ERROR would train everyone to ignore
errors, and would page someone at 3am for a success. Getting log levels right is an
underrated signal of operational maturity.

This gave the exact per-stage timing table used to characterise the system:

```
stage=admit took=0s          stage=debounce took=0s      stage=correlate took=0s
stage=enrich took=22ms       stage=sanitize took=3ms     stage=retrieve took=703ms
stage=prompt took=0s         stage=plan took=1m1.387s    stage=output-guard took=1ms
```

You can read the cost model straight off that: everything is free except retrieval (one
embedding call) and planning (four model calls).

### Panic recovery

```go
func WithRecover[T any](log ports.Logger) Middleware[T] {
    return func(next Stage[T]) Stage[T] {
        return Of(next.Name(), func(ctx context.Context, in T) (out T, err error) {
            defer func() {
                if r := recover(); r != nil {
                    out = in
                    err = fmt.Errorf("stage %s panicked: %v", next.Name(), r)
                    log.Error("stage panic", "stage", next.Name(), "panic", fmt.Sprint(r))
                }
            }()
            return next.Process(ctx, in)
        })
    }
}
```

A nil map write in a third-party JSON path would otherwise take the whole process down and
lose every in-flight incident. Note the **named return values** — that is what lets the
deferred function set `out` and `err`, a Go idiom worth being able to explain.

### Timeout

```go
func WithTimeout[T any](d time.Duration) Middleware[T] {
    return func(next Stage[T]) Stage[T] {
        if d <= 0 { return next }
        return Of(next.Name(), func(ctx context.Context, in T) (T, error) {
            ctx, cancel := context.WithTimeout(ctx, d)
            defer cancel()
            return next.Process(ctx, in)
        })
    }
}
```

Per-stage, from `WT_STAGE_TIMEOUT`. Note `if d <= 0 { return next }` — zero disables the
middleware rather than creating a context that expires immediately.

### The observer hook

```go
type Observer func(stage string, took time.Duration, err error)
func WithObserver[T any](obs Observer) Middleware[T] { ... }
```

Not currently wired, and it is the extension point for metrics: a Prometheus histogram of
stage duration would be five lines here and **the pipeline package would still not import
Prometheus**. Nice answer to "how would you add observability?"

### Registration order

```go
.Use(
    pipeline.WithRecover[*domain.Incident](log),
    pipeline.WithLogging[*domain.Incident](log),
    pipeline.WithTimeout[*domain.Incident](cfg.Runtime.StageTimeout),
)
```

Applied in reverse, so the effective nesting is `Recover(Logging(Timeout(stage)))`:

- Recover is outermost, so it catches panics from anything inside, including the middleware.
- Logging is next, so it observes and reports the timeout as a normal stage error.
- Timeout is innermost, closest to the real work.

Being able to explain *why that order* is the difference between having used middleware and
having understood it.

## Control flow: drop vs error

Two sentinel errors in `domain`:

```go
var (
    ErrDrop    = errors.New("dropped")            // deliberate, non-exceptional stop
    ErrBlocked = errors.New("blocked by policy")  // policy refusal
)

func Drop(reason string, args ...any) error {
    return fmt.Errorf("%w: %s", ErrDrop, fmt.Sprintf(reason, args...))
}
func IsDrop(err error) bool { return errors.Is(err, ErrDrop) }
```

`%w` **wraps**, which is what makes `errors.Is` work through layers of context. So a stage
returns a rich message and callers still classify it correctly:

```go
return domain.Drop("duplicate signature %s within %s", key, window)
// → "dropped: duplicate signature 65a937f408edc78e within 1h0m0s", and IsDrop() == true
```

The service then branches on the *kind* of outcome:

```go
switch {
case err == nil:
    t.log.Info("incident triaged", ...)
case domain.IsDrop(err):
    t.bus.Publish(ctx, bus.TopicIncidentDropped, out)      // expected, informational
case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
    t.park(ctx, "pipeline-timeout", err.Error(), out)      // retryable, park it
default:
    t.park(ctx, "pipeline", err.Error(), out)              // unexpected, park it
}
```

Three outcome classes, three different behaviours, one `error` return type. This is idiomatic
Go error handling and a very good thing to be able to walk through.

## The envelope

One payload type flows through everything: `*domain.Incident`. Each stage reads what it
needs and writes what it produced.

Why a single mutable envelope rather than a different type per stage:

- Adding a field is one struct change, not a new type and conversions at every boundary.
- The envelope carries its own audit trail (`Trace`, `Meta`), so debugging does not require
  correlating logs.
- Persisting it is one `json.Marshal`.

The cost, which you should volunteer: it is mutable shared state, and a stage *could*
misbehave by clobbering another's field. It is safe here because **exactly one goroutine owns
an incident at a time** — the worker pool hands it to one worker, and the pipeline is
sequential. The only concurrency inside a stage is the enrich fan-out, which collects into
local slices under a mutex and merges once at the end.

## Interview answers to have ready

**"Why a pipeline instead of just calling functions in order?"**
Because order becomes data. Stages are independently testable, cross-cutting concerns live in
middleware instead of being repeated, and reordering is a slice edit. That mattered
concretely: a security fix required moving redaction ahead of the first database write and the
first model call, and it was two lines in the composition root with zero stage changes.

**"Why generics?"**
Compile-time payload safety without `interface{}` and type assertions in every stage, and the
runner stays reusable for a different payload type. Before generics I would have written the
type assertion boilerplate 14 times.

**"How do you handle a stage that fails?"**
The pipeline stops and returns the payload as mutated so far, plus the error. The caller
classifies it: a deliberate drop is logged at info and published as a dropped event; a
timeout or unexpected failure is published as failed and parked in the dead letter queue with
the full payload. Panics are converted to errors by middleware so one bad adapter cannot take
the process down.

**"What happens to the incident if the model call fails?"**
It was already persisted at `incident.sanitized`, before retrieval and before the model, so
the evidence survives. The failure lands in the DLQ with the payload and can be replayed
through `Triage.Process` without re-fetching anything.

---

Next: **Part 14 — The Event Bus and Projectors**, the third architectural idea.
