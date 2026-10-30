package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sanjaysharmaoff/watchtower/internal/domain"
	"github.com/sanjaysharmaoff/watchtower/internal/ports"
	"github.com/sanjaysharmaoff/watchtower/internal/wire"
)

func init() {
	wire.VectorStores.Register("postgres", func(e *wire.Env) (ports.VectorStore, error) {
		pool, err := Pool(e)
		if err != nil {
			return nil, err
		}
		return &Vectors{pool: pool}, nil
	})
}

// Vectors is the pgvector-backed runbook corpus.
type Vectors struct{ pool *pgxpool.Pool }

// Upsert implements ports.VectorStore.
func (v *Vectors) Upsert(ctx context.Context, rb domain.Runbook) error {
	if len(rb.Embedding) == 0 {
		return fmt.Errorf("runbook %s has no embedding", rb.ID)
	}
	commands, err := json.Marshal(rb.Commands)
	if err != nil {
		return fmt.Errorf("marshal commands: %w", err)
	}

	const q = `
		INSERT INTO historical_runbooks
			(id, title, signature, root_cause, commands, tags, reviewer, content, embedding, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9::vector,$10)
		ON CONFLICT (id) DO UPDATE SET
			title      = EXCLUDED.title,
			root_cause = EXCLUDED.root_cause,
			commands   = EXCLUDED.commands,
			tags       = EXCLUDED.tags,
			content    = EXCLUDED.content,
			embedding  = EXCLUDED.embedding`

	if _, err := v.pool.Exec(ctx, q,
		rb.ID, rb.Title, rb.Signature, rb.RootCause, commands, rb.Tags, rb.Reviewer,
		rb.Text(), encodeVector(rb.Embedding), rb.CreatedAt,
	); err != nil {
		return fmt.Errorf("upsert historical_runbooks: %w", err)
	}
	return nil
}

// Search implements ports.VectorStore using cosine distance.
func (v *Vectors) Search(ctx context.Context, vec []float32, k int) ([]domain.RunbookMatch, error) {
	if len(vec) == 0 {
		return nil, fmt.Errorf("empty query vector")
	}
	if k <= 0 {
		k = 10
	}

	const q = `
		SELECT id, title, COALESCE(signature,''), COALESCE(root_cause,''), commands,
		       COALESCE(tags,'{}'), COALESCE(reviewer,''), content, created_at,
		       1 - (embedding <=> $1::vector) AS similarity
		FROM historical_runbooks
		WHERE embedding IS NOT NULL
		ORDER BY embedding <=> $1::vector
		LIMIT $2`

	rows, err := v.pool.Query(ctx, q, encodeVector(vec), k)
	if err != nil {
		return nil, fmt.Errorf("vector search: %w", err)
	}
	defer rows.Close()

	var out []domain.RunbookMatch
	for rows.Next() {
		var (
			rb       domain.Runbook
			commands []byte
			score    float64
		)
		if err := rows.Scan(&rb.ID, &rb.Title, &rb.Signature, &rb.RootCause, &commands,
			&rb.Tags, &rb.Reviewer, &rb.SourceText, &rb.CreatedAt, &score); err != nil {
			return nil, fmt.Errorf("scan runbook: %w", err)
		}
		if len(commands) > 0 {
			_ = json.Unmarshal(commands, &rb.Commands)
		}
		if score < 0 {
			score = 0
		}
		out = append(out, domain.RunbookMatch{Runbook: rb, Score: score, Stage: "pgvector"})
	}
	return out, rows.Err()
}

// encodeVector renders a slice in pgvector's literal format.
func encodeVector(vec []float32) string {
	var b strings.Builder
	b.Grow(len(vec) * 10)
	b.WriteByte('[')
	for i, f := range vec {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(f), 'f', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}
