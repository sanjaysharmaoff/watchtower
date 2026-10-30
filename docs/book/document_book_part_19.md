# Part 19 — Code Walkthrough: Stages, Part B (guard → publish)

The expensive half. This is also where the two security-relevant stages live, so it is the part
worth knowing best.

## The order, and why it changed

```
... sanitize → input-guard → publish:incident.sanitized → retrieve →
    input-guard:retrieved → prompt → plan → output-guard → score → publish:incident.triaged
```

Redaction sits **before** the first database write and **before** the first outbound model
call. The original order had it after both, and that was a real defect found by running the
system against real data (full story in Part 23). The comment in the composition root records
the reasoning:

```go
// Redaction sits ahead of both the first database write and the first outbound
// model call, so a credential that leaked into a log line is never written to
// disk and never leaves the process.
stages.InputGuard(redactors),
stages.Publish(a.broker, bus.TopicIncidentSanitized),
```

## `InputGuard` — redaction

```go
func InputGuard(redactors []ports.Redactor) Stage {
    return InputGuardNamed("input-guard", redactors)
}

func InputGuardNamed(name string, redactors []ports.Redactor) Stage {
    return mutator(name, func(_ context.Context, inc *domain.Incident) error {
        if len(redactors) == 0 { return nil }
        clean := func(s string) string {
            for _, r := range redactors {
                s = r.Redact(s)
            }
            return s
        }

        for i := range inc.Alerts {
            a := &inc.Alerts[i]
            a.Summary = clean(a.Summary)
            a.Description = clean(a.Description)
            for k, v := range a.Annotations { a.Annotations[k] = clean(v) }
            for k, v := range a.Labels      { a.Labels[k] = clean(v) }
        }
        for i := range inc.Logs   { inc.Logs[i].Message = clean(inc.Logs[i].Message) }
        for i := range inc.Events { inc.Events[i].Message = clean(inc.Events[i].Message) }
        for i := range inc.Matches {
            rb := &inc.Matches[i].Runbook
            rb.RootCause = clean(rb.RootCause)
            for j := range rb.Commands {
                rb.Commands[j].Run = clean(rb.Commands[j].Run)
                rb.Commands[j].Explain = clean(rb.Commands[j].Explain)
            }
        }
        ...
    })
}
```

Details worth knowing:

**`a := &inc.Alerts[i]`, and `for i := range` everywhere.** Ranging by value would copy the
struct and mutate the copy — the classic Go mistake. Taking the address of the slice element
mutates in place.

**Every text field is covered**: alert summaries and descriptions, all annotation and label
values, log messages, event messages, and retrieved runbook text. Missing one is a leak, so the
list is exhaustive rather than "the ones likely to matter".

**`InputGuardNamed`** exists so the same guard can appear twice in a chain with distinguishable
log lines. The second pass, `input-guard:retrieved`, runs after retrieval — defence in depth
over material that came out of a *shared* database which another tool might have written to.
Redaction is idempotent, so a second pass is free of side effects.

**It records what ran**: `inc.SetMeta("redactors", "secrets+pii+custom")`. If you later find a
leak, you can tell from the stored incident which redactors were configured at the time.

Verified on real data: the alert description contained
`postgres://svc:s3cr3tp4ss@db.internal:5432/checkout`, and the stored incident had
`plaintext_secret_at_rest = false` with `[REDACTED:DSN_PASSWORD]` present.

## `Retrieve` — RAG

```go
type RetrieveOptions struct {
    TopK     int
    TopN     int
    MinScore float64
    QueryOf  func(*domain.Incident) string
}

func Retrieve(emb ports.Embedder, store ports.VectorStore, rr ports.Reranker, opts RetrieveOptions) Stage {
    // defaults: TopK 10, TopN 2, QueryOf = DefaultQuery
    return mutator("retrieve", func(ctx context.Context, inc *domain.Incident) error {
        if emb == nil || store == nil {
            inc.Note("retrieval disabled")
            return nil
        }
        query := opts.QueryOf(inc)
        if strings.TrimSpace(query) == "" {
            inc.Note("retrieval skipped: empty query")
            return nil
        }
        inc.SetMeta("rag_query", truncate(query, 240))

        vec, err := emb.Embed(ctx, query)
        if err != nil {
            inc.Note("embedding failed, continuing without runbooks: %v", err)
            return nil
        }
        matches, err := store.Search(ctx, vec, opts.TopK)
        if err != nil {
            inc.Note("vector search failed, continuing without runbooks: %v", err)
            return nil
        }
        if len(matches) == 0 {
            inc.Note("no historical runbooks matched")
            return nil
        }
        if rr != nil {
            reranked, err := rr.Rerank(ctx, query, matches, opts.TopN)
            if err != nil {
                inc.Note("rerank failed, using raw similarity order: %v", err)
            } else {
                matches = reranked
            }
        }
        kept := make([]domain.RunbookMatch, 0, opts.TopN)
        for _, m := range matches {
            if len(kept) >= opts.TopN { break }
            if m.Score < opts.MinScore { continue }
            kept = append(kept, m)
        }
        inc.Matches = kept
        inc.Note("retrieved %d runbook(s) above score %.2f", len(kept), opts.MinScore)
        return nil
    })
}
```

**Count the early returns: five, and every one of them returns `nil`.** No embedder, empty
query, embedding failed, search failed, nothing matched, rerank failed — each is recorded as a
note and the pipeline continues. Retrieval is an enhancement; losing it must never lose the
incident. The consequence flows automatically into the score, which awards up to 0.4 for
retrieval support.

**`MinScore` filters after reranking**, so a weak match is discarded rather than misleading the
model. An irrelevant runbook in a prompt is worse than no runbook, because the model will try to
use it.

**`QueryOf` is injectable**, defaulting to `DefaultQuery`:

```go
func DefaultQuery(inc *domain.Incident) string {
    var b strings.Builder
    primary := inc.PrimaryAlert()
    b.WriteString(primary.Name)
    if primary.Summary != "" { b.WriteString(": "); b.WriteString(primary.Summary) }
    if t := primary.Target(); t != "unknown" { b.WriteString(" ["); b.WriteString(t); b.WriteString("]") }
    for _, a := range inc.Alerts[1:min(len(inc.Alerts), 4)] { b.WriteString("; "); b.WriteString(a.Name) }
    for _, l := range TopLines(inc.Logs, 3) { b.WriteString("\n"); b.WriteString(truncate(l.Message, 200)) }
    for _, e := range inc.Events[:min(len(inc.Events), 2)] {
        b.WriteString("\n")
        b.WriteString(fmt.Sprintf("%s %s", e.Reason, truncate(e.Message, 160)))
    }
    return strings.TrimSpace(b.String())
}
```

The query is **alert names + target + the three most severe log lines + two events**. Not the
whole incident: an embedding of everything is an embedding of nothing in particular — the
distinguishing signal gets averaged away. `TopLines` sorts by `LevelRank()` so errors outrank
info lines.

Real query produced:

```
ContainerMemoryLimitReached: checkout-api reached its memory limit and was killed again
[prod-eu-1/checkout/checkout-api]; ContainerOOMKilled; PodRestartRateHigh; DependencyUnreachable
level=error msg="fatal error: runtime: out of memory (heap 512MiB, limit 512MiB) req_id…"
```

## `BuildPrompt`

```go
func BuildPrompt(b ports.PromptBuilder) Stage {
    return mutator("prompt", func(_ context.Context, inc *domain.Incident) error {
        p, err := b.Build(inc)
        if err != nil {
            return fmt.Errorf("build prompt: %w", err)
        }
        inc.Prompt = p
        return nil
    })
}
```

Note this one **does** return the error rather than swallowing it. No prompt means no plan, so
there is nothing to degrade to.

## `Plan` — the self-correction loop

```go
func Plan(planner ports.Planner, critic ports.Critic, opts PlanOptions) Stage {
    if opts.MaxAttempts <= 0 { opts.MaxAttempts = 1 }
    return mutator("plan", func(ctx context.Context, inc *domain.Incident) error {
        prompt := inc.Prompt
        var lastErr error

        for attempt := 1; attempt <= opts.MaxAttempts; attempt++ {
            inc.Attempts = attempt
            plan, err := planner.Plan(ctx, prompt)
            if err != nil {
                lastErr = err
                inc.Note("planner attempt %d failed: %v", attempt, err)
                continue
            }
            plan.Model = planner.Name()
            inc.Plan = &plan
            lastErr = nil

            if !opts.CriticOn || critic == nil {
                return nil
            }
            verdict, err := critic.Review(ctx, prompt, plan)
            if err != nil {
                inc.Note("critic unavailable on attempt %d, accepting draft: %v", attempt, err)
                return nil
            }
            verdict.Model = critic.Name()
            inc.Verdict = &verdict
            if verdict.Approved {
                inc.Note("critic approved draft on attempt %d", attempt)
                return nil
            }
            if len(verdict.Issues) == 0 && strings.TrimSpace(verdict.Notes) == "" {
                // A rejection with no stated objection gives the next attempt nothing to
                // correct, so retrying is pure token spend. Keep the draft, flag it, and
                // let the confidence score carry the doubt to the reviewer.
                verdict.Issues = []string{"reviewer rejected the plan without giving a reason, so it was not rewritten"}
                inc.Verdict = &verdict
                inc.Note("critic rejected draft %d without reasons, keeping draft", attempt)
                return nil
            }
            inc.Note("critic rejected draft %d: %s", attempt, strings.Join(verdict.Issues, "; "))
            if attempt == opts.MaxAttempts { return nil }
            prompt = withCritique(inc.Prompt, plan, verdict)
        }

        if lastErr != nil {
            return fmt.Errorf("planner exhausted %d attempt(s): %w", opts.MaxAttempts, lastErr)
        }
        return nil
    })
}
```

Walk through the exit paths, because the interviewer will:

| Situation | Behaviour |
| --- | --- |
| planner errored | note it, try again, and only fail the stage if every attempt failed |
| critic disabled or nil | keep the draft, return |
| **critic itself errored** | keep the draft, note it — a broken reviewer must not block delivery |
| critic approved | done |
| critic rejected with reasons, attempts left | rebuild the prompt with the objections and retry |
| critic rejected with reasons, last attempt | keep the draft; the human sees the objections in the email |
| **critic rejected with no reasons** | keep the draft, synthesise one issue, stop retrying |

The last row is the refinement discovered by running a weak model, and it is a strong
"unreliable dependency" answer: retrying with no information is spending money for nothing.

The rewrite prompt:

```go
func withCritique(base domain.Prompt, draft domain.Plan, v domain.Verdict) domain.Prompt {
    var b strings.Builder
    b.WriteString(base.User)
    b.WriteString("\n\n## Previous attempt (rejected)\n")
    b.WriteString("root_cause: " + draft.RootCause + "\n")
    for _, s := range draft.Steps { b.WriteString("- " + s.Run + "\n") }
    b.WriteString("\n## Reviewer objections\n")
    for _, issue := range v.Issues { b.WriteString("- " + issue + "\n") }
    if v.Notes != "" { b.WriteString("\n" + v.Notes + "\n") }
    b.WriteString("\nProduce a corrected plan that resolves every objection. Do not repeat the rejected commands verbatim unless they were not the subject of an objection.")
    out := domain.Prompt{System: base.System, User: b.String(), Meta: map[string]string{}}
    for k, v := range base.Meta { out.Meta[k] = v }
    out.Meta["retry"] = "true"
    return out
}
```

Note it builds on `inc.Prompt` — the **original**, not the previously modified one — so
objections do not accumulate across attempts and the prompt cannot grow without bound.

Verified sequence: `critic rejected draft 1: A command references a resource that does not
appear in the context` → `critic approved draft on attempt 2` → confidence `0.80`.

## `OutputGuard` — the last gate before a human

```go
func OutputGuard(inspectors []ports.CommandInspector, classifier ports.RiskClassifier, mode GuardMode) Stage {
    return mutator("output-guard", func(_ context.Context, inc *domain.Incident) error {
        if inc.Plan == nil { return nil }
        blocked, relabelled := 0, 0
        for i := range inc.Plan.Steps {
            if classifier != nil {
                actual := classifier.Classify(inc.Plan.Steps[i])
                if inc.Plan.Steps[i].Risk != actual {
                    relabelled++
                    inc.Plan.Steps[i].Risk = actual
                }
            }
            cmd := inc.Plan.Steps[i]
            for _, ins := range inspectors {
                if err := ins.Inspect(cmd); err != nil {
                    inc.Plan.Steps[i].Blocked = true
                    inc.Plan.Steps[i].Reason = ins.Name() + ": " + strings.TrimPrefix(err.Error(), domain.ErrBlocked.Error()+": ")
                    blocked++
                    break
                }
            }
        }
        inc.SetMeta("blocked_commands", itoa(blocked))
        if relabelled > 0 {
            inc.SetMeta("risk_relabelled", itoa(relabelled))
            inc.Note("risk classifier corrected %d self-declared risk level(s)", relabelled)
        }
        if blocked > 0 {
            inc.Note("output guard blocked %d of %d command(s)", blocked, len(inc.Plan.Steps))
        }
        if blocked > 0 && mode == GuardHalt {
            return domain.Block("%d command(s) violated policy", blocked)
        }
        if blocked > 0 && len(inc.Plan.Executable()) == 0 {
            inc.Note("no safe commands remain; delivering diagnosis only")
        }
        return nil
    })
}
```

Five decisions in 30 lines:

**Risk is recomputed first.** The model's `risk` field is overwritten by the classifier's
verdict before any inspector runs. Verified: `risk_relabelled: "2"` — the model had called
`kubectl set resources` and `kubectl rollout restart` "read".

**`break` after the first failing inspector.** One reason is enough; the reviewer does not need
a list.

**Blocked commands are marked, not deleted.** `Blocked = true` plus a `Reason` naming the rule
(`denylist: recursive delete (matched "rm -rf /var/lib/data")`). The reviewer sees exactly what
was proposed and why it was refused. Deleting it would hide information from the person
responsible for the decision.

**Two modes.** `strip` (default) keeps the rest of the plan; `halt` refuses the whole incident
via `domain.Block`. Configurable because different organisations have different appetites, and
strip is the better default: a diagnosis with one command removed is still useful.

**The "nothing left" case is explicit.** If every command was blocked, a note says the email is
a diagnosis only, and the scorer subtracts 0.25. The human is told, in the email, that there is
nothing to run.

## `Score`

```go
func Score(s ports.Scorer) Stage {
    return mutator("score", func(_ context.Context, inc *domain.Incident) error {
        inc.Confidence = s.Score(inc)
        if inc.Plan != nil {
            inc.Plan.Confidence = inc.Confidence
        }
        inc.SetMeta("confidence", strings.TrimRight(strings.TrimRight(formatFloat(inc.Confidence), "0"), "."))
        return nil
    })
}
```

Note it **overwrites** `inc.Plan.Confidence` — the model's own claim is replaced by the system's
computed value. The model's number survives only as one lightly-weighted input inside the
scorer (0.1 maximum). A confident hallucination cannot present itself as high confidence.

## `Publish` and `Tap`

```go
func Publish(b bus.Bus, topic string) Stage {
    return pipeline.Of("publish:"+topic, func(ctx context.Context, inc *domain.Incident) (*domain.Incident, error) {
        b.Publish(ctx, topic, inc)
        return inc, nil
    })
}

func Tap(name string, fn func(ctx context.Context, inc *domain.Incident)) Stage { ... }
```

Four lines, and they are the pipeline's entire relationship with persistence and email. The
stage name includes the topic (`publish:incident.sanitized`), so the assembled-pipeline log line
shows exactly where the announcements happen.

`Tap` is the escape hatch for a synchronous side effect when ordering matters more than
decoupling. Currently unused — an honest thing to note if asked.

---

Next: **Part 20 — the ingest and enrich adapters**, including the sanitizer algorithms.
