# Part 15 — Real Models over the OpenAI-Compatible API (Commits 71–75)

**Goal:** one adapter, written with `net/http` and no vendor SDK, that drives OpenAI, Azure OpenAI,
vLLM, llama.cpp and Ollama. You can complete this part with **no API key** by running Ollama
locally.

Optional setup for this part:
```bash
docker run -d -p 11434:11434 -v ollama:/root/.ollama --name ollama ollama/ollama
docker exec ollama ollama pull qwen2.5:1.5b
docker exec ollama ollama pull nomic-embed-text
```

---

## Commit 71 — `feat(openai): HTTP client with retry and JSON-mode fallback`

**Files:** `internal/adapters/cognition/llm/openai/client.go`

**Build:**

```go
type Client struct {
    http    *http.Client
    baseURL string
    apiKey  string
    timeout time.Duration
    log     ports.Logger
}

func Shared(e *wire.Env) (*Client, error)      // memoized under the key "openai.client"
```
`Shared` errors when the API key is empty, and trims a trailing `/` from the base URL.

Request and response types: `chatMessage{Role, Content}`,
`chatRequest{Model, Messages, Temperature, MaxTokens, ResponseFormat map[string]string}`,
`chatResponse{Choices[]{Message, FinishReason}, Usage{PromptTokens, CompletionTokens, TotalTokens}}`.

```go
type ChatOptions struct {
    Model, System, User string
    Temperature float64
    MaxTokens   int
    JSONOnly    bool
}

func (c *Client) Chat(ctx context.Context, opts ChatOptions) (string, error)
func (c *Client) chat(ctx context.Context, opts ChatOptions) (string, error)
func (c *Client) post(ctx context.Context, path string, payload, into any) error
```

`Chat` — the **degrade-once wrapper**:
```go
// When JSON mode is requested and the server rejects it, the call is retried once
// without it. Not every OpenAI-compatible server implements response_format, and
// the plan parser already tolerates fenced or prefixed JSON, so refusing to
// degrade here would mean refusing to work with half the ecosystem.
out, err := c.chat(ctx, opts)
if err != nil && opts.JSONOnly && mentionsResponseFormat(err) {
    c.log.Warn("server rejected JSON mode, retrying without it", "model", opts.Model)
    opts.JSONOnly = false
    return c.chat(ctx, opts)
}
```
plus `func mentionsResponseFormat(err error) bool` matching `response_format`, `json_object` or
`json mode` in the lowercased message.

`chat` — build the request with system and user messages, set
`ResponseFormat = map[string]string{"type": "json_object"}` when `JSONOnly`, POST to
`/chat/completions`, error on zero choices, log a debug line with the model, total tokens and finish
reason, and return the content.

`post` — marshal, apply `c.timeout` as a context deadline, then wrap everything in
`retry.Do(ctx, retry.Default(), ...)`:
- request construction failure → `retry.Permanent(err)`
- transport error → plain error (**retryable**)
- read the body through `io.LimitReader(resp.Body, 8<<20)`
- `429` or `>= 500` → plain error, `"transient status %d: %s"` (**retryable**)
- `>= 400` → `retry.Permanent(...)` (**not** retryable)
- unmarshal failure → `retry.Permanent(...)`

Plus `func snippet(b []byte) string` truncating to 300 characters.

**Verify:** build. With Ollama running:
```bash
curl -s http://localhost:11434/v1/chat/completions -H "Content-Type: application/json" \
  -d '{"model":"qwen2.5:1.5b","messages":[{"role":"user","content":"say hi"}]}'
```
Confirm the endpoint answers before you rely on it.

**Why:** **error classification** is the reason this is a separate commit. `429` and `5xx` are worth
retrying; `4xx` means you sent something wrong and retrying is pure delay.

`io.LimitReader` bounds the response so a misbehaving or hostile endpoint cannot exhaust memory.

The JSON-mode fallback is what makes one adapter serve the whole ecosystem. Writing it as a wrapper
around a private `chat` keeps the retry-once logic out of the request path.

No vendor SDK: this is roughly 150 lines, has zero dependencies, and you control the retries and the
error semantics.

---

## Commit 72 — `feat(openai): embeddings`

**Files:** `internal/adapters/cognition/llm/openai/client.go` (extend)

**Build:**

```go
type embedRequest  struct { Model, Input string }
type embedResponse struct { Data []struct{ Embedding []float32 } }

func (c *Client) Embeddings(ctx context.Context, model, input string) ([]float32, error)
```
POST to `/embeddings`; error on an empty `Data` or an empty first embedding.

```go
// dimensionsFor covers the models whose width is known, and otherwise trusts the
// configured value, so any OpenAI-compatible server (Ollama, vLLM, a gateway) works
// without a code change.
func dimensionsFor(model string, configured int) int
```
`3-large` → 3072; `3-small` / `ada-002` → 1536; `nomic-embed` → 768; `mxbai-embed` / `bge-m3` →
1024; `all-minilm` → 384; otherwise the configured value, falling back to 1536.

**Verify:** build; scratch-call `Embeddings(ctx, "nomic-embed-text", "hello")` against Ollama and
confirm you get 768 floats.

**Why:** hardcoding a dimension table would break the moment someone points this at a model you have
not heard of. Falling back to the configured `WT_VECTOR_DIMS` means the operator can always tell the
system the truth.

---

## Commit 73 — `feat(openai): planner and plan parsing`

**Files:** `internal/adapters/cognition/llm/openai/cognition.go`

**Build:**

```go
type Planner struct { c *Client; model string; temp float64; maxTokens int }
func (p Planner) Name() string                        // "openai:" + model
func (p Planner) Plan(ctx context.Context, prompt domain.Prompt) (domain.Plan, error)
```
Call `Chat` with `JSONOnly: true`, then `ParsePlan`, then set `plan.Model = p.Name()`.

```go
func ParsePlan(raw string) (domain.Plan, error)
```
Input: whatever the model returned. Output: a `domain.Plan`.
1. `body := extractJSON(raw)`; error when empty
2. unmarshal into a DTO with `root_cause`, `impact`, `confidence`, and
   **`Verification json.RawMessage`** / **`Rollback json.RawMessage`**, plus
   `steps []struct{ Run, Explain, Risk string }`
3. map into the plan, running the raw fields through `flexStrings`
4. skip steps with an empty `run`; map `risk` via `parseRisk`
5. **error when the plan has neither a root cause nor any steps**

```go
func extractJSON(raw string) string
```
Strip a leading ``` fence and its language line, cut at the closing fence, then return the substring
from the first `{` to the last `}`. Empty string when there is no object.

```go
func flexStrings(raw json.RawMessage) []string
func flatten(v any) []string
```
`flexStrings` — try `[]string`, then a bare `string` (split on newlines), then unmarshal into `any`
and `flatten`. `flatten` handles `nil`, `string`, `float64`/`bool`, `[]any` (recursive) and
`map[string]any` (**sorted keys**; a numeric key is an array index in disguise, a real key becomes a
`"key: value"` prefix).

Plus `parseRisk(v string) domain.RiskLevel` and `compact([]string) []string`.

Register `"openai"` in `wire.Planners` using `cfg.LLM.PlannerModel`, `Temperature`, `MaxTokens`.

**Verify:**

```bash
WT_LLM_PROVIDER=openai WT_LLM_BASE_URL=http://localhost:11434/v1 \
WT_OPENAI_API_KEY=ollama-local WT_PLANNER_MODEL=qwen2.5:1.5b \
WT_LLM_TIMEOUT=300s WT_STAGE_TIMEOUT=900s WT_JOB_TIMEOUT=20m \
./bin/watchtowerd
```
Post an alert. It will take 30–90 seconds on CPU. You should get a real plan.

**Why:** `extractJSON` and `flexStrings` exist because **model output is untrusted input from a
foreign system**. Strict typing at that boundary is a liability, not a virtue.

`flexStrings` specifically fixes a defect found on the real stack: the model returned `issues` as an
**object** instead of an array of strings, `json.Unmarshal` failed, and an entire critique was
silently discarded — the critic appeared to be running while contributing nothing. Building the
tolerant version now means you never see that.

The "neither root cause nor steps" check is the floor: below that, the response is not a plan and the
attempt should fail so the loop can retry.

---

## Commit 74 — `feat(openai): critic, narrator and embedder`

**Files:** `internal/adapters/cognition/llm/openai/cognition.go` (extend)

**Build:**

`const criticSystem` — an adversarial reviewer prompt. Reject when:
- the root cause is not supported by the evidence
- a command references a resource not in the context
- a step is destructive, irreversible, or broader in scope than the incident
- the plan changes state before establishing what is wrong
- verification is missing for a step that changes state

Reply with `{"approved": bool, "issues": [...], "notes": "..."}` and nothing else.

```go
type Critic struct { c *Client; model string; maxTokens int }
func (c Critic) Review(ctx context.Context, prompt domain.Prompt, plan domain.Plan) (domain.Verdict, error)
```
Marshal the plan, append it to the prompt under `## Plan under review`, call `Chat` with
`JSONOnly: true`, `extractJSON`, then unmarshal with **`Issues json.RawMessage`** and run it through
`flexStrings`.

```go
type Narrator struct { c *Client; model string; maxTokens int }
func (n Narrator) Narrate(ctx context.Context, p domain.Prompt) (string, error)   // no JSON mode

type Embedder struct { c *Client; model string; dims int }
func (e Embedder) Name() string                                                    // "openai:" + model
func (e Embedder) Dimensions() int
func (e Embedder) Embed(ctx context.Context, text string) ([]float32, error)
```

Register `"openai"` in `wire.Critics`, `wire.Narrators` and `wire.Embedders` — the last using
`dimensionsFor(cfg.LLM.EmbedModel, cfg.RAG.VectorDims)`.

**Verify:**

```bash
WT_LLM_PROVIDER=openai WT_LLM_BASE_URL=http://localhost:11434/v1 \
WT_OPENAI_API_KEY=x WT_PLANNER_MODEL=qwen2.5:1.5b WT_CRITIC_MODEL=qwen2.5:1.5b \
WT_EMBEDDER_DRIVER=openai WT_EMBED_MODEL=nomic-embed-text WT_VECTOR_DIMS=768 \
WT_LLM_TIMEOUT=300s WT_STAGE_TIMEOUT=900s WT_JOB_TIMEOUT=20m WT_LOG_LEVEL=debug \
./bin/watchtowerd
```
Post an alert and watch for **multiple** `completion done` lines — planner, critic, and possibly a
second round. Look in the trace for either `critic approved draft on attempt 1` or
`critic rejected draft 1: <reasons>` followed by `critic approved draft on attempt 2`.

**Two things you will probably observe with a small model**, both of which the system already
handles:
- risk labels that are wrong (`kubectl delete pod` marked `read`) — commit 48's classifier corrects
  them, and `risk_relabelled` appears in `Meta`
- a rejection with no stated issues — commit 45's step 7 keeps the draft instead of burning a retry

**Why:** the critic is a **different system prompt against the same interface**. A model asked to
*produce* optimises for plausibility; the same model asked to *check* against explicit criteria is
doing an easier task and is not defending its own work — it does not know it wrote the draft.

Separate `Critic` and `Planner` interfaces mean you can run a cheap planner with an expensive critic,
or two different providers, purely from config.

---

## Commit 75 — `feat(openai): LLM reranker`

**Files:** `internal/adapters/cognition/llm/openai/rerank.go`

**Build:**

`const rerankSystem` — score each candidate 0.0–1.0 on whether it addresses the same failure mode
and whether its commands would apply to this target. Reply with
`{"scores":[{"index":1,"score":0.0,"reason":"..."}]}`.

```go
type Reranker struct { c *Client; model string }
func (r Reranker) Name() string                                    // "llm:" + model
func (r Reranker) Rerank(ctx context.Context, query string, matches []domain.RunbookMatch, top int) ([]domain.RunbookMatch, error)
```
Build a user message with `## Incident` (the query) and `## Candidate runbooks` (numbered, each
truncated to 1200 characters). Call `Chat` with `JSONOnly: true`, extract and unmarshal. Map each
score back by `index - 1`, ignoring out-of-range indices; set `Score = clamp01(...)`,
`Stage = "rerank:" + r.Name()`, `Reason = s.Reason`. **When nothing usable came back, return the
first `top` matches unchanged.** Sort by score descending, truncate.

Register `"llm"` in `wire.Rerankers`. Add the blank import for `cognition/llm/openai` to
`plugins.go`.

**Verify:** with a learned runbook in the corpus and `WT_RERANK_DRIVER=llm`, post a similar incident
and check the stored match:

```
"stage": "rerank:llm:qwen2.5:1.5b",
"reason": "Does not address the same failure mode.",
"score": 0.36
```

**Why:** this is the **cross-encoder idea** — a model reading the query and the candidate *together*,
which is far more accurate than comparing two independently-computed vectors. Be precise about what
it is not: a purpose-built cross-encoder like `bge-reranker` would be a third driver behind the same
interface and would be both cheaper and better. This is the architecture, with a general model
standing in.

The "nothing usable came back" fallback matters — a reranker that returns an empty set because the
model was creative would silently discard all your retrieval work.

---

## End of Part 15

Real models, real embeddings, real reranking — and the same binary still runs with
`WT_LLM_PROVIDER=stub` and no network.

```bash
go build ./... && go vet ./... && gofmt -l .
```

Next: **Part 16** — PostgreSQL with partitions and pgvector.
