package wire

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Factory builds one adapter from the shared environment.
type Factory[T any] func(e *Env) (T, error)

// Registry maps driver names to factories for a single port.
type Registry[T any] struct {
	kind  string
	mu    sync.RWMutex
	items map[string]Factory[T]
}

// NewRegistry creates a registry for a port named kind.
func NewRegistry[T any](kind string) *Registry[T] {
	return &Registry[T]{kind: kind, items: map[string]Factory[T]{}}
}

// Register adds a factory. Called from adapter init functions; panics on a
// duplicate name because that is always a programming mistake.
func (r *Registry[T]) Register(name string, f Factory[T]) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.items[name]; dup {
		panic(fmt.Sprintf("wire: duplicate %s driver %q", r.kind, name))
	}
	r.items[name] = f
}

// Build resolves a driver name into an instance.
func (r *Registry[T]) Build(e *Env, name string) (T, error) {
	r.mu.RLock()
	f, ok := r.items[strings.ToLower(strings.TrimSpace(name))]
	r.mu.RUnlock()
	if !ok {
		var zero T
		return zero, fmt.Errorf("wire: unknown %s driver %q (available: %s)", r.kind, name, strings.Join(r.Names(), ", "))
	}
	return f(e)
}

// BuildAll resolves several driver names, keeping order.
func (r *Registry[T]) BuildAll(e *Env, names []string) ([]T, error) {
	out := make([]T, 0, len(names))
	for _, n := range names {
		if n == "" || strings.EqualFold(n, "none") {
			continue
		}
		v, err := r.Build(e, n)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// Has reports whether a driver is registered.
func (r *Registry[T]) Has(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.items[strings.ToLower(strings.TrimSpace(name))]
	return ok
}

// Names lists registered drivers.
func (r *Registry[T]) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.items))
	for k := range r.items {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
