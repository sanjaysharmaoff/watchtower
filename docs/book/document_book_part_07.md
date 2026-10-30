# Part 7 — Docker and Docker Compose

Docker is how the nine services in this project run on one laptop without installing
anything. This part covers the concepts, then reads the project's own Dockerfile and
compose files line by line.

## Images and containers

An **image** is a read-only filesystem plus metadata (which command to run, which port to
expose). A **container** is a running instance of an image. One image, many containers.

Analogy that works in interviews: image is a class, container is an instance.

A container is not a virtual machine. It shares the host's Linux kernel and is isolated
using kernel features (namespaces for what it can see, cgroups for what it can use). That
is why containers start in milliseconds and a VM takes a minute — and also why the memory
`limit` from Part 6 is enforced by the *kernel*, which is what "OOMKilled" means.

## The registry

Images live in registries — Docker Hub by default. `pgvector/pgvector:pg16` means
organisation `pgvector`, image `pgvector`, tag `pg16`. Tags are mutable labels, not
guarantees, which is why pinning matters: this project pins `prom/prometheus:v2.55.1`
and `grafana/loki:2.9.8` rather than using `latest`.

## Reading this project's Dockerfile

```dockerfile
FROM golang:1.24-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags "-s -w -X github.com/sanjaysharmaoff/watchtower/internal/bootstrap.Version=${VERSION}" \
    -o /out/watchtowerd ./cmd/watchtowerd

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/watchtowerd /app/watchtowerd
COPY --from=build /src/migrations /app/migrations

USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/app/watchtowerd"]
```

Every line is a decision you can defend:

**`FROM ... AS build` / second `FROM`** — a **multi-stage build**. The first stage has the
whole Go toolchain (hundreds of MB). The second stage copies only the compiled binary.
The toolchain never ships.

**`COPY go.mod go.sum` before `COPY . .`** — Docker caches each layer and invalidates
everything after the first change. Dependencies change rarely, source changes constantly.
Copying dependency files first means editing a `.go` file does not re-download modules.
This is *the* classic Docker layer-caching idiom.

**`CGO_ENABLED=0`** — build a fully static binary with no libc dependency. Required for
distroless/scratch base images. Go can do this because its runtime does not need libc,
unless you use packages that bind to C.

**`-trimpath`** — strip local filesystem paths from the binary. Reproducible builds, and
no leaking of `/home/yourname/...` into stack traces.

**`-ldflags "-s -w -X ...Version=${VERSION}"`** — `-s -w` strip debug info (smaller
binary); `-X` **injects a value into a variable at link time**. That is how
`bootstrap.Version` becomes `compose` without touching source, and why
`GET /healthz` returns `{"status":"ok","version":"compose"}`.

**`gcr.io/distroless/static-debian12:nonroot`** — a base image with no shell, no package
manager, no coreutils. Just enough to run a static binary. Two benefits: the attack
surface is tiny (an attacker who gets code execution has no `sh` to work with), and the
image is a few MB. The `:nonroot` variant plus `USER nonroot:nonroot` means the process
does not run as root — if it is compromised, it cannot write to the filesystem or bind
privileged ports.

**`COPY migrations`** — the SQL is shipped even though the app applies schema itself, so
an operator can run it manually when `WT_STORE_AUTO_MIGRATE=false`.

## Docker Compose

Compose runs several containers together from one YAML file: a private network, DNS by
service name, volumes, dependency ordering, environment injection.

**The single most useful fact:** inside a compose network, services reach each other by
**service name**. That is why WatchTower's config says `postgres:5432`, `redis:6379`,
`loki:3100`, `ollama:11434` — no IP addresses anywhere. Alertmanager posts to
`http://watchtower:8080/alerts` for the same reason.

### Ports: publishing vs internal

```yaml
ports:
  - "8080:8080"     # hostPort:containerPort
```

`ports` publishes a container port on your **host** so *you* can reach it from a browser
or curl. Containers talking to each other do not need it — they use the internal network.
This project publishes everything anyway, so you can inspect each piece.

### Volumes: two kinds

```yaml
volumes:
  - pgdata:/var/lib/postgresql/data                    # named volume: managed, persistent
  - ./deploy/prometheus/prometheus.yml:/etc/prometheus/prometheus.yml:ro   # bind mount: host file
```

- **Named volume** (`pgdata`, `ollama`) — Docker-managed storage that survives
  `docker compose down`. Containers are ephemeral; databases and 1GB model files must not
  be. `down -v` is what actually deletes them.
- **Bind mount** — maps a host path into the container. Used for config files, with `:ro`
  for read-only. Edit `prometheus.yml` on your laptop, the container sees it immediately.

### depends_on and health checks

```yaml
depends_on:
  postgres:
    condition: service_healthy
  ollama-init:
    condition: service_completed_successfully
```

Plain `depends_on` only waits for *started*, which is nearly useless — Postgres takes
seconds after start before it accepts connections. `condition: service_healthy` waits for
the health check to pass:

```yaml
healthcheck:
  test: ["CMD-SHELL", "pg_isready -U watchtower -d watchtower"]
  interval: 5s
  timeout: 5s
  retries: 20
```

`service_completed_successfully` waits for a **one-shot** container to exit 0. This
project uses it for model downloads:

```yaml
ollama-init:
  image: ollama/ollama:latest
  entrypoint: ["/bin/sh", "-c"]
  command:
    - >-
      until ollama list >/dev/null 2>&1; do echo "waiting for ollama"; sleep 3; done;
      ollama pull qwen2.5:1.5b &&
      ollama pull nomic-embed-text &&
      echo "models ready"
  environment:
    OLLAMA_HOST: http://ollama:11434
```

WatchTower waits for that to finish, so the first incident never races a cold model
store. Init containers like this are a standard pattern and a good thing to point at.

## The nine services

| Service | Image | Role |
| --- | --- | --- |
| `watchtower` | built from source | the application |
| `postgres` | `pgvector/pgvector:pg16` | storage + vector search |
| `redis` | `redis:7-alpine` | shared debounce gate + embedding cache |
| `ollama` | `ollama/ollama` | serves the models |
| `ollama-init` | `ollama/ollama` | one-shot model download |
| `prometheus` | `prom/prometheus:v2.55.1` | evaluates the alert rules |
| `alertmanager` | `prom/alertmanager:v0.27.0` | groups and posts the webhook |
| `loki` | `grafana/loki:2.9.8` | log storage |
| `log-pusher` | `python:3.11-alpine` | writes realistic logs into Loki |
| `mailpit` | `axllent/mailpit` | fake SMTP server with a web UI |

**Mailpit** deserves a note: it speaks real SMTP on port 1025 and shows what it received
at `http://localhost:8025`. Nothing leaves your machine. The standard way to test email
without spamming anyone.

## The overlay pattern

```powershell
docker compose -f docker-compose.yml -f docker-compose.minikube.yml up -d
```

Multiple `-f` files are **merged**, later ones overriding earlier ones. The base file is
the self-contained stack. The overlay adds Kubernetes:

```yaml
services:
  watchtower:
    networks: [default, minikube]
    environment:
      WT_ENRICH_PROVIDERS: loki,k8s
      WT_K8S_API_SERVER: https://minikube:8443
      WT_K8S_CA_FILE: /etc/watchtower/minikube-ca.crt
      WT_K8S_TOKEN: ${WT_K8S_TOKEN:?run deploy/k8s/wire-minikube.ps1 first}
    volumes:
      - ./deploy/k8s/minikube-ca.crt:/etc/watchtower/minikube-ca.crt:ro

networks:
  minikube:
    external: true
    name: minikube
```

Three techniques worth naming:

- **`networks: [default, minikube]`** — one container on two networks. It keeps talking to
  Postgres on the compose network while reaching the API server on minikube's.
- **`external: true`** — do not create this network, attach to one that already exists
  (minikube made it).
- **`${WT_K8S_TOKEN:?message}`** — compose variable substitution with a required-error. If
  the variable is missing, compose refuses to start and prints the message. Compose reads
  it from the `.env` file in the project root, which `wire-minikube.ps1` writes.

Why an overlay instead of just adding k8s to the base file: plain `docker compose up` must
work for someone with no Kubernetes. The k8s provider's factory *errors* when it has no
token, and a failed factory fails startup by design. The overlay keeps the default path
working.

## Commands you will actually use

```bash
docker compose up -d --build          # build and start everything, detached
docker compose ps                     # what is running, and health
docker compose logs -f watchtower     # follow one service's logs
docker compose up -d --build watchtower   # rebuild and restart just one service
docker compose exec -T postgres psql -U watchtower -d watchtower -c "select 1;"
docker compose run --rm watchtower --job digest   # one-off run with the same config
docker compose down                   # stop and remove containers
docker compose down -v                # ...and delete the volumes (data loss)
```

`exec` runs a command in a *running* container; `run` starts a *new* one. `--rm` cleans it
up afterwards. `docker compose run --rm watchtower --job digest` is how the daily report
was tested — same image, same environment, one job, then exit.

## Interview answers to have ready

**"Walk me through your Dockerfile."**
Multi-stage: build with the Go toolchain, ship only a static binary on distroless-nonroot.
Dependency files copied before source for layer caching. `CGO_ENABLED=0` for a static
binary, `-trimpath` for reproducibility, `-ldflags -X` to inject the version at link time.
Final image has no shell and does not run as root.

**"How do the services find each other?"**
Compose DNS by service name on a private network. No IPs or host ports in any
configuration — `postgres:5432`, `loki:3100`, `ollama:11434`.

**"How do you make sure the app does not start before its dependencies?"**
`depends_on` with `condition: service_healthy` for Postgres, which has a `pg_isready`
health check, and `condition: service_completed_successfully` for the one-shot container
that downloads the models.

**"Is this production-ready deployment?"**
No, and deliberately not. Compose is for local development and demonstration. Production
would be Kubernetes manifests or Helm: the app is already a stateless static binary
configured purely by environment variables, with `/healthz` and `/readyz` endpoints, so
it maps directly onto a Deployment with probes, plus a CronJob per `--job`.

---

Next: **Part 8 — PostgreSQL**, including partitioning and JSONB.
