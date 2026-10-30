package config

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Config is the whole knob surface of WatchTower. Driver fields are adapter
// names resolved against the registries at startup, which is how an
// implementation gets swapped without a code change.
type Config struct {
	Env     string
	Log     LogConfig
	HTTP    HTTPConfig
	Worker  WorkerConfig
	Ingest  IngestConfig
	Enrich  EnrichConfig
	Clean   CleanConfig
	RAG     RAGConfig
	LLM     LLMConfig
	Guard   GuardConfig
	Store   StoreConfig
	Notify  NotifyConfig
	Cron    CronConfig
	Runtime RuntimeConfig
}

// LogConfig controls the logging adapter.
type LogConfig struct {
	Level  string // debug|info|warn|error
	Format string // text|json
}

// HTTPConfig controls the inbound edge.
type HTTPConfig struct {
	Addr            string
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	IdleTimeout     time.Duration
	ShutdownGrace   time.Duration
	MaxBodyBytes    int64
	AuthToken       string
	PublicBaseURL   string
	ApprovalSecret  string
	ApprovalTimeout time.Duration
}

// WorkerConfig controls the ingestion worker pool.
type WorkerConfig struct {
	Workers    int
	QueueSize  int
	JobTimeout time.Duration
}

// IngestConfig controls decoding, debouncing and correlation.
type IngestConfig struct {
	Decoder         string // alertmanager|generic
	GateDriver      string // memory|redis
	DebounceWindow  time.Duration
	DropResolved    bool
	MinSeverity     string
	CorrelationKeys []string
	CorrelationWin  time.Duration
}

// EnrichConfig controls context providers.
type EnrichConfig struct {
	Providers []string // loki|k8s|demo
	Timeout   time.Duration
	LogLimit  int
	Lookback  time.Duration
	Loki      LokiConfig
	K8s       K8sConfig
}

// LokiConfig is the Loki adapter configuration.
type LokiConfig struct {
	URL      string
	Tenant   string
	Query    string
	Username string
	Password string
}

// K8sConfig is the Kubernetes event adapter configuration.
type K8sConfig struct {
	APIServer string
	Token     string
	CAFile    string
	Insecure  bool
	Limit     int
}

// CleanConfig controls the sanitizer chain.
type CleanConfig struct {
	Processors  []string // dedupe|compress|signal|cap
	MaxLines    int
	MaxLineLen  int
	MinLevel    string
	Keywords    []string
	StripDigits bool
}

// RAGConfig controls retrieval.
type RAGConfig struct {
	EmbedderDriver string // openai|stub
	VectorDriver   string // postgres|memory
	RerankDriver   string // heuristic|llm|none
	CacheDriver    string // memory|redis|none
	TopK           int
	TopN           int
	VectorDims     int
	CacheTTL       time.Duration
	MinScore       float64
	RedisAddr      string
	RedisPassword  string
	RedisDB        int
}

// LLMConfig controls the cognitive core.
type LLMConfig struct {
	Provider     string // openai|stub
	BaseURL      string
	APIKey       string
	PlannerModel string
	CriticModel  string
	EmbedModel   string
	Temperature  float64
	MaxTokens    int
	Timeout      time.Duration
	MaxAttempts  int
	CriticOn     bool
}

// GuardConfig controls both guardrails.
type GuardConfig struct {
	Redactors     []string // secrets|pii|custom
	CustomRedact  []string
	Inspectors    []string // denylist|allowlist|flags
	DenyPatterns  []string
	AllowBinaries []string
	DenyFlags     []string
	Mode          string // strip|halt
}

// StoreConfig controls persistence.
type StoreConfig struct {
	Driver         string // postgres|memory
	DSN            string
	MaxConns       int
	Migrate        bool
	RetentionDays  int
	PartitionAhead int
	MemoryLimit    int
}

// NotifyConfig controls delivery.
type NotifyConfig struct {
	Driver        string // smtp|stdout|none
	Renderer      string // html
	From          string
	To            []string
	SMTPHost      string
	SMTPPort      int
	SMTPUser      string
	SMTPPassword  string
	SMTPTLS       bool
	MinConfidence float64
}

// CronConfig controls scheduled maintenance.
type CronConfig struct {
	Enabled       bool
	Timezone      string
	PartitionSpec string
	DigestSpec    string
	DigestWindow  time.Duration
}

// RuntimeConfig controls process-wide behaviour.
type RuntimeConfig struct {
	BusBuffer    int
	BusWorkers   int
	StageTimeout time.Duration
}

// Load reads configuration from the environment, applying defaults that let the
// service boot with no dependencies at all (in-memory everything, stub models).
func Load() Config {
	cfg := Config{
		Env: str("ENV", "dev"),
		Log: LogConfig{
			Level:  str("LOG_LEVEL", "info"),
			Format: str("LOG_FORMAT", "text"),
		},
		HTTP: HTTPConfig{
			Addr:            str("HTTP_ADDR", ":8080"),
			ReadTimeout:     duration("HTTP_READ_TIMEOUT", 15*time.Second),
			WriteTimeout:    duration("HTTP_WRITE_TIMEOUT", 30*time.Second),
			IdleTimeout:     duration("HTTP_IDLE_TIMEOUT", 60*time.Second),
			ShutdownGrace:   duration("HTTP_SHUTDOWN_GRACE", 10*time.Second),
			MaxBodyBytes:    int64(integer("HTTP_MAX_BODY_BYTES", 2<<20)),
			AuthToken:       str("HTTP_AUTH_TOKEN", ""),
			PublicBaseURL:   str("PUBLIC_BASE_URL", "http://localhost:8080"),
			ApprovalSecret:  str("APPROVAL_SECRET", "watchtower-dev-secret"),
			ApprovalTimeout: duration("APPROVAL_TTL", 168*time.Hour),
		},
		Worker: WorkerConfig{
			Workers:    integer("WORKERS", 8),
			QueueSize:  integer("QUEUE_SIZE", 512),
			JobTimeout: duration("JOB_TIMEOUT", 3*time.Minute),
		},
		Ingest: IngestConfig{
			Decoder:         str("ALERT_DECODER", "alertmanager"),
			GateDriver:      str("GATE_DRIVER", "memory"),
			DebounceWindow:  duration("DEBOUNCE_WINDOW", time.Hour),
			DropResolved:    boolean("DROP_RESOLVED", true),
			MinSeverity:     str("MIN_SEVERITY", "warning"),
			CorrelationKeys: list("CORRELATION_KEYS", []string{"cluster", "namespace", "service"}),
			CorrelationWin:  duration("CORRELATION_WINDOW", 5*time.Minute),
		},
		Enrich: EnrichConfig{
			Providers: list("ENRICH_PROVIDERS", []string{"demo"}),
			Timeout:   duration("ENRICH_TIMEOUT", 8*time.Second),
			LogLimit:  integer("ENRICH_LOG_LIMIT", 100),
			Lookback:  duration("ENRICH_LOOKBACK", 15*time.Minute),
			Loki: LokiConfig{
				URL:      str("LOKI_URL", ""),
				Tenant:   str("LOKI_TENANT", ""),
				Query:    str("LOKI_QUERY", ""),
				Username: str("LOKI_USERNAME", ""),
				Password: str("LOKI_PASSWORD", ""),
			},
			K8s: K8sConfig{
				APIServer: str("K8S_API_SERVER", ""),
				Token:     str("K8S_TOKEN", ""),
				CAFile:    str("K8S_CA_FILE", ""),
				Insecure:  boolean("K8S_INSECURE", false),
				Limit:     integer("K8S_EVENT_LIMIT", 40),
			},
		},
		Clean: CleanConfig{
			Processors:  list("SANITIZE_CHAIN", []string{"dedupe", "signal", "compress", "cap"}),
			MaxLines:    integer("SANITIZE_MAX_LINES", 60),
			MaxLineLen:  integer("SANITIZE_MAX_LINE_LEN", 400),
			MinLevel:    str("SANITIZE_MIN_LEVEL", "warn"),
			Keywords:    list("SANITIZE_KEYWORDS", []string{"error", "fail", "timeout", "refused", "panic", "oom", "exception", "denied", "unavailable"}),
			StripDigits: boolean("SANITIZE_STRIP_DIGITS", true),
		},
		RAG: RAGConfig{
			EmbedderDriver: str("EMBEDDER_DRIVER", "stub"),
			VectorDriver:   str("VECTOR_DRIVER", "memory"),
			RerankDriver:   str("RERANK_DRIVER", "heuristic"),
			CacheDriver:    str("CACHE_DRIVER", "memory"),
			TopK:           integer("RAG_TOP_K", 10),
			TopN:           integer("RAG_TOP_N", 2),
			VectorDims:     integer("VECTOR_DIMS", 1536),
			CacheTTL:       duration("RAG_CACHE_TTL", 24*time.Hour),
			MinScore:       decimal("RAG_MIN_SCORE", 0.2),
			RedisAddr:      str("REDIS_ADDR", "localhost:6379"),
			RedisPassword:  str("REDIS_PASSWORD", ""),
			RedisDB:        integer("REDIS_DB", 0),
		},
		LLM: LLMConfig{
			Provider:     str("LLM_PROVIDER", "stub"),
			BaseURL:      str("LLM_BASE_URL", "https://api.openai.com/v1"),
			APIKey:       str("OPENAI_API_KEY", ""),
			PlannerModel: str("PLANNER_MODEL", "gpt-4o-mini"),
			CriticModel:  str("CRITIC_MODEL", "gpt-4o-mini"),
			EmbedModel:   str("EMBED_MODEL", "text-embedding-3-small"),
			Temperature:  decimal("LLM_TEMPERATURE", 0.1),
			MaxTokens:    integer("LLM_MAX_TOKENS", 1400),
			Timeout:      duration("LLM_TIMEOUT", 45*time.Second),
			MaxAttempts:  integer("LLM_MAX_ATTEMPTS", 2),
			CriticOn:     boolean("LLM_CRITIC_ENABLED", true),
		},
		Guard: GuardConfig{
			Redactors:     list("INPUT_REDACTORS", []string{"secrets", "pii", "custom"}),
			CustomRedact:  list("INPUT_REDACT_PATTERNS", nil),
			Inspectors:    list("OUTPUT_INSPECTORS", []string{"denylist", "flags", "allowlist"}),
			DenyPatterns:  list("GUARD_DENY_PATTERNS", nil),
			AllowBinaries: list("GUARD_ALLOW_BINARIES", nil),
			DenyFlags:     list("GUARD_DENY_FLAGS", nil),
			Mode:          str("GUARD_MODE", "strip"),
		},
		Store: StoreConfig{
			Driver:         str("STORE_DRIVER", "memory"),
			DSN:            str("POSTGRES_DSN", ""),
			MaxConns:       integer("POSTGRES_MAX_CONNS", 10),
			Migrate:        boolean("STORE_AUTO_MIGRATE", true),
			RetentionDays:  integer("STORE_RETENTION_DAYS", 30),
			PartitionAhead: integer("STORE_PARTITION_AHEAD", 7),
			MemoryLimit:    integer("STORE_MEMORY_LIMIT", 500),
		},
		Notify: NotifyConfig{
			Driver:        str("NOTIFY_DRIVER", "stdout"),
			Renderer:      str("RENDERER", "html"),
			From:          str("MAIL_FROM", "watchtower@example.com"),
			To:            list("MAIL_TO", []string{"sre@example.com"}),
			SMTPHost:      str("SMTP_HOST", ""),
			SMTPPort:      integer("SMTP_PORT", 587),
			SMTPUser:      str("SMTP_USERNAME", ""),
			SMTPPassword:  str("SMTP_PASSWORD", ""),
			SMTPTLS:       boolean("SMTP_TLS", true),
			MinConfidence: decimal("NOTIFY_MIN_CONFIDENCE", 0),
		},
		Cron: CronConfig{
			Enabled:       boolean("CRON_ENABLED", true),
			Timezone:      str("CRON_TZ", "UTC"),
			PartitionSpec: str("CRON_PARTITION_SPEC", "0 0 * * *"),
			DigestSpec:    str("CRON_DIGEST_SPEC", "0 8 * * *"),
			DigestWindow:  duration("DIGEST_WINDOW", 24*time.Hour),
		},
		Runtime: RuntimeConfig{
			BusBuffer:    integer("BUS_BUFFER", 512),
			BusWorkers:   integer("BUS_WORKERS", 4),
			StageTimeout: duration("STAGE_TIMEOUT", 60*time.Second),
		},
	}
	return cfg
}

// Validate catches configuration that cannot possibly work, early and loudly.
func (c Config) Validate() error {
	var errs []string

	if c.HTTP.Addr == "" {
		errs = append(errs, "http addr is empty")
	}
	if c.Worker.Workers <= 0 {
		errs = append(errs, "workers must be > 0")
	}
	if c.RAG.TopN > c.RAG.TopK {
		errs = append(errs, "rag top_n cannot exceed top_k")
	}
	if c.Store.Driver == "postgres" && c.Store.DSN == "" {
		errs = append(errs, "postgres store selected but POSTGRES_DSN is empty")
	}
	if c.RAG.VectorDriver == "postgres" && c.Store.DSN == "" {
		errs = append(errs, "postgres vector store selected but POSTGRES_DSN is empty")
	}
	if c.LLM.Provider == "openai" && c.LLM.APIKey == "" {
		errs = append(errs, "openai provider selected but OPENAI_API_KEY is empty")
	}
	if c.RAG.EmbedderDriver == "openai" && c.LLM.APIKey == "" {
		errs = append(errs, "openai embedder selected but OPENAI_API_KEY is empty")
	}
	if c.RAG.VectorDriver == "postgres" && c.RAG.EmbedderDriver == "stub" && c.RAG.VectorDims != 256 {
		errs = append(errs, "the stub embedder produces 256-dimension vectors: set WT_VECTOR_DIMS=256 or use the openai embedder with pgvector")
	}
	if c.Notify.Driver == "smtp" && c.Notify.SMTPHost == "" {
		errs = append(errs, "smtp notifier selected but SMTP_HOST is empty")
	}
	if c.Env == "prod" && c.HTTP.AuthToken == "" {
		errs = append(errs, "WT_HTTP_AUTH_TOKEN is required when ENV=prod: the /alerts and /runbook endpoints would otherwise be unauthenticated")
	}
	if c.Env == "prod" && c.HTTP.ApprovalSecret == "watchtower-dev-secret" {
		errs = append(errs, "WT_APPROVAL_SECRET must be set when ENV=prod so approval links cannot be forged")
	}

	if len(errs) == 0 {
		return nil
	}
	return errors.New("invalid config: " + strings.Join(errs, "; "))
}

// Summary is a one-line description of the selected drivers, logged at boot so
// there is never any doubt about which adapters are live.
func (c Config) Summary() string {
	return fmt.Sprintf(
		"env=%s store=%s vector=%s embedder=%s rerank=%s llm=%s notify=%s gate=%s cache=%s enrich=%s",
		c.Env, c.Store.Driver, c.RAG.VectorDriver, c.RAG.EmbedderDriver, c.RAG.RerankDriver,
		c.LLM.Provider, c.Notify.Driver, c.Ingest.GateDriver, c.RAG.CacheDriver,
		strings.Join(c.Enrich.Providers, "+"),
	)
}
