# Part 11 — Redis, SMTP and Cron

Three smaller technologies, each solving one specific problem. Short part, but each one has
an interview question attached.

## Redis

Redis is an in-memory key/value store. Because it lives in RAM it answers in
microseconds, and because it is a separate process, **several application replicas can
share state through it**. That second property is the only reason this project uses it.

### The problem it solves here

The debouncer remembers "I have already handled signature X". With one WatchTower replica,
an in-memory map is perfect. With two replicas behind a load balancer, Alertmanager's
webhook lands on replica A, the duplicate lands on replica B, and B has never heard of that
signature. **The engineer gets two emails.** The in-memory gate is correct and useless at
the same time.

Same for the embedding cache: replica A pays for an embedding, replica B pays again.

So Redis is not "for speed" here. It is for **shared state across replicas**. Say it that
way; it shows you understand *why*, not just *what*.

### SET NX: distributed locking in one command

```go
func (r *Redis) Allow(ctx context.Context, key string, window time.Duration) (bool, error) {
    ok, err := r.client.SetNX(ctx, r.prefix+key, []byte("1"), window)
    if err != nil {
        return false, fmt.Errorf("redis gate: %w", err)
    }
    return ok, nil
}
```

`SETNX` = "SET if Not eXists". It is **atomic**: exactly one caller gets `true`, everyone
else gets `false`, even if a hundred replicas call simultaneously. With a TTL attached, the
key expires on its own — no cleanup job, no cron, no leak. That is the entire distributed
debouncer, in one command.

This is worth memorising as a pattern: *SET NX with TTL is the simplest correct distributed
lock / once-only guard.*

### TTL

Every key here has an expiry: the gate's key expires after the debounce window, cached
embeddings after `WT_RAG_CACHE_TTL` (24h). Redis is memory, so unbounded keys mean eventual
eviction or death. TTL makes the data structure self-cleaning.

### The shared client

Both the gate and the cache need Redis. Neither should own the connection — if one closed
it, the other would break. So the client lives in the `Env` resource pool
(`internal/platform/redisx`), behind a deliberately narrow interface:

```go
type Client interface {
    Get(ctx context.Context, key string) ([]byte, bool, error)
    Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
    SetNX(ctx context.Context, key string, value []byte, ttl time.Duration) (bool, error)
    Ping(ctx context.Context) error
    Close() error
}
```

Five methods, not the ~200 of the Redis protocol. Only what the project actually needs,
which makes it trivially fake-able in a test and keeps `go-redis` from leaking into the
rest of the code.

Note also that `Redis.Close()` on the *cache* adapter is a no-op — the shared client is
closed once by the `Env`, not by each consumer. Getting that wrong is how you get "use of
closed connection" bugs.

### Fail-open

If Redis is unreachable, the gate returns an error, and the debounce stage does this:

```go
allowed, err := gate.Allow(ctx, key, window)
if err != nil {
    // Fail open: a broken gate must never stop real incidents.
    inc.Note("debounce gate error, failing open: %v", err)
    return nil
}
```

Broken Redis means duplicate emails. Fail-closed would mean **no** emails during a real
outage — which is precisely when you need them. Choosing the direction of failure
deliberately, and writing the reason in the code, is the kind of thing interviewers
remember.

## SMTP and email

**SMTP** (Simple Mail Transfer Protocol) is how mail is sent. Your app connects to a relay
(Amazon SES, SendGrid, or a corporate server), authenticates, and hands over the message.
The relay handles the hard parts: DNS, retries, reputation, DKIM signing.

### The conversation

```
connect tcp relay:587
EHLO                      → capabilities offered, including STARTTLS
STARTTLS                  → upgrade the plaintext socket to TLS
AUTH PLAIN                → username / password
MAIL FROM: <watchtower@example.com>
RCPT TO: <sre@example.com>
DATA                      → headers + body, terminated by a lone "."
QUIT
```

Port 587 with STARTTLS is the modern default (465 is implicit TLS; 25 is server-to-server).
Go's standard library `net/smtp` implements all of it — no dependency needed.

Two things the adapter does that are easy to get wrong:

**It refuses to silently downgrade.** If TLS is required but the relay does not offer
STARTTLS, it errors rather than sending credentials in plaintext:

```go
if s.useTLS {
    if ok, _ := client.Extension("STARTTLS"); ok {
        if err := client.StartTLS(&tls.Config{ServerName: s.host, MinVersion: tls.VersionTLS12}); err != nil {
            return fmt.Errorf("smtp: starttls: %w", err)
        }
    } else {
        return fmt.Errorf("smtp: relay does not offer STARTTLS but TLS is required")
    }
}
```

**It bounds the connection by the caller's deadline**, so a wedged relay cannot pin a
worker goroutine forever.

### multipart/alternative

The email is sent as two versions of the same message:

```
Content-Type: multipart/alternative; boundary="wt-<uuid>"

--wt-...
Content-Type: text/plain; charset="utf-8"
   ...the plain text runbook...
--wt-...
Content-Type: text/html; charset="utf-8"
   ...the styled HTML with Approve/Edit/Reject buttons...
--wt-...--
```

The client picks the richest part it can render. Terminal mail clients, pagers and
text-only readers get the plain part — and for an on-call engineer at 3am on a phone, that
is not a nicety. Accessibility and reachability, same mechanism.

### Header injection

```go
// sanitizeHeader strips CR/LF so metadata can never inject extra headers.
func sanitizeHeader(v string) string {
    return strings.NewReplacer("\r", " ", "\n", " ").Replace(v)
}
```

Metadata from an incident is written into `X-WatchTower-*` headers. If a value contained a
newline, an attacker could append arbitrary headers — including `Bcc:`. This is the email
equivalent of SQL injection and it is why the values are sanitized. A small line that
signals you think about untrusted data.

### Why HTML email is written the way it is

Email clients strip `<style>` blocks and do not support modern CSS, so the template uses
**inline styles only** and simple layout. It still uses real semantic HTML — `<h1>`, `<h2>`,
`<ol>`, `<ul>`, `<code>`, `lang="en"` — so screen readers work and the plain-text fallback
is meaningful.

### Mailpit

In development the relay is **Mailpit**: a real SMTP server on port 1025 with a web UI on
8025 that shows what it received. Nothing leaves the machine. `WT_SMTP_TLS=false` because
it is a local plaintext relay. Verified: four messages captured, including the daily pulse.

Switching to production is configuration only:

```bash
WT_SMTP_HOST=email-smtp.eu-west-1.amazonaws.com
WT_SMTP_PORT=587
WT_SMTP_USERNAME=...
WT_SMTP_PASSWORD=...
WT_SMTP_TLS=true
```

### The signed approval links

The email's buttons are GET links, because email clients cannot send POST requests with
headers. So the link itself has to be the credential:

```
/runbook/approve?incident=<id>&decision=approve&exp=<unix>&sig=<hmac>
```

The signature is HMAC-SHA256 over `incident|decision|exp` with `WT_APPROVAL_SECRET`:

```go
func (s hmacSigner) Sign(payload string) string {
    mac := hmac.New(sha256.New, s.secret)
    mac.Write([]byte(payload))
    return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
func (s hmacSigner) Verify(payload, signature string) bool {
    expected := s.Sign(payload)
    return hmac.Equal([]byte(expected), []byte(signature))
}
```

Three properties to be able to state:

- **Unforgeable** without the secret. Changing `decision=approve` to something else
  invalidates the signature.
- **Expiring**, via `exp`, checked server-side (`WT_APPROVAL_TTL`, default 168h). A
  forwarded email does not grant permanent authority.
- **`hmac.Equal`, not `==`** — constant-time comparison, so an attacker cannot learn the
  correct signature byte-by-byte from response timing.

This is the one endpoint that bypasses the bearer token, and that is a documented, deliberate
tradeoff: usability of one-click approval versus header-based auth. The mitigation is the
signature plus the expiry, and the fact that approval only stores a runbook — it never
executes anything.

## Cron

**Cron** is time-based scheduling. The five-field spec is `minute hour day-of-month month
day-of-week`:

```
0 0 * * *     midnight every day        → partition maintenance
0 8 * * *     08:00 every day           → the daily pulse
*/5 * * * *   every 5 minutes
0 */6 * * *   every 6 hours
```

This project uses `robfig/cron/v3`, wrapped behind `ports.Scheduler` so the mechanism is
swappable:

```go
type Scheduler interface {
    Schedule(name, spec string, job func(context.Context)) error
    Start()
    Stop(ctx context.Context) error
}
```

The wrapper adds three things the raw library does not:

- **Panic recovery** per job, so one broken job cannot kill the process.
- **A context with timeout** per run, and cancellation on shutdown.
- **Structured logging** of start, finish and duration.

Timezone matters: `cron.WithLocation`, driven by `WT_CRON_TZ` (default UTC). "08:00" is
meaningless without a zone, and using the server's local zone is how you get reports at
random hours after a datacentre migration.

### The alternative execution model

The same jobs run as one-shots:

```bash
./bin/watchtowerd --job maintenance
./bin/watchtowerd --job digest
```

Set `WT_CRON_ENABLED=false` and let Kubernetes CronJobs do the scheduling instead. Why
support both: internal cron is convenient for a single deployment, but in Kubernetes the
platform already has a scheduler with retries, history and alerting on failure — and if you
run three replicas with internal cron, you send three daily reports. Supporting both is one
`switch` statement in `App.RunOnce`, and it is a strong answer to "how would this run in
production?"

---

That completes the technology tour. Next: **Part 12 — Ports and Adapters**, where the
architecture starts.
