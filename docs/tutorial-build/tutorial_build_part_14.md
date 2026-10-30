# Part 14 — Embedding Cache, Reranking, Retry (Commits 66–70)

**Goal:** make retrieval cheaper and more precise, and add the backoff helper the real model client
will need in Part 15.

---

## Commit 66 — `feat(cache): in-memory byte cache`

**Files:** `internal/adapters/rag/cache/cache.go`

**Build:**

```go
type entry struct { value []byte; expires time.Time }

type Memory struct {
    clock ports.Clock
    max   int
    mu    sync.RWMutex
    items map[string]entry
}
func NewMemory(c ports.Clock, max int) *Memory

func (m *Memory) Get(_ context.Context, key string) ([]byte, bool, error)
func (m *Memory) Set(_ context.Context, key string, value []byte, ttl time.Duration) error
func (m *Memory) Close() error
func (m *Memory) evict()
```

`Get` — read under `RLock`; when the entry has expired, delete it under a write lock and report a
miss. `Set` — compute `expires` from the clock when `ttl > 0`; call `evict()` when at capacity.

`evict` — expired entries first, then arbitrary keys, **over-evicting by 10%**:
```go
// evict clears expired entries, then drops arbitrary keys if still over budget.
// Approximate eviction is fine: a cache miss only costs one embedding call.
drop := len(m.items) - m.max + m.max/10
```

Also add `type Nop struct{}` implementing all three methods as no-ops.

Register `"memory"` (with a 10000 ceiling) and `"none"` in `wire.Caches`.

**Verify:** build; scratch-test that a value with a 1ns TTL is a miss on the next `Get` using a
`clock.Fixed` you advance.

**Why:** deliberately **not LRU**. Proper LRU needs a linked list plus a map and more locking, and
the cost of a wrong eviction here is one embedding call. Write the comment justifying the choice —
choosing the cheap-and-adequate algorithm *on purpose* is a senior move; doing it by accident is not.

Over-evicting by 10% means eviction is not triggered again on the very next write.

---

## Commit 67 — `feat(embed): caching decorator`

**Files:** `internal/adapters/rag/embed/cached/cached.go`

**Build:**

```go
type Embedder struct {
    inner ports.Embedder
    cache ports.Cache
    ttl   time.Duration
    log   ports.Logger
}

func Wrap(inner ports.Embedder, cache ports.Cache, ttl time.Duration, log ports.Logger) ports.Embedder
```
**Return `inner` unchanged when either `inner` or `cache` is nil.**

- `func (e *Embedder) Name() string` → `e.inner.Name() + "+cache"`
- `func (e *Embedder) Dimensions() int` → delegate
- `func (e *Embedder) Embed(ctx, text) ([]float32, error)`:
  1. `key := e.keyFor(text)`
  2. cache hit and decodes → log a debug hit and return it
  3. cache read error → **warn and continue**
  4. call the inner embedder; on error return it
  5. write to the cache; **on error warn and return the vector anyway**

- `func (e *Embedder) keyFor(text string) string`:
  ```go
  sum := sha256.Sum256([]byte(e.inner.Name() + "\x00" + text))
  return "emb:" + hex.EncodeToString(sum[:16])
  ```
- `func encode(vec []float32) []byte` / `func decode(raw []byte) ([]float32, error)` — raw
  little-endian float32 via `math.Float32bits` / `Float32frombits`; `decode` errors when the length
  is 0 or not a multiple of 4.

Wire it in `bootstrap/app.go`, right after building the embedder:
```go
if cfg.RAG.CacheDriver != "none" {
    embedCache, err := wire.Caches.Build(env, cfg.RAG.CacheDriver)
    if err != nil { return err }
    env.OnClose(embedCache.Close)
    // Decorator, not a config flag inside the embedder: the OpenAI adapter never
    // learns that caching exists.
    embedder = cached.Wrap(embedder, embedCache, cfg.RAG.CacheTTL, log)
}
```
Add the blank import for `rag/cache` (but **not** for `cached` — it is used by name).

**Verify:**

```bash
WT_LOG_LEVEL=debug ./bin/watchtowerd
```
Post the same alert twice with a short `WT_DEBOUNCE_WINDOW=5s` and look for
`embedding cache hit` on the second run. Also check the startup line now reports
`embedder=stub` while the decorated name appears as `hashing+cache` where it is logged.

**Why:** this is **composition instead of configuration**. The OpenAI adapter contains no cache
code, the cache contains no embedding code, and the decorator knows nothing about either
implementation. Adding caching required changing neither.

Two details worth pointing at: the **model name is in the key** (separated by a NUL byte, so
`"ab"+"c"` cannot collide with `"a"+"bc"`), which is what stops a model switch from serving vectors
of the wrong dimension. And vectors are stored as **raw float32 bytes** — 4 bytes per dimension
instead of ~12 as JSON, with no parse cost.

A broken cache is never fatal on any of the three paths. A cache that can take down the feature it
accelerates is worse than no cache.

---

## Commit 68 — `feat(rerank): heuristic reranker`

**Files:** `internal/adapters/rag/rerank/heuristic.go`

**Build:**

```go
type Heuristic struct { Clock ports.Clock }
func (Heuristic) Name() string                                    // "heuristic"
func (h Heuristic) Rerank(_ context.Context, query string, matches []domain.RunbookMatch, top int) ([]domain.RunbookMatch, error)
```

For each match:
- `overlap := jaccard(tokenSet(query), tokenSet(m.Runbook.Text()))`
- `score := 0.6*m.Score + 0.4*overlap`
- `+0.05` when the runbook's first three title words appear in the query → reason
  *"title appears in query"*
- `+0.05` when `CreatedAt` is within 30 days → reason *"verified in the last 30 days"*
- `-0.20` when the runbook has no commands → reason *"no commands recorded"*
- set `m.Score = clamp01(score)`, `m.Stage = "rerank:heuristic"`, and
  `m.Reason = "lexical overlap 8%; verified in the last 30 days"` style

Then sort by score descending and truncate to `top`.

Helpers: `tokenSet(s string) map[string]struct{}` (lowercased, tokens of length ≥ 3),
`jaccard(a, b map[string]struct{}) float64` (intersection over union), `firstWords`, `clamp01`,
`pct`.

**Verify:** build; scratch-test that a runbook sharing many words with the query outranks one that
merely has a high vector score.

**Why:** **Jaccard similarity** is the fraction of distinct words two texts share. Blending it 60/40
with the vector score combines semantic neighbourhood with literal overlap — a cheap approximation
of what a cross-encoder does properly.

The recency bonus encodes a real operational belief: a runbook verified last week is likelier to
still be correct than one from a year ago, because infrastructure drifts.

Every match carries a human-readable `Reason` that ends up in the email. **Explainable ranking** —
the reviewer can see why a runbook was chosen and dismiss it when the reason is weak.

---

## Commit 69 — `feat(rerank): passthrough, and wire the reranker`

**Files:** `internal/adapters/rag/rerank/heuristic.go` (extend),
`internal/bootstrap/app.go` (edit), `internal/bootstrap/plugins.go` (edit)

**Build:**

```go
type Passthrough struct{}
func (Passthrough) Name() string                                  // "none"
func (Passthrough) Rerank(_ context.Context, _ string, matches []domain.RunbookMatch, top int) ([]domain.RunbookMatch, error)
```
Keeps the vector order and only truncates.

Register `"heuristic"` (passing `e.Clock`) and `"none"`. In `app.go`, build
`reranker, err := wire.Rerankers.Build(env, cfg.RAG.RerankDriver)` and pass it into
`stages.Retrieve(embedder, vectors, reranker, ...)` in place of `nil`. Add the blank import for
`rag/rerank`.

**Verify:**

```bash
./bin/watchtowerd
# post an alert; the trace's retrieved line should now reflect reranked scores,
# and the email should show a Reason for each match
WT_RERANK_DRIVER=none ./bin/watchtowerd   # scores fall back to raw similarity
```

**Why:** having a `none` driver is not padding. It is how you A/B the reranker's contribution — same
fixtures, one environment variable, compare precision@2. That measurement is only possible because
the driver is swappable.

---

## Commit 70 — `feat(platform): retry with backoff`

**Files:** `internal/platform/retry/retry.go`

**Build:**

```go
type Policy struct { Attempts int; Base, Max time.Duration }
func Default() Policy                              // 3 attempts, 200ms base, 2s cap

func Do(ctx context.Context, p Policy, fn func(ctx context.Context) error) error
func Permanent(err error) error
```

`Do`:
- treat `Attempts <= 0` as 1
- before each attempt check `ctx.Err()` and return `errors.Join(last, ctx.Err())` if done
- success → nil
- **`errors.As` into an internal `permanent` type → return the wrapped error immediately, no retry**
- last attempt → break
- otherwise `wait := Base * 2^attempt`, capped at `Max`; sleep on a timer that also selects on
  `ctx.Done()`

`permanent` is an unexported struct with `Error() string` and `Unwrap() error`; `Permanent(nil)`
returns nil.

**Verify:** build; scratch-test that a function failing twice then succeeding returns nil after
three calls, and that one returning `retry.Permanent(err)` is called exactly **once**.

**Why:** **classification is the whole point.** A retry policy without a way to say "do not retry
this" is a bug generator — retrying a malformed request or a bad API key three times just wastes
three times as long. Commit 71 uses `Permanent` for `4xx` and plain errors for `429`/`5xx`.

Selecting on `ctx.Done()` while sleeping means a shutdown does not have to wait out the backoff.

---

## End of Part 14

Retrieval is cached, reranked and explainable, and you have the backoff helper the model client
needs.

```bash
go build ./... && go vet ./... && gofmt -l .
```

Next: **Part 15** — real language models over the OpenAI-compatible API.
