# Part 13 — Retrieval, and Closing the Learning Loop (Commits 61–65)

**Goal:** the milestone of the whole project. By commit 65, a runbook a human approved is retrieved
for the *next* similar incident, and the planner uses it. No API key required.

---

## Commit 61 — `feat(embed): offline hashing embedder`

**Files:** `internal/adapters/rag/embed/hashing/hashing.go`

**Build:**

```go
type Embedder struct { dims int }
func New(dims int) Embedder                                   // defaults to 256
func (Embedder) Name() string                                 // "hashing"
func (e Embedder) Dimensions() int
func (e Embedder) Embed(_ context.Context, text string) ([]float32, error)
```

Input: any text. Output: a fixed-width vector. Algorithm — the **hashing trick**:
1. `var reToken = regexp.MustCompile(`[a-z0-9_.\-/]+`)`; tokenise the lowercased text
2. skip tokens shorter than 2 characters and a small `stopWords` set
3. for each token: `h := fnv.New32a()`, write the term, `idx := int(h.Sum32()) % e.dims` (correct a
   negative index), `vec[idx] += weight`
4. weight 1.0 for the token, **and 0.5 for the bigram `previous + " " + current`** so word order
   carries a little signal
5. **L2 normalise** at the end:
   ```go
   // L2 normalize so cosine similarity is a plain dot product.
   inv := float32(1 / math.Sqrt(norm))
   ```
   Return the zero vector unchanged when the norm is 0.

Register `"stub"` in `wire.Embedders` with `New(256)`.

**Verify:** build, then in a scratch main embed two similar sentences and two unrelated ones, and
compute the dot products by hand. The similar pair should score noticeably higher.

**Why:** the point is that the **entire RAG path runs offline and deterministically** — cosine
search, score thresholds, cache keys, all of it — so local development and tests exercise the same
code as production.

Be honest about what it is: **lexical, not semantic**. It will not match "OOM killed" to "terminated
by cgroup ceiling". Say that in the commit message so nobody mistakes it for an embedding model.

L2 normalising up front means the vector store's cosine calculation is just a dot product.

---

## Commit 62 — `feat(vector): in-memory vector store`

**Files:** `internal/adapters/rag/vector/memory/memory.go`

**Build:**

```go
type Store struct {
    mu    sync.RWMutex
    items map[string]domain.Runbook
    order []string
}
func New() *Store

func (s *Store) Upsert(_ context.Context, rb domain.Runbook) error
func (s *Store) Search(_ context.Context, vec []float32, k int) ([]domain.RunbookMatch, error)
func (s *Store) Len() int
func cosine(a, b []float32) float64
```

`Upsert` — default a missing `ID` to `Signature + ":" + Title`; append to `order` when new; store.
`Search` — input: a query vector and `k`; output: up to `k` matches sorted by descending score, each
with `Stage: "vector"`. Skip runbooks with no embedding.
`cosine` — the formula, using `math.Sqrt`, returning 0 when either norm is 0 and clamping to
`[0,1]`.

Register `"memory"` in `wire.VectorStores`.

**Verify:** build; scratch-test that upserting three runbooks and searching with a vector close to
one of them ranks it first.

**Why:** brute-force O(n) cosine is fine for hundreds or low thousands of approved runbooks, and it
removes pgvector from the local development path entirely. `sort.SliceStable` keeps ties in insertion
order so results are reproducible.

Writing this before pgvector is deliberate: it forces the `VectorStore` interface to be about
*similarity search*, not about SQL.

---

## Commit 63 — `feat(stages): the retrieval query`

**Files:** `internal/stages/retrieve.go`

**Build:**

```go
func DefaultQuery(inc *domain.Incident) string
```
Input: the incident. Output: the text to embed. Compose, in order:
- the primary alert's `Name`, then `": " + Summary`
- `" [" + Target() + "]"` when the target is known
- up to three more alert names, `"; "`-joined
- the **three most severe** log lines, each truncated to 200 characters
- up to two events as `"<Reason> <message>"`, truncated to 160

```go
func TopLines(lines []domain.LogLine, n int) []domain.LogLine
```
Copy the slice, insertion-sort by `LevelRank()` descending, return the first `n`.

Plus `truncate(s string, max int) string` and a local `min(a, b int) int`.

**Verify:** build, then print `DefaultQuery` for a real incident. It should read like a short,
information-dense description of the failure.

**Why:** the query is **alert names + target + the most severe logs + a couple of events** — not the
whole incident. An embedding of everything is an embedding of nothing in particular: the
distinguishing signal gets averaged away. This is the single most important tuning decision in a RAG
system and it is worth being able to justify.

---

## Commit 64 — `feat(stages): the retrieval stage`

**Files:** `internal/stages/retrieve.go` (extend)

**Build:**

```go
type RetrieveOptions struct {
    TopK     int
    TopN     int
    MinScore float64
    QueryOf  func(*domain.Incident) string
}

func Retrieve(emb ports.Embedder, store ports.VectorStore, rr ports.Reranker, opts RetrieveOptions) Stage
```
Defaults: `TopK` 10, `TopN` 2, `QueryOf` = `DefaultQuery`.

Flow, and **every failure path returns `nil`**:
1. `emb == nil || store == nil` → `inc.Note("retrieval disabled")`
2. empty query → `inc.Note("retrieval skipped: empty query")`
3. `inc.SetMeta("rag_query", truncate(query, 240))`
4. `emb.Embed` fails → `inc.Note("embedding failed, continuing without runbooks: %v", err)`
5. `store.Search` fails → `inc.Note("vector search failed, continuing without runbooks: %v", err)`
6. no matches → `inc.Note("no historical runbooks matched")`
7. when `rr != nil`, rerank to `TopN`; **on error keep the vector order** and note it
8. keep at most `TopN` matches whose `Score >= MinScore`
9. `inc.Matches = kept` and `inc.Note("retrieved %d runbook(s) above score %.2f", ...)`

**Verify:** build.

**Why:** count the early returns — six, all returning nil. Retrieval is an **enhancement**; losing it
must never lose the incident. The consequence propagates automatically into the confidence score,
which awards up to 0.4 for retrieval support, so a cold plan is correctly reported as less
trustworthy.

`MinScore` filtering **after** reranking matters: an irrelevant runbook in the prompt is worse than
no runbook, because the model will try to use it.

`QueryOf` being injectable means you can experiment with query construction without touching the
stage.

---

## Commit 65 — `feat(services): learn approved runbooks` 🎉

**Files:** `internal/services/feedback.go` (replace the stub `learn`),
`internal/bootstrap/app.go` (edit), `internal/bootstrap/plugins.go` (edit)

**Build:**

```go
func (f *Feedback) learn(ctx context.Context, summary domain.ApprovedSummary) error
```
1. `f.embedder == nil || f.vectors == nil` → `fmt.Errorf("retrieval corpus is not configured")`
2. `runbook := summary.AsRunbook()`
3. **refuse junk**: no commands *and* no root cause →
   `fmt.Errorf("nothing worth embedding: no commands and no root cause")`
4. `vec, err := f.embedder.Embed(ctx, runbook.Text())`
5. set `runbook.Embedding = vec` and `runbook.SourceText = runbook.Text()`
6. `f.vectors.Upsert(ctx, runbook)` — on error **park it** and return the error
7. publish `bus.TopicRunbookLearned`

In `app.go`:
- build `embedder` (`cfg.RAG.EmbedderDriver`), `vectors` (`cfg.RAG.VectorDriver`)
- pass both into `services.NewFeedback(...)` instead of `nil, nil`
- insert into the pipeline, **after `publish:incident.sanitized`**:
  ```go
  stages.Retrieve(embedder, vectors, nil, stages.RetrieveOptions{
      TopK: cfg.RAG.TopK, TopN: cfg.RAG.TopN, MinScore: cfg.RAG.MinScore,
  }),
  stages.InputGuardNamed("input-guard:retrieved", redactors),
  ```
  (the reranker is `nil` until commit 69)
- blank imports for `rag/embed/hashing` and `rag/vector/memory`

**Verify — this is the milestone:**

```bash
go build -o bin/watchtowerd ./cmd/watchtowerd && ./bin/watchtowerd

# 1. first incident — no corpus yet
curl -X POST localhost:8080/alerts -H "Content-Type: application/json" \
  --data-binary @examples/alerts/alertmanager-oom.json
#    trace should say: "no historical runbooks matched"

# 2. approve it WITH edited commands, via the API (note the id from the response above)
curl -X POST localhost:8080/runbook/approve -H "Content-Type: application/json" -d '{
  "incident_id":"<id>","decision":"edit","reviewer":"sre-oncall",
  "root_cause":"Memory limit of 32Mi is below the working set after release 4.2.1",
  "commands":[{"run":"kubectl -n checkout set resources deployment/checkout-api --limits=memory=512Mi",
               "explain":"Raise the limit to the observed working set"}]}'
#    log should now say: feedback recorded ... learned=true

# 3. a DIFFERENT but similar incident (different alertname → different signature → passes debounce)
curl -X POST localhost:8080/alerts -H "Content-Type: application/json" \
  --data-binary @examples/alerts/alertmanager-oom-repeat.json
```

In step 3 the trace must say **`retrieved 1 runbook(s) above score 0.20`**, the email's *"Historical
runbooks used"* section must name it, and the confidence should be **higher** than step 1's.

If step 3 retrieves nothing, lower `WT_RAG_MIN_SCORE` — the offline hashing embedder scores lower
than a real embedding model, and on the real stack this match came in around 0.36.

**Why:** this is the commit that makes the project interesting rather than a wrapper. Human judgement
became retrieval material became a better plan, with no training run and no fine-tuning — and the
new knowledge is available on the *very next* incident.

Note the ordering you just created: `input-guard` → `publish:sanitized` → `retrieve` →
`input-guard:retrieved`. Redaction happens **before** the embedding call, so a credential that leaked
into a log line is never sent to an embedding provider. In the original build of this project it ran
*after*, and that was a real defect. The second guard pass covers material coming back out of a
shared corpus.

Also note step 3 of `learn`: refusing to embed something with no commands and no root cause. Junk in
the corpus is worse than a smaller corpus, because every future retrieval pays for it.

---

## End of Part 13

**Milestone reached.** The system learns from the humans who review it.

```bash
go build ./... && go vet ./... && gofmt -l .
```

Next: **Part 14** — the embedding cache, reranking, and a retry helper.
