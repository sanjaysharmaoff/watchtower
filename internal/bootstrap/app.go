// Package bootstrap is the composition root.
//
// This is the only package allowed to know which adapters exist and in what order
// the stages run. Everything else in WatchTower is wired through interfaces, so
// this file is where the shape of a deployment is decided.
package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sanjaysharmaoff/watchtower/internal/adapters/inbound/httpapi"
	"github.com/sanjaysharmaoff/watchtower/internal/adapters/ingest/worker"
	"github.com/sanjaysharmaoff/watchtower/internal/adapters/rag/embed/cached"
	"github.com/sanjaysharmaoff/watchtower/internal/bus"
	"github.com/sanjaysharmaoff/watchtower/internal/config"
	"github.com/sanjaysharmaoff/watchtower/internal/domain"
	"github.com/sanjaysharmaoff/watchtower/internal/pipeline"
	"github.com/sanjaysharmaoff/watchtower/internal/platform/clock"
	"github.com/sanjaysharmaoff/watchtower/internal/platform/logging"
	"github.com/sanjaysharmaoff/watchtower/internal/platform/sign"
	"github.com/sanjaysharmaoff/watchtower/internal/ports"
	"github.com/sanjaysharmaoff/watchtower/internal/projectors"
	"github.com/sanjaysharmaoff/watchtower/internal/services"
	"github.com/sanjaysharmaoff/watchtower/internal/stages"
	"github.com/sanjaysharmaoff/watchtower/internal/wire"
)

// Version is stamped at build time with -ldflags.
var Version = "dev"

// App is a fully wired WatchTower instance.
type App struct {
	cfg    config.Config
	log    ports.Logger
	env    *wire.Env
	broker *bus.Async
	pool   *worker.Pool
	server *httpapi.Server
	sched  ports.Scheduler

	Triage      *services.Triage
	Feedback    *services.Feedback
	Digest      *services.Digest
	Maintenance *services.Maintenance
}

// New builds everything from configuration. It fails fast and loudly: a
// misconfigured adapter is a startup error, never a surprise at 3am.
func New(cfg config.Config) (*App, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	log := logging.New(cfg.Log.Level, cfg.Log.Format)
	env := wire.NewEnv(cfg, log, clock.System(), clock.UUID())

	log.Info("watchtower starting", "version", Version, "drivers", cfg.Summary())

	app := &App{cfg: cfg, log: log, env: env}
	if err := app.build(); err != nil {
		_ = env.Close()
		return nil, err
	}
	return app, nil
}

func (a *App) build() error {
	cfg, env, log := a.cfg, a.env, a.log

	// --- storage ---------------------------------------------------------------
	incidentStore, err := wire.IncidentStores.Build(env, cfg.Store.Driver)
	if err != nil {
		return err
	}
	summaryStore, err := wire.SummaryStores.Build(env, cfg.Store.Driver)
	if err != nil {
		return err
	}
	dlq, err := wire.DeadLetterQueues.Build(env, cfg.Store.Driver)
	if err != nil {
		return err
	}
	partitions, err := wire.PartitionManagers.Build(env, cfg.Store.Driver)
	if err != nil {
		return err
	}

	// --- retrieval -------------------------------------------------------------
	embedder, err := wire.Embedders.Build(env, cfg.RAG.EmbedderDriver)
	if err != nil {
		return err
	}
	vectors, err := wire.VectorStores.Build(env, cfg.RAG.VectorDriver)
	if err != nil {
		return err
	}
	reranker, err := wire.Rerankers.Build(env, cfg.RAG.RerankDriver)
	if err != nil {
		return err
	}
	if cfg.RAG.CacheDriver != "none" {
		embedCache, err := wire.Caches.Build(env, cfg.RAG.CacheDriver)
		if err != nil {
			return err
		}
		env.OnClose(embedCache.Close)
		// Decorator, not a config flag inside the embedder: the OpenAI adapter never
		// learns that caching exists.
		embedder = cached.Wrap(embedder, embedCache, cfg.RAG.CacheTTL, log)
	}

	// --- cognition -------------------------------------------------------------
	promptBuilder, err := wire.PromptBuilders.Build(env, "sre")
	if err != nil {
		return err
	}
	planner, err := wire.Planners.Build(env, cfg.LLM.Provider)
	if err != nil {
		return err
	}
	var critic ports.Critic
	if cfg.LLM.CriticOn {
		if critic, err = wire.Critics.Build(env, cfg.LLM.Provider); err != nil {
			return err
		}
	}
	narrator, err := wire.Narrators.Build(env, cfg.LLM.Provider)
	if err != nil {
		return err
	}
	scorer, err := wire.Scorers.Build(env, "weighted")
	if err != nil {
		return err
	}

	// --- guardrails ------------------------------------------------------------
	redactors, err := wire.Redactors.BuildAll(env, cfg.Guard.Redactors)
	if err != nil {
		return err
	}
	inspectors, err := wire.Inspectors.BuildAll(env, cfg.Guard.Inspectors)
	if err != nil {
		return err
	}
	riskClassifier, err := wire.RiskClassifiers.Build(env, "heuristic")
	if err != nil {
		return err
	}

	// --- ingestion -------------------------------------------------------------
	decoder, err := wire.AlertDecoders.Build(env, cfg.Ingest.Decoder)
	if err != nil {
		return err
	}
	gate, err := wire.Gates.Build(env, cfg.Ingest.GateDriver)
	if err != nil {
		return err
	}
	correlator, err := wire.Correlators.Build(env, "window")
	if err != nil {
		return err
	}
	providers, err := wire.ContextProviders.BuildAll(env, cfg.Enrich.Providers)
	if err != nil {
		return err
	}
	processors, err := wire.LogProcessors.BuildAll(env, cfg.Clean.Processors)
	if err != nil {
		return err
	}

	// --- delivery --------------------------------------------------------------
	renderer, err := wire.Renderers.Build(env, cfg.Notify.Renderer)
	if err != nil {
		return err
	}
	notifier, err := wire.Notifiers.Build(env, cfg.Notify.Driver)
	if err != nil {
		return err
	}

	// --- event bus and projectors ---------------------------------------------
	a.broker = bus.NewAsync(log, env.Clock, cfg.Runtime.BusBuffer, cfg.Runtime.BusWorkers)
	projectors.NewPersist(incidentStore, dlq, env.IDs, env.Clock, log).Register(a.broker)
	projectors.NewDeliver(renderer, notifier, dlq, env.IDs, env.Clock, cfg.Notify.MinConfidence, log).Register(a.broker)
	projectors.NewAudit(log).Register(a.broker)

	// --- the pipeline ----------------------------------------------------------
	// Reordering this slice reorders the system. Nothing else needs to change.
	pipe := pipeline.New[*domain.Incident]("triage",
		stages.Admit(stages.AdmitOptions{
			DropResolved: cfg.Ingest.DropResolved,
			MinSeverity:  domain.ParseSeverity(cfg.Ingest.MinSeverity),
		}),
		stages.Debounce(gate, cfg.Ingest.DebounceWindow, nil),
		stages.Correlate(correlator),
		stages.Enrich(providers, cfg.Enrich.Timeout),
		stages.Sanitize(processors),
		// Redaction sits ahead of both the first database write and the first
		// outbound model call, so a credential that leaked into a log line is never
		// written to disk and never leaves the process.
		stages.InputGuard(redactors),
		stages.Publish(a.broker, bus.TopicIncidentSanitized),
		stages.Retrieve(embedder, vectors, reranker, stages.RetrieveOptions{
			TopK:     cfg.RAG.TopK,
			TopN:     cfg.RAG.TopN,
			MinScore: cfg.RAG.MinScore,
		}),
		// Second pass, over what retrieval just pulled out of the shared corpus.
		stages.InputGuardNamed("input-guard:retrieved", redactors),
		stages.BuildPrompt(promptBuilder),
		stages.Plan(planner, critic, stages.PlanOptions{
			MaxAttempts: cfg.LLM.MaxAttempts,
			CriticOn:    cfg.LLM.CriticOn && critic != nil,
		}),
		stages.OutputGuard(inspectors, riskClassifier, stages.GuardMode(cfg.Guard.Mode)),
		stages.Score(scorer),
		stages.Publish(a.broker, bus.TopicIncidentTriaged),
	).Use(
		pipeline.WithRecover[*domain.Incident](log),
		pipeline.WithLogging[*domain.Incident](log),
		pipeline.WithTimeout[*domain.Incident](cfg.Runtime.StageTimeout),
	)
	log.Info("pipeline assembled", "stages", pipe.String())

	// --- services --------------------------------------------------------------
	a.pool = worker.New(cfg.Worker.Workers, cfg.Worker.QueueSize, cfg.Worker.JobTimeout, log)
	a.Triage = services.NewTriage(pipe, a.pool, a.broker, dlq, env.IDs, env.Clock, log)
	a.Feedback = services.NewFeedback(incidentStore, summaryStore, vectors, embedder, dlq, a.broker, env.IDs, env.Clock, log)
	a.Digest = services.NewDigest(summaryStore, narrator, renderer, notifier, a.broker, env.Clock, cfg.Cron.DigestWindow, log)
	a.Maintenance = services.NewMaintenance(partitions, cfg.Store.PartitionAhead, cfg.Store.RetentionDays, log)

	// --- inbound edge ----------------------------------------------------------
	a.server = httpapi.New(httpapi.Deps{
		Config:   cfg.HTTP,
		Log:      log,
		Clock:    env.Clock,
		Decoder:  decoder,
		Alerts:   a.Triage,
		Feedback: a.Feedback,
		Signer:   sign.HMAC(cfg.HTTP.ApprovalSecret),
		Version:  Version,
		Stats:    a.pool.Stats,
		Ready:    a.ready,
	})

	// --- schedules -------------------------------------------------------------
	driver := "cron"
	if !cfg.Cron.Enabled {
		driver = "none"
	}
	if a.sched, err = wire.Schedulers.Build(env, driver); err != nil {
		return err
	}
	if err := a.sched.Schedule("partition-manager", cfg.Cron.PartitionSpec, func(ctx context.Context) {
		if err := a.Maintenance.Run(ctx); err != nil {
			log.Error("maintenance failed", "err", err.Error())
		}
	}); err != nil {
		return err
	}
	if err := a.sched.Schedule("daily-pulse", cfg.Cron.DigestSpec, func(ctx context.Context) {
		if err := a.Digest.Run(ctx); err != nil {
			log.Error("daily pulse failed", "err", err.Error())
		}
	}); err != nil {
		return err
	}

	return nil
}

// ready is the readiness probe: the pipeline is only useful if storage accepts
// writes, so a failed partition check reports not-ready.
func (a *App) ready(ctx context.Context) error {
	if a.Maintenance == nil {
		return errors.New("not initialized")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return a.Maintenance.Run(ctx)
}

// Run starts every component and blocks until ctx is cancelled, then shuts down.
func (a *App) Run(ctx context.Context) error {
	a.broker.Start()
	a.sched.Start()

	// Make sure today's partition exists before the first write lands.
	startupCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	if err := a.Maintenance.Run(startupCtx); err != nil {
		a.log.Warn("startup maintenance failed, continuing", "err", err.Error())
	}
	cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- a.server.Start() }()

	select {
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("http server: %w", err)
		}
		return nil
	case <-ctx.Done():
		a.log.Info("shutdown signal received")
		return a.Shutdown()
	}
}

// RunOnce executes a single maintenance job and returns, which is how WatchTower
// runs as a Kubernetes CronJob instead of using its own scheduler.
func (a *App) RunOnce(ctx context.Context, job string) error {
	a.broker.Start()
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = a.broker.Close(closeCtx)
		_ = a.env.Close()
	}()

	switch job {
	case "digest", "pulse":
		return a.Digest.Run(ctx)
	case "maintenance", "partitions":
		return a.Maintenance.Run(ctx)
	default:
		return fmt.Errorf("unknown job %q (want digest or maintenance)", job)
	}
}

// Shutdown stops components in dependency order: stop accepting, drain work,
// flush events, close resources.
func (a *App) Shutdown() error {
	grace := a.cfg.HTTP.ShutdownGrace
	if grace <= 0 {
		grace = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()

	var errs []error
	if err := a.server.Shutdown(ctx); err != nil {
		errs = append(errs, fmt.Errorf("http shutdown: %w", err))
	}
	if err := a.sched.Stop(ctx); err != nil {
		errs = append(errs, fmt.Errorf("scheduler stop: %w", err))
	}
	if err := a.pool.Shutdown(ctx); err != nil {
		errs = append(errs, fmt.Errorf("worker shutdown: %w", err))
	}
	if err := a.broker.Close(ctx); err != nil {
		errs = append(errs, fmt.Errorf("bus close: %w", err))
	}
	if err := a.env.Close(); err != nil {
		errs = append(errs, fmt.Errorf("resource close: %w", err))
	}

	a.log.Info("watchtower stopped")
	return errors.Join(errs...)
}
