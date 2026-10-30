package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sanjaysharmaoff/watchtower/internal/domain"
	"github.com/sanjaysharmaoff/watchtower/internal/ports"
	"github.com/sanjaysharmaoff/watchtower/internal/wire"
)

func init() {
	wire.SummaryStores.Register("postgres", func(e *wire.Env) (ports.SummaryStore, error) {
		pool, err := Pool(e)
		if err != nil {
			return nil, err
		}
		return &Summaries{pool: pool}, nil
	})
	wire.DeadLetterQueues.Register("postgres", func(e *wire.Env) (ports.DeadLetterQueue, error) {
		pool, err := Pool(e)
		if err != nil {
			return nil, err
		}
		return &DLQ{pool: pool, log: e.Log.With("component", "dlq")}, nil
	})
}

// Summaries persists reviewed outcomes.
type Summaries struct{ pool *pgxpool.Pool }

// SaveSummary implements ports.SummaryStore.
func (s *Summaries) SaveSummary(ctx context.Context, sum domain.ApprovedSummary) error {
	commands, err := json.Marshal(sum.Commands)
	if err != nil {
		return fmt.Errorf("marshal commands: %w", err)
	}
	labels, err := json.Marshal(orEmpty(sum.Labels))
	if err != nil {
		return fmt.Errorf("marshal labels: %w", err)
	}

	const q = `
		INSERT INTO approved_summaries
			(id, incident_id, signature, title, root_cause, commands, decision, reviewer, notes, services, severity, labels, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		ON CONFLICT (id) DO UPDATE SET
			decision   = EXCLUDED.decision,
			commands   = EXCLUDED.commands,
			root_cause = EXCLUDED.root_cause,
			notes      = EXCLUDED.notes,
			reviewer   = EXCLUDED.reviewer`

	if _, err := s.pool.Exec(ctx, q,
		sum.ID, sum.IncidentID, sum.Signature, sum.Title, sum.RootCause, commands,
		string(sum.Decision), sum.Reviewer, sum.Notes, sum.Services, string(sum.Severity), labels, sum.CreatedAt,
	); err != nil {
		return fmt.Errorf("insert approved_summaries: %w", err)
	}
	return nil
}

// ListSummaries implements ports.SummaryStore.
func (s *Summaries) ListSummaries(ctx context.Context, from, to time.Time) ([]domain.ApprovedSummary, error) {
	const q = `
		SELECT id, incident_id, COALESCE(signature,''), title, COALESCE(root_cause,''), commands,
		       decision, COALESCE(reviewer,''), COALESCE(notes,''), COALESCE(services,'{}'), COALESCE(severity,''), created_at
		FROM approved_summaries
		WHERE created_at >= $1 AND created_at < $2
		ORDER BY created_at DESC`

	rows, err := s.pool.Query(ctx, q, from, to)
	if err != nil {
		return nil, fmt.Errorf("select approved_summaries: %w", err)
	}
	defer rows.Close()

	var out []domain.ApprovedSummary
	for rows.Next() {
		var (
			sum      domain.ApprovedSummary
			commands []byte
			decision string
			severity string
		)
		if err := rows.Scan(&sum.ID, &sum.IncidentID, &sum.Signature, &sum.Title, &sum.RootCause,
			&commands, &decision, &sum.Reviewer, &sum.Notes, &sum.Services, &severity, &sum.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan approved_summaries: %w", err)
		}
		if len(commands) > 0 {
			_ = json.Unmarshal(commands, &sum.Commands)
		}
		sum.Decision = domain.Decision(decision)
		sum.Severity = domain.Severity(severity)
		out = append(out, sum)
	}
	return out, rows.Err()
}

// DLQ is the durable failure sink.
type DLQ struct {
	pool *pgxpool.Pool
	log  ports.Logger
}

// Park implements ports.DeadLetterQueue.
func (d *DLQ) Park(ctx context.Context, dl domain.DeadLetter) error {
	const q = `
		INSERT INTO dead_letter_queue (id, stage, reason, ref_id, payload, at)
		VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (id) DO NOTHING`

	var payload any
	if len(dl.Payload) > 0 {
		payload = dl.Payload
	}
	if _, err := d.pool.Exec(ctx, q, dl.ID, dl.Stage, dl.Reason, dl.RefID, payload, dl.At); err != nil {
		// Last resort: if even the DLQ write fails, the log line is the record.
		d.log.Error("dead letter write failed", "stage", dl.Stage, "ref", dl.RefID, "reason", dl.Reason, "err", err.Error())
		return fmt.Errorf("insert dead_letter_queue: %w", err)
	}
	return nil
}

func orEmpty(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}
