package ports

import (
	"context"
	"time"

	"github.com/sanjaysharmaoff/watchtower/internal/domain"
)

// Embedder turns text into a vector. Implemented by OpenAI, by a local model, or
// by a deterministic stub for offline runs.
type Embedder interface {
	Name() string
	Dimensions() int
	Embed(ctx context.Context, text string) ([]float32, error)
}

// VectorStore is similarity search plus write-back over the runbook corpus.
type VectorStore interface {
	Search(ctx context.Context, vec []float32, k int) ([]domain.RunbookMatch, error)
	Upsert(ctx context.Context, rb domain.Runbook) error
}

// Reranker narrows a candidate set. A cheap heuristic and a cross-encoder both
// satisfy this, so cost can be traded for quality via config.
type Reranker interface {
	Name() string
	Rerank(ctx context.Context, query string, matches []domain.RunbookMatch, top int) ([]domain.RunbookMatch, error)
}

// Cache is an opaque byte cache. Typed access is layered on top by decorators, so
// the port itself stays trivial to implement.
type Cache interface {
	Get(ctx context.Context, key string) ([]byte, bool, error)
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
	Close() error
}
