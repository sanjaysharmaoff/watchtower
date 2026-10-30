# Part 25 — The Interview Playbook

Everything you need on the day: the pitch, the whiteboard script, ~40 questions with answers, the
stories, the honest weaknesses, and what not to say.

## 1. The 30-second version

Use this when someone says "tell me about a project."

> WatchTower is an AI-assisted alert triage service in Go. A production alert comes in from
> Prometheus, and it groups related alerts, pulls the logs and Kubernetes events around the
> failure, redacts any secrets, retrieves runbooks that fixed similar problems before, asks a
> model for a remediation plan, has a second model attack that plan, blocks any dangerous
> command, and emails the on-call engineer a runbook with Approve, Edit and Reject buttons.
> What they approve becomes retrieval material, so it learns the failures that organisation
> actually has.
>
> It is ports and adapters: about 30 small interfaces, every one with at least two
> implementations, so the whole pipeline runs on a laptop with no infrastructure at all, or
> against real Prometheus, Loki, Kubernetes, pgvector and real models — and the difference is
> environment variables.

Then stop. Let them pick the thread.

## 2. The whiteboard script

Draw four boxes left to right. Talk while you draw; do not draw in silence.

```
 SOURCES            WATCHTOWER (Go)                            HUMAN
 ┌──────────┐   ┌──────────────────────────────────┐   ┌─────────────┐
 │Prometheus│──▶│ POST /alerts → worker pool        │   │             │
 │+ Alertmgr│   │   ↓                               │   │  SRE email  │
 └──────────┘   │ admit → debounce → correlate      │──▶│  Approve /  │
 ┌──────────┐   │   ↓                               │   │  Edit /     │
 │  Loki    │◀──│ enrich (parallel) → sanitize      │   │  Reject     │
 └──────────┘   │   ↓                               │   └──────┬──────┘
 ┌──────────┐   │ INPUT GUARD (redact secrets)      │          │
 │ k8s API  │◀──│   ↓                               │          │
 └──────────┘   │ retrieve (pgvector + rerank)      │◀─────────┘
                │   ↓                               │   embed approved runbook
                │ plan ⇄ critic  (self-correction)  │
                │   ↓                               │
                │ OUTPUT GUARD (lint commands)      │
                │   ↓                               │
                │ score → publish                   │
                └──────────────────────────────────┘
                      ↓ Postgres (partitioned) + DLQ
```

Then say these four things, in this order:

1. **"Everything before retrieval is cheap and everything after is expensive, so the order is
   deliberate."** 150 log lines become 3; duplicates are dropped for an hour; five alerts become
   one incident. Measured: enrich 22ms, sanitize 3ms, retrieve 703ms, plan 61s.
2. **"Two guardrails, and the second one does not trust the model."** Secrets are redacted before
   anything is stored or sent. Commands are linted whole and per segment, and the risk label is
   recomputed from the command text rather than taken from the model.
3. **"Nothing executes."** The system proposes; a human decides. There is no executor in the
   codebase.
4. **"The human's decision closes the loop."** An approved runbook is embedded and retrieved for
   the next similar incident.

## 3. The numbers to have memorised

| Fact | Number |
| --- | --- |
| Log compression | 150 lines in → 3 out |
| Correlation | 5 alerts → 1 incident → 1 email |
| Latency split | 62s total, 61s of it the model |
| Enrichment | 19 real k8s events + 150 Loki lines, ~13ms each, in parallel |
| Confidence after self-correction | 0.80 |
| Codebase | ~8,000 lines, 77 Go files, 4 dependencies |
| Dead letters after all runs | 0 |
| Restructure blast radius | 20 packages moved, 6 files changed |
| Real bugs found by running it | 4 |

Four dependencies: `pgx`, `go-redis`, `robfig/cron`, `google/uuid`. No web framework, no ORM, no
model SDK, no Kubernetes client. Worth saying — it demonstrates taste.

## 4. Architecture questions

**Q: What architecture, and why?**
Ports and adapters. Eight external technologies, and I wanted the whole pipeline to run with none
of them present. The application declares ~30 small interfaces; every technology is an adapter.
`domain` imports only the standard library, `ports` imports only `domain`, and `bootstrap` is the
one place that knows about everybody.

**Q: How do you decide interface boundaries?**
By who consumes them. I have four storage interfaces rather than one `Storage`: the persist
projector needs incidents and the DLQ, the digest needs only `ListSummaries`, maintenance needs
only partitions. One big interface would hand every consumer capabilities it should not have and
force the in-memory implementation to implement everything to be useful for one test.

**Q: Isn't 30 interfaces over-engineering?**
It would be if they were speculative. Every one has at least two implementations that are actually
used, and I deleted four — `QueryBuilder`, `Migrator`, `Closer`, `HealthChecker` — once it was
clear nothing consumed them. It paid for itself concretely: the system runs with zero
infrastructure, real infrastructure is config-only, and a 20-package restructure touched 6 files.

**Q: Why a pipeline instead of ordered function calls?**
Order becomes data. Stages are independently testable, cross-cutting concerns live in middleware
rather than being repeated 14 times, and reordering is a slice edit. That mattered: a security fix
required moving redaction ahead of the first database write and the first model call, and it was
two lines with no stage modified.

**Q: Why generics in the pipeline?**
Compile-time payload safety without `interface{}` and a type assertion in every stage, and the
runner stays reusable for a different payload type.

**Q: Why an event bus in a single process?**
One file, ~150 lines, and it inverts control at the outbound edge: the pipeline neither imports
nor knows about persistence, email or metrics. It also let me put persistence and delivery on
*different* topics — evidence is stored mid-pipeline before the risky part, delivery happens at
the end. And swapping it for NATS is one adapter behind the same interface.

**Q: How does driver selection work?**
Adapters self-register into a typed registry in `init()`, keyed by name; the composition root
resolves config → registry → instance. Same pattern as `database/sql` drivers. A typo produces a
startup error listing the valid names.

**Q: How do adapters share a database connection?**
A memoized resource pool keyed by string on the build environment. Every Postgres adapter asks for
`"postgres.pool"`; the first call builds it and registers a shutdown hook. Lazy, so a memory-only
configuration never opens a connection.

## 5. Technology questions

**Q: Prometheus pull vs push?**
Pull gives liveness for free — a failed scrape is `up == 0`, which is itself a signal. It also
decouples applications from monitoring topology. The tradeoff is short-lived jobs, which is what
the Pushgateway is for.

**Q: What is cardinality?**
The number of distinct label combinations, each a stored series. A high-variety label like a
request id explodes memory. The same idea appears in my log deduplication: request ids are
normalized away before comparison, because otherwise identical lines look unique.

**Q: Alertmanager already groups. Why correlate again?**
Alertmanager groups by exact label equality on a fixed key set and cannot reason across groups
over time. My correlator groups by topology labels within a sliding window and merges alerts from
*separate* webhooks minutes apart. I also set `group_by` deliberately coarse so more context
arrives in one payload.

**Q: Loki vs Elasticsearch?**
Loki indexes labels, not content, so ingestion and storage are much cheaper. The constraint is
that queries need a label anchor, which is fine because an alert always tells me the namespace and
service. ELK would be one more `ContextProvider`, about 80 lines.

**Q: Why partition Postgres?**
Retention becomes `DROP TABLE` rather than a mass `DELETE`: constant time, no vacuum pressure, no
WAL flood. Inserts also stay fast because today's indexes are small. I deliberately do not create
a DEFAULT partition, because once it holds a row for a day, creating that day's real partition
fails.

**Q: Explain embeddings and cosine similarity.**
A model turns text into a fixed-length vector where similar meanings point in similar directions.
Cosine measures the angle, so a long document is not treated as more relevant for being long. With
normalised vectors it is just a dot product.

**Q: Why pgvector rather than a dedicated vector database?**
The corpus is human-approved runbooks, so hundreds or low thousands, and I already needed Postgres
for incidents and reporting. One database means one pool, one backup, one thing to operate. A
dedicated vector store earns its place at a scale I do not have.

**Q: Exact or approximate search?**
HNSW, so approximate. The index has to be built with the operator class matching the query's
distance operator or it will not be used.

**Q: Why rerank?**
Vector search compares two independently-computed summary vectors, which gets you the right
neighbourhood and no further. Reranking re-scores the top 10 with something more accurate. The
default is a free lexical heuristic; the LLM reranker reads query and candidate together, which is
the cross-encoder idea. It is *not* a true cross-encoder — that would drop in as a third driver.

**Q: Why not fine-tune?**
Retrieval updates instantly — an engineer approves a runbook and the next incident can use it —
and it can cite its source, which matters for trust. Fine-tuning needs a training run per update.

## 6. Reliability and safety questions

**Q: What happens if Loki is down?**
The enrich stage fans out under one timeout; a failing provider contributes an error note to the
incident trace and the pipeline continues with whatever else returned. The model works from less
evidence and the confidence score drops, because evidence depth feeds it. Losing logs must never
lose the incident.

**Q: What if Redis is down?**
The debounce gate fails **open** — a comment in the code says why. Broken Redis means possibly
duplicate emails; failing closed would mean no emails during an outage, which is exactly when they
matter.

**Q: What if the database write fails?**
The failure is parked in the dead letter queue with the full JSON payload, and processing
continues. If even the DLQ write fails, the structured log line is the record of last resort.
Across every run so far the DLQ has stayed at zero.

**Q: What if the model returns garbage?**
Markdown fences and surrounding prose are stripped, shape variance is coerced, and a plan with
neither a root cause nor steps fails the attempt and is retried up to `WT_LLM_MAX_ATTEMPTS`. If
every attempt fails, the incident is parked — and the evidence was already persisted before the
model was called.

**Q: How do you stop it doing something dangerous?**
Four layers, and the last two do not trust the model at all. RAG grounds it; the prompt forbids
inventing resources and explicitly allows saying "insufficient evidence"; a second model reviews
the draft against stated criteria; then deterministic code recomputes every risk label from the
command text and lints each command whole and per segment against a catalogue of destructive
patterns. And nothing executes without a human.

**Q: Give me a concrete example of the model being wrong.**
Two, both real. It labelled `kubectl delete pod` as read-only, which is why the risk classifier
exists and overrides it. And it invented a `kubectl resize` subcommand that does not exist — the
critic caught that one as a resource not present in the context.

**Q: Why is redaction where it is in the pipeline?**
Because it was originally in the wrong place. It ran after persistence and after the embedding
call, so credentials from a log line were written to Postgres and could be sent to the model
provider in the RAG query. I proved it with a SQL `LIKE` on the stored payload, moved the stage
ahead of both, and added a second pass over material retrieved from the shared corpus. Now
`plaintext_secret_at_rest` is false.

**Q: Are your endpoints authenticated?**
`POST /alerts` and `POST /runbook/approve` require a bearer token, compared in constant time, and
the service refuses to start in `prod` without one. `GET /runbook/approve` is deliberately exempt
because email clients cannot set headers — it is authenticated by an HMAC signature over incident,
decision and expiry, with a server-side expiry check. That is a documented tradeoff with a
compensating control, and approval only stores a runbook; it never executes anything.

**Q: What is your Kubernetes blast radius?**
`get` and `list` on events, pods and workloads. No write verb anywhere, enforced by the API server
rather than by my code. TLS verified against the cluster CA. In-cluster it uses the projected
service account token.

## 7. Scale, cost and operations

**Q: How does this scale?**
Horizontally, with two config changes: `WT_GATE_DRIVER=redis` so debouncing is shared, and
`WT_CACHE_DRIVER=redis` so one replica's embedding is another's cache hit. Cron moves to
Kubernetes CronJobs so N replicas do not send N reports. One documented gap remains: the
correlation window is still per-process, so correlation could be partial across replicas. The fix
is a Redis-backed correlator, and the interface already allows it.

**Q: What does it cost to run?**
Per incident with the critic on and one retry: two planner calls, two critic calls, one embedding
— fractions of a cent on `gpt-4o-mini`. The controls are upstream of the model: debounce drops
duplicates for an hour, correlation collapses a cascade into one plan, the sanitizer takes 150 log
lines to 3, retrieval sends two runbooks not ten, embeddings are cached by content hash and model
name, and the default reranker is free.

**Q: Where is the time going?**
62 seconds total, 61 of it model inference on a 1.5B CPU model. Everything else — three HTTP
fetches, two database writes, a vector search and an email — was under a second combined. On
`gpt-4o-mini` the whole thing would be a few seconds.

**Q: How would you deploy this?**
It is already a static distroless binary configured purely by environment variables, with
`/healthz` and `/readyz`. So: a Deployment with those probes, a ConfigMap for settings and a
Secret for credentials, `WT_CRON_ENABLED=false` with two CronJobs running `--job maintenance` and
`--job digest`, Redis for shared state, and managed Postgres with pgvector.

**Q: Liveness vs readiness — how did you choose?**
Liveness is cheap and dependency-free: if it consulted the database, a brief blip would get healthy
pods killed. Readiness actually runs partition maintenance, which exercises connectivity and DDL —
if partitions cannot be created, writes will fail, so the instance should not take traffic. It is
idempotent, so probing it constantly is safe.

**Q: How would you monitor it?**
There is an `Observer` middleware hook in the pipeline that is not yet wired; a Prometheus
histogram of stage duration and outcome would be about five lines there, and the pipeline package
would still not import Prometheus. I would alert on dead letter queue growth, `429` rate,
per-stage error rate, and the ratio of approvals to rejections — that last one is the real
quality signal for the model.

**Q: How do you test it?**
Every dependency is a small interface, so tests build the real pipeline with fake adapters — no
mocking framework, no network. The in-memory adapters are the *default* configuration, so the
offline path is exercised constantly rather than rotting. I also used a throwaway probe test to
verify the guardrail against a catalogue of destructive commands, which is how I found the
`curl | bash` gap. And I would be honest that the automated test suite is thin — that is the first
thing I would add.

## 8. The four-bugs story

This is your strongest answer to "tell me about a difficult bug" or "how do you know it works?"
Lead with the pattern, then give one bug in detail.

> Everything passed locally, so I built the real stack — Prometheus, Alertmanager, Loki, Postgres
> with pgvector, Kubernetes, real models via Ollama, real SMTP — and ran it end to end. That found
> four bugs the offline path could not.
>
> The hardest was a startup deadlock. The service hung after printing one log line, but only with
> Postgres or Redis. `wire.Resource` held its mutex while calling the adapter factory, and
> factories register their own shutdown hook via `OnClose`, which took the same mutex. Go mutexes
> are not reentrant, so the goroutine deadlocked against itself. The in-memory adapters never
> register a closer, which is exactly why every local test passed. Two fixes: separate locks for
> the resource map and the closer list, and release the lock while the factory runs.
>
> Two were security ordering defects: persistence and the embedding call both happened before
> redaction, so a database password that had leaked into a log line was written to Postgres and
> could be sent to the model provider. I proved it with a SQL `LIKE` on the stored payload and
> fixed it by moving the guard stage earlier.
>
> The fourth: the model returned `issues` as an object instead of an array, a strict
> `json.Unmarshal` failed, and every critique was silently discarded — the critic appeared to run
> while contributing nothing. After coercing the JSON, the very next run showed the
> self-correction loop working for the first time: rejected with reasons, rewritten, approved,
> confidence 0.80.
>
> The lesson I took is that the offline happy path passed every single time. Three of those needed
> real infrastructure to appear and one needed a real, slightly sloppy model.

## 9. Weaknesses to volunteer

Raise these before you are asked. It reads as judgement rather than blind spots.

1. **Plan quality is capped by the model I demonstrated with.** A 1.5B model on CPU invents
   subcommands. The plumbing is proven, the plans are not production-grade. Two env vars switch to
   `gpt-4o-mini`.
2. **Automated test coverage is thin.** I verified end to end by running the real stack and
   inspecting the database, and I used a throwaway probe test for the guardrails. The first thing I
   would add is a proper suite — the interfaces make it easy, which is precisely why not having
   done it is a gap and not an excuse.
3. **The correlation window is per-process.** Correct on one replica, partial across several. The
   Redis-backed version is the fix.
4. **The reranker is not a true cross-encoder.** Same two-stage architecture, a general model
   standing in.
5. **ELK is not implemented**, though the sketch mentioned it. One more `ContextProvider`.
6. **Bus events are dropped when the buffer is full.** Deliberate — I will not stall ingestion for
   an audit event — but it means the "every incident is stored" guarantee has a theoretical hole. A
   persistent queue is the real answer.
7. **Confidence weights are hand-tuned.** Explainable, but not calibrated. Once there are enough
   approve/reject decisions they become labelled training data, and `ports.Scorer` means a learned
   scorer is one more driver.
8. **Feedback returns success after a failed database write** because the decision is durable in
   the DLQ. Defensible, but it is a lie of omission to the caller and I would rather return 202
   with a warning.

## 10. "What would you do next?"

In priority order, which is itself a signal:

1. **Tests.** Table-driven tests per stage with fake adapters, plus a golden-file test for the
   prompt builder.
2. **Metrics.** Wire the `Observer` hook to Prometheus; alert on DLQ growth and the
   approve/reject ratio.
3. **DLQ replay.** `Triage.Process` is already exported for exactly this; it is a small CLI.
4. **Redis-backed correlation**, to close the multi-replica gap.
5. **A real cross-encoder reranker** — `bge-reranker` behind the existing interface.
6. **Learned confidence**, trained on accumulated approve/reject decisions.
7. **Sandboxed dry-run.** Not execution — running the *read-only* steps automatically and
   attaching their output to the email, so the engineer opens it with the diagnosis already done.
   That is the next real product step, and it stays inside the "never mutate without a human"
   boundary.

## 11. Traps and how to handle them

**"Why not use LangChain / an agent framework?"**
I needed four things: an HTTP call to a chat endpoint, JSON extraction, a retry loop and a vector
query. That is a few hundred lines of Go I fully understand, versus a large dependency whose
control flow I do not. The pieces I would have got for free — prompt templating, chaining,
retrieval — are the pieces where I wanted precise control, especially around guardrail ordering.

**"This is just a wrapper around an LLM."**
The model is maybe 15% of the value. Grouping, deduplication, evidence gathering, guardrails and
the feedback loop are the other 85%, and they are all deterministic Go. Take the model out and you
still have a system that turns five pages into one enriched incident with the relevant logs
attached. Take the pipeline out and you have a chatbot that hallucinates `kubectl` commands.

**"Would you run this in production tomorrow?"**
Not as-is, and for specific reasons: I would want a proper test suite, metrics wired to the
observer hook, Redis-backed correlation for multi-replica, and a period running in shadow mode
comparing its diagnoses against what engineers actually did. The architecture is production-shaped
— static binary, env config, health probes, graceful shutdown, DLQ — but "production-shaped" and
"production-proven" are different words.

**"What was the hardest design decision?"**
Whether to execute commands. Autonomous remediation is the more impressive demo and the wrong
product. A model that is wrong 5% of the time, at 100% autonomy, causes a second outage in 5% of
incidents. So the system proposes and a human decides, and the human's decision becomes the
training signal that would eventually justify more autonomy. I would rather build the trust ladder
than skip it.

## 12. Final checklist for the day

- [ ] Can you say the 30-second pitch without notes?
- [ ] Can you draw the four boxes and name the stages in order?
- [ ] Do you know the five numbers: 150→3, 5→1, 62s/61s, 0.80, 0 dead letters?
- [ ] Can you explain embeddings and cosine similarity to a non-specialist?
- [ ] Can you tell the deadlock story in 90 seconds?
- [ ] Can you name three weaknesses without being prompted?
- [ ] Can you explain why nothing executes?
- [ ] Have you run `docker compose up --build` recently enough to be sure it works?
- [ ] Can you show `GET /healthz`, the Mailpit inbox, and one SQL query against
      `raw_anomalies`?

If you have a laptop with you, have the stack running before you walk in. A working demo with a
real email in Mailpit beats any amount of description.

---

**End of the book.** Parts 1–2 for orientation, 3–11 for the technologies, 12–15 for the
architecture, 16–23 for the code, 24 for configuration, and this part for the interview. Go build
something else next.
