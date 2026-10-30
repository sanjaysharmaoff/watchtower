# Part 12 — The Feedback Endpoints and Service (Commits 56–60)

**Goal:** the human can approve, edit or reject, and the decision is recorded durably. The
*learning* half comes in Part 13; this part closes the human half of the loop.

---

## Commit 56 — `feat(httpapi): JSON feedback endpoint`

**Files:** `internal/adapters/inbound/httpapi/handlers.go` (replace the stub)

**Build:**

```go
type feedbackRequest struct {
    IncidentID string           `json:"incident_id"`
    Signature  string           `json:"signature,omitempty"`
    Decision   string           `json:"decision"`
    Reviewer   string           `json:"reviewer,omitempty"`
    Title      string           `json:"title,omitempty"`
    RootCause  string           `json:"root_cause,omitempty"`
    Commands   []domain.Command `json:"commands,omitempty"`
    Notes      string           `json:"notes,omitempty"`
}

func (s *Server) handleFeedbackJSON(w http.ResponseWriter, r *http.Request)
```
1. wrap the body in `http.MaxBytesReader`, decode; on failure `400` with
   `{"error":"invalid JSON: ..."}`
2. `400` when `IncidentID` is blank
3. build a `domain.Feedback` using `domain.ParseDecision(req.Decision)` and
   `s.deps.Clock.Now()` for `ReceivedAt`
4. `s.deps.Feedback.Submit(r.Context(), fb)` — on error `500`
5. success → `200` with `{"status":"recorded","incident_id":...,"decision":...}`

**Verify:** build. It will return `500` until commit 60 wires a real sink — that is expected.

**Why:** this is the endpoint that carries the **most valuable** feedback: `decision: "edit"` with
corrected commands. Approve is a weak signal ("good enough"); edit tells you exactly what the right
answer was, which is the difference between a thumbs-up and a label.

It is protected by the bearer token, unlike the GET link, because tooling *can* set headers.

---

## Commit 57 — `feat(httpapi): one-click approval link`

**Files:** `internal/adapters/inbound/httpapi/handlers.go` (replace the stub)

**Build:**

```go
func (s *Server) handleFeedbackLink(w http.ResponseWriter, r *http.Request)
```
Read `incident`, `decision`, `exp`, `sig` from the query. Then, **in this order**:

1. any missing → `400` HTML *"Link is incomplete"*
2. `strconv.ParseInt(exp, 10, 64)` fails → `400` HTML *"Link is malformed"*
3. rebuild `payload := fmt.Sprintf("%s|%s|%s", incidentID, decision, exp)` and
   **`s.deps.Signer.Verify(payload, sig)`** — on failure, log a warning with the client IP and
   return `403` HTML *"Signature does not match — this link was not issued by WatchTower, or it was
   altered in transit."*
4. `s.deps.Clock.Now().Unix() > expUnix` → `410 Gone` HTML *"Link has expired"*, and **tell them
   what to do instead**: use `POST /runbook/approve`
5. submit the feedback; on error `500` HTML with the message
6. success → `200` HTML with a decision-specific message:
   - approve → *"The runbook was stored and embedded. The next incident with this signature will
     retrieve it."*
   - edit → *"Send the corrected commands to POST /runbook/approve with decision "edit" ..."*
   - reject → *"Recorded as rejected. This plan will not enter the retrieval corpus."*

**Verify:** build. Full end-to-end verification lands in commit 60.

**Why:** the order of checks 3 and 4 is deliberate — **signature before expiry**, so an attacker
cannot learn whether an incident id exists by observing which error they get.

Every response is an HTML page with a **next step**, because the reader is an engineer under
pressure, not a machine. A bare `403` would leave them stuck.

This is the endpoint exempt from the bearer token (commit 27). The signature and the expiry are the
credential, and the compensating control is that approval only *stores* a runbook — it never
executes anything.

---

## Commit 58 — `feat(services): building the reviewed summary`

**Files:** `internal/services/feedback.go`

**Build:**

```go
type Feedback struct {
    incidents ports.IncidentStore
    summaries ports.SummaryStore
    vectors   ports.VectorStore      // may be nil — retrieval arrives in Part 13
    embedder  ports.Embedder         // may be nil
    dlq       ports.DeadLetterQueue
    bus       bus.Bus
    ids       ports.IDGen
    clock     ports.Clock
    log       ports.Logger
}

func NewFeedback(...) *Feedback
func (f *Feedback) buildSummary(ctx context.Context, fb domain.Feedback) domain.ApprovedSummary
```

`buildSummary` — input: what the reviewer sent; output: a complete record. Start from the feedback
fields, then look up the incident:
- on error, warn **only when it is not `domain.ErrNotFound`**, default the title to
  `"Incident " + id`, and return what you have
- otherwise fill the gaps: title from `inc.Title()`, signature from `inc.Signature`, plus
  `Services`, `Severity` and `Labels` from `inc.PrimaryAlert().Labels`
- when the incident has a plan: fill `RootCause` if empty, and if no commands were supplied use
  **`inc.Plan.Executable()`** with the comment:
  ```go
  // Only the commands policy allowed through are eligible to be learned.
  ```

**Verify:** build.

**Why:** this is what makes a bare one-click *approve* produce a full record — everything the
reviewer did not send is filled from the stored incident. Without it, clicking a link would store a
row with almost nothing in it.

Handling `ErrNotFound` quietly while warning on other errors is the distinction between a normal
case (the incident aged out) and a real problem (the database is broken). Treating them the same
means either noisy logs or hidden failures.

`Executable()` is the safety property from commit 5 paying off: a blocked command can never enter
the corpus, even via a careless approval, and the feedback service does not need to know guardrails
exist.

---

## Commit 59 — `feat(services): recording the decision`

**Files:** `internal/services/feedback.go` (extend)

**Build:**

```go
func (f *Feedback) Submit(ctx context.Context, fb domain.Feedback) error
func (f *Feedback) park(ctx context.Context, stage, reason string, summary domain.ApprovedSummary)
```

`Submit`:
1. error when `IncidentID` is blank
2. **default an empty decision to `DecisionReject`** — fail closed
3. default `ReceivedAt` to now
4. `summary := f.buildSummary(ctx, fb)`
5. `SaveSummary` — **on error, park it and continue**, with the comment:
   ```go
   // The decision is too valuable to drop: park it and keep going, because the
   // runbook can still be learned and the DLQ row can be replayed later.
   ```
6. publish `bus.TopicFeedbackAccepted`
7. when `!fb.Decision.Learnable()` → log `"feedback recorded"` with `learned=false` and return nil
8. otherwise call `f.learn(ctx, summary)` — for now a stub returning
   `fmt.Errorf("retrieval corpus is not configured")` when the embedder or vector store is nil;
   on error, **warn and return nil** (learning is best effort)
9. log `"feedback recorded"` with `learned=true`

**Verify:** build.

**Why:** three decisions to be able to defend.

**Fail closed on a missing decision**, because this path writes to the corpus.

**A failed write does not abort.** The decision is durable in the DLQ and the runbook can still be
learned. But volunteer the tradeoff: the API returns success after a database failure. Defensible
because nothing is lost, and I would rather return `202` with a warning — say so before an
interviewer finds it.

**Learning is best effort.** Recording is the contract; learning is the enhancement. If embedding is
down, the decision must still be captured.

---

## Commit 60 — `chore(bootstrap): wire the feedback service`

**Files:** `internal/bootstrap/app.go` (edit)

**Build:**

Add `Feedback *services.Feedback` to `App`, construct it in `build()` with `nil` for the vector
store and embedder for now, and pass it into `httpapi.Deps`:

```go
a.Feedback = services.NewFeedback(incidentStore, summaryStore, nil, nil, dlq,
    a.broker, env.IDs, env.Clock, log)

a.server = httpapi.New(httpapi.Deps{
    ...
    Feedback: a.Feedback,
    Signer:   sign.HMAC(cfg.HTTP.ApprovalSecret),
    Version:  Version,
    Stats:    a.pool.Stats,
})
```
You also need `summaryStore, err := wire.SummaryStores.Build(env, cfg.Store.Driver)`.

**Verify — the human half of the loop, end to end:**

```bash
go build -o bin/watchtowerd ./cmd/watchtowerd && ./bin/watchtowerd

curl -X POST localhost:8080/alerts -H "Content-Type: application/json" \
  --data-binary @examples/alerts/alertmanager-oom.json
```
Copy the **Approve** link from the printed email and open it in a browser. You should get the
*"Approved"* page, and the log should show
`feedback recorded ... decision=approve learned=false` (false is correct — no corpus yet).

Now the API path with an edit:
```bash
curl -X POST http://localhost:8080/runbook/approve -H "Content-Type: application/json" -d '{
  "incident_id": "<paste>",
  "decision": "edit",
  "reviewer": "sre-oncall",
  "root_cause": "Memory limit of 32Mi is below the working set after release 4.2.1",
  "commands": [
    {"run": "kubectl -n checkout set resources deployment/checkout-api --limits=memory=512Mi",
     "explain": "Raise the limit to the observed working set"}
  ]
}'
```
→ `{"decision":"edit","incident_id":"...","status":"recorded"}`

Also verify the negative paths: change one character of a link's `sig` (expect `403`), and hand-craft
a link with `exp` in the past (expect `410`).

**Why:** the loop is now closed on the human side — the system asks, a person answers, and the answer
is stored. Part 13 makes that answer *useful* by embedding it.

**Note the bug lurking here** and consider fixing it as an extra commit: `Submit` generates a fresh
`f.ids.New()` every call, so clicking the Approve link twice creates **two** summary rows. Email
clients prefetch links, so this will happen. A deterministic id — a hash of
`incident_id|decision|reviewer` — or a unique constraint on `(incident_id, decision)` fixes it.

---

## End of Part 12

An engineer can approve, edit or reject, and every decision is durably recorded.

```bash
go build ./... && go vet ./... && gofmt -l .
```

Next: **Part 13** — retrieval, and the commit where the learning loop actually closes.
