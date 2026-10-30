// Package memory is an in-process vector store with brute-force cosine search.
//
// Fine for a few thousand runbooks, which is more than most teams accumulate, and
// it removes pgvector from the local development path entirely.
package memory

import (
	"context"
	"math"
	"sort"
	"sync"

	"github.com/sanjaysharmaoff/watchtower/internal/domain"
	"github.com/sanjaysharmaoff/watchtower/internal/ports"
	"github.com/sanjaysharmaoff/watchtower/internal/wire"
)

func init() {
	wire.VectorStores.Register("memory", func(e *wire.Env) (ports.VectorStore, error) {
		return New(), nil
	})
}

// Store holds runbooks and their vectors.
type Store struct {
	mu    sync.RWMutex
	items map[string]domain.Runbook
	order []string
}

// New builds an empty store.
func New() *Store { return &Store{items: map[string]domain.Runbook{}} }

// Upsert implements ports.VectorStore.
func (s *Store) Upsert(_ context.Context, rb domain.Runbook) error {
	if rb.ID == "" {
		rb.ID = rb.Signature + ":" + rb.Title
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.items[rb.ID]; !exists {
		s.order = append(s.order, rb.ID)
	}
	s.items[rb.ID] = rb
	return nil
}

// Search implements ports.VectorStore.
func (s *Store) Search(_ context.Context, vec []float32, k int) ([]domain.RunbookMatch, error) {
	if k <= 0 {
		k = 10
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	matches := make([]domain.RunbookMatch, 0, len(s.items))
	for _, id := range s.order {
		rb, ok := s.items[id]
		if !ok || len(rb.Embedding) == 0 {
			continue
		}
		matches = append(matches, domain.RunbookMatch{
			Runbook: rb,
			Score:   cosine(vec, rb.Embedding),
			Stage:   "vector",
		})
	}
	sort.SliceStable(matches, func(i, j int) bool { return matches[i].Score > matches[j].Score })
	if len(matches) > k {
		matches = matches[:k]
	}
	return matches, nil
}

// Len reports how many runbooks are stored.
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.items)
}

// cosine returns similarity in [0,1] for non-negative-length vectors, clamped so
// callers never see a negative score.
func cosine(a, b []float32) float64 {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	var dot, na, nb float64
	for i := 0; i < n; i++ {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	sim := dot / (math.Sqrt(na) * math.Sqrt(nb))
	if sim < 0 {
		return 0
	}
	if sim > 1 {
		return 1
	}
	return sim
}
