# Part 4 — Alertmanager and the Webhook Contract

Prometheus fires alerts. Alertmanager decides what a human (or WatchTower) actually
receives. This part also gives you the exact JSON shape WatchTower parses, which is the
system's front door.

## What Alertmanager is

A separate service, also a single Go binary, that receives alerts from one or more
Prometheus instances and handles delivery. Its jobs:

- **Deduplication** — the same alert from two Prometheus replicas becomes one.
- **Grouping** — several alerts that belong together become one notification.
- **Routing** — different alerts to different destinations (page the DB team, email the
  web team).
- **Silencing** — mute known-noisy alerts for a window, e.g. during maintenance.
- **Inhibition** — suppress lower alerts when a bigger one is firing ("cluster down"
  suppresses "service unreachable").

WatchTower is a **receiver**: a destination Alertmanager sends to.

## The config in this project, line by line

```yaml
# deploy/alertmanager/alertmanager.yml
global:
  resolve_timeout: 2m

route:
  receiver: watchtower
  group_by: [cluster, namespace]
  group_wait: 5s
  group_interval: 30s
  repeat_interval: 1h

receivers:
  - name: watchtower
    webhook_configs:
      - url: http://watchtower:8080/alerts
        send_resolved: true
        max_alerts: 0
        http_config:
          authorization:
            type: Bearer
            credentials: local-dev-token
```

- **`group_by: [cluster, namespace]`** — all alerts sharing a cluster and namespace are
  bundled into one notification. Deliberately coarse here: WatchTower does its own,
  smarter correlation, so the less Alertmanager splits things up, the more context
  arrives in a single payload. That is a design decision worth stating in an interview.
- **`group_wait: 5s`** — after the first alert in a new group, wait 5 seconds for
  siblings before sending. This is why five alerts arrive in one request instead of five.
  Production values are 30s–1m.
- **`group_interval: 30s`** — minimum gap before sending an *updated* notification for a
  group that has changed (a new alert joined).
- **`repeat_interval: 1h`** — if nothing changes, re-send the same group every hour as a
  reminder. Important gotcha while testing: after the first delivery you will not get
  another for an hour unless the group changes, which is why the demo adds a *new rule*
  to trigger a fresh incident rather than waiting.
- **`send_resolved: true`** — also notify when alerts stop firing. WatchTower's `admit`
  stage drops fully-resolved groups (`WT_DROP_RESOLVED=true`), because there is nothing
  to remediate.
- **`max_alerts: 0`** — no cap on how many alerts are packed into one webhook.
- **`authorization`** — sends `Authorization: Bearer local-dev-token`, which satisfies
  WatchTower's `WT_HTTP_AUTH_TOKEN`. This is how the webhook is authenticated.

## The webhook payload

This is the contract. Alertmanager POSTs JSON that looks like this (trimmed):

```json
{
  "version": "4",
  "groupKey": "{}:{alertname=\"PodMemoryHigh\"}",
  "status": "firing",
  "receiver": "watchtower",
  "commonLabels":      { "cluster": "prod-eu-1", "namespace": "checkout", "service": "checkout-api" },
  "commonAnnotations": { "runbook_url": "https://wiki.example.com/runbooks/oom" },
  "alerts": [
    {
      "status": "firing",
      "fingerprint": "8f1c9a2b3d4e5f60",
      "labels":      { "alertname": "ContainerOOMKilled", "severity": "critical", "pod": "checkout-api-7d9c6f8b4-x2ktz" },
      "annotations": { "summary": "checkout-api memory usage at 98% of limit",
                       "description": "Container ... exceeded its memory limit ..." },
      "startsAt": "2026-07-30T09:12:00Z",
      "endsAt":   "0001-01-01T00:00:00Z",
      "generatorURL": "http://prometheus.example.com/graph"
    }
  ]
}
```

Six things to notice, because the decoder handles all of them:

1. **`commonLabels` / `commonAnnotations`** hold values shared by every alert in the
   group; per-alert `labels` / `annotations` hold the rest. A correct decoder must
   **merge** them, with the per-alert values winning.
2. **`fingerprint`** is Alertmanager's stable hash of the alert's label set. It is the
   natural deduplication key, and WatchTower uses it as an alert's identity.
3. **`labels.alertname`** is where the rule's name ends up. There is no `name` field.
4. **`status`** exists at both group and alert level. The alert-level one wins; if
   absent, fall back to the group's.
5. **`endsAt`** is `0001-01-01T00:00:00Z` — Go's zero time serialised — for firing
   alerts. Treating that as a real timestamp is a classic bug.
6. **Severity is a label, not a field.** And it is free-form text: `critical`, `crit`,
   `P1`, `page` all appear in the wild.

## How WatchTower decodes it

`internal/adapters/ingest/decode/alertmanager.go`. The essence:

```go
for _, a := range p.Alerts {
    labels := merge(p.CommonLabels, a.Labels)
    anns   := merge(p.CommonAnnotations, a.Annotations)

    out = append(out, domain.Alert{
        Fingerprint: a.Fingerprint,
        Name:        pick(labels, "alertname", "alert", "rule"),
        Status:      status,
        Severity:    domain.ParseSeverity(pick(labels, "severity", "priority")),
        Service:     pick(labels, "service", "app", "job", "deployment", "container"),
        Namespace:   pick(labels, "namespace", "ns"),
        Cluster:     pick(labels, "cluster", "env", "environment"),
        Summary:     pick(anns, "summary", "title", "message"),
        Description: pick(anns, "description", "detail"),
        RunbookURL:  pick(anns, "runbook_url", "runbook"),
        StartsAt:    a.StartsAt,
        EndsAt:      zeroIfEpoch(a.EndsAt),
        Source:      "alertmanager",
    })
}
```

The interesting part is `pick(labels, "service", "app", "job", "deployment",
"container")`: **first non-empty wins**. Different teams label the same concept
differently, and the decoder absorbs that variance instead of forcing every team to
relabel. Same for severity: `ParseSeverity` maps `crit`, `fatal`, `page`, `p1` all onto
`critical`.

This is the *anti-corruption layer* idea — messy external vocabulary is translated once,
at the edge, into one clean internal type (`domain.Alert`). Nothing downstream ever sees
Alertmanager's shape.

## The second decoder

There is also a `generic` decoder that accepts WatchTower's own `domain.Alert` shape —
a single object, an array, or `{"alerts":[...]}`. It exists so you can test by hand with
curl and so internal producers do not have to imitate Alertmanager. Selected with
`WT_ALERT_DECODER=generic`.

Adding support for Grafana alerting or Datadog webhooks means writing one more
`AlertDecoder`. Nothing else changes. That is the payoff of putting an interface here.

## Hands-on

- `http://localhost:9093` — the Alertmanager UI. You will see the group and its alerts.
- Watch the delivery from WatchTower's side:
  `docker compose logs -f watchtower | Select-String "incident queued"`
- The recorded evidence from the real run: the webhook arrived from container IP
  `172.20.0.4`, WatchTower answered `202`, and the payload carried five alerts.

## Interview answers to have ready

**"Why not have Prometheus call your service directly?"**
Prometheus intentionally has no notification logic — that separation is core to its
design. Alertmanager adds grouping, deduplication, silencing and inhibition, which are
all things I would otherwise have to build.

**"Alertmanager already groups. Why correlate again?"**
Alertmanager groups by exact label equality on a fixed key set, and it cannot reason
about time windows across groups. My correlator groups by topology labels within a
sliding window and merges alerts that arrived in *separate* webhooks minutes apart. I
also deliberately set `group_by` coarse so Alertmanager splits less and I get more
context in one payload.

**"What happens if you are slow to respond?"**
The handler decodes, enqueues onto a bounded worker pool and returns `202` immediately.
Enrichment and model calls happen off the request path, so Alertmanager never times out
and retries. If the queue is full it gets `429` with `Retry-After`, which is an honest
answer rather than silently dropping the alert.

---

Next: **Part 5 — Loki and LogQL**, where the evidence comes from.
