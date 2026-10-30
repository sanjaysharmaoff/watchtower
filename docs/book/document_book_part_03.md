# Part 3 — Prometheus, From Zero

Prometheus is where every incident in this system begins. If you understand only one
external technology deeply, make it this one: interviewers ask about it constantly.

## What Prometheus is

Prometheus is an open-source **monitoring system and time-series database**. It does
three things:

1. **Collects** numbers from your services on a schedule.
2. **Stores** them as time series (a number tagged with labels, over time).
3. **Evaluates rules** over them and fires alerts.

It is a single Go binary with a local database. That simplicity is why it became the
default in the Kubernetes world.

## The pull model (this is an interview favourite)

Most older monitoring systems are **push**: your application sends metrics out to a
collector. Prometheus is **pull**: your application exposes an HTTP endpoint, usually
`/metrics`, and Prometheus fetches it every N seconds. Fetching is called **scraping**.

```
$ curl http://checkout-api:9100/metrics
process_resident_memory_bytes 5.28e+08
http_requests_total{method="GET",status="200"} 48213
up 1
```

Why pull matters:

- Prometheus always knows whether a target is reachable. If a scrape fails, it records
  `up = 0` for that target. In a push system, silence is ambiguous — did the service
  die, or is the network slow, or was it never deployed?
- No application needs to know where the monitoring system lives.
- You can curl the same endpoint by hand to debug.

**This project exploits the pull model directly.** The demo stack points Prometheus at
two targets that do not exist:

```yaml
# deploy/prometheus/prometheus.yml
  - job_name: checkout-api
    static_configs:
      - targets: ["checkout-api.checkout.svc:9100"]
        labels:
          namespace: checkout
          service: checkout-api
          pod: checkout-api-7d9c6f8b4-x2ktz
```

Because nothing answers on that address, `up{job="checkout-api"}` becomes `0` on the
first scrape, which makes the alert rules fire about thirty seconds after startup with
no manual poking. That is how the whole demo is self-triggering.

## Time series, labels and cardinality

A time series is identified by a **metric name plus a set of labels**:

```
up{job="checkout-api", namespace="checkout", service="checkout-api", cluster="prod-eu-1"}
```

Each unique label combination is a **separate series**. Labels are how you slice data
("show me errors per service") and they are how alerts carry context — the labels on
the series become the labels on the alert, which become the fields WatchTower reads.

**Cardinality** is the number of distinct series. Putting something high-variety in a
label — a user id, a request id, a timestamp — multiplies series count and can take
Prometheus down. The rule: labels are for things with few, stable values.

This matters for WatchTower too. It is exactly why the log sanitizer erases request ids
before comparing lines (Part 20): high-variety fields make otherwise-identical things
look unique.

## PromQL in five minutes

PromQL is the query language. You only need to recognise a few shapes.

```promql
up                                  # every up series
up{job="checkout-api"}              # filtered by label
up{job="checkout-api"} == 0         # a boolean condition — this is what an alert uses
rate(http_requests_total[5m])       # per-second rate over a 5 minute window
sum by (service) (rate(errors_total[5m]))   # aggregate, keeping the service label
histogram_quantile(0.99, ...)       # p99 latency
```

A rule fires **for every series the expression returns**. If ten pods match, you get ten
alerts. That is one source of the flood WatchTower absorbs.

## Alert rules

Rules live in YAML. Here is a real one from this project, annotated:

```yaml
# deploy/prometheus/rules/watchtower-demo.rules.yml
- alert: ContainerOOMKilled              # the alert's name
  expr: up{job="checkout-api"} == 0      # the condition
  for: 15s                               # must stay true this long before firing
  labels:
    severity: critical                   # extra labels attached to the alert
  annotations:
    summary: "{{ $labels.service }} container OOM killed, memory limit reached"
    description: >-
      Container {{ $labels.service }} in namespace {{ $labels.namespace }} exceeded its
      memory limit and was terminated by the kernel...
    runbook_url: https://wiki.example.com/runbooks/oom
```

Four concepts to be able to explain:

- **`expr`** — the condition. When it returns data, the rule is active.
- **`for`** — the debounce built into Prometheus itself. `for: 15s` means the condition
  must hold for 15 continuous seconds before the alert fires. This suppresses momentary
  blips. Real production values are usually 5–10 minutes.
- **`labels`** — machine-readable metadata. `severity` is the important one: WatchTower
  maps it onto its own severity scale and uses it for the `WT_MIN_SEVERITY` floor.
- **`annotations`** — human-readable text. `{{ $labels.x }}` is Go template syntax, so
  the message gets the actual service name substituted in. WatchTower reads `summary`,
  `description` and `runbook_url` from here.

### Pending vs firing

Between "condition became true" and "`for` duration elapsed" the alert is **pending**.
After that it is **firing**. Only firing alerts get sent onward. You can see both states
at `http://localhost:9090/alerts`.

## external_labels

```yaml
global:
  external_labels:
    cluster: prod-eu-1
```

These labels are attached to everything leaving this Prometheus instance. In a company
with one Prometheus per cluster, `cluster` is how you know which one an alert came from.
WatchTower reads it as part of an alert's "target" (`cluster/namespace/service`) and
uses it as a correlation key.

## The three rules in this project, and why there are three

```yaml
- alert: ContainerOOMKilled          expr: up{job="checkout-api"} == 0   severity: critical
- alert: PodRestartRateHigh          expr: up{job="checkout-api"} == 0   severity: warning
- alert: DependencyUnreachable       expr: up{job="payments-api"} == 0   severity: warning
```

All three fire off essentially the same condition on purpose. It creates the exact
situation WatchTower exists for: **several differently-named alerts, of different
severities, across two services, at the same time.** That is what exercises correlation
and severity selection. Two more rules were added later during testing to produce a
*new* incident signature that would pass the debouncer.

## What Prometheus does not do

Prometheus evaluates and fires. It does not group, deduplicate, silence, route or
notify. That is a separate component — Alertmanager — which is Part 4.

## Hands-on: what to click

With the stack running:

- `http://localhost:9090/targets` — every scrape target and its state. The two
  fabricated ones will be red with "connection refused". That is intended.
- `http://localhost:9090/alerts` — pending and firing alerts.
- `http://localhost:9090/graph` — type `up` and execute. You will see `1` for
  Prometheus itself and `0` for the fake targets.
- Reload rules without restarting: `curl -X POST http://localhost:9090/-/reload`
  (works because the container runs with `--web.enable-lifecycle`).

## Interview answers to have ready

**"Why Prometheus rather than a push system?"**
Pull gives you liveness for free — a failed scrape is itself a signal, `up == 0`. It
also decouples applications from the monitoring topology. The tradeoff is that
Prometheus must be able to reach every target, which is awkward for short-lived jobs;
that is what the Pushgateway exists for.

**"What is cardinality and why do you care?"**
The number of distinct label combinations. Each one is a stored series, so a
high-variety label like a request id can explode memory. In WatchTower the same idea
appears in log deduplication: request ids are normalized away before comparison,
because otherwise identical lines look unique.

**"How does your system avoid being overwhelmed by 900 rules?"**
Three layers. Prometheus `for:` clauses suppress blips. Alertmanager groups related
alerts into one webhook. Then WatchTower debounces by incident signature for an hour and
correlates by topology, so a cascade becomes one incident and one email.

---

Next: **Part 4 — Alertmanager**, which decides what actually reaches WatchTower.
