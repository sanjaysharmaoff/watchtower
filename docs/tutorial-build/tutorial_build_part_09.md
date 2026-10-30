# Part 9 — Prompts and the Offline Cognitive Core (Commits 41–45)

**Goal:** the system produces an actual remediation plan. No API key, no network — a rule-based
planner and critic behind the same interfaces the real models will use in Part 15.

---

## Commit 41 — `feat(prompt): the SRE triage prompt`

**Files:** `internal/adapters/cognition/prompt/builder.go`

**Build:**

```go
const PlanSchema = `{
  "root_cause": "string, one or two sentences, name the failing component",
  "impact": "string, what users or systems are affected right now",
  "steps": [
    {"run": "single shell command", "explain": "why this command", "risk": "read|mutate|dangerous"}
  ],
  "verification": ["command or observation that confirms the fix worked"],
  "rollback": ["command or observation that undoes the change"],
  "confidence": 0.0
}`
```

`const systemPrompt` containing, as explicit rules:
1. diagnose from the evidence; **never invent** log lines, metrics or resource names not in the
   context
2. prefer read-only investigation first, then the smallest reversible change
3. never propose destructive operations — name them: recursive deletes, namespace or volume
   deletion, `--force`, infrastructure teardown, data-destroying SQL
4. one command per step; no `<placeholder>` unless the context genuinely lacks the value
5. **if the evidence is insufficient, say so in `root_cause` and propose diagnostics only**
6. reuse a matching historical runbook, adapting resource names
7. reply with a single JSON object matching the schema; no prose, no fences

```go
type SRE struct { MaxLogs, MaxEvents int }
func (b SRE) Build(inc *domain.Incident) (domain.Prompt, error)
```
Input: the incident. Output: `domain.Prompt{System, User, Meta}`. Error when there are no alerts.
The user message, in this order:
- `# Incident <id>` then `severity`, `target`, `services`, `detected`
- `## Firing alerts` — one bullet per alert with severity, name, target, summary, detail, labels
- `## Orchestrator events` — the last `MaxEvents`, one bullet each
- `## Log evidence (deduplicated and filtered)` — the last `MaxLogs` inside a fenced block
- `## Verified historical runbooks (highest similarity first)` — each with title, similarity,
  cause and `$ command` lines. **When there are none, say so explicitly:**
  `"None matched. Treat this as a new failure mode."`
- `## Required output` followed by `PlanSchema`

`Meta` carries `incident_id`, `signature`, `severity`.

Helpers: `trimLogs`, `trimEvents`, `oneLine(s string, max int)` (collapse newlines, truncate),
`joinOr(in []string, fallback string)`, and `labelLine(m map[string]string)` which **sorts the keys**:

```go
sort.Strings(keys) // deterministic prompts keep the embedding cache useful
```

Register `"sre"` in `wire.PromptBuilders` with `MaxLogs: cfg.Clean.MaxLines, MaxEvents: 15`.

**Verify:** build, then in a scratch main build a prompt from a decoded fixture and print it. Read
it as if you were the model — is everything it needs present, and is anything ambiguous?

**Why:** six prompt-engineering techniques are in that system prompt and you should be able to name
them: role assignment, grounding constraint, ordering preference, explicit prohibitions, an escape
hatch, and an output contract. The escape hatch matters most — permission to say "insufficient
evidence" removes the pressure to invent, which is a major hallucination trigger.

`PlanSchema` lives in the **same file** as the prompt so the instruction and the parser cannot drift
apart.

Sorting label keys is not cosmetic: Go map iteration is randomised, so unsorted labels would produce
a different prompt string every run, defeating the embedding cache and making behaviour
irreproducible.

Stating "None matched" explicitly is important — a silence invites the model to fill it.

---

## Commit 42 — `feat(prompt): the digest prompt`

**Files:** `internal/adapters/cognition/prompt/builder.go` (extend)

**Build:**

```go
func Digest(d domain.Digest) domain.Prompt
```
Input: the aggregated digest. Output: a prompt asking for an executive summary. Body:
- the window, then `totals: N reviewed, N approved, N edited, N rejected`
- one bullet per entry: severity, title, services, decision, and the root cause
- when there are no entries, say so
- then the instruction: a Markdown summary with (1) one paragraph on overall reliability,
  (2) recurring themes most frequent first, naming services, (3) at most three concrete follow-up
  actions. Under 300 words, using only the data above.

System message: *"You are an SRE lead writing a factual daily reliability report. No filler, no
speculation beyond the data."*

**Verify:** build.

**Why:** all model-facing wording lives in one package, so prompt iteration never touches pipeline
code. Note this returns a plain `domain.Prompt` rather than going through `PromptBuilder` — the
digest has a different input type, and forcing it into the same interface would mean an interface
with two incompatible contracts.

---

## Commit 43 — `feat(stub): rule-based planner`

**Files:** `internal/adapters/cognition/llm/stub/stub.go`

**Build:**

```go
type Planner struct{}
func (Planner) Name() string                                              // "stub:rules"
func (Planner) Plan(_ context.Context, p domain.Prompt) (domain.Plan, error)
```

Input: the prompt (as text). Output: a plausible diagnostic plan. Implementation:
- `var reTarget = regexp.MustCompile(`(?m)^target:\s*(.+)$`)` and
  `func targetFrom(user string) (namespace, service string)` — parse the `target:` line the prompt
  builder wrote, splitting on `/`, defaulting to `default`/`app`
- `plan.RootCause = "Rule-based triage (no model configured): " + guessCause(text)` — **label
  itself honestly**
- always start with two read-only steps: `kubectl -n <ns> get pods -l app=<svc> -o wide` and
  `kubectl -n <ns> logs -l app=<svc> --tail=200 --since=15m`
- then a keyword switch adding two more steps per case: oom/memory, crashloop/backoff,
  disk/space/inode, connection-refused/timeout, certificate/x509/tls, latency/p99/saturation
- `plan.Verification` — two entries, including "the originating alert should resolve within one
  evaluation interval"
- `plan.Rollback` — "No state was changed by these steps, so there is nothing to roll back."
- `plan.Confidence = 0.35`

Plus `func guessCause(text string) string` and `func containsAny(haystack string, needles ...string) bool`.

Register `"stub"` in `wire.Planners`.

**Verify:** build, then in a scratch main feed it the prompt from commit 41 and print the plan.
The namespace and service in the commands must match your fixture (`checkout`/`checkout-api`).

**Why:** this is what makes the project runnable with no API key and deterministic in tests — and
it is a real component, not a mock: the keyword rules produce genuinely useful diagnostic commands.

**It labels itself in every plan it produces**, so its output can never be mistaken for a model's.
That honesty is deliberate and worth keeping.

Note that it works entirely from the prompt *text*, which is why it has to parse `target:` back out.
That is the price of `Planner` taking a `domain.Prompt` rather than the incident — and it is the
right price, because it means the interface does not leak the envelope into every model provider.

---

## Commit 44 — `feat(stub): rule-based critic and narrator`

**Files:** `internal/adapters/cognition/llm/stub/stub.go` (extend)

**Build:**

```go
type Critic struct{}
func (Critic) Name() string                                               // "stub:rules"
func (Critic) Review(_ context.Context, _ domain.Prompt, plan domain.Plan) (domain.Verdict, error)
```
Input: a draft plan. Output: a verdict. Reject with a stated issue when:
- there are no steps
- `RootCause` is empty
- any step still contains a `<placeholder>` — check for both `<` and `>`
- any step is self-declared `RiskDangerous`
- the plan mutates state (any `RiskMutate` step) but has **no** verification

`Approved = len(issues) == 0`, `Notes = "reviewed by the offline rule-based critic"`.

```go
type Narrator struct{}
func (Narrator) Narrate(_ context.Context, p domain.Prompt) (string, error)
```
Output: a Markdown block saying it was generated without a model, then the `totals:` and `- [` lines
echoed from the prompt inside a fence.

Register `"stub"` in `wire.Critics` and `wire.Narrators`.

**Verify:** build; scratch-test that a plan with `kubectl delete pod <pod-name>` is rejected for the
placeholder, and that a mutating plan with no verification is rejected.

**Why:** the stub critic is genuinely useful rather than a stub in spirit. Those five rules catch
real model failure modes, and they run for free. When you plug in a real critic in Part 15 you can
compare the two and see which catches more.

---

## Commit 45 — `feat(stages): prompt and the planner/critic loop`

**Files:** `internal/stages/cognition.go`, `internal/bootstrap/app.go` (edit),
`internal/bootstrap/plugins.go` (edit)

**Build:**

```go
func BuildPrompt(b ports.PromptBuilder) Stage
```
Call `b.Build(inc)`; **return the error** (`fmt.Errorf("build prompt: %w", err)`) rather than
swallowing it — no prompt means no plan, so there is nothing to degrade to. Assign `inc.Prompt`.

```go
type PlanOptions struct { MaxAttempts int; CriticOn bool }
func Plan(planner ports.Planner, critic ports.Critic, opts PlanOptions) Stage
```
Loop `attempt := 1..MaxAttempts`, setting `inc.Attempts = attempt`:
1. `planner.Plan(ctx, prompt)` — on error, note it, remember it as `lastErr`, `continue`
2. set `plan.Model = planner.Name()`, `inc.Plan = &plan`, clear `lastErr`
3. when the critic is off or nil → return nil
4. `critic.Review(ctx, prompt, plan)` — **on error, note it and return nil**; a broken reviewer must
   not block delivery
5. set `verdict.Model`, `inc.Verdict = &verdict`
6. approved → note it and return nil
7. **rejected with no issues and no notes** → synthesise
   `"reviewer rejected the plan without giving a reason, so it was not rewritten"`, note
   `"critic rejected draft %d without reasons, keeping draft"`, and return nil
8. rejected with reasons → note them; if this was the last attempt return nil; otherwise
   `prompt = withCritique(inc.Prompt, plan, verdict)` and loop

After the loop, when `lastErr != nil`, return
`fmt.Errorf("planner exhausted %d attempt(s): %w", opts.MaxAttempts, lastErr)`.

```go
func withCritique(base domain.Prompt, draft domain.Plan, v domain.Verdict) domain.Prompt
```
Build on **`base.User`** (the original, not the previously modified prompt) and append
`## Previous attempt (rejected)` with the root cause and steps, `## Reviewer objections` with the
issues and notes, then *"Produce a corrected plan that resolves every objection. Do not repeat the
rejected commands verbatim unless they were not the subject of an objection."* Copy `Meta` and add
`retry: "true"`.

Wire it up: build `promptBuilder` (`"sre"`), `planner` (`cfg.LLM.Provider`), and `critic` only when
`cfg.LLM.CriticOn`; append `stages.BuildPrompt(promptBuilder)` and
`stages.Plan(planner, critic, stages.PlanOptions{MaxAttempts: cfg.LLM.MaxAttempts, CriticOn: cfg.LLM.CriticOn && critic != nil})`.
Add the blank imports for `cognition/prompt` and `cognition/llm/stub`. Update the temporary output
`Tap` to print `inc.Plan.RootCause` and the steps.

**Verify:**

```bash
go build -o bin/watchtowerd ./cmd/watchtowerd && ./bin/watchtowerd
curl -X POST localhost:8080/alerts -H "Content-Type: application/json" \
  --data-binary @examples/alerts/alertmanager-oom.json
```
You should now see a root cause and four `kubectl` commands printed. The pipeline line should read
`... -> publish:incident.sanitized -> prompt -> plan`.

**Why:** the loop lives in a **stage**, not inside a model adapter. That is why any planner can be
paired with any critic — including a real planner with this offline critic — and why the retry policy
is visible in the composition root instead of buried in a vendor package.

Walk the exit paths again and notice that only one of them fails the stage: every attempt erroring.
Everything else keeps the draft and records the doubt, because a mediocre plan with a recorded
objection is more useful to a human than no email.

Step 7 is a refinement that came from running a weak model: it returned `approved: false` with no
issues at all, so retrying gave the planner nothing to correct — pure token spend. Keep the draft,
flag it, let the confidence score carry the doubt.

---

## End of Part 9

The system produces plans, reviews them, and rewrites them when the review has substance.

```bash
go build ./... && go vet ./... && gofmt -l .
```

Next: **Part 10** — the output guardrail and confidence scoring, so a plan can be trusted enough to
send.
