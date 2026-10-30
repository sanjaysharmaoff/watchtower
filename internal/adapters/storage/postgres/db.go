// Package postgres implements the storage ports on PostgreSQL, including the
// pgvector-backed runbook corpus and the daily partition lifecycle.
package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sanjaysharmaoff/watchtower/internal/ports"
	"github.com/sanjaysharmaoff/watchtower/internal/wire"
)

// Pool returns the process-wide connection pool, creating it on first use.
func Pool(e *wire.Env) (*pgxpool.Pool, error) {
	return wire.Resource(e, "postgres.pool", func() (*pgxpool.Pool, error) {
		if e.Cfg.Store.DSN == "" {
			return nil, fmt.Errorf("postgres: DSN is not configured")
		}
		cfg, err := pgxpool.ParseConfig(e.Cfg.Store.DSN)
		if err != nil {
			return nil, fmt.Errorf("postgres: parse DSN: %w", err)
		}
		if e.Cfg.Store.MaxConns > 0 {
			cfg.MaxConns = int32(e.Cfg.Store.MaxConns)
		}
		cfg.MaxConnLifetime = 30 * time.Minute
		cfg.MaxConnIdleTime = 5 * time.Minute

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		pool, err := pgxpool.NewWithConfig(ctx, cfg)
		if err != nil {
			return nil, fmt.Errorf("postgres: connect: %w", err)
		}
		if err := pool.Ping(ctx); err != nil {
			pool.Close()
			return nil, fmt.Errorf("postgres: ping: %w", err)
		}
		e.OnClose(func() error { pool.Close(); return nil })
		e.Log.Info("postgres connected", "max_conns", cfg.MaxConns)

		if e.Cfg.Store.Migrate {
			if err := migrate(ctx, pool, e.Cfg.RAG.VectorDims, e.Log); err != nil {
				pool.Close()
				return nil, err
			}
		}
		return pool, nil
	})
}

// migrate creates everything WatchTower needs, idempotently. Schema lives in code
// so a fresh environment boots without an out-of-band migration step; the same
// DDL is mirrored in migrations/001_init.sql for teams that prefer to apply it
// themselves with WT_STORE_AUTO_MIGRATE=false.
func migrate(ctx context.Context, pool *pgxpool.Pool, dims int, log ports.Logger) error {
	if dims <= 0 {
		dims = 1536
	}

	stmts := []string{
		`CREATE TABLE IF NOT EXISTS raw_anomalies (
			id              TEXT        NOT NULL,
			created_at      TIMESTAMPTZ NOT NULL,
			signature       TEXT        NOT NULL,
			correlation_key TEXT,
			severity        TEXT,
			services        TEXT[],
			alert_count     INT         NOT NULL DEFAULT 0,
			log_count       INT         NOT NULL DEFAULT 0,
			confidence      DOUBLE PRECISION,
			payload         JSONB       NOT NULL,
			PRIMARY KEY (created_at, id)
		) PARTITION BY RANGE (created_at)`,

		`CREATE INDEX IF NOT EXISTS raw_anomalies_signature_idx ON raw_anomalies (signature, created_at DESC)`,

		`CREATE TABLE IF NOT EXISTS approved_summaries (
			id          TEXT PRIMARY KEY,
			incident_id TEXT NOT NULL,
			signature   TEXT,
			title       TEXT NOT NULL,
			root_cause  TEXT,
			commands    JSONB NOT NULL DEFAULT '[]'::jsonb,
			decision    TEXT NOT NULL,
			reviewer    TEXT,
			notes       TEXT,
			services    TEXT[],
			severity    TEXT,
			labels      JSONB NOT NULL DEFAULT '{}'::jsonb,
			created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,

		`CREATE INDEX IF NOT EXISTS approved_summaries_created_idx ON approved_summaries (created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS approved_summaries_incident_idx ON approved_summaries (incident_id)`,

		`CREATE TABLE IF NOT EXISTS dead_letter_queue (
			id      TEXT PRIMARY KEY,
			stage   TEXT NOT NULL,
			reason  TEXT NOT NULL,
			ref_id  TEXT,
			payload JSONB,
			at      TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,

		`CREATE INDEX IF NOT EXISTS dead_letter_queue_at_idx ON dead_letter_queue (at DESC)`,
	}

	for _, stmt := range stmts {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("postgres: migrate: %w", err)
		}
	}

	// The runbook corpus needs pgvector. Treat a missing extension as fatal only
	// when the vector store is actually pointed at Postgres, which the caller
	// signals by asking for this migration in the first place.
	if _, err := pool.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS vector`); err != nil {
		log.Warn("pgvector extension unavailable, runbook corpus disabled on this database", "err", err.Error())
		return nil
	}
	runbooks := []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS historical_runbooks (
			id         TEXT PRIMARY KEY,
			title      TEXT NOT NULL,
			signature  TEXT,
			root_cause TEXT,
			commands   JSONB NOT NULL DEFAULT '[]'::jsonb,
			tags       TEXT[],
			reviewer   TEXT,
			content    TEXT NOT NULL,
			embedding  vector(%d),
			created_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`, dims),
		`CREATE INDEX IF NOT EXISTS historical_runbooks_signature_idx ON historical_runbooks (signature)`,
		`CREATE INDEX IF NOT EXISTS historical_runbooks_embedding_idx ON historical_runbooks USING hnsw (embedding vector_cosine_ops)`,
	}
	for _, stmt := range runbooks {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			log.Warn("runbook corpus DDL failed", "err", err.Error())
			return nil
		}
	}

	log.Info("postgres schema ready", "vector_dims", dims)
	return nil
}
