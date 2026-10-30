// Package ports declares the seams of the system.
//
// Every interface here is deliberately tiny so an adapter can be written (or
// replaced) without touching anything else. Ports may only import domain and the
// standard library.
package ports

import (
	"context"
	"io"
	"time"

	"github.com/sanjaysharmaoff/watchtower/internal/domain"
)

// AlertDecoder turns a raw request body into normalized alerts. Alertmanager,
// Grafana, a generic JSON shape: one adapter each.
type AlertDecoder interface {
	Name() string
	Decode(r io.Reader) ([]domain.Alert, error)
}

// AlertSink accepts normalized alerts for triage. The HTTP layer depends on this
// and nothing else.
type AlertSink interface {
	Submit(ctx context.Context, alerts []domain.Alert) (string, error)
}

// FeedbackSink accepts SRE decisions.
type FeedbackSink interface {
	Submit(ctx context.Context, fb domain.Feedback) error
}

// Gate is a keyed admission control primitive. The debouncer uses it; so could
// a rate limiter. In-memory and Redis implementations are interchangeable.
type Gate interface {
	Allow(ctx context.Context, key string, window time.Duration) (bool, error)
}

// Correlator groups an incoming incident with recent related ones.
type Correlator interface {
	Correlate(ctx context.Context, inc *domain.Incident) error
}

// ContextProvider fetches extra evidence for an incident. Loki, the Kubernetes
// event API, a tracing backend: all the same shape, all optional, all allowed to
// fail without killing the run.
type ContextProvider interface {
	Name() string
	Fetch(ctx context.Context, inc *domain.Incident) (domain.ContextFragment, error)
}

// LogProcessor is one link of the sanitizer chain: dedupe, compress, filter.
type LogProcessor interface {
	Name() string
	Process(lines []domain.LogLine) []domain.LogLine
}

// Dispatcher runs work off the request path with bounded concurrency.
type Dispatcher interface {
	Submit(job func(context.Context)) error
	Shutdown(ctx context.Context) error
}
