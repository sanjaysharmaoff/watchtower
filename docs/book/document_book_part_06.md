# Part 6 — Kubernetes, From Zero

This is the biggest technology in the stack. You do not need to be an expert; you need to
understand the seven objects this project touches and the security model it uses.

## The problem Kubernetes solves

You have 200 services and 50 machines. Somebody has to decide which service runs on
which machine, restart it when it crashes, replace it when a machine dies, give it a
stable network name, and roll out new versions without downtime. Doing that by hand does
not scale.

Kubernetes ("k8s") is a **container orchestrator**. You describe the desired state in
YAML — "I want 3 copies of this image, each limited to 512MB of memory" — and Kubernetes
continuously works to make reality match. That loop is called **reconciliation**, and it
is the single most important idea in the system: you declare *what*, not *how*.

## The objects you need to know

**Container** — a packaged process with its own filesystem. Docker builds them (Part 7).

**Pod** — the smallest unit Kubernetes schedules. Usually one container, sometimes a
couple that must share a network and disk. Pods are disposable: they get killed and
recreated with new names constantly. Pod names look like
`checkout-api-64f8f5474b-x9bn6` — deployment name, ReplicaSet hash, random suffix.

**Deployment** — "keep N pods of this image running, and roll out changes safely." You
almost never create pods directly; you create a Deployment and it manages a ReplicaSet
which manages pods. This project's manifest:

```yaml
# deploy/k8s/failing-workload.yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: checkout-api
  namespace: checkout
spec:
  replicas: 1
  selector:
    matchLabels:
      app: checkout-api
  template:
    metadata:
      labels:
        app: checkout-api
        service: checkout-api
    spec:
      containers:
        - name: checkout-api
          image: busybox:1.36
          command: ["/bin/sh", "-c", "echo starting checkout-api 4.2.1; tail /dev/zero"]
          resources:
            limits:   { memory: "32Mi", cpu: "100m" }
            requests: { memory: "16Mi", cpu: "50m" }
```

**Namespace** — a folder for objects. `checkout`, `watchtower`, `kube-system`. Used for
isolation and access control. Alert labels carry the namespace, which is how WatchTower
knows where to look for logs and events.

**Labels** — arbitrary key/value pairs on objects. Everything selects by labels:
Deployments find their pods by label, Services route to pods by label, Prometheus
attaches them to metrics, Loki uses them as stream keys. **Labels are the joining key
across the entire stack**, and that is why WatchTower's correlation is label-based too.

**Service** — a stable name and IP in front of a set of pods. Pods come and go;
`payments-api.checkout.svc` does not.

**Event** — a short-lived record of something that happened. This is what WatchTower
reads, so it gets its own section.

## requests, limits, and OOMKilled

```yaml
resources:
  requests: { memory: "16Mi" }   # reserve this much; used for scheduling decisions
  limits:   { memory: "32Mi" }   # hard ceiling; exceed it and the kernel kills you
```

- **request** = what the pod is guaranteed, and what the scheduler uses to pick a node.
- **limit** = the hard ceiling. Exceed the memory limit and the Linux kernel's OOM killer
  terminates the container. Kubernetes records the reason as **OOMKilled** and restarts
  it.

That is the single most common real-world Kubernetes failure, which is why it is the
demo scenario. The container runs `tail /dev/zero`, which buffers without bound, so it
blows past 32Mi in seconds, gets killed, restarts, and repeats.

**CrashLoopBackOff** is the related state: a container that keeps failing is restarted
with exponential backoff, and Kubernetes reports `BackOff` events.

Verified output from the real cluster:

```
NAME                            READY   STATUS      RESTARTS
checkout-api-64f8f5474b-x9bn6   0/1     OOMKilled   3 (32s ago)
payments-api-7b4ffbb85c-zfm84   0/1     Error       5 (2m25s ago)
```

## Probes

```yaml
readinessProbe:
  httpGet: { path: /healthz, port: 8080 }
  initialDelaySeconds: 2
  periodSeconds: 3
```

- **readiness** — "can this pod serve traffic?" Fails, and it is removed from the Service
  endpoints but not restarted.
- **liveness** — "is this pod alive?" Fails, and it gets restarted.

A failing readiness probe produces `Unhealthy` events, which the demo's second workload
generates on purpose.

## Events: what WatchTower actually reads

An Event is Kubernetes telling you what it did and why:

```
TYPE      REASON      OBJECT                              MESSAGE
Warning   OOMKilling  pod/checkout-api-64f8f5474b-x9bn6   Memory cgroup out of memory: killed process
Warning   BackOff     pod/checkout-api-64f8f5474b-x9bn6   Back-off restarting failed container
Warning   Unhealthy   pod/payments-api-7b4ffbb85c-zfm84   Readiness probe failed: connection refused
Normal    Scheduled   pod/checkout-api-64f8f5474b-x9bn6   Successfully assigned checkout/... to minikube
```

Fields: **type** (`Normal` or `Warning`), **reason** (a short machine token), **object**
(what it happened to), **message** (human text), **count** (how many times), and
timestamps.

Why events are gold for triage: they are the platform's own explanation of the failure.
The log says "out of memory"; the event says "the kernel killed you because you exceeded
your cgroup limit, and this is the fourth time." That is the difference between a symptom
and a cause.

Events **expire**, typically after an hour. So they must be captured when the incident
happens, which is exactly what the enrichment stage does.

## How WatchTower reads them: the API server

Every Kubernetes cluster has an **API server**, an HTTPS REST API that is the single
front door to everything. `kubectl` is just a client for it. So is WatchTower:

```
GET https://minikube:8443/api/v1/namespaces/checkout/events?limit=60
Authorization: Bearer <service account token>
```

`internal/adapters/enrich/k8s/k8s.go` does exactly that with `net/http` and parses the
JSON. **No client-go.** That is a deliberate decision worth defending:

> The official Kubernetes Go client is enormous and pulls in a large dependency tree. I
> need one read-only endpoint. Plain `net/http` against a documented REST API is about
> 200 lines, has no dependencies, and keeps the module small. If I needed watches,
> informers, caching or CRDs, client-go would be the right call.

### Noise filtering

Most events are `Normal` and irrelevant. The adapter keeps all `Warning` events, and
keeps `Normal` ones only when the object name relates to the alert's service:

```go
if !strings.EqualFold(item.Type, "Warning") && service != "" &&
   !strings.Contains(item.InvolvedObject.Name, service) {
    continue
}
```

## Security: ServiceAccounts and RBAC

This is where interviewers probe, because it is where people are careless.

**ServiceAccount** — an identity for a program (as opposed to a human). It gets a token.

**RBAC** (Role-Based Access Control) — what an identity is allowed to do, expressed as
verbs (`get`, `list`, `watch`, `create`, `delete`) on resources (`pods`, `events`).
`Role` is namespace-scoped; `ClusterRole` is cluster-wide; a `RoleBinding` or
`ClusterRoleBinding` attaches it to an identity.

This project's identity, from `deploy/k8s/watchtower-reader.yaml`:

```yaml
kind: ClusterRole
metadata:
  name: watchtower-event-reader
rules:
  - apiGroups: [""]
    resources: ["events", "pods", "services", "endpoints", "nodes"]
    verbs: ["get", "list"]
  - apiGroups: ["apps"]
    resources: ["deployments", "replicasets", "statefulsets", "daemonsets"]
    verbs: ["get", "list"]
```

**There is not a single write verb.** Say this out loud in an interview:

> WatchTower's cluster identity has `get` and `list` and nothing else. It cannot create,
> patch or delete anything, in any namespace. That is enforced by the cluster, not by my
> code — even if the service were compromised, or a model hallucinated a mutation, the
> API server would refuse. The commands in the email are executed by a human with their
> own credentials.

### In-cluster vs out-of-cluster

When a pod runs inside Kubernetes, its token and the cluster CA are mounted at
`/var/run/secrets/kubernetes.io/serviceaccount/`. The adapter auto-detects them:

```go
if cfg.APIServer == "" { cfg.APIServer = "https://kubernetes.default.svc" }
if cfg.Token == "" {
    if raw, err := os.ReadFile(serviceAccountFile); err == nil {
        cfg.Token = strings.TrimSpace(string(raw))
    }
}
```

So in production you configure nothing. For local development the token is minted with
`kubectl -n watchtower create token watchtower --duration=24h`.

### TLS, done properly

The adapter builds its own HTTP client that trusts the cluster CA:

```go
case cfg.CAFile != "":
    pem, _ := os.ReadFile(cfg.CAFile)
    pool := x509.NewCertPool()
    pool.AppendCertsFromPEM(pem)
    tlsCfg.RootCAs = pool
```

`WT_K8S_INSECURE=true` exists to skip verification, but it is off by default and you
have to opt in deliberately. In the compose overlay, WatchTower joins minikube's Docker
network and connects to `https://minikube:8443`, because `DNS:minikube` is a Subject
Alternative Name on the API server certificate:

```
DNS:minikubeCA, DNS:control-plane.minikube.internal, DNS:minikube,
DNS:kubernetes.default.svc, ... IP Address:127.0.0.1, IP Address:192.168.49.2
```

That is a nice detail to mention: verification is real, not disabled, even locally.

## minikube

**minikube** runs a single-node Kubernetes cluster on your laptop, here inside a Docker
container. Alternatives: kind, k3d, Docker Desktop's built-in cluster.

```powershell
pwsh deploy/k8s/wire-minikube.ps1     # start cluster, apply RBAC + broken workloads, mint token, copy CA
docker compose -f docker-compose.yml -f docker-compose.minikube.yml up -d
```

The script is idempotent — re-run it when the 24h token expires.

## kubectl commands worth knowing

```bash
kubectl get pods -n checkout                       # what is running
kubectl get pods -n checkout -o wide               # plus node and IP
kubectl describe pod <name> -n checkout            # full detail incl. last termination reason
kubectl logs <name> -n checkout                    # current logs
kubectl logs <name> -n checkout --previous         # logs of the container that just died
kubectl get events -n checkout --sort-by=.lastTimestamp
kubectl get events -n checkout --field-selector type=Warning
kubectl -n checkout set resources deployment/checkout-api --limits=memory=512Mi
kubectl -n checkout rollout status deployment/checkout-api
```

Those last two are what the model proposes for an OOM incident, and `--previous` is the
one people forget: when a container has already restarted, its current logs are empty and
the evidence is in the *previous* instance.

## Interview answers to have ready

**"Why not client-go?"**
One read-only REST endpoint versus a very large dependency tree. Plain `net/http` was
about 200 lines with no dependencies. I would switch to client-go the moment I needed
watches, informers or CRDs.

**"What are the security implications of giving a service cluster access?"**
Mine has `get` and `list` on events, pods and workloads — no write verbs anywhere. TLS is
verified against the cluster CA. In-cluster it uses the projected service account token,
which is short-lived and rotated. The blast radius of a compromise is read access to
metadata I already put in emails.

**"How would you handle multiple clusters?"**
`ContextProvider` is an interface and the enrich stage takes a slice of them, so one
provider per cluster, each with its own API server and token, all fanned out in parallel.
The alert's `cluster` label would select which fragment matters. No pipeline change.

---

Next: **Part 7 — Docker and Docker Compose**, the thing that runs all of this.
