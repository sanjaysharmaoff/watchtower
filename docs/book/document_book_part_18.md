# Part 18 — Code Walkthrough: Stages, Part A (admit → sanitize)

The first five stages are the cheap ones. Their entire job is to make sure the expensive
stages run as rarely as possible, on as little data as possible. That framing is worth stating
in an interview: **the pipeline is ordered by cost.**

## `stages.go` — the shared shape

```go
type Stage = pipeline.Stage[*domain.Incident]
type Chain = pipeline.Pipeline[*domain.Incident]

func mutator(name string, fn func(ctx context.Context, inc *domain.Incident) error) Stage {
    return pipeline.Of(name, func(ctx context.Context, inc *domain.Incident) (*domain.Incident, error) {
        if inc == nil {
            return nil, domain.Drop("nil incident")
        }
        if err := fn(ctx, inc); err != nil {
            return inc, err
        }
        return inc, nil
    })
}
```

`Stage` and `Chain` are **type aliases** (`=`), not new types, so they are interchangeable with
the generic originals — purely a readability win.

`mutator` captures the common shape: check nil once, run the logic, return the same pointer.
Every stage below is a few lines because this boilerplate exists in one place.

## 1. `Admit` — the cheapest possible filter

```go
type AdmitOptions struct {
    DropResolved bool
    MinSeverity  domain.Severity
}

func Admit(opts AdmitOptions) Stage {
    return mutator("admit", func(_ context.Context, inc *domain.Incident) error {
        if len(inc.Alerts) == 0 {
            return domain.Drop("no alerts in payload")
        }
        if opts.DropResolved {
            firing := inc.Alerts[:0]
            for _, a := range inc.Alerts {
                if !a.IsResolved() {
                    firing = append(firing, a)
                }
            }
            inc.Alerts = firing
            if len(inc.Alerts) == 0 {
                return domain.Drop("all alerts resolved")
            }
            inc.Recompute()
        }
        if floor := opts.MinSeverity.Rank(); floor > 0 && inc.Severity.Rank() < floor {
            return domain.Drop("severity %s below floor %s", inc.Severity, opts.MinSeverity)
        }
        return nil
    })
}
```

Three things to notice.

**`firing := inc.Alerts[:0]`** is the standard Go filter-in-place idiom. It reuses the existing
backing array — zero allocation — because appending to a zero-length slice that shares the
original's storage overwrites it as it goes. Worth being able to explain: it is safe here
because the write index never overtakes the read index.

**Resolved alerts are dropped, then `Recompute()` is called.** Removing alerts can change the
severity, the service list and the signature, so the derived fields must be refreshed. Forget
this and you would debounce against a signature that no longer describes the incident.

**`floor > 0 &&`** means `WT_MIN_SEVERITY=unknown` disables the check rather than dropping
everything. Config that says "no floor" should not mean "reject all".

Cost: microseconds, no I/O. This is the correct first stage.

## 2. `Debounce` — the money-saver

```go
func Debounce(gate ports.Gate, window time.Duration, keyOf func(*domain.Incident) string) Stage {
    if keyOf == nil {
        keyOf = func(inc *domain.Incident) string { return inc.Signature }
    }
    return mutator("debounce", func(ctx context.Context, inc *domain.Incident) error {
        key := keyOf(inc)
        allowed, err := gate.Allow(ctx, key, window)
        if err != nil {
            // Fail open: a broken gate must never stop real incidents.
            inc.Note("debounce gate error, failing open: %v", err)
            return nil
        }
        if !allowed {
            return domain.Drop("duplicate signature %s within %s", key, window)
        }
        inc.SetMeta("debounce_key", key)
        return nil
    })
}
```

The **entire decision lives in `ports.Gate`**, so in-memory and Redis behave identically from
here. This stage contains no map, no TTL logic and no Redis knowledge.

`keyOf` is an injected function with a sensible default. So debouncing by something other than
signature — per-service, per-cluster — is a one-line change at the call site, no new stage.

**Fail open** is the important line, and the comment states the reasoning. If Redis is down,
the choice is "possibly duplicate emails" versus "no emails at all". During an infrastructure
outage — exactly when alerts matter most — silence is the worse failure. Being able to explain
*which direction* you chose to fail and *why* is what separates a considered design from an
accident.

Verified behaviour:

```
level=INFO msg="stage dropped payload" stage=debounce
  reason="dropped: duplicate signature 65a937f408edc78e within 1h0m0s"
```

Logged at INFO, not ERROR, because it is the system working.

## 3. `Correlate`

```go
func Correlate(c ports.Correlator) Stage {
    return mutator("correlate", func(ctx context.Context, inc *domain.Incident) error {
        if err := c.Correlate(ctx, inc); err != nil {
            inc.Note("correlation skipped: %v", err)
        }
        return nil
    })
}
```

Four lines, and note it **swallows the error into a note**. Failed correlation means a
less-complete incident, not a lost one. That is a recurring pattern: enrichment-type failures
degrade, never abort.

The algorithm lives in the adapter (Part 20).

## 4. `Enrich` — the concurrency

The most interesting stage mechanically.

```go
func Enrich(providers []ports.ContextProvider, timeout time.Duration) Stage {
    return mutator("enrich", func(ctx context.Context, inc *domain.Incident) error {
        if len(providers) == 0 {
            inc.Note("no context providers configured")
            return nil
        }
        if timeout > 0 {
            var cancel context.CancelFunc
            ctx, cancel = context.WithTimeout(ctx, timeout)
            defer cancel()
        }

        var (
            mu        sync.Mutex
            fragments = make([]domain.ContextFragment, 0, len(providers))
            wg        sync.WaitGroup
        )

        for _, p := range providers {
            wg.Add(1)
            go func(p ports.ContextProvider) {
                defer wg.Done()
                defer func() {
                    if r := recover(); r != nil {
                        mu.Lock()
                        fragments = append(fragments, domain.ContextFragment{Provider: p.Name(), Err: "panic during fetch"})
                        mu.Unlock()
                    }
                }()

                start := time.Now()
                frag, err := p.Fetch(ctx, inc)
                frag.Provider = p.Name()
                frag.Took = time.Since(start)
                if err != nil {
                    frag.Err = err.Error()
                }
                mu.Lock()
                fragments = append(fragments, frag)
                mu.Unlock()
            }(p)
        }
        wg.Wait()

        // Deterministic order regardless of which provider answered first.
        sort.Slice(fragments, func(i, j int) bool { return fragments[i].Provider < fragments[j].Provider })
        for _, f := range fragments {
            inc.AddFragment(f)
            if f.Err != "" {
                inc.Note("provider %s failed: %s", f.Provider, f.Err)
                continue
            }
            inc.Note("provider %s: %d logs, %d events in %s", f.Provider, len(f.Logs), len(f.Events), f.Took.Round(time.Millisecond))
        }

        sort.SliceStable(inc.Logs, func(i, j int) bool { return inc.Logs[i].Timestamp.Before(inc.Logs[j].Timestamp) })
        sort.SliceStable(inc.Events, func(i, j int) bool { return inc.Events[i].Timestamp.Before(inc.Events[j].Timestamp) })
        return nil
    })
}
```

Six things an interviewer might probe:

**Fan-out with one shared timeout.** Loki and Kubernetes run concurrently, so total latency is
the slowest provider, not the sum. Measured: 13ms and 11ms in parallel. The `context.WithTimeout`
is derived once and passed to all of them, so `WT_ENRICH_TIMEOUT` bounds the *stage*, not each
provider.

**`go func(p ports.ContextProvider)`** — the loop variable is passed as a parameter. In Go
versions before 1.22 the loop variable was shared across iterations and this would be a classic
bug. Go 1.22 changed the semantics, so it is no longer required, but passing it explicitly is
clearer and version-proof. Being able to discuss that is a nice Go-specific detail.

**Per-goroutine `recover`.** A panicking provider becomes a fragment with `Err: "panic during
fetch"`. Without it, a panic in a goroutine takes down the whole process — `recover` in the
parent cannot catch a child goroutine's panic. That is a genuinely important Go fact.

**Mutex-guarded append.** Slice append is not safe for concurrent use; growing reallocates.
The critical section is two lines, which is why a mutex beats a channel here.

**Deterministic ordering.** Goroutines finish in random order, so fragments are sorted by
provider name before being folded in. Without it, the incident's log ordering — and therefore
the prompt, and therefore the model's output — would vary run to run for identical input.
Non-determinism you did not need is a debugging tax.

**Chronological merge.** Logs and events from all providers are sorted by timestamp, so the
model reads one interleaved timeline rather than "all Loki lines, then all Kubernetes events".
Causality is easier to see in time order. `SliceStable` preserves the relative order of equal
timestamps.

## 5. `Sanitize` — where the cost collapses

```go
func Sanitize(processors []ports.LogProcessor) Stage {
    return mutator("sanitize", func(_ context.Context, inc *domain.Incident) error {
        before := len(inc.Logs)
        lines := inc.Logs
        for _, p := range processors {
            lines = p.Process(lines)
        }
        inc.Logs = lines
        inc.SetMeta("logs_in", itoa(before))
        inc.SetMeta("logs_out", itoa(len(lines)))
        if before > 0 {
            inc.Note("sanitizer kept %d of %d log lines", len(lines), before)
        }
        return nil
    })
}
```

The stage is a **fold over a list of processors**. It contains no cleaning logic at all — the
four processors do the work (Part 20), and their order comes from `WT_SANITIZE_CHAIN`.

The `logs_in` / `logs_out` metadata is how the compression claim became a measurement:

```json
"meta": { "logs_in": "150", "logs_out": "3" }
"trace": [ "sanitizer kept 3 of 150 log lines" ]
```

That is a **50× reduction** in what reaches storage and the prompt. If someone asks "how do you
control token cost", this is the number to quote.

Note `itoa` is a tiny local helper rather than `strconv.Itoa` — a small stylistic quirk of the
package, worth knowing so you are not surprised by it.

## Cost ordering, restated

| Stage | Measured | I/O |
| --- | --- | --- |
| admit | 0s | none |
| debounce | 0s | one Redis SETNX at most |
| correlate | 0s | none (in-memory window) |
| enrich | 22ms | parallel HTTP to Loki + Kubernetes |
| sanitize | 3ms | none, pure CPU |
| retrieve | 703ms | one embedding call + one vector query |
| plan | 61s | up to four model calls |

Everything before `retrieve` costs approximately nothing. Every alert that gets dropped by
`admit` or `debounce`, and every log line removed by `sanitize`, is money not spent in the last
two rows. **That is the argument for this ordering**, and it is the cost model of the whole
system in one table.

---

Next: **Part 19 — Stages, Part B**: retrieval, guardrails, the model loop and scoring.
