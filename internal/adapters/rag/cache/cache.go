// Package cache implements the byte cache port.
package cache

import (
	"context"
	"sync"
	"time"

	"github.com/sanjaysharmaoff/watchtower/internal/platform/redisx"
	"github.com/sanjaysharmaoff/watchtower/internal/ports"
	"github.com/sanjaysharmaoff/watchtower/internal/wire"
)

func init() {
	wire.Caches.Register("memory", func(e *wire.Env) (ports.Cache, error) {
		return NewMemory(e.Clock, 10000), nil
	})
	wire.Caches.Register("none", func(e *wire.Env) (ports.Cache, error) {
		return Nop{}, nil
	})
	wire.Caches.Register("redis", func(e *wire.Env) (ports.Cache, error) {
		client, err := redisx.Shared(e)
		if err != nil {
			return nil, err
		}
		return &Redis{client: client, prefix: "wt:cache:"}, nil
	})
}

type entry struct {
	value   []byte
	expires time.Time
}

// Memory is a bounded in-process cache with TTL support.
type Memory struct {
	clock ports.Clock
	max   int

	mu    sync.RWMutex
	items map[string]entry
}

// NewMemory builds an in-memory cache holding at most max entries.
func NewMemory(c ports.Clock, max int) *Memory {
	if max <= 0 {
		max = 1000
	}
	return &Memory{clock: c, max: max, items: make(map[string]entry, max/4)}
}

// Get implements ports.Cache.
func (m *Memory) Get(_ context.Context, key string) ([]byte, bool, error) {
	m.mu.RLock()
	e, ok := m.items[key]
	m.mu.RUnlock()
	if !ok {
		return nil, false, nil
	}
	if !e.expires.IsZero() && m.clock.Now().After(e.expires) {
		m.mu.Lock()
		delete(m.items, key)
		m.mu.Unlock()
		return nil, false, nil
	}
	return e.value, true, nil
}

// Set implements ports.Cache.
func (m *Memory) Set(_ context.Context, key string, value []byte, ttl time.Duration) error {
	var expires time.Time
	if ttl > 0 {
		expires = m.clock.Now().Add(ttl)
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	if len(m.items) >= m.max {
		m.evict()
	}
	m.items[key] = entry{value: value, expires: expires}
	return nil
}

// evict clears expired entries, then drops arbitrary keys if still over budget.
// Approximate eviction is fine: a cache miss only costs one embedding call.
func (m *Memory) evict() {
	now := m.clock.Now()
	for k, e := range m.items {
		if !e.expires.IsZero() && now.After(e.expires) {
			delete(m.items, k)
		}
	}
	drop := len(m.items) - m.max + m.max/10
	for k := range m.items {
		if drop <= 0 {
			return
		}
		delete(m.items, k)
		drop--
	}
}

// Close implements ports.Cache.
func (m *Memory) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.items = map[string]entry{}
	return nil
}

// Redis is the shared-cache implementation.
type Redis struct {
	client redisx.Client
	prefix string
}

// Get implements ports.Cache.
func (r *Redis) Get(ctx context.Context, key string) ([]byte, bool, error) {
	return r.client.Get(ctx, r.prefix+key)
}

// Set implements ports.Cache.
func (r *Redis) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	return r.client.Set(ctx, r.prefix+key, value, ttl)
}

// Close implements ports.Cache. The shared client is closed by the Env, not here.
func (r *Redis) Close() error { return nil }

// Nop caches nothing.
type Nop struct{}

// Get implements ports.Cache.
func (Nop) Get(context.Context, string) ([]byte, bool, error) { return nil, false, nil }

// Set implements ports.Cache.
func (Nop) Set(context.Context, string, []byte, time.Duration) error { return nil }

// Close implements ports.Cache.
func (Nop) Close() error { return nil }
