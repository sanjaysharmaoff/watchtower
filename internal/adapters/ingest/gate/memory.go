// Package gate implements keyed admission control for the debouncer.
package gate

import (
	"context"
	"sync"
	"time"

	"github.com/sanjaysharmaoff/watchtower/internal/ports"
	"github.com/sanjaysharmaoff/watchtower/internal/wire"
)

func init() {
	wire.Gates.Register("memory", func(e *wire.Env) (ports.Gate, error) {
		return NewMemory(e.Clock), nil
	})
}

// Memory is a thread-safe in-process gate. Good default for a single replica;
// swap the driver to redis the moment there are two.
type Memory struct {
	clock ports.Clock
	mu    sync.Mutex
	seen  map[string]time.Time
	last  time.Time
}

// NewMemory builds an in-memory gate.
func NewMemory(c ports.Clock) *Memory {
	return &Memory{clock: c, seen: map[string]time.Time{}}
}

// Allow reports whether key has not been seen inside window, recording it if so.
func (m *Memory) Allow(_ context.Context, key string, window time.Duration) (bool, error) {
	if window <= 0 {
		return true, nil
	}
	now := m.clock.Now()

	m.mu.Lock()
	defer m.mu.Unlock()

	if now.Sub(m.last) > window {
		m.prune(now, window)
		m.last = now
	}
	if until, ok := m.seen[key]; ok && now.Before(until) {
		return false, nil
	}
	m.seen[key] = now.Add(window)
	return true, nil
}

// prune keeps the map from growing without bound under high alert churn.
func (m *Memory) prune(now time.Time, window time.Duration) {
	for k, until := range m.seen {
		if now.After(until.Add(window)) {
			delete(m.seen, k)
		}
	}
}

// Size reports how many keys are being tracked.
func (m *Memory) Size() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.seen)
}
