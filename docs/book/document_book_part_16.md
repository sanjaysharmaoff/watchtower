# Part 16 — Code Walkthrough: the `domain` Package

The code walkthrough starts here, at the centre of the onion. `internal/domain` is the
vocabulary of the system: the nouns everything else passes around.

**The rule for this package:** standard library only. No HTTP, no SQL, no model providers,
no config. You can verify it:

```bash
go list -deps ./internal/domain | Select-String "watchtower"
# → only the domain package itself
```

Five files: `alert.go`, `telemetry.go`, `incident.go`, `runbook.go`, `plan.go`,
`feedback.go`, `errors.go`.

## `Severity` — normalising a mess

```go
type Severity string

const (
    SeverityCritical Severity = "critical"
    SeverityWarning  Severity = "warning"
    SeverityInfo     Severity = "info"
    SeverityUnknown  Severity = "unknown"
)

func (s Severity) Rank() int {
    switch strings.ToLower(string(s)) {
    case "critical", "crit", "fatal", "emergency", "page", "p1": return 3
    case "warning", "warn", "major", "p2":                       return 2
    case "info", "notice", "low", "p3":                          return 1
    default:                                                     return 0
    }
}

func ParseSeverity(v string) Severity { ... }
```

Two ideas here:

**A string type, not an enum of ints.** It serialises to readable JSON and SQL, and unknown
values from the outside world survive round-tripping instead of becoming `3`.

**`Rank()` makes it orderable.** Comparing strings tells you nothing; `Rank()` is what lets
the system pick the most severe alert in a group and enforce a severity floor. Real
Prometheus deployments use `crit`, `P1`, `page` interchangeably, and `ParseSeverity` collapses
all of them.

## `Alert`

```go
type Alert struct {
    Fingerprint string            `json:"fingerprint,omitempty"`
    Name        string            `json:"name"`
    Status      string            `json:"status,omitempty"`
    Severity    Severity          `json:"severity"`
    Service     string            `json:"service,omitempty"`
    Namespace   string            `json:"namespace,omitempty"`
    Cluster     string            `json:"cluster,omitempty"`
    Summary     string            `json:"summary,omitempty"`
    Description string            `json:"description,omitempty"`
    RunbookURL  string            `json:"runbook_url,omitempty"`
    Labels      map[string]string `json:"labels,omitempty"`
    Annotations map[string]string `json:"annotations,omitempty"`
    StartsAt    time.Time         `json:"starts_at,omitempty"`
    EndsAt      time.Time         `json:"ends_at,omitempty"`
    Source      string            `json:"source,omitempty"`
}
```

Note the design: the fields WatchTower *reasons about* are promoted to real fields
(`Severity`, `Service`, `Namespace`, `Cluster`), while the full label and annotation maps are
kept for anything else. Promotion means the pipeline never does string lookups for the common
case; keeping the maps means no information is thrown away.

### Behaviour on the type, not in a service

```go
func (a Alert) Identity() string {
    if a.Fingerprint != "" { return a.Fingerprint }
    return fmt.Sprintf("%s|%s|%s|%s", a.Name, a.Cluster, a.Namespace, a.Service)
}
```

Prefer the source's own stable hash; fall back to a composite key. This is the deduplication
key used when merging correlated alerts.

```go
func (a Alert) Target() string {
    parts := make([]string, 0, 3)
    for _, p := range []string{a.Cluster, a.Namespace, a.Service} {
        if p != "" { parts = append(parts, p) }
    }
    if len(parts) == 0 { return "unknown" }
    return strings.Join(parts, "/")
}
```

`prod-eu-1/checkout/checkout-api`. Used in the email subject, the prompt header, and the RAG
query. **The stub planner parses it back out** to build `kubectl -n <ns>` commands — so this
one function is the canonical "where is this burning" format.

```go
func (a Alert) Label(keys ...string) string {
    for _, k := range keys {
        if v, ok := a.Labels[k]; ok && v != "" { return v }
        if v, ok := a.Annotations[k]; ok && v != "" { return v }
    }
    return ""
}
```

Variadic first-non-empty lookup across both maps. Callers write
`primary.Label("service", "app", "job", "deployment", "container")` and stop caring how a
particular team labels things. Used by the Loki and Kubernetes providers and by the correlator.

```go
func TopSeverity(alerts []Alert) Severity     // highest Rank() in the set
func ServicesOf(alerts []Alert) []string      // sorted unique services
```

`ServicesOf` **sorts**, which matters: it feeds the incident signature, and an unsorted set
would produce a different hash for the same group depending on arrival order.

## `telemetry.go` — logs and events

```go
type LogLine struct {
    Timestamp   time.Time         `json:"ts"`
    Level       string            `json:"level,omitempty"`
    Message     string            `json:"message"`
    Source      string            `json:"source,omitempty"`
    Occurrences int               `json:"occurrences,omitempty"`
    Labels      map[string]string `json:"labels,omitempty"`
}
```

`Occurrences` is the interesting field. The deduplicator collapses identical lines and
**counts** them, so "this happened 47 times" survives compression. Losing that would lose the
most diagnostic fact in the log.

```go
func (l LogLine) String() string {
    ...
    line := strings.TrimSpace(fmt.Sprintf("%s %s %s", ts, strings.ToUpper(l.Level), l.Message))
    if l.Occurrences > 1 {
        line = fmt.Sprintf("%s (x%d)", line, l.Occurrences)
    }
    return line
}
```

One rendering used by the prompt, the plain-text email and the HTML email. One format, three
consumers, no drift.

```go
func (l LogLine) LevelRank() int {
    switch strings.ToLower(strings.Trim(l.Level, "[]")) {
    case "fatal", "panic", "critical", "crit": return 5
    case "error", "err", "eror":               return 4
    case "warn", "warning":                    return 3
    case "info", "notice":                     return 2
    case "debug", "trace":                     return 1
    default:                                   return 0
    }
}
```

Same trick as severity: text becomes orderable, which is what lets the sanitizer keep
"warnings and above" and lets `TopLines` pick the most severe lines for the RAG query. Note
`strings.Trim(l.Level, "[]")` — real logs write `[ERROR]`. And `"eror"`, a common typo, is
mapped deliberately.

```go
type ClusterEvent struct {
    Timestamp time.Time
    Type      string   // Normal | Warning
    Reason    string   // OOMKilling, BackOff, Unhealthy
    Object    string   // pod/checkout-api-...
    Message   string
    Source    string
    Count     int
}
```

Named `ClusterEvent`, not `K8sEvent`, on purpose: the type describes the *concept*, so a Nomad
or ECS provider can produce them without the name lying. Naming domain types after the
concept rather than the current vendor is a small discipline that pays off.

```go
type ContextFragment struct {
    Provider string
    Logs     []LogLine
    Events   []ClusterEvent
    Notes    map[string]string
    Err      string            // ← a failed provider returns a fragment carrying Err
    Took     time.Duration
}
```

**`Err` as a field, not a returned error.** This is the shape that makes partial enrichment
work: a provider that fails contributes a fragment saying so, and the pipeline keeps going.
The failure is data about the incident, not control flow.

## `Incident` — the envelope

```go
type Incident struct {
    ID             string
    CorrelationKey string
    Signature      string
    Severity       Severity
    Services       []string
    Alerts         []Alert
    Logs           []LogLine
    Events         []ClusterEvent
    Fragments      []ContextFragment
    Matches        []RunbookMatch
    Prompt         Prompt         `json:"-"`
    Plan           *Plan
    Verdict        *Verdict
    Attempts       int
    Confidence     float64
    Meta           map[string]string
    Trace          []string
    CreatedAt      time.Time
}
```

This one struct is the pipeline's payload. Reading it top to bottom tells you what the system
does: alerts come in, evidence is attached, matches are retrieved, a plan and a verdict are
produced, a confidence is computed, and `Meta`/`Trace` record how.

Two field-level decisions:

**`Prompt` has `json:"-"`.** It is derived from the other fields and would double the stored
payload size for no benefit.

**`Plan` and `Verdict` are pointers.** `nil` means "this never happened", which is
meaningfully different from an empty plan. Every consumer checks `if inc.Plan == nil`, and
that distinction is what lets the guard, scorer and renderer all handle a model failure
gracefully.

### The signature

```go
func (i *Incident) computeSignature() string {
    names := make([]string, 0, len(i.Alerts))
    for _, a := range i.Alerts {
        names = append(names, strings.ToLower(a.Name))
    }
    sort.Strings(names)
    seed := strings.Join(names, ",") + "|" + strings.Join(i.Services, ",")
    sum := sha256.Sum256([]byte(seed))
    return hex.EncodeToString(sum[:])[:16]
}
```

The signature is **the identity of a kind of failure**, not of an occurrence. Same alert names
plus same services equals same signature, e.g. `65a937f408edc78e`. It is used for:

- **Debouncing** — the gate key. Same failure within the window is dropped.
- **Learning** — stored on an approved runbook, and the scorer awards a bonus when a retrieved
  runbook's signature matches the current incident exactly.

Sorting before hashing is essential (order-independence), and lowercasing absorbs
capitalisation differences. Truncating SHA-256 to 16 hex characters is 64 bits — plenty
against accidental collision, and short enough to read in a log line.

Note what is deliberately *not* in the signature: pod names, timestamps, log content. Those
change every occurrence; including them would defeat the whole purpose.

### `Recompute` and `MergeAlerts`

```go
func (i *Incident) Recompute() {
    i.Severity = TopSeverity(i.Alerts)
    i.Services = ServicesOf(i.Alerts)
    i.Signature = i.computeSignature()
    if i.CorrelationKey == "" { i.CorrelationKey = i.Signature }
}
```

Derived fields are recomputed rather than maintained by hand, so they cannot drift after the
alert set changes.

```go
func (i *Incident) MergeAlerts(extra []Alert) int {
    known := make(map[string]struct{}, len(i.Alerts))
    for _, a := range i.Alerts { known[a.Identity()] = struct{}{} }
    added := 0
    for _, a := range extra {
        if _, dup := known[a.Identity()]; dup { continue }
        known[a.Identity()] = struct{}{}
        i.Alerts = append(i.Alerts, a)
        added++
    }
    if added > 0 { i.Recompute() }
    return added
}
```

Idempotent merge, returning how many were actually new — which is what the correlator reports
in the trace ("correlated 2 recent alert(s)"). `map[string]struct{}` is the idiomatic Go set:
`struct{}` occupies zero bytes.

### The self-documenting trace

```go
func (i *Incident) Note(format string, args ...any) {
    i.Trace = append(i.Trace, fmt.Sprintf(format, args...))
}

func (i *Incident) SetMeta(key, value string) { ... }
```

This is one of the most useful things in the codebase. The incident carries its own
explanation:

```json
"trace": [
  "provider k8s: 0 logs, 19 events in 13ms",
  "provider loki: 150 logs, 0 events in 11ms",
  "sanitizer kept 3 of 150 log lines",
  "retrieved 1 runbook(s) above score 0.20",
  "critic rejected draft 1: A command references a resource that does not appear in the context",
  "critic approved draft on attempt 2",
  "risk classifier corrected 2 self-declared risk level(s)"
],
"meta": { "logs_in": "150", "logs_out": "3", "confidence": "0.8",
          "redactors": "secrets+pii+custom", "blocked_commands": "0", "risk_relabelled": "2" }
```

Why this beats logging: it is stored *with* the incident, so debugging one incident three days
later needs one SQL query rather than log correlation by request id. It also ships to the
reviewer in the email's collapsible "Pipeline trace" section — the human can see *why* the
system concluded what it did. Observability that travels with the payload.

### Convenience accessors

```go
func (i *Incident) PrimaryAlert() Alert           // highest severity; safe on empty
func (i *Incident) Title() string                 // "summary (+2 related)"
func (i *Incident) AddFragment(f ContextFragment) // append + fold logs/events up
func (i *Incident) TopMatch() (RunbookMatch, bool)
```

`PrimaryAlert` returns a usable zero value rather than panicking on an empty set — every
accessor in this package is safe to call on a partially-built incident, because the guard,
scorer and renderer all run on incidents that may have failed mid-pipeline.

## `runbook.go`

```go
type RiskLevel string
const (
    RiskRead      RiskLevel = "read"      // inspect only
    RiskMutate    RiskLevel = "mutate"    // changes state, reversible
    RiskDangerous RiskLevel = "dangerous" // destructive or irreversible
)

type Command struct {
    Run     string
    Explain string
    Risk    RiskLevel
    Blocked bool
    Reason  string
}
```

`Blocked` and `Reason` are the guardrail's output. Crucially, a blocked command is **kept**,
not deleted — the reviewer sees the command *and* why policy refused it. Silently disappearing
a proposal would hide information from the only party qualified to judge it.

```go
type Runbook struct {
    ID, Title, Signature, RootCause string
    Commands   []Command
    Tags       []string
    Labels     map[string]string
    Reviewer   string
    Embedding  []float32 `json:"-"`
    SourceText string    `json:"-"`
    CreatedAt  time.Time
}

func (r Runbook) Text() string { ... }   // canonical text for embedding and reranking
```

`Text()` is the single definition of "what does this runbook say", used for embedding, for the
lexical reranker and for storage in the `content` column. One definition means the vector and
the reranked text can never disagree. `Embedding` is `json:"-"` because 768 floats have no
business in an API response.

## `plan.go`

```go
type Plan struct {
    RootCause    string
    Impact       string
    Steps        []Command
    Verification []string
    Rollback     []string
    Confidence   float64
    Model        string
    Raw          string `json:"-"`
}

func (p *Plan) Executable() []Command { /* steps where !Blocked */ }
func (p *Plan) Blocked() []Command    { /* steps where Blocked */ }
func (p *Plan) Script() string        { /* executable steps as a shell snippet */ }
```

`Executable()` is used in three places, and each one matters:

- the email shows those as the proposed steps
- **only those are eligible to be learned** when the reviewer approves
- the scorer penalises heavily when the list is empty

So a blocked command can never enter the retrieval corpus, even by a careless approval. That
is a nice safety property to point out: the guardrail's decision propagates into the learning
loop automatically, without the feedback service knowing anything about guardrails.

All three methods are **nil-safe** (`if p == nil { return nil }`), which is what lets callers
skip the nil checks.

```go
type Decision string
const (
    DecisionApprove Decision = "approve"
    DecisionEdit    Decision = "edit"
    DecisionReject  Decision = "reject"
)

func ParseDecision(v string) Decision { /* tolerant: approved, accept, ok → approve */ }
func (d Decision) Learnable() bool    { return d == DecisionApprove || d == DecisionEdit }
```

`Learnable()` encodes a product rule in one place: approve and edit teach the system, reject
does not. Note that `ParseDecision` defaults to **reject** for anything unrecognised — fail
closed on the path that writes to the corpus.

## `feedback.go`

`Feedback` (what the reviewer sent), `ApprovedSummary` (the durable record), `Digest` (the
daily report), `Message` (a rendered notification), `DeadLetter` (a parked failure).

```go
func (s ApprovedSummary) AsRunbook() Runbook { ... }
```

One explicit conversion from "what a human decided" to "retrieval material". The learning loop
in one method.

```go
type Message struct {
    To      []string
    Subject string
    Text    string
    HTML    string
    Meta    map[string]string
}
```

Transport-agnostic on purpose: SMTP uses all of it, stdout uses `Text`, a future Slack adapter
would use `Text` and ignore `HTML`. The renderer does not know how the message travels.

```go
type DeadLetter struct {
    ID, Stage, Reason, RefID string
    Payload []byte
    At      time.Time
}
```

`Payload []byte` is the full JSON of whatever failed. That is what makes replay possible
rather than aspirational.

## `errors.go`

```go
var (
    ErrDrop        = errors.New("dropped")
    ErrBlocked     = errors.New("blocked by policy")
    ErrNotFound    = errors.New("not found")
    ErrUnsupported = errors.New("unsupported")
    ErrUnavailable = errors.New("dependency unavailable")
)

func Drop(reason string, args ...any) error  { return fmt.Errorf("%w: %s", ErrDrop, fmt.Sprintf(reason, args...)) }
func Block(reason string, args ...any) error { return fmt.Errorf("%w: %s", ErrBlocked, ...) }

func IsDrop(err error) bool    { return errors.Is(err, ErrDrop) }
func IsBlocked(err error) bool { return errors.Is(err, ErrBlocked) }
```

Sentinel errors plus `%w` wrapping plus helper constructors. The `%w` verb is what allows
`errors.Is` to see through the added context, so callers can classify an error without string
matching. `IsDrop`/`IsBlocked` mean no caller has to know *how* the wrapping works.

`ErrNotFound` is what lets the feedback service distinguish "that incident is gone" from "the
database is broken" — one is a normal case handled quietly, the other is worth a warning.

## Why this package is worth talking about

Say something like:

> The domain package has no dependencies, so it is trivially testable and it can never be
> polluted by a transport or a driver detail. It is also where the interesting rules live —
> the incident signature, the severity ordering, the learnability of a decision — as methods
> on types rather than scattered across services. The types carry their own audit trail, which
> means debugging one incident is one SQL query instead of log correlation.

---

Next: **Part 17 — the `ports` package**, interface by interface.
