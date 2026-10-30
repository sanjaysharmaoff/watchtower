
flowchart TD
    %% Styling
    classDef target fill:#ffebee,stroke:#c62828,stroke-width:2px,color:#000
    classDef goApp fill:#e3f2fd,stroke:#1565c0,stroke-width:2px,color:#000
    classDef database fill:#fff8e1,stroke:#f57f17,stroke-width:2px,color:#000
    classDef ai fill:#ede7f6,stroke:#4527a0,stroke-width:2px,color:#000
    classDef delivery fill:#e8f5e9,stroke:#2e7d32,stroke-width:2px,color:#000
    classDef external fill:#f1f8e9,stroke:#558b2f,stroke-width:2px,color:#000
    classDef guardrail fill:#ffecb3,stroke:#f57f17,stroke-width:3px,color:#000,stroke-dasharray: 5 5

    subgraph Sources ["🎯 Target Environment Sources"]
        Prom["Prometheus & Alertmanager\n(Evaluates 900+ Rules)"]:::target
        Loki["Loki / ELK\n(Log Streams)"]:::external
        K8s["Kubernetes API\n(Deployment Events)"]:::external
    end

    subgraph Go_Ingestion ["1. Edge Ingestion & Normalization"]
        Webhook["Webhook Receiver\n(POST /alerts)"]:::goApp
        WorkerPool["Worker Pool Manager\n(Limits Concurrency & Protects Pipeline)"]:::goApp
        Debouncer["Thread-Safe Debouncer\n(Drops duplicate alerts < 1h)"]:::goApp
        Correlator["Correlation Engine\n(Groups related failing services)"]:::goApp
        Enricher["Context Enricher\n(Fetches 100 Logs + K8s Events)"]:::goApp
        Sanitizer["Go Log Sanitizer\n(1. Deduplicator | 2. Compressor | 3. Signal Filterer)"]:::goApp
    end

    subgraph Go_RAG ["2. RAG & Search Engine"]
        Cache["Embedding Cache\n(In-Memory/Redis)"]:::goApp
        EmbedAPI["OpenAI text-embedding-3"]:::ai
        Retrieval["Vector Retrieval Engine\n(Generates Query String)"]:::goApp
        Reranker["Cross-Encoder Reranker\n(Filters Top 10 -> Top 2 matches)"]:::ai
    end

    subgraph Go_Intelligence ["3. Cognitive Core & Guardrails"]
        InputGuard["⚠️ Input Policy Guardrail\n(Redacts PII, Tokens, Secrets)"]:::guardrail
        Prompt["Prompt Builder\n(Merges Context + RAG + Alerts)"]:::goApp
        Planner["LLM Action Planner\n(gpt-4o-mini, Strict JSON)"]:::ai
        Critic["LLM Critic\n(Self-Correction Loop)"]:::ai
        OutputGuard["⚠️ Output Policy Guardrail\n(AST Linter: Blocks 'rm -rf', '--force')"]:::guardrail
        Scorer["Confidence Scoring"]:::goApp
    end

    subgraph Go_Maintenance ["4. Crons & Maintenance"]
        PartMan["Partition Manager\n(robfig/cron @ 00:00)\nAuto-creates/drops partitions"]:::goApp
        DailyPulse["The Daily Pulse\n(robfig/cron @ 08:00)\nBuilds 24h Executive Summary"]:::goApp
    end

    subgraph Databases ["🗄️ Storage Layer (PostgreSQL)"]
        TableRaw[("raw_anomalies\n(Daily Partitions, Clean Logs Only)")]:::database
        TableApproved[("approved_summaries\n(Root Causes & Verified Fixes)")]:::database
        TableRunbooks[("historical_runbooks\n(pgvector)")]:::database
        TableDLQ[("dead_letter_queue\n(Catches all DB write/lock failures)")]:::database
    end

    subgraph Delivery ["📫 Delivery & Feedback Loop"]
        SMTP["SMTP Relay (AWS SES)\n(Delivers HTML Emails)"]:::delivery
        SRE_Team(["SRE Team\n(Clicks Approve / Edit / Reject)"]):::delivery
        SRE_Webhook["Feedback API\n(POST /runbook/approve)"]:::goApp
        Embedder["Embedding Generator\n(Embeds approved CLI for RAG)"]:::goApp
    end

    %% Phase 1: Ingestion
    Prom -- "Fires JSON Alert" --> Webhook
    Webhook -- "Enqueues Payload" --> WorkerPool
    WorkerPool --> Debouncer
    Debouncer -- "If Allowed" --> Correlator
    Correlator -- "Triggers Fetch" --> Enricher
    Loki -. "Returns Logs" .-> Enricher
    K8s -. "Returns Events" .-> Enricher
    Enricher --> Sanitizer
   
    %% Phase 1b: Storage Write & DLQ
    Sanitizer -- "INSERT Compressed Context" --> TableRaw
    Sanitizer -. "ON DB FAIL" .-> TableDLQ

    %% Phase 2: RAG
    Sanitizer -- "Pass Highest Severity Log" --> Cache
    Cache -- "Cache Miss" --> EmbedAPI
    EmbedAPI --> Retrieval
    Cache -- "Cache Hit (Saves $$$)" --> Retrieval
    Retrieval -. "Similarity Query" .-> TableRunbooks
    TableRunbooks -. "Returns Top 10 Runbooks" .-> Reranker
    Reranker -- "Pass Top 2 Matches" --> InputGuard

    %% Phase 3: Cognitive Generation
    Sanitizer -- "Pass Raw Alert/Event Context" --> InputGuard
    InputGuard -- "Clean Data" --> Prompt
    Prompt --> Planner
    Planner -- "Draft CLI Runbook" --> Critic
    Critic -- "If Flawed" --> Planner
    Critic -- "If Approved" --> OutputGuard
    OutputGuard -- "Safe Command" --> Scorer

    %% Phase 4: Delivery & The Loop
    Scorer -- "Trigger Interactive Alert" --> SMTP
    SMTP --> SRE_Team
    SRE_Team -- "Verifies & Submits" --> SRE_Webhook
   
    %% Phase 5: Learning & Storage write
    SRE_Webhook -- "Save Record" --> TableApproved
    SRE_Webhook -. "ON DB FAIL" .-> TableDLQ
    SRE_Webhook -- "Pass to Embedder" --> Embedder
    Embedder -- "Text to Vector" --> EmbedAPI
    EmbedAPI -- "Returns Vector" --> Embedder
    Embedder -- "INSERT INTO (Learns)" --> TableRunbooks

    %% Phase 6: Maintenance & Reporting
    PartMan -- "Executes DDL CREATE/DROP" --> TableRaw
    TableApproved -. "SELECT entries WHERE time > NOW() - 24h" .-> DailyPulse
    DailyPulse -- "Prompt: Compile Markdown Report" --> Planner
    DailyPulse -- "Send Daily Health Report" --> SMTP