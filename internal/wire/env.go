// Package wire holds the plugin registries and the shared build environment.
//
// Adapters register themselves here in init(). The composition root imports the
// adapter packages for their side effects only, so adding an implementation never
// requires editing a switch statement.
package wire

import (
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/sanjaysharmaoff/watchtower/internal/config"
	"github.com/sanjaysharmaoff/watchtower/internal/ports"
)

// Env is what every adapter factory receives: configuration, platform services
// and a memoized resource pool for expensive shared handles (a database pool, a
// Redis client) that several adapters need but none of them owns.
type Env struct {
	Cfg   config.Config
	Log   ports.Logger
	Clock ports.Clock
	IDs   ports.IDGen
	HTTP  *http.Client

	// Two independent locks on purpose. A resource factory is expected to register
	// its own shutdown hook while it is being built, so guarding the resource map
	// and the closer list with one mutex would deadlock the moment a factory calls
	// OnClose. sync.Mutex is not reentrant.
	mu        sync.Mutex
	resources map[string]any

	closeMu sync.Mutex
	closers []func() error
}

// NewEnv builds a build environment.
func NewEnv(cfg config.Config, log ports.Logger, clock ports.Clock, ids ports.IDGen) *Env {
	return &Env{
		Cfg:   cfg,
		Log:   log,
		Clock: clock,
		IDs:   ids,
		HTTP: &http.Client{
			Timeout: 60 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        64,
				MaxIdleConnsPerHost: 16,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		resources: map[string]any{},
	}
}

// OnClose registers a shutdown hook for a resource the Env created. Safe to call
// from inside a resource factory.
func (e *Env) OnClose(fn func() error) {
	e.closeMu.Lock()
	defer e.closeMu.Unlock()
	e.closers = append(e.closers, fn)
}

// Close runs every registered shutdown hook, last registered first.
func (e *Env) Close() error {
	e.closeMu.Lock()
	closers := append([]func() error(nil), e.closers...)
	e.closers = nil
	e.closeMu.Unlock()

	var firstErr error
	for i := len(closers) - 1; i >= 0; i-- {
		if err := closers[i](); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Resource returns a memoized shared handle, building it on first use. Two
// adapters asking for "postgres.pool" get the same pool without either of them
// knowing about the other.
//
// The lock is released while build runs, so a factory is free to call OnClose or
// ask for another resource. Adapters are constructed sequentially at startup, so
// the double-check on insert is belt and braces rather than a hot path.
func Resource[T any](e *Env, key string, build func() (T, error)) (T, error) {
	if existing, ok := e.lookup(key); ok {
		typed, ok := existing.(T)
		if !ok {
			var zero T
			return zero, fmt.Errorf("resource %q already registered as %T", key, existing)
		}
		return typed, nil
	}

	value, err := build()
	if err != nil {
		var zero T
		return zero, err
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if existing, ok := e.resources[key]; ok {
		if typed, ok := existing.(T); ok {
			return typed, nil
		}
	}
	e.resources[key] = value
	return value, nil
}

func (e *Env) lookup(key string) (any, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	v, ok := e.resources[key]
	return v, ok
}
