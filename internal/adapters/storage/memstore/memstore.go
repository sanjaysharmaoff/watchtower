// Package memstore implements the storage ports in memory.
//
// It keeps the service runnable with zero infrastructure, which is what makes the
// whole pipeline demonstrable on a laptop. Bounded by a ring so a long-running dev
// process cannot leak.
package memstore

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/sanjaysharmaoff/watchtower/internal/domain"
	"github.com/sanjaysharmaoff/watchtower/internal/ports"
	"github.com/sanjaysharmaoff/watchtower/internal/wire"
)

func init() {
	wire.IncidentStores.Register("memory", func(e *wire.Env) (ports.IncidentStore, error) {
		return shared(e), nil
	})
	wire.SummaryStores.Register("memory", func(e *wire.Env) (ports.SummaryStore, error) {
		return shared(e), nil
	})
	wire.DeadLetterQueues.Register("memory", func(e *wire.Env) (ports.DeadLetterQueue, error) {
		return shared(e), nil
	})
	wire.PartitionManagers.Register("memory", func(e *wire.Env) (ports.PartitionManager, error) {
		return shared(e), nil
	})
	wire.DeadLetterQueues.Register("log", func(e *wire.Env) (ports.DeadLetterQueue, error) {
		return LogOnly{log: e.Log.With("component", "dlq")}, nil
	})
}

// shared makes every port view the same underlying store, so a summary written
// through one interface is visible through another.
func shared(e *wire.Env) *Store {
	s, err := wire.Resource(e, "memstore", func() (*Store, error) {
		return New(e.Cfg.Store.MemoryLimit, e.Log.With("component", "memstore")), nil
	})
	if err != nil {
		return New(e.Cfg.Store.MemoryLimit, e.Log)
	}
	return s
}

// Store is an in-memory implementation of every storage port.
type Store struct {
	limit int
	log   ports.Logger

	mu        sync.RWMutex
	incidents map[string]*domain.Incident
	order     []string
	summaries []domain.ApprovedSummary
	dead      []domain.DeadLetter
}

// New builds a bounded in-memory store.
func New(limit int, log ports.Logger) *Store {
	if limit <= 0 {
		limit = 500
	}
	return &Store{limit: limit, log: log, incidents: map[string]*domain.Incident{}}
}

// SaveIncident implements ports.IncidentStore.
func (s *Store) SaveIncident(_ context.Context, inc *domain.Incident) error {
	if inc == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.incidents[inc.ID]; !exists {
		s.order = append(s.order, inc.ID)
	}
	s.incidents[inc.ID] = inc
	for len(s.order) > s.limit {
		delete(s.incidents, s.order[0])
		s.order = s.order[1:]
	}
	return nil
}

// GetIncident implements ports.IncidentStore.
func (s *Store) GetIncident(_ context.Context, id string) (*domain.Incident, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	inc, ok := s.incidents[id]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return inc, nil
}

// SaveSummary implements ports.SummaryStore.
func (s *Store) SaveSummary(_ context.Context, sum domain.ApprovedSummary) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, existing := range s.summaries {
		if existing.ID == sum.ID {
			s.summaries[i] = sum
			return nil
		}
	}
	s.summaries = append(s.summaries, sum)
	if len(s.summaries) > s.limit {
		s.summaries = s.summaries[len(s.summaries)-s.limit:]
	}
	return nil
}

// ListSummaries implements ports.SummaryStore.
func (s *Store) ListSummaries(_ context.Context, from, to time.Time) ([]domain.ApprovedSummary, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]domain.ApprovedSummary, 0, len(s.summaries))
	for _, sum := range s.summaries {
		if sum.CreatedAt.Before(from) || !sum.CreatedAt.Before(to) {
			continue
		}
		out = append(out, sum)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// Park implements ports.DeadLetterQueue.
func (s *Store) Park(_ context.Context, dl domain.DeadLetter) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dead = append(s.dead, dl)
	if len(s.dead) > s.limit {
		s.dead = s.dead[len(s.dead)-s.limit:]
	}
	s.log.Warn("dead letter parked in memory", "stage", dl.Stage, "ref", dl.RefID, "reason", dl.Reason)
	return nil
}

// DeadLetters returns a copy of the parked failures.
func (s *Store) DeadLetters() []domain.DeadLetter {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]domain.DeadLetter(nil), s.dead...)
}

// EnsurePartitions implements ports.PartitionManager as a no-op: memory has no
// partitions, but the maintenance service should not have to care.
func (s *Store) EnsurePartitions(context.Context, int) error { return nil }

// DropPartitionsOlderThan implements ports.PartitionManager by trimming the ring.
func (s *Store) DropPartitionsOlderThan(_ context.Context, days int) error {
	if days <= 0 {
		return nil
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -days)

	s.mu.Lock()
	defer s.mu.Unlock()
	kept := make([]string, 0, len(s.order))
	for _, id := range s.order {
		inc := s.incidents[id]
		if inc != nil && inc.CreatedAt.Before(cutoff) {
			delete(s.incidents, id)
			continue
		}
		kept = append(kept, id)
	}
	s.order = kept
	return nil
}

// LogOnly is a dead letter queue that only writes a log line. Useful when there
// is no database at all and losing the payload is acceptable.
type LogOnly struct{ log ports.Logger }

// Park implements ports.DeadLetterQueue.
func (l LogOnly) Park(_ context.Context, dl domain.DeadLetter) error {
	l.log.Error("dead letter", "stage", dl.Stage, "ref", dl.RefID, "reason", dl.Reason, "payload_bytes", len(dl.Payload))
	return nil
}
