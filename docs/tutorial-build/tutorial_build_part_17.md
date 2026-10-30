# Part 17 — Redis, Loki and Kubernetes (Commits 81–85)

**Goal:** shared state across replicas, and real telemetry instead of the demo provider.

Setup:
```bash
docker run -d --name wt-redis -p 6379:6379 redis:7-alpine
docker run -d --name wt-loki  -p 3100:3100 grafana/loki:2.9.8
go get github.com/redis/go-redis/v9
```

---

## Commit 81 — `feat(redisx): shared Redis client`

**Files:** `internal/platform/redisx/redisx.go`

**Build:**

```go
type Client interface {
    Get(ctx context.Context, key string) ([]byte, bool, error)
    Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
    SetNX(ctx context.Context, key string, value []byte, ttl time.Duration) (bool, error)
    Ping(ctx context.Context) error
    Close() error
}

func Shared(e *wire.Env) (Client, error)
```
Memoized under `"redis.client"`. Build a `goredis.NewClient` from `cfg.RAG.RedisAddr`,
`RedisPassword`, `RedisDB`, with `DialTimeout: 5s`, `ReadTimeout: 3s`, `WriteTimeout: 3s`. **Ping
with a 5-second context**; on failure close the client and return the error. Then `e.OnClose(c.Close)`
and log `"redis connected"`.

An unexported `client` struct wrapping `*goredis.Client`:
- `Get` — map `goredis.Nil` to `(nil, false, nil)`, a real error to `(nil, false, err)`
- `Set`, `SetNX`, `Ping`, `Close` — thin delegations

**Verify:** build; scratch-call `Shared` and `Ping`.

**Why:** **five methods, not the ~200 of the Redis protocol.** Only what the project needs, which
makes it trivially fake-able in a test and keeps `go-redis` out of the rest of the codebase.

Two adapters — the gate and the cache — need Redis, and neither should own the connection: if one
closed it, the other would break. So it lives in the `Env` resource pool and the `Env` closes it once.

Pinging at construction turns a wrong address into a **startup** failure rather than a mysterious
runtime error on the first alert.

---

## Commit 82 — `feat(gate,cache): Redis drivers`

**Files:** `internal/adapters/ingest/gate/redis.go`,
`internal/adapters/rag/cache/cache.go` (extend)

**Build:**

`gate/redis.go`:
```go
type Redis struct { client redisx.Client; prefix string }   // prefix "wt:gate:"

func (r *Redis) Allow(ctx context.Context, key string, window time.Duration) (bool, error)
```
Nine lines: return `(true, nil)` when `window <= 0`, otherwise
`r.client.SetNX(ctx, r.prefix+key, []byte("1"), window)` and wrap any error.

`cache/cache.go`:
```go
type Redis struct { client redisx.Client; prefix string }   // prefix "wt:cache:"
func (r *Redis) Get(...)  // delegate with the prefix
func (r *Redis) Set(...)  // delegate with the prefix
func (r *Redis) Close() error { return nil }                // ← deliberately a no-op
```

Register `"redis"` in `wire.Gates` and `wire.Caches`, both obtaining the client via
`redisx.Shared(e)`.

**Verify:**

```bash
WT_GATE_DRIVER=redis WT_CACHE_DRIVER=redis WT_DEBOUNCE_WINDOW=2m ./bin/watchtowerd
```
Post the same alert twice — the second must be debounced. Then:
```bash
docker exec -it wt-redis redis-cli keys 'wt:*'
docker exec -it wt-redis redis-cli ttl 'wt:gate:<signature>'
```
You should see the gate key with a TTL counting down, and cache keys after an embedding.

Kill Redis (`docker stop wt-redis`) and post an alert: it must still be processed, with the trace
noting `debounce gate error, failing open`.

**Why:** **`SETNX` with a TTL is the entire distributed debouncer.** It is atomic — exactly one
caller gets `true` even with a hundred replicas — and the key expires on its own, so there is no
cleanup job and no leak. Worth memorising as a pattern.

Redis here is **not for speed**, it is for **shared state across replicas**. With one replica the
in-memory map is perfect; with two, the duplicate lands on the other replica and the engineer gets
two emails. Say it that way.

`Redis.Close()` on the cache being a no-op is not laziness: the shared client belongs to the `Env`.
Getting that wrong is how you produce "use of closed connection" bugs.

---

## Commit 83 — `feat(enrich): Loki log provider`

**Files:** `internal/adapters/enrich/loki/loki.go`

**Build:**

```go
type Provider struct {
    cfg      config.LokiConfig
    http     *http.Client
    clock    ports.Clock
    log      ports.Logger
    limit    int
    lookback time.Duration
}
func (p *Provider) Name() string                                       // "loki"
func (p *Provider) Fetch(ctx context.Context, inc *domain.Incident) (domain.ContextFragment, error)
func (p *Provider) buildQuery(inc *domain.Incident) string
func parseNanos(s string) time.Time
func detectLevel(line string) string
```

`buildQuery` — read `namespace` and `service` off the primary alert via `Label(...)`.
- when `cfg.Query` is set, substitute `{{namespace}}`, `{{service}}`, `{{cluster}}` and return it
- otherwise build `{namespace="x", app="y"} |~ "(?i)(error|fatal|panic|fail|timeout|refused|exception|denied)"`
- **return `""` when there is no label to anchor on** — the caller turns that into an error rather
  than running an unbounded query

`Fetch`:
1. empty query → return the fragment with an error
2. record the query in `frag.Notes["query"]`
3. `GET <url>/loki/api/v1/query_range` with `query`, `start` and `end` as **nanosecond** unix
   strings (`end = clock.Now()`, `start = end - lookback`), `limit`, `direction=backward`
4. set `X-Scope-OrgID` when a tenant is configured; `req.SetBasicAuth` when a username is
5. non-2xx → error with the status
6. decode `{"data":{"result":[{"stream":{...},"values":[["<ns>","<line>"],...]}]}}`
7. map every value pair into a `domain.LogLine` with `detectLevel(line)`,
   `Source: "loki:" + pod`, `Occurrences: 1` and the stream labels
8. record `frag.Notes["returned"]`

Register `"loki"` in `wire.ContextProviders`, **erroring when `WT_LOKI_URL` is unset**.

**Verify:** push some logs and read them back:

```bash
# push (adjust the timestamp to now, in nanoseconds)
curl -s -X POST http://localhost:3100/loki/api/v1/push -H "Content-Type: application/json" -d '{
 "streams":[{"stream":{"namespace":"checkout","app":"checkout-api"},
 "values":[["'$(date +%s)000000000'","level=error msg=\"fatal error: out of memory\""]]}]}'

WT_ENRICH_PROVIDERS=loki WT_LOKI_URL=http://localhost:3100 ./bin/watchtowerd
```
Post an alert and expect a trace line like `provider loki: 1 logs, 0 events in 12ms`.

**Why:** **nanosecond timestamps** are Loki's convention and the most common source of "why is my
query empty".

The regex line filter runs **inside Loki**, so only interesting lines cross the network — cheaper
than fetching everything and filtering in Go.

Refusing to run without a label anchor is deliberate: Loki indexes labels, not content, so an
unanchored query is a full scan. The `{{placeholder}}` override exists because every organisation
labels differently and the derived query is only a good guess.

`detectLevel` sniffing the level out of unstructured text is what lets the sanitizer *rank* lines
later. Without it every line looks equally important.

---

## Commit 84 — `feat(enrich): Kubernetes event provider`

**Files:** `internal/adapters/enrich/k8s/k8s.go`

**Build:**

Constants for the in-cluster defaults: `https://kubernetes.default.svc`, and the service account
token and CA paths under `/var/run/secrets/kubernetes.io/serviceaccount/`.

```go
type Provider struct { cfg config.K8sConfig; http *http.Client; log ports.Logger }
func (*Provider) Name() string                                          // "k8s"
func (p *Provider) Fetch(ctx context.Context, inc *domain.Incident) (domain.ContextFragment, error)
func httpClient(cfg config.K8sConfig) (*http.Client, error)
func firstTime(values ...string) time.Time
```

The registration function does the **auto-detection**: default the API server to the in-cluster
address, read the token from the mounted file when unset, use the mounted CA when present, and
**error when there is still no token**.

`httpClient` — build a `tls.Config` with `MinVersion: tls.VersionTLS12`; when `Insecure` set
`InsecureSkipVerify` (opt-in only), otherwise when a CA file is configured read it into an
`x509.NewCertPool` and set `RootCAs`. 15-second timeout.

`Fetch`:
1. no `namespace` label → return an error
2. `GET <api>/api/v1/namespaces/<ns>/events?limit=N` with `Authorization: Bearer <token>`
3. `401`/`403` → an error **naming the missing RBAC**: *"the service account needs get/list on
   events"*
4. decode `items[]` with `type`, `reason`, `message`, `count`, `lastTimestamp`, `firstTimestamp`,
   `eventTime`, and `involvedObject{kind,name}`
5. **noise filter**: keep every `Warning`; keep a `Normal` event only when its object name contains
   the alert's service
6. build `domain.ClusterEvent` values with `Object: "<kind>/<name>"` and
   `Timestamp: firstTime(lastTimestamp, eventTime, firstTimestamp)`
7. sort by timestamp; record `namespace` and `returned` in `Notes`

Register `"k8s"` in `wire.ContextProviders`.

Also write `deploy/k8s/watchtower-reader.yaml`: a `watchtower` namespace, a ServiceAccount, and a
ClusterRole with **`get` and `list` only** on `events`, `pods`, `services`, `endpoints`, `nodes` and
the `apps` workloads — **no write verbs anywhere** — plus the binding.

**Verify:** with a cluster (`minikube start`):
```bash
kubectl apply -f deploy/k8s/watchtower-reader.yaml
kubectl -n watchtower create token watchtower --duration=24h

WT_ENRICH_PROVIDERS=loki,k8s \
WT_K8S_API_SERVER=$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}') \
WT_K8S_TOKEN=<paste> WT_K8S_CA_FILE=$HOME/.minikube/ca.crt \
./bin/watchtowerd
```
Post an alert with `namespace=checkout` and expect `provider k8s: 0 logs, N events in 11ms`. Deploy
something that crashloops and check the Warning events appear.

**Why:** **no client-go.** One read-only REST endpoint versus a very large dependency tree. Around
200 lines with zero dependencies. State the boundary: you would switch to client-go the moment you
needed watches, informers or CRDs.

The RBAC having **no write verbs** is the strongest thing to say about this component: even a fully
compromised WatchTower cannot change the cluster, and that is enforced by the API server rather than
by your code.

`InsecureSkipVerify` exists but is off and opt-in. TLS verification against the real cluster CA is
the default path.

Auto-detecting the in-cluster token and CA means production configuration is *nothing* — this only
looks verbose because local development is the awkward case.

---

## Commit 85 — `chore(bootstrap): providers, drivers, and the demo fallback`

**Files:** `internal/bootstrap/plugins.go`, `.env.example`

**Build:**

Add the blank imports for `platform/redisx` consumers — `enrich/loki`, `enrich/k8s` — and confirm
`ingest/gate` and `rag/cache` are already there.

Write `.env.example` documenting **every** variable in sections, with the defaults and a one-line
comment each. Open it with a note saying the defaults boot a working instance with no
infrastructure, and include the placeholder blocks for Loki, Kubernetes, Redis, SMTP and the model
provider.

**Verify:** the three configurations must all work from the same binary:

```bash
# 1. nothing at all
./bin/watchtowerd

# 2. real storage and real logs
WT_STORE_DRIVER=postgres WT_POSTGRES_DSN='...' \
WT_ENRICH_PROVIDERS=loki WT_LOKI_URL=http://localhost:3100 \
WT_GATE_DRIVER=redis WT_CACHE_DRIVER=redis ./bin/watchtowerd

# 3. add the cluster
WT_ENRICH_PROVIDERS=loki,k8s WT_K8S_API_SERVER=... WT_K8S_TOKEN=... ./bin/watchtowerd
```

Check the startup line reports what you expect:
```
drivers="env=dev store=postgres vector=postgres embedder=stub rerank=heuristic llm=stub
         notify=stdout gate=redis cache=redis enrich=loki+k8s"
```

**Why:** this commit is where the "plug and play" claim becomes demonstrable. **Three deployment
shapes, one binary, zero code differences.** That is worth its own commit and its own paragraph in
the README.

Note that `WT_ENRICH_PROVIDERS` is a *list* — `loki,k8s` fans out in parallel, and `demo` remains
available for anyone without infrastructure. `BuildAll` skipping `"none"` means an operator can
disable one entry without editing the list.

---

## End of Part 17

Real logs, real cluster events, shared debouncing and a shared cache.

```bash
go build ./... && go vet ./... && gofmt -l .
```

Next: **Part 18** — cron, the daily digest, maintenance and graceful shutdown. Feature-complete at
commit 90.
