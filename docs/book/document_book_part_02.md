# Part 2 — The Problem: Why Alert Triage Is Painful

Before any technology, you need the story. Interviewers grade problem understanding
harder than they grade code.

## What SRE means

SRE stands for Site Reliability Engineering. An SRE keeps production systems running:
they build monitoring, respond when things break, and reduce the chance of it breaking
again. The person currently responsible for responding is **on-call**. They carry a
pager (these days, a phone) and are expected to react within minutes.

## What an alert is

Software emits **metrics**: numbers describing itself. Memory used. Requests per
second. Error count. A monitoring system samples those numbers continuously and
evaluates **rules** over them, like:

> if a container's memory usage stays above 95% of its limit for 10 minutes, something
> is wrong

When a rule's condition holds, it **fires** and becomes an **alert**. The alert
travels to whoever is on call.

## The failure mode this project attacks

A mature company has hundreds or thousands of alert rules. The original design sketch
for this project says "Evaluates 900+ Rules". At that scale four things go wrong.

**1. One incident produces many alerts.** A database gets slow. The service that reads
it starts timing out. The service in front of that starts returning errors. Its memory
climbs because requests queue up. Now five rules fire. The on-call engineer gets five
pages for one problem, at 3am, and has to work out that they are one problem.

**2. The same alert fires over and over.** A flapping service can re-trigger the same
rule every evaluation interval. Twenty pages, one cause.

**3. Alerts say what, never why.** An alert is a sentence: "checkout-api memory usage
at 98% of limit." It does not include the logs, the recent deployments, or the
Kubernetes events that explain it. The engineer must now open four browser tabs,
remember the right query syntax, and assemble context by hand while under pressure.

**4. The knowledge never accumulates.** The engineer eventually finds it: the memory
limit was set too low after last week's release. They fix it, close the incident, go
back to bed. Three weeks later it happens to a different service and a different
engineer starts from zero, because the fix lived in one person's memory or in a Slack
thread nobody can find.

The industry term for the human cost of this is **alert fatigue**: when engineers
receive so many low-value alerts that they start ignoring them, which is exactly when
the important one arrives.

## What WatchTower does about each

| Problem | WatchTower's answer | Where in the code |
| --- | --- | --- |
| Many alerts, one incident | Correlate by shared topology labels within a time window | `adapters/ingest/correlate` |
| Same alert repeatedly | Debounce by incident signature for a configurable window | `stages/admit.go` + `adapters/ingest/gate` |
| No context | Fetch logs and cluster events automatically, in parallel | `stages/enrich.go` + `adapters/enrich/*` |
| Too much context | Deduplicate, filter to signal, compress, cap | `adapters/enrich/sanitize` |
| No diagnosis | Ask a language model for a structured plan, with a second model reviewing it | `stages/cognition.go` + `adapters/cognition/llm` |
| Knowledge does not accumulate | Approved runbooks are embedded and retrieved for future similar incidents | `services/feedback.go` + `adapters/rag` |
| Models say dangerous things | Recompute risk, lint every command, block destructive ones | `adapters/cognition/guard` |

That table **is** the architecture. If you can reproduce it from memory you can explain
the whole project.

## The critical product decision: it never executes anything

WatchTower could run the commands it proposes. It deliberately does not. There is no
executor anywhere in the codebase.

Reasons, in the order an interviewer will care about them:

1. **A language model that is wrong 5% of the time is unacceptable at 100% autonomy.**
   Five percent of remediation attempts causing a second outage is worse than the
   original problem.
2. **Trust has to be earned before it is spent.** The system first has to prove its
   diagnoses are good. The approval loop is how it accumulates that evidence.
3. **The human review is the training signal.** Approve, Edit and Reject are not just
   safety, they are the data that makes retrieval better.

The design phrase to use: **human-in-the-loop, with the loop closing back into
retrieval.**

## What "good" looks like for the on-call engineer

Before: five pages, four browser tabs, twenty minutes of assembling context, a decision
made from memory.

After: one email. Subject line names the service and the severity. Body has the
diagnosis, two to four specific shell commands with reasons, verification steps, the
log lines that justify the conclusion, the Kubernetes events, and the runbook that
worked last time. Three buttons at the bottom. The engineer's job shrinks to the part
that actually needs a human: judgement.

## Why "AI" is the smallest part of the value

Worth internalising, because it is a great thing to say out loud in an interview:

> The language model is maybe 15% of the value. Grouping, deduplication, evidence
> gathering and the feedback loop are the other 85%, and they are all deterministic Go
> code. The model writes the prose and picks the commands. Everything that makes the
> output trustworthy is ordinary engineering.

That sentence tells an interviewer you can tell the difference between a demo and a
system.

## Vocabulary you now own

- **SRE** — the discipline of keeping production reliable
- **on-call** — the person currently responsible for responding
- **metric** — a number a system reports about itself over time
- **alert rule** — a condition over metrics that, when true, fires
- **alert** — a fired rule, delivered to a human
- **incident** — one real-world problem, which may have produced many alerts
- **alert fatigue** — desensitisation caused by too many low-value alerts
- **runbook** — a written procedure for handling a specific failure
- **triage** — deciding what is wrong and what to do about it, quickly
- **human-in-the-loop** — automation that proposes, with a human deciding

---

Next: **Part 3 — Prometheus**, where the alerts come from.
