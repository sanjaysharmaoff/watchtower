# The WatchTower Book

A 25-part guide written for someone who knows basic Go and nothing about Prometheus, Loki,
Kubernetes, Docker, pgvector, embeddings or RAG. It ends with an interview playbook.

Read in order, or jump to what you need.

## Orientation

| Part | Title | What it covers |
| --- | --- | --- |
| [01](document_book_part_01.md) | How to Use This Book, and the 60-Second Pitch | the pitch, the numbers to memorise, how to navigate |
| [02](document_book_part_02.md) | The Problem: Why Alert Triage Is Painful | SRE, on-call, alert fatigue, what the project answers |

## Technology tour — every external technology from zero

| Part | Title | What it covers |
| --- | --- | --- |
| [03](document_book_part_03.md) | Prometheus | pull model, metrics, labels, cardinality, PromQL, alert rules |
| [04](document_book_part_04.md) | Alertmanager and the Webhook Contract | grouping, routing, the exact JSON payload, the decoder |
| [05](document_book_part_05.md) | Loki and LogQL | label-indexed logs, streams, the query this project builds |
| [06](document_book_part_06.md) | Kubernetes | pods, deployments, limits, OOMKilled, events, RBAC, minikube |
| [07](document_book_part_07.md) | Docker and Docker Compose | images, multi-stage builds, volumes, health gates, overlays |
| [08](document_book_part_08.md) | PostgreSQL | JSONB, table partitioning, upserts, pgx pooling, migrations |
| [09](document_book_part_09.md) | Embeddings, Vector Search, pgvector and RAG | vectors, cosine, HNSW, two-stage retrieval, the learning loop |
| [10](document_book_part_10.md) | Language Models | tokens, JSON mode, prompt design, planner/critic, cost control |
| [11](document_book_part_11.md) | Redis, SMTP and Cron | SET NX, multipart email, HMAC links, scheduling |

## Architecture

| Part | Title | What it covers |
| --- | --- | --- |
| [12](document_book_part_12.md) | Ports and Adapters | the central idea, the three-layer rule, all ~30 ports |
| [13](document_book_part_13.md) | The Pipeline Pattern | generic stages, middleware, drop vs error |
| [14](document_book_part_14.md) | The Event Bus and Projectors | publish/subscribe, handler isolation, projectors |
| [15](document_book_part_15.md) | Registry, Composition Root, Resource Pool | plugin registration, wiring, the deadlock story |

## Code walkthrough

| Part | Title | What it covers |
| --- | --- | --- |
| [16](document_book_part_16.md) | The `domain` Package | Alert, Incident, signature, Plan, Runbook, errors |
| [17](document_book_part_17.md) | The `ports` Package | every interface, why it has that shape, who implements it |
| [18](document_book_part_18.md) | Stages, Part A | admit, debounce, correlate, enrich, sanitize |
| [19](document_book_part_19.md) | Stages, Part B | guards, retrieval, the model loop, scoring, publish |
| [20](document_book_part_20.md) | Ingest and Enrich Adapters | gates, correlator, the four sanitizer algorithms |
| [21](document_book_part_21.md) | RAG and Cognition Adapters | cache decorator, rerankers, redactors, linters, scorer |
| [22](document_book_part_22.md) | Storage, Delivery, HTTP, Worker Pool | Postgres, email, the edge, backpressure |
| [23](document_book_part_23.md) | Services, Bootstrap, Full Trace | use cases, shutdown order, one real incident second by second |

## Reference

| Part | Title | What it covers |
| --- | --- | --- |
| [24](document_book_part_24.md) | Configuration Bible | every variable, four profiles, swap recipes, troubleshooting |
| [25](document_book_part_25.md) | Interview Playbook | pitch, whiteboard script, ~40 Q&A, the bug stories, weaknesses |

## Short on time?

- **One evening:** 01, 02, 12, 25
- **A weekend:** everything
- **Right before the interview:** 25, then the numbers table in 01
