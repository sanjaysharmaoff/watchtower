# Part 19 — Docker, Compose, and the Real Stack (Commits 91–95)

**Goal:** `docker compose up --build` brings up real Prometheus rules firing into real Alertmanager,
real Loki logs, real pgvector, real models and real SMTP — with no cloud account and no API key.

---

## Commit 91 — `feat(docker): multi-stage image`

**Files:** `Dockerfile`, `.dockerignore`

**Build:**

```dockerfile
FROM golang:1.24-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags "-s -w -X github.com/<you>/watchtower/internal/bootstrap.Version=${VERSION}" \
    -o /out/watchtowerd ./cmd/watchtowerd

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/watchtowerd /app/watchtowerd
COPY --from=build /src/migrations /app/migrations

USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/app/watchtowerd"]
```

`.dockerignore`: `bin/`, `.git/`, `*.md`, `.env`.

**Verify:**

```bash
docker build --build-arg VERSION=local -t watchtower:local .
docker images watchtower:local                       # should be ~15-25MB
docker run --rm watchtower:local --version           # "watchtower local"
```

**Why:** every line is a decision you should be able to defend.

**Dependency files copied before the source** — Docker invalidates every layer after the first change,
and dependencies change rarely while source changes constantly. This is *the* classic layer-caching
idiom.

**`CGO_ENABLED=0`** produces a fully static binary with no libc dependency, which is what makes a
distroless base possible.

**`-ldflags -X`** injects `bootstrap.Version` **at link time**, which is why `--version` and
`/healthz` report `local` without any source change. `-s -w` strip debug info; `-trimpath` removes
local filesystem paths so builds are reproducible and stack traces do not leak `/home/you/...`.

**distroless-nonroot** has no shell, no package manager, no coreutils. An attacker with code execution
has no `sh` to work with, and `USER nonroot` means the process cannot write to the filesystem or bind
privileged ports.

---

## Commit 92 — `feat(notify): SMTP notifier`

**Files:** `internal/adapters/delivery/notify/smtp.go`

**Build:**

```go
type SMTP struct {
    host string; port int
    username, password string
    useTLS bool
    from   string
    to     []string
    clock  ports.Clock
    ids    ports.IDGen
    log    ports.Logger
}

func (s *SMTP) Name() string
func (s *SMTP) Notify(ctx context.Context, msg domain.Message) error
func (s *SMTP) build(msg domain.Message, to []string) string
func sanitizeHeader(v string) string
```

`Notify`:
1. recipients from the message, falling back to config; error when there are none
2. compute a deadline: 30 seconds, or the caller's if sooner; dial with
   `(&net.Dialer{Deadline: deadline}).DialContext` and `conn.SetDeadline`
3. `smtp.NewClient(conn, s.host)`, `defer client.Quit()`
4. when `useTLS`: check `client.Extension("STARTTLS")` and call
   `client.StartTLS(&tls.Config{ServerName: s.host, MinVersion: tls.VersionTLS12})`.
   **When the relay does not offer STARTTLS, return an error — never downgrade silently.**
5. `client.Auth(smtp.PlainAuth("", user, pass, host))` when a username is set
6. `Mail`, `Rcpt` per recipient, `Data`, write, `Close`
7. log `"email delivered"` with the recipients and subject

`build` — a `multipart/alternative` message with a boundary of `"wt-" + s.ids.New()`, headers
`From`, `To`, `Subject` (via `mime.QEncoding.Encode`), `Date` (RFC1123Z), `MIME-Version`, plus
`X-WatchTower-<KEY>` headers from `msg.Meta` — **each value passed through `sanitizeHeader`**. Then
the `text/plain` part and, when present, the `text/html` part.

```go
// sanitizeHeader strips CR/LF so metadata can never inject extra headers.
func sanitizeHeader(v string) string {
    return strings.NewReplacer("\r", " ", "\n", " ").Replace(v)
}
```

Register `"smtp"` in `wire.Notifiers`, erroring when `WT_SMTP_HOST` is unset.

**Verify:**

```bash
docker run -d --name wt-mail -p 8025:8025 -p 1025:1025 axllent/mailpit

WT_NOTIFY_DRIVER=smtp WT_SMTP_HOST=localhost WT_SMTP_PORT=1025 WT_SMTP_TLS=false \
WT_MAIL_FROM=watchtower@example.com WT_MAIL_TO=sre@example.com ./bin/watchtowerd
```
Post an alert and open http://localhost:8025 — the email should be there with working buttons.

**Why:** **refusing to downgrade** is the security decision. Sending `AUTH PLAIN` credentials over a
plaintext socket because the relay did not offer STARTTLS is exactly how credentials leak.

`sanitizeHeader` is header injection defence — the email equivalent of SQL injection. A newline in a
metadata value would let an attacker append arbitrary headers, including `Bcc:`.

**`multipart/alternative`** so the client picks what it can render. For an on-call engineer on a phone
at 3am, the plain-text part is not a nicety.

Bounding the connection by the caller's deadline means a wedged relay cannot pin a worker goroutine
forever.

---

## Commit 93 — `feat(compose): base stack with Postgres, Redis and Mailpit`

**Files:** `docker-compose.yml`, `Makefile` (edit)

**Build:**

Services:
- **`watchtower`** — `build: {context: ., args: {VERSION: compose}}`, port `8080:8080`, and an
  `environment:` block selecting `WT_STORE_DRIVER=postgres`, `WT_VECTOR_DRIVER=postgres`,
  `WT_GATE_DRIVER=redis`, `WT_CACHE_DRIVER=redis`, `WT_NOTIFY_DRIVER=smtp` with
  `WT_SMTP_HOST=mailpit`, `WT_SMTP_PORT=1025`, `WT_SMTP_TLS=false`, plus
  `WT_HTTP_AUTH_TOKEN=local-dev-token` and `WT_APPROVAL_SECRET=local-dev-approval-secret`.
  **All hostnames are service names** — `postgres:5432`, `redis:6379`, `mailpit:1025`.
- **`postgres`** — `pgvector/pgvector:pg16`, a `pgdata` named volume, and a health check:
  ```yaml
  healthcheck:
    test: ["CMD-SHELL", "pg_isready -U watchtower -d watchtower"]
    interval: 5s
    timeout: 5s
    retries: 20
  ```
- **`redis`** — `redis:7-alpine` with `command: ["redis-server", "--save", "", "--appendonly", "no"]`
- **`mailpit`** — ports `8025` and `1025`

`watchtower.depends_on` uses **`condition: service_healthy`** for Postgres.

Add Makefile targets: `up` (`docker compose up --build`), `down` (`docker compose down -v`),
`logs` (`docker compose logs -f watchtower`), and `demo` (curl the fixture).

**Verify:**

```bash
docker compose up -d --build
docker compose ps                       # postgres healthy, watchtower up
docker compose logs watchtower | grep "postgres connected"

curl -X POST localhost:8080/alerts -H "Content-Type: application/json" \
  -H "Authorization: Bearer local-dev-token" \
  --data-binary @examples/alerts/alertmanager-oom.json
```
Read the mail at http://localhost:8025.

**Why:** the single most useful fact about Compose: **services reach each other by service name** on
the private network, which is why no configuration contains an IP address.

Plain `depends_on` only waits for *started*, which is nearly useless — Postgres takes seconds after
start before it accepts connections. `condition: service_healthy` waits for the health check.

Named volumes because containers are ephemeral and databases must not be. `down -v` is what actually
deletes them.

---

## Commit 94 — `feat(compose): Prometheus, Alertmanager, Loki and local models`

**Files:** `deploy/prometheus/prometheus.yml`,
`deploy/prometheus/rules/watchtower-demo.rules.yml`, `deploy/alertmanager/alertmanager.yml`,
`deploy/logs/push_logs.py`, `docker-compose.yml` (extend)

**Build:**

`prometheus.yml` — `scrape_interval: 10s`, `evaluation_interval: 10s`,
`external_labels: {cluster: prod-eu-1}`, `rule_files: [/etc/prometheus/rules/*.yml]`, alerting
pointed at `alertmanager:9093`, and **two scrape jobs targeting addresses that do not exist**
(`checkout-api.checkout.svc:9100`, `payments-api.checkout.svc:9100`), each with
`namespace`, `service` and `pod` labels.

`watchtower-demo.rules.yml` — three or more rules, all on `up{job="..."} == 0` with `for: 15s`:
- `ContainerOOMKilled`, severity `critical`, with a `summary`, a `description` that **includes a fake
  DSN password and an email address**, and a `runbook_url`
- `PodRestartRateHigh`, severity `warning`
- `DependencyUnreachable` on the other job, severity `warning`

`alertmanager.yml` — route everything to a `watchtower` webhook receiver at
`http://watchtower:8080/alerts`, with `group_by: [cluster, namespace]`, `group_wait: 5s`,
`group_interval: 30s`, `repeat_interval: 1h`, `send_resolved: true`, and
`http_config.authorization` carrying `local-dev-token`.

`deploy/logs/push_logs.py` — a small loop POSTing to `http://loki:3100/loki/api/v1/push` every five
seconds with two streams (`checkout-api`, `payments-api`) whose lines include OOM errors, GC warnings,
retries, health checks and **one line containing a database password**, each with a random
`req_id`. Use `time.time_ns()` for timestamps.

Compose additions:
- `prometheus` — `prom/prometheus:v2.55.1`, `--web.enable-lifecycle`, config and rules bind-mounted
  `:ro`, port 9090
- `alertmanager` — `prom/alertmanager:v0.27.0`, config bind-mounted, port 9093
- `loki` — `grafana/loki:2.9.8` with the built-in `local-config.yaml`, port 3100
- `log-pusher` — `python:3.11-alpine` running the mounted script
- `ollama` — `ollama/ollama:latest`, port 11434, an `ollama` named volume
- `ollama-init` — a **one-shot** container that waits for Ollama, pulls `qwen2.5:1.5b` and
  `nomic-embed-text`, then exits
- point `watchtower` at all of it: `WT_ENRICH_PROVIDERS=loki`, `WT_LOKI_URL=http://loki:3100`,
  `WT_LLM_PROVIDER=openai`, `WT_LLM_BASE_URL=http://ollama:11434/v1`,
  `WT_OPENAI_API_KEY=ollama-local`, `WT_PLANNER_MODEL=qwen2.5:1.5b`,
  `WT_EMBEDDER_DRIVER=openai`, `WT_EMBED_MODEL=nomic-embed-text`, `WT_VECTOR_DIMS=768`, and
  generous budgets (`WT_LLM_TIMEOUT=300s`, `WT_STAGE_TIMEOUT=900s`, `WT_JOB_TIMEOUT=20m`)
- `watchtower.depends_on` adds `ollama-init: {condition: service_completed_successfully}`

**Verify — the whole thing, hands off:**

```bash
docker compose up -d --build         # first run pulls ~4GB, mostly Ollama and the models
docker compose logs -f watchtower
```
Within about a minute of startup you should see, **with no curl at all**:
```
incident queued ... alerts=3 severity=critical
request POST /alerts status=202 remote=172.x.x.x        ← real Alertmanager
stage ok stage=enrich   took=22ms                        ← real Loki
stage ok stage=retrieve took=703ms                       ← real embeddings + pgvector
completion done model=qwen2.5:1.5b tokens=1168           ← a real model
incident triaged confidence=0.59
email delivered component=smtp to=sre@example.com
```
Then check the evidence:
```bash
docker compose exec -T postgres psql -U watchtower -d watchtower -A -t \
  -c "select payload->'meta', payload->'trace' from raw_anomalies order by created_at desc limit 1;"
# expect logs_in=150, logs_out=3, and redactors=secrets+pii+custom

docker compose exec -T postgres psql -U watchtower -d watchtower -A -t \
  -c "select (payload::text like '%<your fake password>%') from raw_anomalies order by created_at desc limit 1;"
# MUST be false
```

**Why:** Prometheus scraping **two addresses that do not exist** is what makes the demo
self-triggering: `up` goes to 0 on the first scrape and the rules fire about thirty seconds later. No
manual poking, which means anyone can evaluate the project by running one command.

The log pusher deliberately emits duplicates with varying request ids **and** a real-looking secret, so
the deduplicator and the input guardrail can be *proved* rather than claimed. That last SQL query
returning `false` is the proof.

`ollama-init` as a one-shot with `service_completed_successfully` means the first incident never races
a cold model store. Init containers are a standard pattern and worth naming.

**Note the gotcha for testing:** Alertmanager's `repeat_interval: 1h` means after the first delivery
you will not get another for an hour unless the group changes. To trigger a fresh incident, add a rule
and `curl -X POST http://localhost:9090/-/reload`.

---

## Commit 95 — `feat(compose): minikube overlay for real Kubernetes events`

**Files:** `docker-compose.minikube.yml`, `deploy/k8s/failing-workload.yaml`,
`deploy/k8s/wire-minikube.ps1` (or `.sh`), `.gitignore` (edit)

**Build:**

`deploy/k8s/failing-workload.yaml` — a `checkout` namespace and two deployments in it:
- **`checkout-api`** — `busybox:1.36` running `tail /dev/zero` with
  `resources.limits.memory: "32Mi"`. It blows past the limit in seconds, gets **OOMKilled**, restarts,
  and repeats — producing real `OOMKilling` and `BackOff` events.
- **`payments-api`** — a container that logs, sleeps and exits 1, with an `httpGet` readiness probe on
  a port nothing listens on — producing `Unhealthy` and `BackOff`.

Both labelled `app:` and `service:` matching the Prometheus labels.

`docker-compose.minikube.yml`:
```yaml
services:
  watchtower:
    networks: [default, minikube]
    environment:
      WT_ENRICH_PROVIDERS: loki,k8s
      WT_K8S_API_SERVER: https://minikube:8443
      WT_K8S_CA_FILE: /etc/watchtower/minikube-ca.crt
      WT_K8S_TOKEN: ${WT_K8S_TOKEN:?run deploy/k8s/wire-minikube.ps1 first}
      WT_K8S_EVENT_LIMIT: "60"
    volumes:
      - ./deploy/k8s/minikube-ca.crt:/etc/watchtower/minikube-ca.crt:ro

networks:
  minikube:
    external: true
    name: minikube
```

`wire-minikube.ps1` — idempotent: start minikube if it is not running, apply both manifests, copy
`~/.minikube/ca.crt` to `deploy/k8s/minikube-ca.crt`, mint a 24h token with
`kubectl -n watchtower create token watchtower --duration=24h`, and write `WT_K8S_TOKEN=...` into
`.env` (preserving any other keys). Print the next command to run.

Add `deploy/k8s/minikube-ca.crt` to `.gitignore`.

**Verify:**

```bash
pwsh deploy/k8s/wire-minikube.ps1
docker compose -f docker-compose.yml -f docker-compose.minikube.yml up -d

kubectl -n checkout get pods                  # expect OOMKilled with a rising restart count
kubectl -n checkout get events --field-selector type=Warning
```
Then trigger a fresh incident (add a rule and reload Prometheus) and check the stored trace:
```bash
docker compose exec -T postgres psql -U watchtower -d watchtower -A -t -c \
 "select e->>'reason' from raw_anomalies, jsonb_array_elements(payload->'events') e
  where e->>'type'='Warning' order by created_at desc limit 5;"
```
You should see real `OOMKilling`, `BackOff` and `Unhealthy` reasons.

**Why:** three Compose techniques worth naming.

**Two networks on one container** — it keeps talking to Postgres on the compose network while reaching
the API server on minikube's.

**`external: true`** — attach to a network minikube already created rather than creating one.

**`${WT_K8S_TOKEN:?message}`** — a required variable with a custom error, so a missing token produces a
clear message instead of a confusing startup failure.

And the reason this is an **overlay** rather than part of the base file: plain `docker compose up` must
work for someone with no Kubernetes. The k8s provider's factory errors without a token, and a failed
factory fails startup by design — so the overlay keeps the default path working.

`https://minikube:8443` works with **full TLS verification** because `DNS:minikube` is a Subject
Alternative Name on the API server certificate. Check it yourself:
```bash
openssl x509 -in ~/.minikube/profiles/minikube/apiserver.crt -noout -text | grep DNS
```

---

## End of Part 19

One command brings up nine services, and alerts flow end to end with no human input.

```bash
docker compose ps
docker compose logs watchtower | grep "watchtower starting"
```

Next: **Part 20** — tests, hardening, CI and documentation. The last five commits.
