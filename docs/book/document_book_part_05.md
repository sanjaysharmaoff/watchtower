# Part 5 — Loki and LogQL, From Zero

Alerts tell you *what*. Logs are how you find out *why*. Loki is where this project gets
them.

## Metrics vs logs vs traces

The "three pillars of observability":

- **Metrics** — numbers over time. Cheap, aggregatable, no detail. "Memory is at 98%."
- **Logs** — timestamped text events. Expensive, detailed. "fatal error: out of memory
  (heap 512MiB, limit 512MiB)".
- **Traces** — the path of one request across services, with timing per hop.

WatchTower uses metrics as the *trigger* and logs plus Kubernetes events as the
*evidence*. Traces are not implemented, but the `ContextProvider` interface is exactly
where a tracing backend would plug in — a good answer to "how would you extend this?"

## What Loki is

Loki is a log aggregation system from Grafana Labs, deliberately designed to be like
Prometheus but for logs. The pitch is: **index labels, not content.**

Traditional systems like Elasticsearch build a full-text index over every word in every
log line. That makes arbitrary search fast but is expensive in CPU, RAM and disk. Loki
instead indexes only a small set of labels (`namespace`, `app`, `pod`) and stores the
log text as compressed chunks. To search content, it fetches the matching chunks and
greps them.

Consequences you should be able to state:

- Ingestion is cheap, storage is cheap, operations are simpler.
- Queries **must** be anchored by labels. You cannot search "all logs everywhere for
  this word" efficiently — you first narrow to a stream.
- That constraint fits alert-driven investigation perfectly: an alert already tells you
  the namespace and service, so you always have a label anchor.

## Streams

A **stream** is one unique label combination, and its log lines in time order:

```
{namespace="checkout", app="checkout-api", pod="checkout-api-7d9c6f8b4-x2ktz"}
  2026-07-30T00:05:01Z  level=error msg="fatal error: runtime: out of memory ..."
  2026-07-30T00:05:03Z  level=warn  msg="gc pressure high: 94% of heap in use ..."
```

Same cardinality warning as Prometheus: do not put request ids in labels.

## LogQL in five minutes

LogQL is Loki's query language. It has two halves.

**1. Stream selector** (mandatory, in braces):

```logql
{namespace="checkout", app="checkout-api"}
```

**2. Line filters** (optional, applied to the text):

```logql
{namespace="checkout"} |= "error"                    # contains
{namespace="checkout"} != "healthz"                  # does not contain
{namespace="checkout"} |~ "(?i)(error|fatal|panic)"  # matches regex, case-insensitive
```

There are also parsers (`| json`, `| logfmt`) and metric queries
(`rate({app="x"} |= "error" [5m])`) that turn logs into numbers. WatchTower uses only
the selector plus a regex line filter, which is all it needs.

## The query WatchTower builds

From `internal/adapters/enrich/loki/loki.go`:

```go
selectors := make([]string, 0, 2)
if ns != "" {
    selectors = append(selectors, fmt.Sprintf(`namespace=%q`, ns))
}
if svc != "" {
    selectors = append(selectors, fmt.Sprintf(`app=%q`, svc))
}
if len(selectors) == 0 {
    return ""            // no labels to anchor on: give up rather than scan everything
}
return fmt.Sprintf(`{%s} |~ "(?i)(error|fatal|panic|fail|timeout|refused|exception|denied)"`,
    strings.Join(selectors, ", "))
```

For the demo incident this produces:

```logql
{namespace="checkout", app="checkout-api"} |~ "(?i)(error|fatal|panic|fail|timeout|refused|exception|denied)"
```

The namespace and service come from the alert's own labels. **The alert tells the query
where to look.** That is the whole trick of automated enrichment.

Three design details worth explaining:

- **It refuses to run without a label anchor.** No namespace and no service means an
  unbounded query, so the provider returns an error instead. The pipeline treats a
  failed provider as a note in the trace, not a failure.
- **The regex pre-filters at the source.** Loki does the grep, so only interesting lines
  cross the network. Cheaper than fetching everything and filtering in Go.
- **Operators can override the whole query** with `WT_LOKI_QUERY`, using `{{namespace}}`,
  `{{service}}` and `{{cluster}}` placeholders. Every organisation labels differently,
  so the fallback is a guess and the override is the escape hatch.

## The HTTP call

Loki's read API, `GET /loki/api/v1/query_range`:

```
/loki/api/v1/query_range
  ?query={namespace="checkout", app="checkout-api"} |~ "(?i)(error|...)"
  &start=1785970000000000000      # nanoseconds since epoch
  &end=1785970900000000000
  &limit=150
  &direction=backward             # newest first
```

Nanosecond timestamps are Loki's convention — a common source of "why is my query
empty" confusion. The window comes from `WT_ENRICH_LOOKBACK` (default 15m) measured
backwards from now, and `limit` from `WT_ENRICH_LOG_LIMIT`.

Two headers matter in real deployments:

- `X-Scope-OrgID` — Loki's multi-tenancy header, set from `WT_LOKI_TENANT`.
- HTTP basic auth, set from `WT_LOKI_USERNAME` / `WT_LOKI_PASSWORD` (Grafana Cloud uses
  this).

## The response, and normalisation

```json
{ "data": { "result": [
  { "stream": { "namespace": "checkout", "app": "checkout-api", "pod": "checkout-api-7d9c..." },
    "values": [ ["1785970812000000000", "level=error msg=\"fatal error: out of memory\""] ] }
] } }
```

`values` is an array of `[timestamp_string, line_string]` pairs. WatchTower converts each
into a `domain.LogLine`, and does one clever small thing:

```go
func detectLevel(line string) string {
    lower := strings.ToLower(line)
    for _, level := range []string{"fatal", "panic", "error", "warn", "info", "debug"} {
        if strings.Contains(lower, level) {
            return level
        }
    }
    return ""
}
```

Loki lines are frequently unstructured text with the severity embedded. Sniffing the
level out means the sanitizer can *rank* lines later ("keep warnings and above"). Without
it, every line would look equally important.

## Why the demo pushes its own logs

The compose stack runs a small Python container (`deploy/logs/push_logs.py`) that POSTs
to Loki's `/loki/api/v1/push` every five seconds. It writes realistic lines: OOM errors,
GC pressure warnings, connection refused, retries, health checks, each with a random
`req_id=...`, and one line containing an accidentally logged database password.

That is not decoration. It gives:

- **Duplicates with varying ids**, so the deduplicator has real work (150 lines → 3).
- **Mixed levels**, so the signal filter has something to discard.
- **A real secret**, so the input guardrail can be *proved* rather than claimed.

In production this role is played by Promtail, Fluent Bit or Grafana Alloy, which tail
container log files and push them to Loki. WatchTower does not care which — it only
reads.

## ELK: what it is and why it is not here

ELK is Elasticsearch + Logstash + Kibana, the older and heavier alternative. The original
design sketch said "Loki / ELK". Only Loki is implemented.

Say this in an interview, because it demonstrates you understand your own architecture:

> ELK is not implemented. It would be one more `ContextProvider`: a `Name()` and a
> `Fetch()` that queries `_search` instead of `query_range` and maps hits into
> `domain.LogLine`. Roughly 80 lines and one line in the plugin list. Nothing else in
> the system would change, because the pipeline only knows the interface.

## Hands-on

```bash
# is Loki alive?
curl http://localhost:3100/ready

# what WatchTower asks, by hand (URL-encode the query)
curl -G http://localhost:3100/loki/api/v1/query_range \
  --data-urlencode 'query={namespace="checkout"} |~ "(?i)error"' \
  --data-urlencode "start=$(( ($(date +%s) - 900) * 1000000000 ))" \
  --data-urlencode "limit=5"
```

## Interview answers to have ready

**"Why Loki over Elasticsearch?"**
Loki indexes labels rather than content, so ingestion and storage are much cheaper and
operations are simpler. The tradeoff is that queries must be anchored by labels, which
is fine here because an alert always tells me the namespace and service. If a team
already runs Elasticsearch, it is one more provider implementing the same interface.

**"What if Loki is down or slow?"**
The enrich stage fans out to all providers in parallel under one timeout
(`WT_ENRICH_TIMEOUT`, default 8s). A provider that fails or hangs contributes an error
note to the incident trace, and the pipeline continues with whatever else came back. The
model then works from fewer logs and the confidence score drops, because evidence depth
is one of its inputs. Losing logs must never lose the incident.

**"How do you keep log volume from blowing up your prompt?"**
Three layers: the regex filter runs inside Loki so less crosses the network; `limit`
caps the fetch at 150; then the sanitizer chain deduplicates, filters by level and
keyword, compresses and hard-caps at 60 lines. Measured result on the real stack: 150 in,
3 out.

---

Next: **Part 6 — Kubernetes**, the largest technology in the stack and the one most
likely to be probed.
