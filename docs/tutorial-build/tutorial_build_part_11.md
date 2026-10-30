# Part 11 — Signed Links, HTML Email, Delivery (Commits 51–55)

**Goal:** replace the temporary stdout `Tap` with a real renderer and a delivery projector. By the
end, an incident produces an interactive email with unforgeable Approve / Edit / Reject links.

---

## Commit 51 — `feat(sign): HMAC link signer`

**Files:** `internal/platform/sign/hmac.go`

**Build:**

```go
type hmacSigner struct { secret []byte }

func HMAC(secret string) ports.Signer
func (s hmacSigner) Sign(payload string) string
func (s hmacSigner) Verify(payload, signature string) bool
```

`Sign` — input: a payload string; output: base64url (**raw**, no padding) HMAC-SHA256.
`Verify` — recompute and compare with **`hmac.Equal`**, never `==`.

**Verify:** build, then scratch-check that `Verify(payload, Sign(payload))` is true and that
changing one character of either the payload or the signature makes it false.

**Why:** `base64.RawURLEncoding` because the value goes in a query string — standard base64 contains
`+` and `/` and padding `=`, all of which need escaping.

`hmac.Equal` is a **constant-time** comparison. With `==`, Go's string comparison short-circuits on
the first differing byte, and an attacker measuring response times could recover the correct
signature byte by byte. This is the single most commonly missed detail in signed-URL implementations.

---

## Commit 52 — `feat(render): email templates`

**Files:** `internal/adapters/delivery/render/templates.go`

**Build:** two `const` strings, `incidentTemplate` and `digestTemplate`.

`incidentTemplate` — a full HTML document with `lang="en"`, a viewport meta tag, and these sections,
each in a bordered card:
1. header — title, incident id, severity, target, confidence and badge
2. **Diagnosis** — root cause, impact, and the critic's verdict with its issues
3. **Proposed steps** — an `<ol>` where each step's command is in a `<code>` block with
   `white-space:pre-wrap; word-break:break-all`, followed by the explanation and `[risk]`. When
   there are no steps, say *"No executable step survived policy review. Treat this as a diagnosis
   only."*
4. **Blocked by policy** — only when present, in a warning-coloured card, each with its reason
5. **Verification** and **Rollback** lists
6. **Your call** — three coloured anchor buttons for Approve / Edit / Reject, with a line saying
   approving teaches retrieval and that links expire
7. **Firing alerts**, **Historical runbooks used**, **Cluster events**, **Log evidence** (the last
   two in `<pre>`)
8. a `<details>` block with the pipeline trace
9. a footer: *"Commands are suggestions produced from the evidence above. Review before running
   anything."*

Template functions you will register in commit 53: `pct` (a float to `"85%"`) and `upper`.

`digestTemplate` — header with the window and the four counts, a **Summary** card rendering
`.Narrative` in a `<pre>` with `font-family:inherit`, and a **Reviewed incidents** list.

**Rules:** inline styles only, and semantic elements throughout.

**Verify:** build. Parsing is checked in commit 53.

**Why:** **inline styles only** because email clients strip `<style>` blocks — this is not a
stylistic choice, it is the medium. Semantic HTML (`<h1>`, `<ol>`, `<code>`, `lang`) still matters:
screen readers work, and the structure survives into the plain-text alternative.

Showing blocked commands **with their reasons** rather than hiding them is the same principle as
commit 50: the human is the decision-maker and needs the full picture.

---

## Commit 53 — `feat(render): incident rendering with signed links`

**Files:** `internal/adapters/delivery/render/html.go`

**Build:**

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

func New(baseURL string, signer ports.Signer, ttl time.Duration, clock ports.Clock, to []string) (*HTML, error)
```
Use **`html/template`**, register the `pct` and `upper` funcs, and **parse both templates once** in
the constructor.

```go
type incidentView struct {
    Incident *domain.Incident
    Plan     *domain.Plan
    Steps    []domain.Command      // Plan.Executable()
    Blocked  []domain.Command      // Plan.Blocked()
    Logs     []domain.LogLine      // last 25
    Events   []domain.ClusterEvent // last 10
    Matches  []domain.RunbookMatch
    Verdict  *domain.Verdict
    Confidence float64
    Badge    string
    Script   string
    Approve, Edit, Reject string
    Trace    []string
}

func (h *HTML) RenderIncident(inc *domain.Incident) (domain.Message, error)
```
Output: a `domain.Message` with `To` defaulted from config, `Subject`
`"[WatchTower][CRITICAL] <title>"`, the rendered `HTML`, a plain-text alternative, and
`Meta{"incident_id":..., "kind":"incident"}`.

```go
func (h *HTML) link(incidentID string, decision domain.Decision) string
```
Input: incident id and decision. Output: `<baseURL>/runbook/approve?...` with query parameters
`incident`, `decision`, `exp` (unix, `now + ttl`) and `sig`. **The signed payload is
`fmt.Sprintf("%s|%s|%d", incidentID, decision, exp)` — all three fields.**

Plus `badge(confidence float64) string` (`high` ≥ 0.75, `medium` ≥ 0.45, else `low`),
`tailLogs`, `tailEvents`.

**Verify:** build, then in a scratch main render a real incident, write the HTML to a file and open
it in a browser. Check the buttons, and paste one link's `sig` into a `Verify` call.

**Why:** `html/template`, not `text/template`. This is security, not preference: it is
context-aware and escapes based on where a value lands. Log lines and model output go into this
email, so without it a log line containing `<script>` would be an XSS vector against whoever opens
it in webmail.

**The signature covers all three fields.** Sign only the incident id and an attacker flips
`decision=reject` to `decision=approve`. Omit the expiry and a forwarded email grants permanent
authority.

Parsing templates in the constructor rather than per email: parsing is expensive, executing is
cheap.

---

## Commit 54 — `feat(render): plain text part and digest rendering`

**Files:** `internal/adapters/delivery/render/html.go` (extend)

**Build:**

```go
func incidentText(inc *domain.Incident, v incidentView) string
```
Output: the same information as plain text —
- header: incident id, severity, target, `Confidence: 61% (medium)`
- root cause, impact
- numbered steps with their explanations
- `BLOCKED: <cmd> (<reason>)` lines
- verification list
- `Historical runbooks used:` with similarity percentages and reasons
- the three links, labelled `Approve:` / `Edit:` / `Reject:`

```go
func (h *HTML) RenderDigest(d domain.Digest) (domain.Message, error)
```
Subject: `"[WatchTower] Daily Pulse 2026-07-30: 1 reviewed, 0 approved"`. `Text` is the narrative;
`HTML` is the rendered digest template.

Register `"html"` in `wire.Renderers`, constructing the signer inline from
`sign.HMAC(cfg.HTTP.ApprovalSecret)` and passing `cfg.HTTP.PublicBaseURL`,
`cfg.HTTP.ApprovalTimeout`, `env.Clock` and `cfg.Notify.To`.

**Verify:** build; render an incident and print only the text part. It must be genuinely usable on
its own.

**Why:** the plain-text part is not a courtesy. Terminal mail clients, pagers and screen readers get
it, and an on-call engineer reading on a phone at 3am may be one of them. Same mechanism serves
accessibility and reachability.

It is also what the `stdout` notifier prints, which means the text path is exercised on **every**
local run rather than rotting until someone complains.

---

## Commit 55 — `feat(projectors): delivery`

**Files:** `internal/projectors/deliver.go`, `internal/bootstrap/app.go` (edit),
`internal/bootstrap/plugins.go` (edit)

**Build:**

```go
type Deliver struct {
    renderer      ports.Renderer
    notifier      ports.Notifier
    dlq           ports.DeadLetterQueue
    ids           ports.IDGen
    clock         ports.Clock
    minConfidence float64
    log           ports.Logger
}

func NewDeliver(renderer, notifier, dlq, ids, clock, minConfidence, log) *Deliver
func (d *Deliver) Register(broker bus.Broker)          // subscribes to TopicIncidentTriaged only
func (d *Deliver) handle(ctx context.Context, e bus.Event) error
func (d *Deliver) park(ctx context.Context, stage, reason string, inc *domain.Incident)
```
`handle`:
1. type-assert the payload; ignore anything else
2. **when `inc.Confidence < d.minConfidence`**, log
   `"delivery suppressed by confidence floor"` with the value and the floor, and return nil
3. render — on error park with stage `"deliver:render"` and return the error
4. notify — on error park with stage `"deliver:notify"` and return the error
5. log `"incident delivered"` with `d.notifier.Name()`

Wire it: build `renderer` (`cfg.Notify.Renderer`) and `notifier` (`cfg.Notify.Driver`), register the
projector, **delete the temporary output `Tap`**, and append
`stages.Publish(a.broker, bus.TopicIncidentTriaged)` as the final stage. Add the blank import for
`adapters/delivery/render`.

**Verify:**

```bash
go build -o bin/watchtowerd ./cmd/watchtowerd && ./bin/watchtowerd
curl -X POST localhost:8080/alerts ... --data-binary @examples/alerts/alertmanager-oom.json
```
The full plain-text email now prints, including three signed links. Copy the Approve link and open
it — you should get `501 Not Implemented` from the stub handler, which commit 57 fixes.

Then test the floor without a rebuild:
```bash
WT_NOTIFY_MIN_CONFIDENCE=0.9 ./bin/watchtowerd
```
The incident should still be persisted, but the email suppressed with a log line saying so.

**Why:** note what the pipeline does **not** contain: no renderer, no notifier, no store. It
publishes two facts. Everything else is a subscriber, and adding a Slack projector later means
subscribing to the same topic — both run, independently, and one failing does not affect the other.

The confidence floor keeping the incident while suppressing the notification is the right shape for
a noise control: nothing is lost, and the threshold can be lowered again after inspecting what was
suppressed.

---

## End of Part 11

Real emails with unforgeable, expiring approval links. Twelve stages.

```bash
go build ./... && go vet ./... && gofmt -l .
```

Next: **Part 12** — the feedback endpoints and service, so the human's decision is recorded.
