# Part 24 — Configuration Bible: Where to Change Things, and How It Is Plug-and-Play

Everything about configuration: how it is read, where to change it, every variable, the four
ready-made profiles, the swap recipes, and troubleshooting.

## How configuration is read

Three layers, in priority order:

1. **`WT_`-prefixed environment variable** — wins
2. **Unprefixed environment variable** — fallback (so `OPENAI_API_KEY` works as-is)
3. **Default in `config.Load()`** — used when neither is set

```go
func lookup(key string) (string, bool) {
    if v, ok := os.LookupEnv(prefix + key); ok && strings.TrimSpace(v) != "" { return v, true }
    if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" { return v, true }
    return "", false
}
```

Before the environment is read, `.env` is loaded — **without overwriting anything already set**:

```go
if _, exists := os.LookupEnv(key); !exists {
    _ = os.Setenv(key, value)
}
```

So a real environment variable always beats the file, which is what you want in production.

**`os.Getenv` appears in exactly one package.** Everything else receives values through a struct.
That is why adapters are usable from tests and from other programs with no environment at all.

Types are parsed by small helpers: `str`, `integer`, `decimal`, `boolean`, `duration` (accepting
`30s`, `5m`, `24h`, or a bare number as seconds), and `list` (comma-separated).

## Where to change things — five places, in order of preference

**1. `.env` in the project root** — local development, single machine.

```bash
cp .env.example .env
# edit, then:
go run ./cmd/watchtowerd
```

Gitignored. `--env-file path` points at a different file.

**2. `docker-compose.yml`, the `environment:` block** — the compose stack. This is where the
verified real-infrastructure configuration lives.

**3. `docker-compose.minikube.yml`** — the overlay that adds Kubernetes.

**4. Real environment variables** — production. Kubernetes `env` / `envFrom` a ConfigMap for
non-secrets and a Secret for credentials.

**5. `internal/config/config.go`** — only to change a *default* or add a *new* knob. You should
almost never need to touch it.

## What "plug and play" actually means here

Two mechanisms, and it is worth being precise about them because it is a claim an interviewer will
test.

**Mechanism 1: driver names.** A config value is a name looked up in a registry (Part 15):

```
WT_STORE_DRIVER=postgres  →  wire.IncidentStores.Build(env, "postgres")  →  *postgres.Incidents
WT_STORE_DRIVER=memory    →  wire.IncidentStores.Build(env, "memory")    →  *memstore.Store
```

No `if`, no switch, no rebuild. A typo gets you a startup error listing valid names.

**Mechanism 2: composed lists.** Some ports take several implementations, and the config gives the
**order**:

```
WT_ENRICH_PROVIDERS=loki,k8s              → []ContextProvider, fanned out in parallel
WT_SANITIZE_CHAIN=dedupe,signal,compress,cap → []LogProcessor, applied in that order
WT_INPUT_REDACTORS=secrets,pii,custom     → []Redactor, applied in that order
WT_OUTPUT_INSPECTORS=denylist,flags,allowlist → []CommandInspector, first failure wins
```

`WT_SANITIZE_CHAIN=dedupe,cap` is a legitimate, working configuration. So is dropping `pii` from
the redactors. **The order of the log-cleaning algorithm is an environment variable.**

What is *not* plug-and-play, so you do not overclaim: the pipeline **stage order** is a slice
literal in `internal/bootstrap/app.go`. Deliberate — reordering security-relevant stages should be
a reviewed code change, not an env var someone sets at 3am.

## The four profiles

### Profile A — laptop, zero infrastructure (the defaults)

```bash
go run ./cmd/watchtowerd
```

Nothing to configure. Memory storage, memory vectors, offline hashing embedder, rule-based
planner, demo enrichment, email printed to stdout, no auth. Full pipeline, no dependencies, no
cost. This is what a reviewer of your GitHub repo will run.

### Profile B — real infrastructure, no cloud account

This is `docker compose up --build`, and it is the verified configuration:

```yaml
WT_STORE_DRIVER: postgres
WT_POSTGRES_DSN: postgres://watchtower:watchtower@postgres:5432/watchtower?sslmode=disable
WT_VECTOR_DRIVER: postgres
WT_VECTOR_DIMS: "768"
WT_LLM_PROVIDER: openai
WT_LLM_BASE_URL: http://ollama:11434/v1     # ← Ollama, OpenAI-compatible
WT_OPENAI_API_KEY: ollama-local
WT_PLANNER_MODEL: qwen2.5:1.5b
WT_EMBED_MODEL: nomic-embed-text
WT_EMBEDDER_DRIVER: openai
WT_RERANK_DRIVER: llm
WT_ENRICH_PROVIDERS: loki
WT_LOKI_URL: http://loki:3100
WT_GATE_DRIVER: redis
WT_CACHE_DRIVER: redis
WT_NOTIFY_DRIVER: smtp
WT_SMTP_HOST: mailpit
WT_LLM_TIMEOUT: 300s      # CPU inference is slow
WT_STAGE_TIMEOUT: 900s
WT_JOB_TIMEOUT: 20m
```

### Profile C — add real Kubernetes

```powershell
pwsh deploy/k8s/wire-minikube.ps1
docker compose -f docker-compose.yml -f docker-compose.minikube.yml up -d
```

Adds `WT_ENRICH_PROVIDERS: loki,k8s`, the API server, the CA and a 24h token.

### Profile D — production shape

```bash
WT_ENV=prod                            # enables the strict validation below
WT_LOG_FORMAT=json                     # for log aggregation
WT_HTTP_AUTH_TOKEN=<random 32+ bytes>
WT_APPROVAL_SECRET=<random 32+ bytes>
WT_PUBLIC_BASE_URL=https://watchtower.internal.example.com

WT_STORE_DRIVER=postgres
WT_POSTGRES_DSN=postgres://user:pass@db:5432/watchtower?sslmode=require
WT_VECTOR_DRIVER=postgres
WT_VECTOR_DIMS=1536
WT_STORE_AUTO_MIGRATE=false            # DBA applies migrations/001_init.sql

WT_LLM_PROVIDER=openai
WT_LLM_BASE_URL=https://api.openai.com/v1
WT_OPENAI_API_KEY=sk-...
WT_PLANNER_MODEL=gpt-4o-mini
WT_EMBED_MODEL=text-embedding-3-small
WT_EMBEDDER_DRIVER=openai
WT_RERANK_DRIVER=heuristic             # free; switch to llm if quality demands it

WT_ENRICH_PROVIDERS=loki,k8s
WT_LOKI_URL=https://loki.internal:3100
WT_LOKI_TENANT=platform

WT_GATE_DRIVER=redis                   # required with >1 replica
WT_CACHE_DRIVER=redis
WT_REDIS_ADDR=redis.internal:6379

WT_NOTIFY_DRIVER=smtp
WT_SMTP_HOST=email-smtp.eu-west-1.amazonaws.com
WT_SMTP_TLS=true
WT_MAIL_TO=sre-oncall@example.com

WT_CRON_ENABLED=false                  # Kubernetes CronJobs instead, to avoid N replicas × N reports
WT_MIN_SEVERITY=critical               # only page for critical
WT_NOTIFY_MIN_CONFIDENCE=0.4           # suppress low-confidence email; incident still stored
```

`WT_ENV=prod` turns on two extra validation rules:

```go
if c.Env == "prod" && c.HTTP.AuthToken == "" {
    errs = append(errs, "WT_HTTP_AUTH_TOKEN is required when ENV=prod: the /alerts and /runbook endpoints would otherwise be unauthenticated")
}
if c.Env == "prod" && c.HTTP.ApprovalSecret == "watchtower-dev-secret" {
    errs = append(errs, "WT_APPROVAL_SECRET must be set when ENV=prod so approval links cannot be forged")
}
```

Verified: setting `WT_ENV=prod` with no token refuses to start and prints both reasons. **The
system will not let you deploy it insecurely by accident** — a very good thing to demonstrate.

## Every variable

### Runtime and logging

| Variable | Default | Notes |
| --- | --- | --- |
| `WT_ENV` | `dev` | `prod` enables strict security validation |
| `WT_LOG_LEVEL` | `info` | `debug` shows every stage and event |
| `WT_LOG_FORMAT` | `text` | `json` for aggregators |

### HTTP edge

| Variable | Default | Notes |
| --- | --- | --- |
| `WT_HTTP_ADDR` | `:8080` | |
| `WT_HTTP_AUTH_TOKEN` | *(empty)* | bearer token; required in prod |
| `WT_HTTP_MAX_BODY_BYTES` | `2097152` | 2MB body cap |
| `WT_HTTP_READ_TIMEOUT` | `15s` | |
| `WT_HTTP_WRITE_TIMEOUT` | `30s` | |
| `WT_HTTP_IDLE_TIMEOUT` | `60s` | |
| `WT_HTTP_SHUTDOWN_GRACE` | `10s` | drain budget |
| `WT_PUBLIC_BASE_URL` | `http://localhost:8080` | **must be reachable by the reviewer**, it is the link prefix |
| `WT_APPROVAL_SECRET` | `watchtower-dev-secret` | HMAC key; changing it invalidates outstanding links |
| `WT_APPROVAL_TTL` | `168h` | link lifetime |
| `WT_ALERT_DECODER` | `alertmanager` | or `generic` |

### Worker pool

| Variable | Default | Notes |
| --- | --- | --- |
| `WT_WORKERS` | `8` | concurrent incidents |
| `WT_QUEUE_SIZE` | `512` | above this, `429` |
| `WT_JOB_TIMEOUT` | `3m` | raise for slow local models |

### Debounce and correlation

| Variable | Default | Notes |
| --- | --- | --- |
| `WT_GATE_DRIVER` | `memory` | `redis` with >1 replica |
| `WT_DEBOUNCE_WINDOW` | `1h` | lower it while testing |
| `WT_DROP_RESOLVED` | `true` | |
| `WT_MIN_SEVERITY` | `warning` | `info`/`warning`/`critical`; `unknown` disables |
| `WT_CORRELATION_KEYS` | `cluster,namespace,service` | grouping labels |
| `WT_CORRELATION_WINDOW` | `5m` | |

### Enrichment

| Variable | Default | Notes |
| --- | --- | --- |
| `WT_ENRICH_PROVIDERS` | `demo` | `loki,k8s`, `none` |
| `WT_ENRICH_TIMEOUT` | `8s` | bounds the whole fan-out |
| `WT_ENRICH_LOG_LIMIT` | `100` | Loki fetch cap |
| `WT_ENRICH_LOOKBACK` | `15m` | how far back to query |
| `WT_LOKI_URL` | *(empty)* | required for the loki provider |
| `WT_LOKI_TENANT` | | `X-Scope-OrgID` |
| `WT_LOKI_USERNAME` / `WT_LOKI_PASSWORD` | | basic auth |
| `WT_LOKI_QUERY` | *(derived)* | override with `{{namespace}}`, `{{service}}`, `{{cluster}}` |
| `WT_K8S_API_SERVER` | *(in-cluster)* | |
| `WT_K8S_TOKEN` | *(service account)* | |
| `WT_K8S_CA_FILE` | *(service account)* | |
| `WT_K8S_INSECURE` | `false` | **leave false** |
| `WT_K8S_EVENT_LIMIT` | `40` | |

### Sanitizer

| Variable | Default | Notes |
| --- | --- | --- |
| `WT_SANITIZE_CHAIN` | `dedupe,signal,compress,cap` | order matters |
| `WT_SANITIZE_MAX_LINES` | `60` | hard cap |
| `WT_SANITIZE_MAX_LINE_LEN` | `400` | |
| `WT_SANITIZE_MIN_LEVEL` | `warn` | |
| `WT_SANITIZE_KEYWORDS` | `error,fail,timeout,...` | keep-if-contains |
| `WT_SANITIZE_STRIP_DIGITS` | `true` | normalise numbers when deduplicating |

### Retrieval

| Variable | Default | Notes |
| --- | --- | --- |
| `WT_EMBEDDER_DRIVER` | `stub` | `openai` for real embeddings |
| `WT_VECTOR_DRIVER` | `memory` | `postgres` for pgvector |
| `WT_RERANK_DRIVER` | `heuristic` | `llm`, `none` |
| `WT_CACHE_DRIVER` | `memory` | `redis`, `none` |
| `WT_RAG_TOP_K` | `10` | candidates fetched |
| `WT_RAG_TOP_N` | `2` | candidates prompted |
| `WT_RAG_MIN_SCORE` | `0.2` | discard threshold |
| `WT_VECTOR_DIMS` | `1536` | **must match the embedder** |
| `WT_RAG_CACHE_TTL` | `24h` | |
| `WT_REDIS_ADDR` / `WT_REDIS_PASSWORD` / `WT_REDIS_DB` | `localhost:6379` / — / `0` | |

### Models

| Variable | Default | Notes |
| --- | --- | --- |
| `WT_LLM_PROVIDER` | `stub` | `openai` for anything OpenAI-compatible |
| `WT_LLM_BASE_URL` | `https://api.openai.com/v1` | point at Ollama/vLLM here |
| `WT_OPENAI_API_KEY` | *(empty)* | any non-empty string for Ollama |
| `WT_PLANNER_MODEL` | `gpt-4o-mini` | |
| `WT_CRITIC_MODEL` | `gpt-4o-mini` | can differ from the planner |
| `WT_EMBED_MODEL` | `text-embedding-3-small` | |
| `WT_LLM_TEMPERATURE` | `0.1` | low for reproducibility |
| `WT_LLM_MAX_TOKENS` | `1400` | |
| `WT_LLM_TIMEOUT` | `45s` | 300s for CPU inference |
| `WT_LLM_MAX_ATTEMPTS` | `2` | planner/critic rounds |
| `WT_LLM_CRITIC_ENABLED` | `true` | off halves the cost |

### Guardrails

| Variable | Default | Notes |
| --- | --- | --- |
| `WT_INPUT_REDACTORS` | `secrets,pii,custom` | |
| `WT_INPUT_REDACT_PATTERNS` | *(empty)* | extra regexes, comma-separated |
| `WT_OUTPUT_INSPECTORS` | `denylist,flags,allowlist` | |
| `WT_GUARD_DENY_PATTERNS` | *(empty)* | extra destructive patterns |
| `WT_GUARD_DENY_FLAGS` | *(empty)* | extra dangerous flags |
| `WT_GUARD_ALLOW_BINARIES` | *(curated ~70)* | empty uses the built-in set |
| `WT_GUARD_MODE` | `strip` | or `halt` |

### Storage

| Variable | Default | Notes |
| --- | --- | --- |
| `WT_STORE_DRIVER` | `memory` | `postgres` |
| `WT_POSTGRES_DSN` | *(empty)* | |
| `WT_POSTGRES_MAX_CONNS` | `10` | |
| `WT_STORE_AUTO_MIGRATE` | `true` | false if a DBA owns the schema |
| `WT_STORE_RETENTION_DAYS` | `30` | partitions dropped past this |
| `WT_STORE_PARTITION_AHEAD` | `7` | partitions created ahead |
| `WT_STORE_MEMORY_LIMIT` | `500` | ring size for the memory store |

### Delivery

| Variable | Default | Notes |
| --- | --- | --- |
| `WT_NOTIFY_DRIVER` | `stdout` | `smtp`, `none`, `memory` |
| `WT_RENDERER` | `html` | |
| `WT_MAIL_FROM` | `watchtower@example.com` | |
| `WT_MAIL_TO` | `sre@example.com` | comma-separated |
| `WT_SMTP_HOST` / `WT_SMTP_PORT` | — / `587` | |
| `WT_SMTP_USERNAME` / `WT_SMTP_PASSWORD` | | |
| `WT_SMTP_TLS` | `true` | false only for a local relay |
| `WT_NOTIFY_MIN_CONFIDENCE` | `0` | suppress below this; still stored |

### Schedules and internals

| Variable | Default | Notes |
| --- | --- | --- |
| `WT_CRON_ENABLED` | `true` | false when using external CronJobs |
| `WT_CRON_TZ` | `UTC` | |
| `WT_CRON_PARTITION_SPEC` | `0 0 * * *` | |
| `WT_CRON_DIGEST_SPEC` | `0 8 * * *` | empty disables |
| `WT_DIGEST_WINDOW` | `24h` | |
| `WT_BUS_BUFFER` | `512` | |
| `WT_BUS_WORKERS` | `4` | |
| `WT_STAGE_TIMEOUT` | `60s` | per stage |

## Swap recipes

**Local models instead of OpenAI** — the one that surprises people:
```bash
WT_LLM_BASE_URL=http://localhost:11434/v1
WT_OPENAI_API_KEY=anything
WT_PLANNER_MODEL=qwen2.5:7b
WT_EMBED_MODEL=nomic-embed-text
WT_VECTOR_DIMS=768
```

**Cheaper operation** — `WT_LLM_CRITIC_ENABLED=false`, `WT_RERANK_DRIVER=heuristic`,
`WT_MIN_SEVERITY=critical`, `WT_DEBOUNCE_WINDOW=4h`, `WT_SANITIZE_MAX_LINES=30`.

**Higher quality** — `WT_PLANNER_MODEL=gpt-4o`, `WT_RERANK_DRIVER=llm`, `WT_RAG_TOP_N=3`,
`WT_LLM_MAX_ATTEMPTS=3`, `WT_ENRICH_LOG_LIMIT=300`.

**Stricter safety** — `WT_GUARD_MODE=halt`,
`WT_GUARD_ALLOW_BINARIES=kubectl,helm,journalctl,df,du,ps,top,dig,curl`,
`WT_INPUT_REDACT_PATTERNS=\b(?:\d{1,3}\.){3}\d{1,3}\b` (adds IP redaction).

**Scaling to N replicas** — `WT_GATE_DRIVER=redis`, `WT_CACHE_DRIVER=redis`,
`WT_CRON_ENABLED=false` plus CronJobs. Remaining gap: the correlation window is still
per-process (documented limitation).

**Testing quickly** — `WT_DEBOUNCE_WINDOW=10s`, `WT_LOG_LEVEL=debug`,
`WT_NOTIFY_DRIVER=stdout`, `WT_ENRICH_PROVIDERS=demo`, `WT_LLM_PROVIDER=stub`.

## Adding a new adapter — the checklist

1. Write one file with `init()` registering it into the right `wire.*` registry.
2. Add one blank import to `internal/bootstrap/plugins.go`.
3. If it needs new config, add the field to `internal/config/config.go` and one line in
   `Load()`.
4. Document it in `.env.example`.
5. Set the driver env var.

Nothing else. Not the pipeline, not the services, not the projectors.

## Verification commands

```bash
# which drivers are actually live
docker compose logs watchtower | Select-String "watchtower starting"

# the assembled pipeline
docker compose logs watchtower | Select-String "pipeline assembled"

# health, and worker counters (needs the token if set)
curl http://localhost:8080/healthz
curl http://localhost:8080/readyz
curl -H "Authorization: Bearer local-dev-token" http://localhost:8080/status

# anything failed and got parked?
docker compose exec -T postgres psql -U watchtower -d watchtower -c "select count(*) from dead_letter_queue;"

# did learning happen?
docker compose exec -T postgres psql -U watchtower -d watchtower \
  -c "select id, title, vector_dims(embedding) from historical_runbooks;"

# send a test alert
curl -X POST http://localhost:8080/alerts -H "Content-Type: application/json" \
  -H "Authorization: Bearer local-dev-token" \
  --data-binary @examples/alerts/alertmanager-oom.json
```

## Troubleshooting

| Symptom | Cause | Fix |
| --- | --- | --- |
| `startup failed: invalid config: ...` | validation caught a bad combination | read the message; it names the variable and the reason |
| `unknown notifier driver "smpt"` | typo | the error lists valid names |
| Alert accepted, no email | dropped by debounce or the severity floor | check `stage dropped payload` in the logs |
| Second identical alert vanishes | working as designed | lower `WT_DEBOUNCE_WINDOW` for testing |
| `expected 768 dimensions, not 256` | embedder and `WT_VECTOR_DIMS` disagree | make them match |
| `postgres vector store selected but POSTGRES_DSN is empty` | missing DSN | set it |
| No Loki logs | wrong labels for your deployment | set `WT_LOKI_QUERY` with placeholders |
| `k8s: no token configured` | not in-cluster and no token | run `wire-minikube.ps1`, or use `WT_ENRICH_PROVIDERS=loki` |
| `smtp: relay does not offer STARTTLS` | plaintext relay with TLS required | `WT_SMTP_TLS=false` for local relays only |
| Model call times out | CPU inference | raise `WT_LLM_TIMEOUT`, `WT_STAGE_TIMEOUT`, `WT_JOB_TIMEOUT` |
| `429` on `/alerts` | queue full | raise `WT_WORKERS` / `WT_QUEUE_SIZE`, or find why jobs are slow |
| Approval link says expired | past `WT_APPROVAL_TTL`, or the secret changed | use `POST /runbook/approve` |
| No alerts arriving at all | Alertmanager `repeat_interval` is 1h | add a rule and `curl -X POST localhost:9090/-/reload` |

---

Next: **Part 25 — the interview playbook.**
