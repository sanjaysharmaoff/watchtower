# Part 1 — How to Use This Book, and the 60-Second Pitch

## Who this book is for

You. Someone who knows basic Go — functions, structs, interfaces, slices, maps,
goroutines maybe — and who has never touched Prometheus, Loki, Kubernetes, pgvector,
Docker Compose, embeddings or RAG. By the end you will be able to sit in an interview,
draw this system on a whiteboard, defend every design decision in it, and answer
"what would you do differently" without flinching.

Nothing here assumes prior operations knowledge. Every technology gets introduced from
zero, in the order it appears in the system.

## What this book contains

| Parts | What you get |
| --- | --- |
| 1–2 | Orientation: the pitch, and the real-world problem the project solves |
| 3–11 | Technology tour. Every external technology explained from zero, with the exact way this project uses it |
| 12–15 | Architecture patterns: ports and adapters, the pipeline, the event bus, the plugin registry |
| 16–23 | Code walkthrough, package by package, ending with a line-by-line trace of one real incident |
| 24 | Configuration bible: every environment variable, where to change it, and the plug-and-play swap recipes |
| 25 | Interview playbook: the pitch script, ~40 likely questions with answers, and the honest weaknesses |

Read 1, 2, then 12, then 25 if you have one evening. Read everything if you have a
weekend. Parts 3–11 are reference material — you can dip into whichever technology the
interviewer asks about.

## The 60-second pitch

Learn this. It is the thing you say when someone asks "so what did you build?"

> WatchTower is an AI-assisted alert triage service for SRE teams, written in Go.
>
> When a production alert fires, Prometheus sends it to WatchTower. WatchTower groups
> it with related alerts, pulls the logs and Kubernetes events from around the failure,
> strips the noise and redacts any secrets, searches a database of runbooks that fixed
> similar problems before, asks a language model for a remediation plan, has a second
> model attack that plan, blocks any dangerous command it proposes, scores its own
> confidence, and emails the on-call engineer a runbook with Approve, Edit and Reject
> buttons.
>
> Whatever the engineer approves gets embedded as a vector and becomes retrieval
> material, so the system gets better at the failures your organisation actually has.
>
> Architecturally it is ports and adapters. Around 30 small Go interfaces, every one
> with at least two implementations. The whole thing runs on a laptop with zero
> infrastructure using in-memory storage and a rule-based planner, or against
> Prometheus, Loki, Kubernetes, Postgres with pgvector and real models — and the
> difference is environment variables, not code.

## The one-sentence version

If you only get one sentence: **it turns a wall of alerts into one reviewed runbook
per incident, and learns from the engineer's decision.**

## The three numbers worth memorising

These make the pitch concrete. All three were measured on the real stack.

1. **150 log lines in, 3 out.** The sanitizer deduplicates by normalized message
   (timestamps, UUIDs, hex ids and numbers erased before comparison), so a thousand
   copies of the same timeout become one line. That is the difference between an
   affordable prompt and an unaffordable one.
2. **5 alerts → 1 incident → 1 email.** Correlation groups by topology labels inside a
   time window, so a cascade is one page, not five.
3. **Confidence 0.80 after a self-correction round.** The critic rejected the first
   draft with stated reasons, the planner rewrote it, the critic approved the rewrite.

## What makes it interesting to an interviewer

Not the AI part. Every candidate has an AI project. These are the things that make
senior engineers lean forward:

- **Cost control before the model call.** Debounce, correlate, deduplicate, filter,
  compress, cap — the expensive resource is protected by five cheap stages.
- **The model is never trusted.** Its JSON is coerced, its risk labels are recomputed,
  its commands are linted, and a second model reviews its work.
- **Nothing executes.** WatchTower proposes; a human decides. That is a deliberate
  product boundary, not a missing feature.
- **Nothing is lost.** Every failure path — bad database write, saturated queue, failed
  email, pipeline panic — ends in a dead letter queue with the full payload.
- **Loose coupling that actually pays off.** During a folder restructure that moved 20
  packages, only 6 Go files needed an import change, because adapters import only
  `domain`, `ports` and `wire`.
- **Four real bugs found by running it, not by reading it.** Part 25 turns this into
  your best interview story.

## How to read the code alongside this book

The repository is laid out so that the folder names tell you the architecture:

```
cmd/watchtowerd        the entrypoint
internal/domain        pure types, no dependencies at all
internal/ports         the ~30 interfaces (the "seams")
internal/pipeline      the generic stage runner
internal/stages        the pipeline steps
internal/bus           in-process publish/subscribe
internal/projectors    bus subscribers (persist, deliver, audit)
internal/services      use cases (triage, feedback, digest, maintenance)
internal/adapters      every concrete implementation, grouped by port
internal/wire          driver registries + shared build environment
internal/bootstrap     the composition root — the only place that knows the order
internal/config        environment variables to struct
internal/platform      logging, clock, signing, retry, redis client
```

If you get lost, the rule is: **`domain` and `ports` know nothing about anybody;
everybody knows about them; `bootstrap` knows about everybody.**

## A note on honesty

The book tells you what has been verified by running it and what has not. Interviewers
respect "I proved X, I did not prove Y" far more than blanket confidence, and one
follow-up question can expose the difference. Part 25 gives you the exact wording.

---

Next: **Part 2 — The Problem: Why Alert Triage Is Painful**, which is the story you
need before any of the technology makes sense.
