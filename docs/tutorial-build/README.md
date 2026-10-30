# Build WatchTower From Scratch — 100 Commits

A commit-by-commit rebuild of the entire project. 20 parts, 5 commits each.

## How to use this

Start from an empty directory. Work through the commits in order. Each commit spec tells you:

- **Files** — exactly what to create or edit
- **Build** — the types and functions, with their inputs and outputs
- **Register** — the registry line, if it is an adapter
- **Verify** — the command that proves the commit works
- **Why** — the reason the code looks like that

Three rules:

1. **Every commit must leave `go build ./...` green.** If it does not compile, it is not a commit.
   That constraint is what forces the dependency order below.
2. **Commit exactly what the spec says**, no more. Resisting the urge to jump ahead is the point.
3. **Run the Verify step before committing.** Several commits are only observable at runtime.

Suggested message format: `feat(scope): summary` / `refactor(scope):` / `test(scope):` /
`chore(scope):` — the scope is the package.

## The shape of the journey

| Commits | You end up with |
| --- | --- |
| 1–10 | Pure domain types and every interface. No behaviour yet. |
| 11–20 | The pipeline engine, config, registries, event bus. Still no adapter. |
| 21–30 | **A running binary.** `POST /alerts` → debounce → memory store → printed email. |
| 31–40 | Enrichment, log sanitizing, correlation, secret redaction. |
| 41–50 | Prompts, the offline planner/critic loop, both guardrails, confidence scoring. |
| 51–60 | HTML email with signed approval links, and the feedback endpoint. |
| 61–70 | Retrieval: embeddings, vector search, reranking, caching. **The learning loop closes.** |
| 71–80 | Real models over the OpenAI-compatible API, and Postgres with pgvector. |
| 81–90 | Loki, Kubernetes, Redis, cron, the digest, graceful shutdown. |
| 91–100 | Docker, the full observability stack, minikube, tests, CI, docs. |

## Milestones — the commits where something visibly changes

| Commit | Milestone |
| --- | --- |
| **30** | First running service: an alert in, an email printed to stdout |
| **40** | Evidence gathering and redaction working; incidents persisted |
| **50** | A full plan with a critic, guardrails and a confidence score |
| **60** | The human can approve, and the decision is recorded |
| **65** | Approved runbooks are retrieved for the next incident — the loop closes |
| **75** | Real language models |
| **80** | Real Postgres with partitioning and pgvector |
| **90** | Feature-complete against the original design |
| **95** | The whole real stack runs with `docker compose up` |
| **100** | Tested, CI'd, documented |

## Parts

| Part | Commits | Title |
| --- | --- | --- |
| [01](tutorial_build_part_01.md) | 1–5 | Project skeleton and the domain vocabulary |
| [02](tutorial_build_part_02.md) | 6–10 | Errors, platform ports, and all the seams |
| [03](tutorial_build_part_03.md) | 11–15 | The pipeline engine and configuration |
| [04](tutorial_build_part_04.md) | 16–20 | Environment, registries, and the event bus |
| [05](tutorial_build_part_05.md) | 21–25 | First stages and first adapters |
| [06](tutorial_build_part_06.md) | 26–30 | Worker pool, HTTP edge, and a running binary |
| [07](tutorial_build_part_07.md) | 31–35 | Enrichment and the sanitizer chain |
| [08](tutorial_build_part_08.md) | 36–40 | Correlation, redaction, and persistence |
| [09](tutorial_build_part_09.md) | 41–45 | Prompts and the offline cognitive core |
| [10](tutorial_build_part_10.md) | 46–50 | The output guardrail and confidence scoring |
| [11](tutorial_build_part_11.md) | 51–55 | Signed links, HTML email, delivery |
| [12](tutorial_build_part_12.md) | 56–60 | The feedback endpoints and service |
| [13](tutorial_build_part_13.md) | 61–65 | Retrieval, and closing the learning loop |
| [14](tutorial_build_part_14.md) | 66–70 | Embedding cache, reranking, retry |
| [15](tutorial_build_part_15.md) | 71–75 | Real models over the OpenAI-compatible API |
| [16](tutorial_build_part_16.md) | 76–80 | PostgreSQL, partitions, and pgvector |
| [17](tutorial_build_part_17.md) | 81–85 | Redis, Loki, and Kubernetes |
| [18](tutorial_build_part_18.md) | 86–90 | Cron, digest, maintenance, shutdown |
| [19](tutorial_build_part_19.md) | 91–95 | Docker, Compose, and the real stack |
| [20](tutorial_build_part_20.md) | 96–100 | Tests, hardening, CI, and documentation |

## Before you start

```bash
go version          # 1.22 or newer; 1.24 recommended
docker --version    # needed from Part 19
```

Module path: use your own. The tutorial writes
`github.com/sanjaysharmaoff/watchtower` — substitute yours consistently.

If you want the reasoning behind any design decision rather than just the instruction,
[`docs/book/`](../book/README.md) is the companion volume: 25 parts explaining every technology
and every architectural choice.
