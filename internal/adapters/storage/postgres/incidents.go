package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sanjaysharmaoff/watchtower/internal/domain"
	"github.com/sanjaysharmaoff/watchtower/internal/ports"
	"github.com/sanjaysharmaoff/watchtower/internal/wire"
)

func init() {
	wire.IncidentStores.Register("postgres", func(e *wire.Env) (ports.IncidentStore, error) {
		pool, err := Pool(e)
		if err != nil {
			return nil, err
		}
		return &Incidents{pool: pool}, nil
	})
}

// Incidents writes the sanitized record into the partitioned raw table.
type Incidents struct{ pool *pgxpool.Pool }

// SaveIncident implements ports.IncidentStore.
func (s *Incidents) SaveIncident(ctx context.Context, inc *domain.Incident) error {
	if inc == nil {
		return errors.New("nil incident")
	}
	payload, err := json.Marshal(inc)
	if err != nil {
		return fmt.Errorf("marshal incident: %w", err)
	}

	const q = `
		INSERT INTO raw_anomalies
			(id, created_at, signature, correlation_key, severity, services, alert_count, log_count, confidence, payload)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (created_at, id) DO UPDATE SET
			payload    = EXCLUDED.payload,
			confidence = EXCLUDED.confidence,
			log_count  = EXCLUDED.log_count`

	_, err = s.pool.Exec(ctx, q,
		inc.ID, inc.CreatedAt, inc.Signature, inc.CorrelationKey, string(inc.Severity),
		inc.Services, len(inc.Alerts), len(inc.Logs), inc.Confidence, payload,
	)
	if err != nil {
		return fmt.Errorf("insert raw_anomalies: %w", err)
	}
	return nil
}

// GetIncident implements ports.IncidentStore.
func (s *Incidents) GetIncident(ctx context.Context, id string) (*domain.Incident, error) {
	const q = `SELECT payload FROM raw_anomalies WHERE id = $1 ORDER BY created_at DESC LIMIT 1`

	var payload []byte
	if err := s.pool.QueryRow(ctx, q, id).Scan(&payload); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("select raw_anomalies: %w", err)
	}
	var inc domain.Incident
	if err := json.Unmarshal(payload, &inc); err != nil {
		return nil, fmt.Errorf("unmarshal incident: %w", err)
	}
	return &inc, nil
}
