# Part 22 — Code Walkthrough: Storage, Delivery, HTTP and the Worker Pool

The outward-facing adapters: `storage/*`, `delivery/*`, `inbound/httpapi` and `ingest/worker`.

## `storage/postgres`

Four files, one shared pool.

- **`db.go`** — `Pool(e)` via the resource pool, plus `migrate()`. Covered in Parts 8 and 15.
- **`incidents.go`** — `IncidentStore`. Upsert into the partitioned table, JSONB payload.
- **`summaries.go`** — `SummaryStore` and the `DLQ`.
- **`partitions.go`** — `PartitionManager`.
- **`vector.go`** — `VectorStore` on pgvector.

### One detail worth quoting: the DLQ's last resort

```go
func (d *DLQ) Park(ctx context.Context, dl domain.DeadLetter) error {
    const q = `INSERT INTO dead_letter_queue (id, stage, reason, ref_id, payload, at)
               VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT (id) DO NOTHING`
    ...
    if _, err := d.pool.Exec(ctx, q, ...); err != nil {
        // Last resort: if even the DLQ write fails, the log line is the record.
        d.log.Error("dead letter write failed", "stage", dl.Stage, "ref", dl.RefID,
            "reason", dl.Reason, "err", err.Error())
        return fmt.Errorf("insert dead_letter_queue: %w", err)
    }
    return nil
}
```

Every failure path needs a terminus. If the database is unreachable, the DLQ write also fails, so
the structured log line becomes the record of last resort. Thinking one level past "and what if
*that* fails" is what an interviewer is checking for.

### `ListSummaries` and COALESCE

```sql
SELECT id, incident_id, COALESCE(signature,''), title, COALESCE(root_cause,''), commands,
       decision, COALESCE(reviewer,''), COALESCE(notes,''), COALESCE(services,'{}'),
       COALESCE(severity,''), created_at
FROM approved_summaries
WHERE created_at >= $1 AND created_at < $2
ORDER BY created_at DESC
```

`COALESCE` everywhere because scanning SQL `NULL` into a Go `string` fails. The alternatives are
`sql.NullString` for every field or `*string` pointers — both noisier. Note the range is
`>= from AND < to`: half-open, so consecutive digest windows can never double-count a row.

## `storage/memstore` — one type, four ports

```go
func init() {
    wire.IncidentStores.Register("memory",    func(e *wire.Env) (ports.IncidentStore, error)    { return shared(e), nil })
    wire.SummaryStores.Register("memory",     func(e *wire.Env) (ports.SummaryStore, error)     { return shared(e), nil })
    wire.DeadLetterQueues.Register("memory",  func(e *wire.Env) (ports.DeadLetterQueue, error)  { return shared(e), nil })
    wire.PartitionManagers.Register("memory", func(e *wire.Env) (ports.PartitionManager, error) { return shared(e), nil })
    wire.DeadLetterQueues.Register("log",     func(e *wire.Env) (ports.DeadLetterQueue, error)  { return LogOnly{...}, nil })
}
```

One `*Store` satisfies four interfaces, and `shared(e)` puts it in the resource pool so all four
registrations return **the same instance** — a summary written through one interface is visible
through another, exactly like a real database.

```go
func shared(e *wire.Env) *Store {
    s, err := wire.Resource(e, "memstore", func() (*Store, error) {
        return New(e.Cfg.Store.MemoryLimit, e.Log.With("component", "memstore")), nil
    })
    if err != nil {
        return New(e.Cfg.Store.MemoryLimit, e.Log)   // degrade to a private instance
    }
    return s
}
```

It is **bounded** — a ring of `WT_STORE_MEMORY_LIMIT` (default 500) — so a long-running dev
process cannot leak:

```go
for len(s.order) > s.limit {
    delete(s.incidents, s.order[0])
    s.order = s.order[1:]
}
```

And the partition methods are honest no-ops with a comment explaining that the maintenance
service should not have to know which storage it is talking to. There is also a `log` DLQ driver
for "no database at all, losing the payload is acceptable".

## `delivery/render` — the email

```go
type HTML struct {
    baseURL string
    signer  ports.Signer
    ttl     time.Duration
    clock   ports.Clock
    to      []string
    incTpl  *template.Template
    digTpl  *template.Template
}
```

**`html/template`, not `text/template`.** This is the security-relevant choice: `html/template`
is context-aware and escapes automatically depending on where a value lands — HTML body,
attribute, URL, JavaScript. Log lines and model output go into this email, so without escaping a
log line containing `<script>` would be an XSS vector against whoever opens it in a webmail
client.

Templates are parsed **once** in the constructor, not per email. Parsing is expensive; executing
is cheap.

```go
func (h *HTML) link(incidentID string, decision domain.Decision) string {
    exp := h.clock.Now().Add(h.ttl).Unix()
    payload := fmt.Sprintf("%s|%s|%d", incidentID, decision, exp)

    q := url.Values{}
    q.Set("incident", incidentID)
    q.Set("decision", string(decision))
    q.Set("exp", strconv.FormatInt(exp, 10))
    q.Set("sig", h.signer.Sign(payload))
    return h.baseURL + "/runbook/approve?" + q.Encode()
}
```

The signature covers **all three** of incident, decision and expiry. Sign only the incident and
an attacker could change `decision=reject` to `decision=approve`; omit the expiry and the link
is valid forever.

Both an HTML and a plain-text body are produced, so terminal mail clients, pagers and screen
readers get something meaningful (Part 11). The text version includes the matched runbooks,
blocked commands with reasons, and the three links.

## `delivery/notify` — four drivers

`smtp` (Part 11), `stdout` (prints the text part; the default so a fresh clone shows output with
no mail config), `none`, and `memory` — a `Collector` that keeps messages in a slice with a
`Sent()` accessor, which makes asserting on delivery in a test a two-line affair.

## `delivery/scheduler` — cron with three additions

The `robfig/cron` wrapper adds per-job panic recovery, a per-run context with a 30-minute
timeout tracked for cancellation on shutdown, and start/finish/duration logging. Plus:

```go
func (s *Cron) Schedule(name, spec string, job func(context.Context)) error {
    if spec == "" {
        s.log.Info("job disabled, empty schedule", "job", name)
        return nil
    }
    ...
}
```

An empty spec disables a job rather than erroring — so `WT_CRON_DIGEST_SPEC=` turns off the daily
report without needing a separate boolean.

## `ingest/worker` — the bounded pool

The single most important operational component, because it is what stops a webhook flood from
becoming an outage.

```go
var ErrQueueFull = errors.New("worker queue is full")
var ErrClosed    = errors.New("worker pool is closed")

func (p *Pool) Submit(job func(context.Context)) error {
    if p.closing.Load() { return ErrClosed }
    select {
    case p.queue <- job:
        p.accepted.Add(1)
        return nil
    default:
        p.rejected.Add(1)
        return ErrQueueFull
    }
}
```

**Non-blocking submit.** `select` with `default` means the HTTP handler never waits. A full queue
is an immediate, honest `429` rather than a request that hangs until Alertmanager times out and
retries — which would add load during overload. That is the difference between backpressure and
collapse.

```go
func (p *Pool) run(job func(context.Context)) {
    defer func() {
        if r := recover(); r != nil {
            p.failed.Add(1)
            p.log.Error("job panicked", "panic", r)
        }
    }()

    ctx := context.Background()
    if p.jobTimeout > 0 {
        var cancel context.CancelFunc
        ctx, cancel = context.WithTimeout(ctx, p.jobTimeout)
        defer cancel()
    }
    job(ctx)
    p.done.Add(1)
}
```

Note `context.Background()`, **not the request's context.** This is subtle and worth being able
to explain: the HTTP handler returns `202` immediately, which cancels the request context. If the
job inherited it, every job would be cancelled the moment the response was written. So the job
gets a fresh context bounded by `WT_JOB_TIMEOUT` instead.

Graceful shutdown drains what is already queued:

```go
case <-p.closed:
    // Drain whatever is already queued before exiting.
    for {
        select {
        case job := <-p.queue:
            p.run(job)
        default:
            return
        }
    }
```

And counters are exposed for the status endpoint, using `atomic.Int64` (Go 1.19+) rather than a
mutex:

```go
func (p *Pool) Stats() map[string]int64 {
    return map[string]int64{
        "accepted": ..., "completed": ..., "rejected": ..., "panics": ..., "queued": int64(len(p.queue)),
    }
}
```

Verified: `{"accepted":2,"completed":2,"panics":0,"queued":0,"rejected":0}`.

## `inbound/httpapi` — the edge

### Routing with Go 1.22 patterns

```go
mux := http.NewServeMux()
mux.HandleFunc("POST /alerts", s.handleAlerts)
mux.HandleFunc("POST /runbook/approve", s.handleFeedbackJSON)
mux.HandleFunc("GET /runbook/approve", s.handleFeedbackLink)
mux.HandleFunc("GET /healthz", s.handleHealth)
mux.HandleFunc("GET /readyz", s.handleReady)
mux.HandleFunc("GET /status", s.handleStatus)
```

Method-aware patterns are new in Go 1.22, which is why there is **no router dependency**. Before
1.22 this needed chi or gorilla/mux, or a manual method check in every handler.

### Middleware chain

```go
handler := s.withRecover(s.withLogging(s.withAuth(mux)))
```

Recover outermost (catches panics from everything, including the auth middleware), then logging
(so a rejected request is still logged), then auth.

### Server timeouts

```go
s.srv = &http.Server{
    Addr:              d.Config.Addr,
    Handler:           handler,
    ReadHeaderTimeout: 5 * time.Second,
    ReadTimeout:       d.Config.ReadTimeout,
    WriteTimeout:      d.Config.WriteTimeout,
    IdleTimeout:       d.Config.IdleTimeout,
}
```

`ReadHeaderTimeout` is the one people omit, and it is the defence against Slowloris — a client
that opens connections and dribbles headers forever to exhaust your connection table. Go's
default `http.Server` has *no* timeouts at all, which is a well-known footgun.

### Auth, and the one documented exception

```go
func (s *Server) withAuth(next http.Handler) http.Handler {
    token := strings.TrimSpace(s.deps.Config.AuthToken)
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        if token == "" || isSignedLink(r) || isPublic(r) {
            next.ServeHTTP(w, r)
            return
        }
        provided := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
        if provided == "" {
            provided = strings.TrimSpace(r.Header.Get("X-WatchTower-Token"))
        }
        if !constantTimeEqual(provided, token) {
            s.log.Warn("unauthenticated request rejected", "path", r.URL.Path, "remote", clientIP(r))
            writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
            return
        }
        next.ServeHTTP(next)
    })
}
```

Three things:

- **`constantTimeEqual`**, not `==`, so response timing cannot leak the token byte by byte.
- **`/healthz` and `/readyz` are public**, because probes generally cannot carry credentials.
- **`GET /runbook/approve` is exempt**, with the reason in a comment:

```go
// GET /runbook/approve is deliberately exempt: it is clicked from an email client
// that cannot set headers, and it carries its own HMAC signature and expiry, which
// is what authenticates it.
```

Naming a deliberate exception and its compensating control is much stronger than pretending
every endpoint is uniformly protected.

### `handleAlerts`

```go
body := http.MaxBytesReader(w, r.Body, s.maxBody())
defer body.Close()

alerts, err := s.deps.Decoder.Decode(body)
if err != nil {
    s.log.Warn("alert decode failed", "decoder", s.deps.Decoder.Name(), "err", err.Error())
    writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
    return
}

id, err := s.deps.Alerts.Submit(r.Context(), alerts)
switch {
case errors.Is(err, worker.ErrQueueFull):
    // Tell the sender to back off rather than pretending we accepted the work.
    w.Header().Set("Retry-After", "30")
    writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "pipeline saturated, retry shortly", "incident_id": id})
case err != nil:
    writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
default:
    writeJSON(w, http.StatusAccepted, map[string]any{"incident_id": id, "alerts": len(alerts), "status": "queued"})
}
```

**`MaxBytesReader`** caps the body at `WT_HTTP_MAX_BODY_BYTES` (2MB), so a huge or malicious
payload cannot exhaust memory. **`202 Accepted`**, not `200`, because the work has been queued
rather than completed — the correct status code, and it tells Alertmanager not to expect results.
**Three distinct outcomes mapped to three status codes**, with `Retry-After` on the backpressure
path.

### `handleFeedbackLink`

```go
expUnix, err := strconv.ParseInt(exp, 10, 64)
...
payload := fmt.Sprintf("%s|%s|%s", incidentID, decision, exp)
if !s.deps.Signer.Verify(payload, sig) {
    s.log.Warn("rejected approval link with bad signature", "incident", incidentID, "remote", clientIP(r))
    writeHTML(w, http.StatusForbidden, "Signature does not match",
        "This link was not issued by WatchTower, or it was altered in transit.")
    return
}
if s.deps.Clock.Now().Unix() > expUnix {
    writeHTML(w, http.StatusGone, "Link has expired",
        "Approval links are short lived. Use the API endpoint POST /runbook/approve to record your decision.")
    return
}
```

Signature **before** expiry, so an attacker cannot learn whether an incident id exists by
observing which error they get. Failed verification is logged with the source IP. And the human
gets an HTML page with an actual next step, not a bare `403` — the person reading it is an
engineer under pressure, and every response tells them what to do next.

`html.EscapeString` is used on everything written into those pages.

### `/healthz` vs `/readyz`

```go
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
    writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": s.deps.Version})
}
```

Liveness: "the process is running." Cheap and dependency-free — if it consulted the database, a
brief database blip would make Kubernetes kill a perfectly healthy pod.

```go
func (a *App) ready(ctx context.Context) error {
    ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
    defer cancel()
    return a.Maintenance.Run(ctx)
}
```

Readiness: "this instance can actually do its job." It runs partition maintenance, which
genuinely exercises DDL and connectivity — if partitions cannot be created, writes will fail, so
the instance should not receive traffic. Using a real operation rather than a `SELECT 1` is a
strong touch. It is also idempotent, so calling it on every probe is safe.

Getting the liveness/readiness distinction right is a standard Kubernetes interview question, and
this codebase has an opinion on it.

---

Next: **Part 23 — services, projectors, bootstrap, and a full trace of one real incident.**
