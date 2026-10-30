# Part 23 — Services, Bootstrap, and a Full Trace of One Real Incident

The last code part. Services first, then `main`, then a second-by-second trace of a real incident
from the running system.

## `services.Triage`

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
```

Seven dependencies, every one an interface or the generic pipeline. No adapter types.

```go
func (t *Triage) Submit(ctx context.Context, alerts []domain.Alert) (string, error) {
    if len(alerts) == 0 {
        return "", fmt.Errorf("no alerts to triage")
    }
    inc := domain.NewIncident(t.ids.New(), t.clock.Now(), alerts)
    t.bus.Publish(ctx, bus.TopicIncidentReceived, inc)

    err := t.disp.Submit(func(jobCtx context.Context) {
        t.Process(jobCtx, inc)
    })
    if err != nil {
        // Backpressure: park the payload so a saturated pipeline loses nothing.
        t.park(ctx, "dispatch", err.Error(), inc)
        return inc.ID, err
    }
    t.log.Info("incident queued", "incident", inc.ID, "alerts", len(inc.Alerts), "severity", inc.Severity)
    return inc.ID, nil
}
```

Three things:

- **It returns as soon as the work is queued.** Alertmanager gets its `202` in about a
  millisecond, so it never times out and retries.
- **It returns `inc.ID` even on failure**, so the caller can reference the incident in a log or a
  support conversation.
- **A rejected submission is parked in the DLQ.** Even a saturated pipeline loses nothing.

```go
func (t *Triage) Process(ctx context.Context, inc *domain.Incident) {
    out, err := t.pipe.Run(ctx, inc)
    switch {
    case err == nil:
        t.log.Info("incident triaged", "incident", out.ID, "confidence", out.Confidence, "attempts", out.Attempts)
    case domain.IsDrop(err):
        out.Note("dropped: %v", err)
        t.bus.Publish(ctx, bus.TopicIncidentDropped, out)
        t.log.Info("incident dropped", "incident", out.ID, "reason", err.Error())
    case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
        out.Note("timed out: %v", err)
        t.bus.Publish(ctx, bus.TopicIncidentFailed, out)
        t.park(ctx, "pipeline-timeout", err.Error(), out)
        t.log.Error("incident timed out", "incident", out.ID, "err", err.Error())
    default:
        out.Note("failed: %v", err)
        t.bus.Publish(ctx, bus.TopicIncidentFailed, out)
        t.park(ctx, "pipeline", err.Error(), out)
        t.log.Error("incident failed", "incident", out.ID, "err", err.Error())
    }
}
```

Four outcomes, four behaviours, one `error` type. Note `Process` is **exported** on purpose:

> Exported so a replay tool or a test can drive it directly without a queue.

That is what makes a DLQ replay tool a small addition rather than a refactor.

## `services.Feedback` — the learning loop

```go
func (f *Feedback) Submit(ctx context.Context, fb domain.Feedback) error {
    if fb.IncidentID == "" { return fmt.Errorf("feedback requires an incident id") }
    if fb.Decision == ""   { fb.Decision = domain.DecisionReject }        // fail closed
    if fb.ReceivedAt.IsZero() { fb.ReceivedAt = f.clock.Now() }

    summary := f.buildSummary(ctx, fb)

    if err := f.summaries.SaveSummary(ctx, summary); err != nil {
        // The decision is too valuable to drop: park it and keep going, because the
        // runbook can still be learned and the DLQ row can be replayed later.
        f.park(ctx, "save-summary", err.Error(), summary)
        f.log.Error("summary write failed, parked in DLQ", "incident", fb.IncidentID, "err", err.Error())
    }
    f.bus.Publish(ctx, bus.TopicFeedbackAccepted, summary)

    if !fb.Decision.Learnable() {
        f.log.Info("feedback recorded", "incident", fb.IncidentID, "decision", fb.Decision, "learned", false)
        return nil
    }
    if err := f.learn(ctx, summary); err != nil {
        f.log.Warn("runbook not learned", "incident", fb.IncidentID, "err", err.Error())
        return nil     // recording succeeded; learning is best effort
    }
    f.log.Info("feedback recorded", "incident", fb.IncidentID, "decision", fb.Decision, "learned", true)
    return nil
}
```

Three deliberate decisions to be able to defend:

**Missing decision defaults to reject.** Fail closed on the path that writes to the corpus.

**A failed write does not abort.** The decision is parked and processing continues, because the
runbook can still be learned and the DLQ row replayed. It also means the API returns success
after a database failure — defensible because the data is durable in the DLQ, but you should
volunteer it as a tradeoff rather than let an interviewer find it.

**Learning is best effort.** If embedding fails, the decision is still recorded. Recording is the
contract; learning is the enhancement.

### Filling in the gaps

```go
func (f *Feedback) buildSummary(ctx context.Context, fb domain.Feedback) domain.ApprovedSummary {
    summary := domain.ApprovedSummary{ ID: f.ids.New(), ... }

    inc, err := f.incidents.GetIncident(ctx, fb.IncidentID)
    if err != nil {
        if !errors.Is(err, domain.ErrNotFound) {
            f.log.Warn("incident lookup failed", "incident", fb.IncidentID, "err", err.Error())
        }
        if summary.Title == "" { summary.Title = "Incident " + fb.IncidentID }
        return summary
    }

    if summary.Title == "" { summary.Title = inc.Title() }
    if summary.Signature == "" { summary.Signature = inc.Signature }
    summary.Services = inc.Services
    summary.Severity = inc.Severity
    summary.Labels   = inc.PrimaryAlert().Labels

    if inc.Plan != nil {
        if summary.RootCause == "" { summary.RootCause = inc.Plan.RootCause }
        if len(summary.Commands) == 0 {
            // Only the commands policy allowed through are eligible to be learned.
            summary.Commands = inc.Plan.Executable()
        }
    }
    return summary
}
```

This is what makes a bare one-click "approve" produce a *complete* record: whatever the reviewer
did not send is filled from the stored incident. `ErrNotFound` is handled quietly while other
errors get a warning — a missing incident is a normal case, a broken database is not.

And the safety property from Part 16 shows up here: `inc.Plan.Executable()` means a blocked
command can never enter the corpus, even via a careless approval. The guardrail's decision
propagates into learning without the feedback service knowing guardrails exist.

```go
func (f *Feedback) learn(ctx context.Context, summary domain.ApprovedSummary) error {
    if f.embedder == nil || f.vectors == nil {
        return fmt.Errorf("retrieval corpus is not configured")
    }
    runbook := summary.AsRunbook()
    if len(runbook.Commands) == 0 && runbook.RootCause == "" {
        return fmt.Errorf("nothing worth embedding: no commands and no root cause")
    }
    vec, err := f.embedder.Embed(ctx, runbook.Text())
    if err != nil { return fmt.Errorf("embed runbook: %w", err) }
    runbook.Embedding  = vec
    runbook.SourceText = runbook.Text()
    if err := f.vectors.Upsert(ctx, runbook); err != nil {
        f.park(ctx, "upsert-runbook", err.Error(), summary)
        return fmt.Errorf("upsert runbook: %w", err)
    }
    f.bus.Publish(ctx, bus.TopicRunbookLearned, runbook)
    return nil
}
```

**The whole learning loop is 20 lines.** Convert, embed, upsert, announce. It refuses to embed
something with no commands and no root cause — junk in the corpus is worse than a smaller corpus.

Verified: a 768-dimension row in `historical_runbooks`, retrieved on the next incident.

## `services.Digest` and `services.Maintenance`

```go
func (d *Digest) Run(ctx context.Context) error {
    to := d.clock.Now()
    from := to.Add(-d.window)

    entries, err := d.summaries.ListSummaries(ctx, from, to)
    if err != nil { return fmt.Errorf("list summaries: %w", err) }

    digest := build(from, to, entries)

    narrative, err := d.narrator.Narrate(ctx, prompt.Digest(digest))
    if err != nil {
        d.log.Warn("narrator failed, sending data-only digest", "err", err.Error())
        narrative = fallbackNarrative(digest)
    }
    digest.Narrative = narrative
    ...
}
```

**It reports on reviewed incidents only** — `approved_summaries`, not `raw_anomalies`. So the
report reflects what humans confirmed, not what a model guessed. That is a product decision worth
stating.

**If the model fails, it still sends** a data-only digest with counts. A report that arrives with
less prose beats a report that does not arrive.

`build()` counts by decision and ranks services by frequency, alphabetical as the tie-break so
output is deterministic.

`Maintenance.Run` attempts both halves even if the first fails, and joins the errors:

```go
if err := m.partitions.EnsurePartitions(ctx, m.ahead); err != nil {
    errs = append(errs, fmt.Errorf("ensure partitions: %w", err))
} else {
    m.log.Info("partitions ensured", "days_ahead", m.ahead)
}
if err := m.partitions.DropPartitionsOlderThan(ctx, m.retention); err != nil {
    errs = append(errs, fmt.Errorf("drop old partitions: %w", err))
}
if len(errs) > 0 { return errors.Join(errs...) }
```

A missing future partition and stale old data are independent problems; failing the first should
not skip the second. `errors.Join` (Go 1.20+) reports both.

## `cmd/watchtowerd/main.go`

```go
func main() {
    envFile := flag.String("env-file", ".env", "path to a KEY=VALUE file loaded before the environment is read")
    showVersion := flag.Bool("version", false, "print the version and exit")
    job := flag.String("job", "", "run one job (digest|maintenance) and exit instead of serving")
    flag.Parse()

    if *showVersion { fmt.Println("watchtower", bootstrap.Version); return }

    if err := config.LoadDotEnv(*envFile); err != nil { ... os.Exit(1) }

    app, err := bootstrap.New(config.Load())
    if err != nil {
        fmt.Fprintf(os.Stderr, "watchtower: startup failed: %v\n", err)
        os.Exit(1)
    }

    ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
    defer stop()

    run := app.Run
    if *job != "" {
        run = func(ctx context.Context) error { return app.RunOnce(ctx, *job) }
    }
    if err := run(ctx); err != nil { ... os.Exit(1) }
}
```

Four responsibilities and no more: flags, config, build, signals. `main` contains no business
logic, which is what makes the whole system testable — everything is reachable through
`bootstrap.New(cfg)`.

`signal.NotifyContext` converts SIGINT/SIGTERM into a cancelled context, which is the modern
idiom (Go 1.16+) and much cleaner than a signal channel. SIGTERM is what Kubernetes sends before
SIGKILL, so honouring it is what makes graceful shutdown work in production.

## Shutdown order

```go
func (a *App) Shutdown() error {
    ctx, cancel := context.WithTimeout(context.Background(), grace)
    defer cancel()

    var errs []error
    if err := a.server.Shutdown(ctx); err != nil { ... }   // 1. stop accepting
    if err := a.sched.Stop(ctx); err != nil { ... }        // 2. stop scheduling
    if err := a.pool.Shutdown(ctx); err != nil { ... }     // 3. drain in-flight work
    if err := a.broker.Close(ctx); err != nil { ... }      // 4. flush events
    if err := a.env.Close(); err != nil { ... }            // 5. close connections
    a.log.Info("watchtower stopped")
    return errors.Join(errs...)
}
```

The order is the whole point, and it is a good interview answer on its own:

1. HTTP first — stop taking new work, let in-flight requests finish.
2. Scheduler — no new cron jobs.
3. Worker pool — drain queued incidents. **This must come before closing connections**, or a
   draining job would fail on a closed pool.
4. Bus — flush pending events so the last email actually gets sent.
5. Resources — close Postgres and Redis last, LIFO.

Everything bounded by `WT_HTTP_SHUTDOWN_GRACE` (10s), and every error collected rather than
returning on the first, so one stubborn component does not skip the rest.

---

# A full trace of one real incident

From the running stack, verbatim. This is the best possible interview material because it is
evidence rather than description.

## Startup

```
msg="watchtower starting" version=compose drivers="env=dev store=postgres vector=postgres
  embedder=openai rerank=llm llm=openai notify=smtp gate=redis cache=redis enrich=loki"
msg="postgres connected" max_conns=10
msg="postgres schema ready" vector_dims=768
msg="redis connected" addr=redis:6379 db=0
msg="pipeline assembled" stages="admit -> debounce -> correlate -> enrich -> sanitize ->
  input-guard -> publish:incident.sanitized -> retrieve -> input-guard:retrieved -> prompt ->
  plan -> output-guard -> score -> publish:incident.triaged"
msg="worker pool started" component=worker workers=8 queue=512
msg="job scheduled" component=cron job=partition-manager spec="0 0 * * *"
msg="job scheduled" component=cron job=daily-pulse spec="0 8 * * *"
msg="partitions ensured" service=maintenance days_ahead=7
msg="http listening" component=http addr=:8080 auth=true
```

Every one of those lines answers a question you would otherwise have to guess at.

## The incident

```
23:30:54.171  incident queued        alerts=3 severity=critical            ← real Alertmanager webhook
23:30:54.172  request POST /alerts   status=202 took=0s remote=172.20.0.4
23:30:54.172  event topic=incident.received
23:30:54.172  stage ok  stage=admit      took=0s
23:30:54.172  stage ok  stage=debounce   took=0s
23:30:54.172  stage ok  stage=correlate  took=0s
23:30:54.194  stage ok  stage=enrich     took=22ms      ← Loki + Kubernetes, in parallel
23:30:54.197  stage ok  stage=sanitize   took=3ms       ← 150 lines → 3
23:30:54.197  stage ok  stage=publish:incident.sanitized
23:30:54.204  incident persisted  projector=persist  topic=incident.sanitized
23:30:54.900  stage ok  stage=retrieve   took=703ms     ← embed + pgvector query
23:30:54.901  stage ok  stage=input-guard took=1ms
23:30:54.901  stage ok  stage=prompt     took=0s
23:31:15.659  completion done  model=qwen2.5:1.5b tokens=1168  ← planner, attempt 1
23:31:33.561  completion done  model=qwen2.5:1.5b tokens=1283  ← critic
23:31:44.169  completion done  model=qwen2.5:1.5b tokens=1273  ← planner, attempt 2
23:31:56.284  completion done  model=qwen2.5:1.5b tokens=1399  ← critic
23:31:56.284  stage ok  stage=plan         took=1m1.387s
23:31:56.284  stage ok  stage=output-guard took=1ms
23:31:56.284  stage ok  stage=score        took=0s
23:31:56.291  incident persisted  projector=persist  topic=incident.triaged
23:31:56.303  email delivered  component=smtp to=sre@example.com
              subject="[WatchTower][CRITICAL] checkout-api container OOM killed, memory limit reached (+2 related)"
```

**Total: 62 seconds, of which 61 was the model.** Everything else — three HTTP fetches, two
database writes, a vector search and an email — took under one second combined. Memorise that
ratio; it is the answer to "where is the time going?"

## What was stored

```json
{
  "meta": {
    "logs_in": "150", "logs_out": "3", "confidence": "0.8",
    "redactors": "secrets+pii+custom", "blocked_commands": "0", "risk_relabelled": "2",
    "correlated_alerts": "4", "debounce_key": "389375c5e0780445",
    "correlation_key": "cluster=prod-eu-1,namespace=checkout,service=checkout-api"
  },
  "trace": [
    "provider k8s: 0 logs, 19 events in 13ms",
    "provider loki: 150 logs, 0 events in 11ms",
    "sanitizer kept 3 of 150 log lines",
    "retrieved 1 runbook(s) above score 0.20",
    "critic rejected draft 1: A command references a resource that does not appear in the context",
    "critic approved draft on attempt 2",
    "risk classifier corrected 2 self-declared risk level(s)"
  ],
  "plaintext_secret_at_rest": false,
  "k8s_warnings": 3
}
```

## And the learning loop closing

Approve with edited commands, then a new incident arrives:

```json
"matches": [{
  "score": 0.3645569798246295,
  "stage": "rerank:llm:qwen2.5:1.5b",
  "reason": "Does not address the same failure mode.",
  "runbook": {
    "signature": "65a937f408edc78e",
    "title": "checkout-api container OOM killed, memory limit reached (+2 related)",
    "root_cause": "Memory limit of 32Mi is below the working set after release 4.2.1",
    "commands": [
      {"run": "kubectl -n checkout set resources deployment/checkout-api --limits=memory=512Mi", ...},
      {"run": "kubectl -n checkout rollout status deployment/checkout-api --timeout=120s", ...}
    ],
    "reviewer": "sre-oncall"
  }
}]
```

And the planner's output for that incident began with **exactly the command the human had
approved**. The loop is closed: human judgement became retrieval material became a better plan.

---

## The four bugs, in one place

Every one was found by running the system, not reading it. This section is your best interview
story; Part 25 gives you the wording.

**1. Startup deadlock.** `wire.Resource` held its mutex while calling the factory, and factories
call `env.OnClose` — the same non-reentrant mutex. Deadlock. Symptom: the service hung after one
log line, and *only* with Postgres or Redis, because in-memory adapters never register a closer.
Every offline test passed. Fixed with separate locks and by releasing the lock during
construction.

**2. Secrets written to disk.** Persistence was published *before* redaction, so credentials from
log lines landed in Postgres. Proved with `payload::text LIKE '%s3cr3tp4ss%'` returning true
mid-pipeline. Fixed by moving `InputGuard` ahead of the publish.

**3. Secrets sent to the model provider.** The embedding call also ran before redaction, so the
RAG query could carry a live database password out of the process. The same reorder fixed it, plus
a second redaction pass over retrieved corpus material.

**4. Critiques silently discarded.** The model returned `issues` as an object, `json.Unmarshal`
failed, and the entire verdict was thrown away — the critic looked like it was running while
contributing nothing. Fixed with `flexStrings` coercion. The next run immediately showed the
self-correction loop working for the first time: rejected with reasons, rewritten, approved,
confidence 0.80.

Notice the pattern in all four: **the offline happy path passed every time.** Bugs 1–3 needed
real infrastructure to appear; bug 4 needed a real, slightly sloppy model. That is the lesson, and
it is a better answer than any individual bug.

---

Next: **Part 24 — the configuration bible.**
