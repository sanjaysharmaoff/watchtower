# Part 9 — Embeddings, Vector Search, pgvector and RAG

This is the part interviewers dig into hardest, because most candidates have used RAG
without being able to explain it. Take your time here.

## The problem: keyword search does not understand meaning

You have 400 approved runbooks. A new alert says *"container OOM killed, memory limit
reached"*. A runbook from last month says *"pod terminated by kernel after exceeding
cgroup memory ceiling"*. There is almost **no word overlap**, yet they are the same
failure. Keyword search misses it. You need search by *meaning*.

## Embeddings

An **embedding** is a list of numbers that represents the meaning of a piece of text. You
send text to a model, it returns a fixed-length array of floats — a **vector**.

```
"container OOM killed, memory limit reached"
   → [0.0231, -0.0187, 0.0412, ... ]     // 768 numbers with nomic-embed-text
```

The property that makes this useful: **texts with similar meaning produce vectors that
point in similar directions**, even with no shared words. The model learned that during
training on huge amounts of text.

The number of dimensions is fixed per model, and this matters operationally:

| Model | Dimensions |
| --- | --- |
| `text-embedding-3-small` (OpenAI) | 1536 |
| `text-embedding-3-large` (OpenAI) | 3072 |
| `nomic-embed-text` (local, used here) | 768 |
| the built-in offline hashing embedder | 256 |

## Cosine similarity

To compare two vectors you measure the **angle** between them, not the distance. Cosine
similarity is the dot product divided by the product of the magnitudes:

```
similarity = (A · B) / (|A| × |B|)
```

It ranges from -1 (opposite) through 0 (unrelated) to 1 (identical direction). Angle
rather than distance, because a longer document produces a longer vector without being
*more* about the topic. Direction carries the meaning; magnitude mostly carries length.

If vectors are **normalised** (scaled to length 1), cosine similarity is just the dot
product, which is cheaper. The offline embedder in this project does exactly that:

```go
// internal/adapters/rag/embed/hashing/hashing.go
// L2 normalize so cosine similarity is a plain dot product.
inv := float32(1 / math.Sqrt(norm))
for i := range vec {
    vec[i] *= inv
}
```

The in-memory vector store's implementation is the formula, literally:

```go
for i := 0; i < n; i++ {
    dot += float64(a[i]) * float64(b[i])
    na  += float64(a[i]) * float64(a[i])
    nb  += float64(b[i]) * float64(b[i])
}
sim := dot / (math.Sqrt(na) * math.Sqrt(nb))
```

## pgvector

**pgvector** is a Postgres extension that adds a `vector` column type, distance operators
and vector indexes. It means no separate vector database.

```sql
CREATE EXTENSION IF NOT EXISTS vector;

CREATE TABLE historical_runbooks (
    id         TEXT PRIMARY KEY,
    title      TEXT NOT NULL,
    signature  TEXT,
    root_cause TEXT,
    commands   JSONB NOT NULL DEFAULT '[]'::jsonb,
    tags       TEXT[],
    reviewer   TEXT,
    content    TEXT NOT NULL,
    embedding  vector(768),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX historical_runbooks_embedding_idx
    ON historical_runbooks USING hnsw (embedding vector_cosine_ops);
```

`vector(768)` fixes the dimension **at the schema level**. Insert a 256-dimension vector
and Postgres rejects it. This is why the dimension is a config value, why the migration
uses it, and why `Config.Validate()` refuses an obviously wrong combination:

```go
if c.RAG.VectorDriver == "postgres" && c.RAG.EmbedderDriver == "stub" && c.RAG.VectorDims != 256 {
    errs = append(errs, "the stub embedder produces 256-dimension vectors: set WT_VECTOR_DIMS=256 ...")
}
```

Catching a dimension mismatch at boot with a sentence that tells you the fix, rather than
at the first insert with a driver error, is exactly the kind of detail worth pointing at.

### The distance operators

```
<->   L2 (Euclidean) distance
<=>   cosine distance      (1 - cosine similarity)
<#>   negative inner product
```

Note `<=>` is a **distance**: 0 means identical, 2 means opposite. To report similarity
you convert:

```sql
SELECT id, title, 1 - (embedding <=> $1::vector) AS similarity
FROM historical_runbooks
WHERE embedding IS NOT NULL
ORDER BY embedding <=> $1::vector
LIMIT $2
```

`ORDER BY <=> LIMIT k` is the **k-nearest-neighbour query** — the whole point of a vector
store. `$1::vector` casts the parameter, because the driver sends a string like
`[0.023,-0.018,...]`:

```go
func encodeVector(vec []float32) string {
    var b strings.Builder
    b.WriteByte('[')
    for i, f := range vec {
        if i > 0 { b.WriteByte(',') }
        b.WriteString(strconv.FormatFloat(float64(f), 'f', -1, 32))
    }
    b.WriteByte(']')
    return b.String()
}
```

### HNSW indexes

Comparing the query against every row is exact but O(n). **HNSW** (Hierarchical Navigable
Small World) is a graph index that makes it roughly logarithmic — an **approximate**
nearest neighbour search. You trade a tiny chance of missing a true neighbour for a very
large speedup.

Be ready for "exact or approximate?": HNSW is approximate; `vector_cosine_ops` tells the
index which distance function to optimise for, and it must match the operator used in the
query or the index will not be used. The alternative index type is IVFFlat, which is
faster to build and needs training data; HNSW is better for incremental inserts, which is
what a growing runbook corpus does.

## RAG: Retrieval-Augmented Generation

RAG means: **before asking the model, retrieve relevant material and put it in the
prompt.**

Why:

- **Knowledge the model never saw.** It was trained on the public internet. It has never
  seen your `checkout-api`, your naming conventions, or the fix your team applied last
  month.
- **Grounding.** Given a verified runbook, the model adapts it instead of inventing.
- **Cheaper than fine-tuning.** No training run, and new knowledge is available the moment
  it is written.

The WatchTower RAG loop, end to end:

1. Build a query string from the incident (alert names, target, top log lines, events).
2. Embed the query → vector.
3`.` `ORDER BY embedding <=> query LIMIT 10` → top 10 candidate runbooks.
4. Rerank those 10 down to the best 2.
5. Put those 2 into the prompt.
6. Model writes a plan.
7. Engineer approves or edits.
8. **The approved runbook is embedded and inserted** → step 3 finds it next time.

Step 8 is what makes it a *loop* rather than a lookup, and it is the most valuable thing
in the project. Verified: after approving one runbook, the next incident retrieved it and
the planner adopted its command verbatim.

## Two-stage retrieval: why rerank at all

Vector search is fast but blunt: it compares one summary vector against another, so it
gets you into the right neighbourhood and no further. **Reranking** re-scores the small
candidate set with a more expensive, more accurate method.

The industry pattern is a **cross-encoder**: a model that reads the query and the
candidate *together* and scores the pair, which is far more accurate than comparing two
independently-computed vectors — and far too slow to run over a whole corpus. So: cheap
recall over thousands, expensive precision over ten.

This project ships two rerankers behind one interface:

```go
type Reranker interface {
    Name() string
    Rerank(ctx context.Context, query string, matches []domain.RunbookMatch, top int) ([]domain.RunbookMatch, error)
}
```

- **`heuristic`** (default, free): blends vector score with lexical overlap, plus small
  bonuses for recency and title match, minus a penalty for runbooks with no commands.
  `score = 0.6*vector + 0.4*jaccard(query, runbook) ± bonuses`.
- **`llm`**: asks the language model to score each candidate 0..1 with a reason. Slower and
  costs tokens, but it reads both texts together, which is the cross-encoder idea
  implemented with a general model.

Verified LLM reranker output from the real stack:

```json
{ "score": 0.3645, "stage": "rerank:llm:qwen2.5:1.5b",
  "reason": "Does not address the same failure mode." }
```

Be honest in interviews: **this is not a true cross-encoder** like `bge-reranker`. It is
the same two-stage architecture with a general model standing in, and a real cross-encoder
would drop in as a third driver behind the same interface.

## The embedding cache

Embedding calls cost money and latency, and the same text embeds to the same vector
forever. So caching is free money — implemented as a **decorator**, not a flag:

```go
func (e *Embedder) keyFor(text string) string {
    sum := sha256.Sum256([]byte(e.inner.Name() + "\x00" + text))
    return "emb:" + hex.EncodeToString(sum[:16])
}
```

Two details worth pointing out:

- The key includes the **model name**, so switching from `nomic-embed-text` to
  `text-embedding-3-small` cannot serve stale vectors of the wrong dimension.
- Vectors are stored as raw little-endian float32 bytes rather than JSON — 4 bytes per
  dimension instead of ~12, and no parse cost.

The OpenAI adapter contains no cache code; the cache contains no embedding code; the
decorator knows nothing about either implementation. That is composition instead of
configuration.

## The offline hashing embedder

To keep the project runnable with no API key at all, there is a deterministic embedder
using the **hashing trick**: hash each token (and each bigram) into one of 256 buckets and
accumulate weights.

```go
add := func(term string, weight float32) {
    h := fnv.New32a()
    h.Write([]byte(term))
    idx := int(h.Sum32()) % e.dims
    vec[idx] += weight
}
for i, t := range tokens {
    if len(t) < 2 || stopWords[t] { continue }
    add(t, 1)
    if i > 0 { add(tokens[i-1]+" "+t, 0.5) }   // bigram: word order carries a little weight
}
```

Say clearly what this is and is not: it captures **lexical** similarity, not semantic. It
will not match "OOM killed" to "terminated by cgroup ceiling". Its purpose is that the
whole pipeline — including cosine search and score thresholds — runs offline and
deterministically, which makes local development and tests behave like production
structurally, if not semantically.

## Retrieval configuration

| Variable | Default | Meaning |
| --- | --- | --- |
| `WT_RAG_TOP_K` | 10 | candidates fetched from the vector store |
| `WT_RAG_TOP_N` | 2 | candidates kept after reranking, i.e. put in the prompt |
| `WT_RAG_MIN_SCORE` | 0.2 | below this, a match is discarded entirely |
| `WT_VECTOR_DIMS` | 1536 | must match the embedder |
| `WT_RAG_CACHE_TTL` | 24h | embedding cache lifetime |

Why `TOP_N = 2`: prompt space is expensive and irrelevant runbooks actively mislead the
model. Two good ones beat ten mediocre ones. `Validate()` also rejects `TOP_N > TOP_K`,
which would be nonsense.

## Failure behaviour

Retrieval is entirely optional. If the embedder or the vector store is missing or erroring,
the stage records a note and the pipeline continues:

```go
vec, err := emb.Embed(ctx, query)
if err != nil {
    inc.Note("embedding failed, continuing without runbooks: %v", err)
    return nil
}
```

The plan is then produced "cold" and the confidence score is lower, because retrieval
support is worth up to 0.4 of it. **Degrade, do not fail** — an incident without a runbook
is still worth emailing.

## Interview answers to have ready

**"Explain embeddings to a non-technical person."**
A model turns text into coordinates. Things that mean similar things land near each other,
even with different words. So "find similar" becomes "find nearby", which is arithmetic.

**"Why cosine and not Euclidean?"**
Cosine measures direction, which is what carries meaning; magnitude mostly reflects
document length. A long document should not be treated as more relevant just for being
long. With normalised vectors the two rankings coincide anyway.

**"Why pgvector rather than Pinecone or Qdrant?"**
The corpus is small — approved runbooks, so hundreds or low thousands — and I already
needed Postgres. One database means one connection pool, one backup, one thing to operate.
A dedicated vector database earns its place at a scale I do not have.

**"How does your system avoid retrieving garbage?"**
Three filters: `TOP_K` bounds the candidates, a reranker re-scores them with lexical or
model judgement, and `MIN_SCORE` discards weak matches outright so an unrelated runbook
never reaches the prompt. The prompt also states explicitly that no matching runbook means
"treat this as a new failure mode."

**"What goes into the corpus?"**
Only human-reviewed outcomes, and only commands that passed the output guardrail —
`inc.Plan.Executable()`. A rejected plan is recorded but never embedded. So the corpus is
verified fixes, not model output.

---

Next: **Part 10 — Large Language Models**, the planner, the critic, and how the model is
kept on a leash.
