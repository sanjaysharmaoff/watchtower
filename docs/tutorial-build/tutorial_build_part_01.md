# Part 1 — Project Skeleton and the Domain Vocabulary (Commits 1–5)

**Goal:** an empty module becomes a package of pure types that describe alerts and incidents.
Nothing runs yet. That is fine — you are building the vocabulary everything else will speak.

**The rule that governs this part:** `internal/domain` imports **only the standard library**.
No HTTP, no SQL, no config. Check it whenever you like:

```bash
go list -deps ./internal/domain | grep watchtower    # only the domain package itself
```

---

## Commit 1 — `chore: initialise the module`

**Files:** `go.mod`, `.gitignore`, `README.md`, `Makefile`

**Build:**

```bash
go mod init github.com/<you>/watchtower
```

`.gitignore`:
```
bin/
dist/
*.exe
.env
.env.local
coverage.out
```

`Makefile` with three targets for now:
```makefile
BINARY := bin/watchtowerd

build:
	go build -o $(BINARY) ./cmd/watchtowerd
vet:
	go vet ./...
fmt:
	gofmt -w .
```

`README.md`: one paragraph saying what you are about to build.

**Verify:** `go build ./...` succeeds (it does nothing, which is correct).

**Why:** `bin/` and `.env` in `.gitignore` from commit 1 means you never accidentally commit a
binary or a secret. The `Makefile` will grow; starting it now means you never type long commands.

---

## Commit 2 — `feat(domain): Alert and Severity`

**Files:** `internal/domain/alert.go`

**Build:**

`type Severity string` with constants `SeverityCritical`, `SeverityWarning`, `SeverityInfo`,
`SeverityUnknown` (values `"critical"`, `"warning"`, `"info"`, `"unknown"`).

- `func (s Severity) Rank() int` — input: the severity; output: `3` for
  `critical/crit/fatal/emergency/page/p1`, `2` for `warning/warn/major/p2`, `1` for
  `info/notice/low/p3`, else `0`. Lowercase the input before comparing.
- `func ParseSeverity(v string) Severity` — input: any free-form severity string; output: one of
  the four canonical constants. Trim and lowercase first.

`type Alert struct` with JSON tags: `Fingerprint`, `Name`, `Status`, `Severity`, `Service`,
`Namespace`, `Cluster`, `Summary`, `Description`, `RunbookURL`,
`Labels map[string]string`, `Annotations map[string]string`, `StartsAt`, `EndsAt time.Time`,
`Source string`. Use `omitempty` on everything except `Name` and `Severity`.

Methods:
- `func (a Alert) Identity() string` — input: the alert; output: `Fingerprint` if non-empty,
  otherwise `"Name|Cluster|Namespace|Service"`. This is the de-duplication key.
- `func (a Alert) Title() string` — output: `Summary`, else `Name`, else `"unnamed alert"`.
- `func (a Alert) Target() string` — output: non-empty `Cluster/Namespace/Service` joined by
  `/`, or `"unknown"` if all three are empty.
- `func (a Alert) IsResolved() bool` — output: true when `Status` case-insensitively equals
  `"resolved"`.
- `func (a Alert) Label(keys ...string) string` — input: candidate keys; output: the first
  non-empty value found across `Labels` **then** `Annotations`, else `""`.

Package functions:
- `func TopSeverity(alerts []Alert) Severity` — output: the highest-`Rank()` severity in the set.
- `func ServicesOf(alerts []Alert) []string` — output: **sorted** unique services, falling back
  to `Name` when `Service` is empty.

**Verify:** `go build ./... && go vet ./...`

**Why:** `Rank()` exists because comparing severity strings tells you nothing, and you will need
"most severe alert in this group" three times later. `Label(keys...)` exists because every team
labels the same concept differently — `service`, `app`, `job`, `deployment` — and absorbing that
variance in one place keeps it out of every caller. `ServicesOf` **must** sort: its output feeds a
hash in commit 4, and an unsorted set would hash differently depending on arrival order.

---

## Commit 3 — `feat(domain): log lines and cluster events`

**Files:** `internal/domain/telemetry.go`

**Build:**

`type LogLine struct` — `Timestamp time.Time`, `Level string`, `Message string`,
`Source string`, `Occurrences int`, `Labels map[string]string`.

- `func (l LogLine) String() string` — output: `"<RFC3339> <LEVEL> <message>"`, trimmed, with
  `" (xN)"` appended when `Occurrences > 1`.
- `func (l LogLine) LevelRank() int` — output: `5` fatal/panic/critical/crit, `4`
  error/err/eror, `3` warn/warning, `2` info/notice, `1` debug/trace, else `0`. Trim `[` and `]`
  from the level first.
- `func LevelRankOf(level string) int` — the same ordering for a bare string.

`type ClusterEvent struct` — `Timestamp`, `Type` (`Normal`/`Warning`), `Reason`, `Object`,
`Message`, `Source`, `Count int`. Plus `func (e ClusterEvent) String() string` rendering
`"<ts> <type> <reason> <object>: <message>"`.

`type ContextFragment struct` — `Provider string`, `Logs []LogLine`, `Events []ClusterEvent`,
`Notes map[string]string`, **`Err string`**, `Took time.Duration`.

**Verify:** `go build ./...`

**Why:** `Occurrences` is what lets you collapse a thousand identical lines into one and still
report "this happened 1000 times" — the most diagnostic fact in a log. `LevelRank` turns text into
something orderable so a filter can say "warnings and above". And `ContextFragment.Err` is a
**field, not a returned error**: a provider that fails contributes a fragment saying so, and the
pipeline keeps going. Name it `ClusterEvent`, not `K8sEvent` — a Nomad provider should be able to
produce one without the type name lying.

---

## Commit 4 — `feat(domain): the Incident envelope`

**Files:** `internal/domain/incident.go`

**Build:**

`type Prompt struct { System, User string; Meta map[string]string }`.

`type Incident struct` — `ID`, `CorrelationKey`, `Signature string`, `Severity`,
`Services []string`, `Alerts []Alert`, `Logs []LogLine`, `Events []ClusterEvent`,
`Fragments []ContextFragment`, `Matches []RunbookMatch`, `Prompt Prompt` (tag `json:"-"`),
`Plan *Plan`, `Verdict *Verdict`, `Attempts int`, `Confidence float64`,
`Meta map[string]string`, `Trace []string`, `CreatedAt time.Time`.

> `RunbookMatch`, `Plan` and `Verdict` arrive in commit 5. Write this file second if you prefer
> a compiling tree at every save; the commit is what must be green, not every keystroke.

- `func NewIncident(id string, at time.Time, alerts []Alert) *Incident` — copies the alerts,
  initialises `Meta`, calls `Recompute()`, returns the envelope.
- `func (i *Incident) Recompute()` — sets `Severity = TopSeverity(...)`,
  `Services = ServicesOf(...)`, `Signature = computeSignature()`, and defaults
  `CorrelationKey` to the signature when empty.
- `func (i *Incident) computeSignature() string` — input: the incident; output: the first 16 hex
  characters of `sha256(sortedLowercasedAlertNames + "|" + services)`.
- `func (i *Incident) MergeAlerts(extra []Alert) int` — input: candidate alerts; output: how many
  were actually new. Skip anything whose `Identity()` is already present; call `Recompute()` only
  if something was added.
- `func (i *Incident) PrimaryAlert() Alert` — output: the highest-severity alert, or a usable
  zero value when there are none. **Must not panic on an empty incident.**
- `func (i *Incident) Title() string` — output: the primary alert's title, plus
  `" (+N related)"` when there is more than one alert.
- `func (i *Incident) SetMeta(key, value string)` and
  `func (i *Incident) Note(format string, args ...any)` — the latter appends a formatted line to
  `Trace`.
- `func (i *Incident) AddFragment(f ContextFragment)` — appends to `Fragments` and folds the
  fragment's logs and events into the top-level slices.
- `func (i *Incident) TopMatch() (RunbookMatch, bool)`.

**Verify:** `go build ./...`

**Why:** this one struct is the payload for the entire pipeline. Reading it top to bottom tells
you what the system does.

The **signature** is the important idea: it identifies a *kind of failure*, not an occurrence.
Sorted alert names plus services, hashed. It is the debounce key, and later the join key between
an incident and a runbook that fixed the same thing. Deliberately excluded: pod names, timestamps,
log content — they change every occurrence and would defeat the purpose.

`Note()` is cheap observability that travels with the payload. Debugging one incident three days
later becomes one SQL query instead of log correlation.

---

## Commit 5 — `feat(domain): runbooks, commands and plans`

**Files:** `internal/domain/runbook.go`, `internal/domain/plan.go`

**Build:**

`runbook.go`:
- `type RiskLevel string` with `RiskRead` (`"read"`), `RiskMutate` (`"mutate"`),
  `RiskDangerous` (`"dangerous"`).
- `type Command struct { Run, Explain string; Risk RiskLevel; Blocked bool; Reason string }`
  plus `func (c Command) Binary() string` returning the lowercased first token.
- `type Runbook struct` — `ID`, `Title`, `Signature`, `RootCause`, `Commands []Command`,
  `Tags []string`, `Labels map[string]string`, `Reviewer string`,
  `Embedding []float32` (`json:"-"`), `SourceText string` (`json:"-"`), `CreatedAt`.
- `func (r Runbook) Text() string` — output: `SourceText` if set, otherwise a canonical rendering
  of title, cause, `$ command  # explain` lines and tags. **One definition of "what this runbook
  says"**, used later for embedding, reranking and storage.
- `type RunbookMatch struct { Runbook Runbook; Score float64; Stage, Reason string }`.

`plan.go`:
- `type Plan struct` — `RootCause`, `Impact string`, `Steps []Command`,
  `Verification []string`, `Rollback []string`, `Confidence float64`, `Model string`,
  `Raw string` (`json:"-"`).
- `func (p *Plan) Executable() []Command` — output: steps where `!Blocked`. **Nil-safe.**
- `func (p *Plan) Blocked() []Command` — output: steps where `Blocked`. Nil-safe.
- `func (p *Plan) Script() string` — output: the executable steps as a shell snippet with
  `# explain` comments.
- `type Verdict struct { Approved bool; Issues []string; Notes, Model string }`.
- `type Decision string` with `DecisionApprove`, `DecisionEdit`, `DecisionReject`;
  `func ParseDecision(v string) Decision` (tolerant: `approved`/`accept`/`ok` → approve;
  **anything unrecognised → reject**);
  `func (d Decision) Learnable() bool` (approve or edit).

**Verify:** `go build ./... && go vet ./... && gofmt -l .` (the last should print nothing)

**Why:** `Blocked bool` plus `Reason` on a `Command` means a refused command is **kept and
labelled**, not deleted — the reviewer sees what was proposed and why policy refused it. Deleting
it would hide information from the only person qualified to judge.

`Executable()` gets used in three places later — the email, the scorer, and the learning step —
and that third one is a free safety property: a blocked command can never enter the retrieval
corpus, even via a careless approval, without the feedback code knowing guardrails exist.

`ParseDecision` defaulting to **reject** is fail-closed on the path that writes to the corpus.

---

## End of Part 1

You have: a module, and a dependency-free package of types describing alerts, telemetry,
incidents, runbooks and plans.

```bash
go build ./... && go vet ./... && gofmt -l .
git log --oneline | head -5
```

Next: **Part 2** — sentinel errors, the platform ports, and every interface in the system.
