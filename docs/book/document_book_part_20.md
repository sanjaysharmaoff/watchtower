# Part 20 — Code Walkthrough: Ingest and Enrich Adapters

Now the implementations. This part covers `adapters/ingest/*` and `adapters/enrich/*`. The
sanitizer algorithms in the second half are the most quotable code in the project.

## `ingest/decode` — the anti-corruption layer

Covered in Part 4. The two things to remember: `merge(common, perAlert)` for labels and
annotations, and `pick(labels, "service", "app", "job", "deployment", "container")` for
first-non-empty lookup. Registers two drivers, `alertmanager` and `generic`.

## `ingest/gate` — memory and Redis

```go
type Memory struct {
    clock ports.Clock
    mu    sync.Mutex
    seen  map[string]time.Time
    last  time.Time
}

func (m *Memory) Allow(_ context.Context, key string, window time.Duration) (bool, error) {
    if window <= 0 { return true, nil }
    now := m.clock.Now()

    m.mu.Lock()
    defer m.mu.Unlock()

    if now.Sub(m.last) > window {
        m.prune(now, window)
        m.last = now
    }
    if until, ok := m.seen[key]; ok && now.Before(until) {
        return false, nil
    }
    m.seen[key] = now.Add(window)
    return true, nil
}

func (m *Memory) prune(now time.Time, window time.Duration) {
    for k, until := range m.seen {
        if now.After(until.Add(window)) {
            delete(m.seen, k)
        }
    }
}
```

Three points:

**Amortised pruning.** Instead of a background goroutine or a per-call sweep, it prunes at most
once per window. A map that only grows is a memory leak, and a cleanup goroutine is another
lifecycle to manage; this is the cheapest correct option. Deleting from a Go map during
`range` over it is explicitly safe.

**Injected clock.** `m.clock.Now()`, never `time.Now()`. A test can jump an hour forward
instantly instead of sleeping.

**`window <= 0` disables it.** Configuration meaning "no debouncing" should not mean "block
everything".

Redis is nine lines because `SETNX` does all the work (Part 11). Same interface, same
behaviour, shared across replicas.

## `ingest/correlate` — the windowed correlator

```go
type Window struct {
    clock  ports.Clock
    keys   []string
    window time.Duration

    mu     sync.Mutex
    groups map[string]*group
}

type group struct {
    alerts   []domain.Alert
    lastSeen time.Time
}
```

```go
func (w *Window) Correlate(_ context.Context, inc *domain.Incident) error {
    key := w.keyOf(inc)
    now := w.clock.Now()

    w.mu.Lock()
    defer w.mu.Unlock()
    w.prune(now)

    g, ok := w.groups[key]
    if !ok {
        g = &group{}
        w.groups[key] = g
    }

    added := inc.MergeAlerts(g.alerts)          // pull historical alerts INTO this incident
    g.alerts = dedupe(append(g.alerts, inc.Alerts...))   // push this incident's alerts into the window
    g.lastSeen = now

    inc.CorrelationKey = key
    inc.SetMeta("correlation_key", key)
    inc.SetMeta("correlated_alerts", itoa(len(inc.Alerts)))
    if added > 0 {
        inc.Note("correlated %d recent alert(s) on %s", added, key)
    }
    inc.Recompute()
    inc.CorrelationKey = key
    return nil
}
```

The algorithm is bidirectional: **pull** recent related alerts into this incident, then **push**
this incident's alerts into the window for the next one. That is what makes alerts arriving in
*separate* webhooks, minutes apart, end up in one incident — something Alertmanager's grouping
cannot do.

The key:

```go
func (w *Window) keyOf(inc *domain.Incident) string {
    primary := inc.PrimaryAlert()
    parts := make([]string, 0, len(w.keys))
    for _, k := range w.keys {
        if v := primary.Label(k); v != "" {
            parts = append(parts, k+"="+v)
        }
    }
    if len(parts) == 0 {
        return "target=" + primary.Target()
    }
    return strings.Join(parts, ",")
}
```

Produces `cluster=prod-eu-1,namespace=checkout,service=checkout-api` from
`WT_CORRELATION_KEYS=cluster,namespace,service`. Because it is config, a team that labels by
`team` or `tier` changes an env var, not code. There is a sensible fallback to the alert target
when no configured label is present.

Two details: `inc.CorrelationKey = key` is assigned **again after `Recompute()`**, because
`Recompute` would otherwise default it to the signature. And `dedupe` sorts by severity
descending, so the most severe alert stays at the front of the window's list.

The known limitation, worth volunteering: this window is **per-process memory**. With multiple
replicas each has its own view, so correlation could be partial. The Redis-backed equivalent
would be the fix, and the interface already allows it — `wire.Correlators.Register("redis", ...)`
plus a config change.

## `enrich/loki` and `enrich/k8s`

Covered in Parts 5 and 6. The pattern both follow, which is the reusable lesson:

1. Read what you need off the alert's labels.
2. Build a query; refuse to run if there is nothing to anchor on.
3. One HTTP call with a context deadline.
4. Map the response into `domain` types.
5. Return a fragment; put failures in `Err`, never panic.

## `enrich/demo`

Generates synthetic logs and events shaped by the alert name, so the pipeline can be exercised
with no Loki and no cluster. Every fragment it produces is tagged:

```go
frag := domain.ContextFragment{
    Provider: "demo",
    Notes:    map[string]string{"synthetic": "true", "warning": "generated locally, not real telemetry"},
}
```

It also generates *duplicates with drifting timestamps and varying request ids*, on purpose, so
the deduplicator has real work to do. Fake data that does not exercise your real code paths is
worse than no fake data.

## `enrich/sanitize` — the four processors

This is the most quotable code in the project. Four `LogProcessor` implementations, composed in
the order given by `WT_SANITIZE_CHAIN` (default `dedupe,signal,compress,cap`).

### The shared regex set

```go
var (
    reHex        = regexp.MustCompile(`\b[0-9a-fA-F]{8,}\b`)
    reUUID       = regexp.MustCompile(`\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-...\b`)
    reNumber     = regexp.MustCompile(`\b\d+\b`)
    reTimestamp  = regexp.MustCompile(`\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:?\d{2})?`)
    reIPv4       = regexp.MustCompile(`\b\d{1,3}(\.\d{1,3}){3}\b`)
    reWhitespace = regexp.MustCompile(`\s+`)
)
```

Compiled once at package level. Compiling a regex inside a loop over 150 log lines is a classic
performance mistake — `MustCompile` is expensive, matching is cheap.

### 1. `Deduper` — the 50× win

```go
func (d Deduper) Process(lines []domain.LogLine) []domain.LogLine {
    index := make(map[string]int, len(lines))
    out := make([]domain.LogLine, 0, len(lines))
    for _, l := range lines {
        key := d.normalize(l.Message)
        if pos, ok := index[key]; ok {
            out[pos].Occurrences++
            if l.Timestamp.After(out[pos].Timestamp) {
                out[pos].Timestamp = l.Timestamp
            }
            continue
        }
        if l.Occurrences == 0 { l.Occurrences = 1 }
        index[key] = len(out)
        out = append(out, l)
    }
    return out
}

// normalize erases the parts of a message that make otherwise identical lines
// look unique: timestamps, ids, addresses and (optionally) all numbers.
func (d Deduper) normalize(msg string) string {
    s := strings.ToLower(msg)
    s = reTimestamp.ReplaceAllString(s, "<ts>")
    s = reUUID.ReplaceAllString(s, "<uuid>")
    s = reIPv4.ReplaceAllString(s, "<ip>")
    s = reHex.ReplaceAllString(s, "<hex>")
    if d.StripDigits {
        s = reNumber.ReplaceAllString(s, "<n>")
    }
    return strings.TrimSpace(reWhitespace.ReplaceAllString(s, " "))
}
```

The idea in one sentence: **two log lines are "the same" if they differ only in their variable
parts.**

```
fatal error: out of memory (heap 512MiB, limit 512MiB) req_id=8f3c9a21
fatal error: out of memory (heap 511MiB, limit 512MiB) req_id=aa41b7e0
                    ↓ normalize ↓
fatal error: out of memory (heap <n>mib, limit <n>mib) req_id=<hex>
```

One line, `Occurrences = 2`. Extended over 150 lines from three templates, that is 150 → 3, and
`(x47)` in the output preserves the frequency — which is often the most diagnostic fact.

Note the timestamp is **advanced to the newest occurrence**, so "when did this last happen"
survives, and the counter is seeded at 1 rather than 0 so `(x2)` means what it says.

The idea generalises: this is the same normalisation trick used for error grouping in Sentry and
for Prometheus label cardinality control (Part 3). Good thing to connect out loud.

### 2. `SignalFilter`

```go
func (f SignalFilter) keep(l domain.LogLine) bool {
    if rank := l.LevelRank(); rank > 0 && rank >= f.minRank {
        return true
    }
    msg := strings.ToLower(l.Message)
    for _, k := range f.keywords {
        if strings.Contains(msg, k) { return true }
    }
    return false
}
```

Keep a line if its level is at or above the floor, **or** it contains a signal keyword
(`error, fail, timeout, refused, panic, oom, exception, denied, unavailable`). The keyword path
exists because plenty of real logs have no level at all.

The safety net matters more than the filter:

```go
// If filtering removed everything, keep the original tail rather than send an
// empty context to the model.
if len(out) == 0 && len(lines) > 0 {
    start := len(lines) - 10
    if start < 0 { start = 0 }
    return lines[start:]
}
```

An over-aggressive filter that produces zero evidence is worse than a noisy one. Fall back to
the last 10 lines. This kind of "never return nothing" defence is exactly what interviewers mean
by defensive design.

### 3. `Compressor`

```go
func (c Compressor) Process(lines []domain.LogLine) []domain.LogLine {
    max := c.MaxLineLen
    if max <= 0 { max = 400 }
    out := make([]domain.LogLine, 0, len(lines))
    for _, l := range lines {
        msg := strings.TrimSpace(reWhitespace.ReplaceAllString(l.Message, " "))
        if len(msg) > max {
            head := max * 2 / 3
            tail := max - head - 5
            msg = msg[:head] + " ... " + msg[len(msg)-tail:]
        }
        l.Message = msg
        if l.Message == "" { continue }
        out = append(out, l)
    }
    return out
}
```

Collapses whitespace, then truncates **from the middle**, keeping two thirds of the head and the
rest of the tail. Why not simply cut the end: a Java stack trace's useful information is the
exception at the start *and* the root cause at the end; the middle is framework frames.

### 4. `Cap` — the hard ceiling

```go
func (c Cap) Process(lines []domain.LogLine) []domain.LogLine {
    max := c.Max
    if max <= 0 { max = 60 }
    if len(lines) <= max { return lines }
    ranked := append([]domain.LogLine(nil), lines...)
    sort.SliceStable(ranked, func(i, j int) bool {
        if ranked[i].LevelRank() != ranked[j].LevelRank() {
            return ranked[i].LevelRank() > ranked[j].LevelRank()
        }
        return ranked[i].Occurrences > ranked[j].Occurrences
    })
    kept := ranked[:max]
    sort.SliceStable(kept, func(i, j int) bool { return kept[i].Timestamp.Before(kept[j].Timestamp) })
    return kept
}
```

Two sorts, and both are necessary:

1. Sort by severity, then by occurrence count → **select** the most important lines.
2. Re-sort the selection by timestamp → **present** them as a readable timeline.

Selection order and presentation order are different problems. Handing a model a severity-sorted
list would destroy the causal sequence it needs to reason about.

`append([]domain.LogLine(nil), lines...)` copies first, so the caller's slice is not reordered —
a stage that silently permutes its input would be an unpleasant surprise.

### Why four processors instead of one function

- Each is independently understandable and testable.
- The order is configuration (`WT_SANITIZE_CHAIN`), so you can experiment without a rebuild.
- A team can drop one (`dedupe,cap`) or add a redaction-aware processor without touching the
  others.
- Each has a single reason to change.

## The pattern shared by every adapter in this part

```go
func init() {
    wire.LogProcessors.Register("dedupe", func(e *wire.Env) (ports.LogProcessor, error) {
        return Deduper{StripDigits: e.Cfg.Clean.StripDigits}, nil
    })
}
```

`init()` → registry → factory reads what it needs from `e.Cfg` → returns the port. Every adapter
in the codebase looks like this, which means learning one teaches you all of them.

---

Next: **Part 21 — the RAG and cognition adapters.**
