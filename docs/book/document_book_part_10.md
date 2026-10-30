# Part 10 — Language Models: Planner, Critic, and Keeping Them on a Leash

You do not need to know how a transformer works. You need to know how to *use* a model as
an unreliable component in a reliable system. That is the interview-relevant skill, and it
is what this project demonstrates.

## The minimum vocabulary

**LLM** — a model that predicts the next chunk of text. Everything else is built on that
one behaviour.

**Token** — the unit of text a model processes: roughly ¾ of a word. "kubectl" might be 3
tokens. You are billed per token, input and output, and there is a maximum context window.

**Prompt** — the text you send. Two conventional parts:
- **system**: standing instructions and role ("You are a senior SRE. Never propose
  destructive commands.")
- **user**: the specific request (this incident's data)

**Temperature** — randomness, 0 to ~2. Low means more deterministic. This project uses
`0.1`, because you want the same incident to produce a similar plan, not creative variety.

**max_tokens** — cap on the response length. Also a cost cap.

**Hallucination** — confident, fluent, wrong. The central engineering problem.

## The API shape

Nearly every provider now speaks the OpenAI-compatible HTTP API:

```
POST /v1/chat/completions
{ "model": "gpt-4o-mini",
  "messages": [ {"role":"system","content":"..."}, {"role":"user","content":"..."} ],
  "temperature": 0.1,
  "max_tokens": 800,
  "response_format": {"type": "json_object"} }
```

Because it is a de-facto standard, **one adapter serves many providers**. This project has a
single `openai` adapter written with `net/http` and no vendor SDK, and it drives OpenAI,
Azure OpenAI, vLLM, llama.cpp and Ollama — the only difference is `WT_LLM_BASE_URL`.

That is a design decision to state proudly: no SDK means no large dependency tree, no
waiting for a vendor to release a Go update, and total control over retries and error
classification.

## Ollama: running models locally

**Ollama** runs open-weight models on your own machine and exposes both its own API and an
OpenAI-compatible one at `/v1`. That is why this project needs no API key to demonstrate
real inference.

```yaml
WT_LLM_PROVIDER: openai
WT_LLM_BASE_URL: http://ollama:11434/v1
WT_OPENAI_API_KEY: ollama-local        # ignored by Ollama, required by the adapter
WT_PLANNER_MODEL: qwen2.5:1.5b
WT_EMBED_MODEL: nomic-embed-text
```

- `qwen2.5:1.5b` — 1.5 billion parameters, ~1GB. Small enough to run on a CPU.
- `nomic-embed-text` — an embedding model, ~274MB, 768 dimensions.

Cost of honesty: a 1.5B model on CPU took **60–90 seconds per plan** and produced
mediocre plans — it invented a `kubectl resize` subcommand that does not exist, and
labelled `kubectl delete pod` as read-only. Switching to `gpt-4o-mini` is two environment
variables. The point of the local model is to prove the plumbing, not the plans.

## JSON mode and why it is not enough

`response_format: {"type": "json_object"}` asks the server to guarantee syntactically valid
JSON. Very useful — and not universally supported, and not a guarantee of the *shape* you
asked for. This project handles both problems.

**Problem 1: the server may reject JSON mode.** Handled by degrading once:

```go
func (c *Client) Chat(ctx context.Context, opts ChatOptions) (string, error) {
    out, err := c.chat(ctx, opts)
    if err != nil && opts.JSONOnly && mentionsResponseFormat(err) {
        c.log.Warn("server rejected JSON mode, retrying without it", "model", opts.Model)
        opts.JSONOnly = false
        return c.chat(ctx, opts)
    }
    return out, err
}
```

**Problem 2: models wrap JSON in prose or markdown fences.** Handled by extraction:

```go
func extractJSON(raw string) string {
    s := strings.TrimSpace(raw)
    if fence := strings.Index(s, "```"); fence >= 0 { ... }   // strip code fences
    start := strings.IndexByte(s, '{')
    end   := strings.LastIndexByte(s, '}')
    if start < 0 || end <= start { return "" }
    return strings.TrimSpace(s[start : end+1])
}
```

**Problem 3 — the one found by running it.** The model returned `issues` as an *object*
instead of an array of strings, a strict `json.Unmarshal` failed, and the entire critique
was thrown away:

```
critic unavailable on attempt 1, accepting draft: decode verdict JSON:
cannot unmarshal object into Go struct field .issues of type string
```

Fixed by decoding into `json.RawMessage` and coercing whatever arrived:

```go
func flexStrings(raw json.RawMessage) []string {
    if len(raw) == 0 { return nil }
    var asStrings []string
    if err := json.Unmarshal(raw, &asStrings); err == nil { return compact(asStrings) }
    var asString string
    if err := json.Unmarshal(raw, &asString); err == nil { return compact(strings.Split(asString, "\n")) }
    var asAny any
    if err := json.Unmarshal(raw, &asAny); err != nil { return nil }
    return compact(flatten(asAny))   // handles arrays of objects and keyed objects
}
```

The principle, which is the real lesson: **model output is untrusted input from a foreign
system.** You parse it defensively, exactly like a third-party webhook. Strict typing at
that boundary is a liability, not a virtue.

## The prompt

`internal/adapters/cognition/prompt/builder.go` holds all model-facing wording in one
place. The system prompt:

```
You are a senior site reliability engineer triaging a production incident.

Rules you must follow:
- Diagnose from the evidence provided. Never invent log lines, metrics or resource names
  that are not in the context.
- Prefer read-only investigation commands first, then the smallest reversible change that
  addresses the cause.
- Never propose destructive operations: no recursive deletes, no namespace or volume
  deletion, no --force, no infrastructure teardown, no data-destroying SQL.
- One command per step. No placeholders like <pod-name> unless the context genuinely does
  not contain the value.
- If the evidence is insufficient, say so in root_cause and propose diagnostic steps only.
- Reuse a verified historical runbook when one matches, adapting resource names to this
  incident.
- Reply with a single JSON object matching the schema. No prose, no markdown fences.
```

Six prompt-engineering techniques are in there, and you should be able to name them:

1. **Role assignment** — "senior SRE" shifts vocabulary and priorities.
2. **Grounding constraint** — "never invent … not in the context" is the anti-hallucination
   instruction.
3. **Ordering preference** — diagnose before mutating.
4. **Explicit prohibitions** — named destructive operations, not vague "be careful".
5. **Escape hatch** — permission to say "insufficient evidence" removes the pressure to
   invent an answer, which is a major hallucination trigger.
6. **Output contract** — a schema, stated literally:

```go
const PlanSchema = `{
  "root_cause": "string, one or two sentences, name the failing component",
  "impact": "string, what users or systems are affected right now",
  "steps": [ {"run": "single shell command", "explain": "why this command", "risk": "read|mutate|dangerous"} ],
  "verification": ["command or observation that confirms the fix worked"],
  "rollback": ["command or observation that undoes the change"],
  "confidence": 0.0
}`
```

The schema constant lives in the same file as the prompt, deliberately, so the instruction
and the parser cannot drift apart.

The user prompt is assembled from the incident in ordered sections: header (id, severity,
target, services), firing alerts, orchestrator events, log evidence in a fenced block,
retrieved runbooks with similarity scores, then the required output shape. When nothing was
retrieved it says so explicitly — *"None matched. Treat this as a new failure mode."* —
because silence invites invention.

One small but real detail: label lines are **sorted** before being rendered.

```go
sort.Strings(keys) // deterministic prompts keep the embedding cache useful
```

Go map iteration is randomised, so unsorted labels would produce a different prompt string
every run, which would defeat caching and make behaviour non-reproducible.

## The planner/critic loop

Two model roles, one interface each:

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

The critic gets its own adversarial system prompt:

```
You review remediation plans written by another engineer before they reach a production
on-call SRE.

Reject the plan if any of these hold:
- The root cause is not supported by the evidence in the incident context.
- A command references a resource that does not appear in the context.
- A step is destructive, irreversible, or broader in scope than the incident.
- The plan changes state before establishing what is wrong.
- Verification is missing for a step that changes state.
```

**Why a second model call helps at all.** A model asked to *produce* optimises for
plausibility. The same model asked to *check* against explicit criteria is doing a
different, easier task, and it is not defending its own work — it does not know it wrote
the draft. This is the "LLM-as-judge" / self-refine pattern. It is not magic; it catches a
useful fraction of obvious errors and it demonstrably worked here.

The loop lives in `internal/stages/cognition.go`, **not** inside a model adapter. That is
why any planner can be paired with any critic, including a real planner with the offline
rule-based critic. The rewrite prompt includes the rejected draft and the objections:

```
## Previous attempt (rejected)
root_cause: ...
- kubectl ...

## Reviewer objections
- A command references a resource that does not appear in the context

Produce a corrected plan that resolves every objection.
```

Verified run:

```
critic rejected draft 1: A command references a resource that does not appear in the context
critic approved draft on attempt 2
confidence 0.80
```

### The refinement found by running it

A weak model sometimes returns `approved: false` with **no issues at all**. Retrying then
gives the planner nothing to correct — pure token spend. So:

```go
if len(verdict.Issues) == 0 && strings.TrimSpace(verdict.Notes) == "" {
    verdict.Issues = []string{"reviewer rejected the plan without giving a reason, so it was not rewritten"}
    inc.Verdict = &verdict
    inc.Note("critic rejected draft %d without reasons, keeping draft", attempt)
    return nil
}
```

Keep the draft, record the doubt, let the confidence score carry it to the human. This is a
great small answer to "how do you handle unreliable model behaviour?"

## Cost control, in order of leverage

Everything expensive happens last, and five cheap stages run first:

1. **Debounce** — a duplicate signature inside the window costs zero tokens.
2. **Correlate** — five alerts become one incident, so one plan instead of five.
3. **Admit** — resolved groups and low severities never reach the model.
4. **Sanitize** — 150 log lines become 3.
5. **Retrieve top 2, not top 10** — bounded prompt size.
6. **Cache embeddings** by content hash and model name.
7. **`max_tokens`** caps the response.
8. **Heuristic reranker by default**, LLM reranker opt-in — one model call saved per
   incident.

Per incident with the critic on and one retry: 2 planner calls, 2 critic calls, 1 embedding.
With `gpt-4o-mini` that is fractions of a cent. The alternative — no debounce, no
correlation, raw logs — could be 100× that.

## The offline stub

`internal/adapters/cognition/llm/stub` implements planner, critic and narrator with
keyword rules, so the whole system runs with no key, no network and no cost:

```go
case containsAny(text, "oomkilled", "out of memory", "memory limit"):
    plan.Steps = append(plan.Steps,
        domain.Command{Run: fmt.Sprintf("kubectl -n %s describe pod -l app=%s | grep -A5 -i 'last state'", ns, svc), ...},
        domain.Command{Run: fmt.Sprintf("kubectl -n %s top pod -l app=%s", ns, svc), ...})
```

It labels itself honestly in every plan it produces — *"Rule-based triage (no model
configured)"* — so its output can never be mistaken for a model's.

The stub critic is genuinely useful, not a stub in spirit: it rejects placeholders (`<...>`
still in a command), self-declared dangerous steps, and state-changing plans with no
verification.

## Interview answers to have ready

**"How do you stop the model hallucinating?"**
Four layers. RAG grounds it in verified runbooks. The prompt forbids inventing resources
and explicitly permits saying "insufficient evidence". A second model checks the draft
against stated criteria. Then deterministic code takes over: risk labels are recomputed
from the command text, every command is linted against a destructive-pattern catalogue, and
nothing executes without a human. I assume the model will be wrong and design for it.

**"What if the model returns garbage?"**
Fenced or prose-wrapped JSON is extracted, shape variance is coerced, and if the plan still
has neither a root cause nor steps the attempt fails and is retried up to
`WT_LLM_MAX_ATTEMPTS`. If every attempt fails, the incident goes to the dead letter queue
with its payload — the evidence was already persisted before the model was called.

**"Why not fine-tune?"**
Retrieval is cheaper, and updates are instant: an engineer approves a runbook and the very
next incident can use it. Fine-tuning needs a training run per update and cannot cite its
source. Retrieval also lets me show the reviewer *which* runbook influenced the plan, which
matters for trust.

**"Why is the loop in a stage rather than in the adapter?"**
So planner and critic are independently swappable and can be different providers. It also
means the retry policy is visible in the composition root rather than buried in a vendor
adapter, and it is testable without a network.

---

Next: **Part 11 — Redis, SMTP and cron**, the last three technologies.
