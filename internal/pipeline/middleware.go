package pipeline

import (
	"context"
	"fmt"
	"time"

	"github.com/sanjaysharmaoff/watchtower/internal/domain"
	"github.com/sanjaysharmaoff/watchtower/internal/ports"
)

// WithLogging traces stage entry and exit. Deliberate drops log at info, real
// failures at error.
func WithLogging[T any](log ports.Logger) Middleware[T] {
	return func(next Stage[T]) Stage[T] {
		return Of(next.Name(), func(ctx context.Context, in T) (T, error) {
			start := time.Now()
			out, err := next.Process(ctx, in)
			took := time.Since(start).Round(time.Millisecond)
			switch {
			case err == nil:
				log.Debug("stage ok", "stage", next.Name(), "took", took.String())
			case domain.IsDrop(err):
				log.Info("stage dropped payload", "stage", next.Name(), "reason", err.Error())
			default:
				log.Error("stage failed", "stage", next.Name(), "took", took.String(), "err", err.Error())
			}
			return out, err
		})
	}
}

// WithRecover converts a panic in any stage into an error so one bad adapter
// cannot take the process down.
func WithRecover[T any](log ports.Logger) Middleware[T] {
	return func(next Stage[T]) Stage[T] {
		return Of(next.Name(), func(ctx context.Context, in T) (out T, err error) {
			defer func() {
				if r := recover(); r != nil {
					out = in
					err = fmt.Errorf("stage %s panicked: %v", next.Name(), r)
					log.Error("stage panic", "stage", next.Name(), "panic", fmt.Sprint(r))
				}
			}()
			return next.Process(ctx, in)
		})
	}
}

// WithTimeout bounds how long a single stage may take.
func WithTimeout[T any](d time.Duration) Middleware[T] {
	return func(next Stage[T]) Stage[T] {
		if d <= 0 {
			return next
		}
		return Of(next.Name(), func(ctx context.Context, in T) (T, error) {
			ctx, cancel := context.WithTimeout(ctx, d)
			defer cancel()
			return next.Process(ctx, in)
		})
	}
}

// Observer is notified after every stage. Metrics backends plug in here without
// the pipeline importing them.
type Observer func(stage string, took time.Duration, err error)

// WithObserver reports stage outcomes to an Observer.
func WithObserver[T any](obs Observer) Middleware[T] {
	return func(next Stage[T]) Stage[T] {
		if obs == nil {
			return next
		}
		return Of(next.Name(), func(ctx context.Context, in T) (T, error) {
			start := time.Now()
			out, err := next.Process(ctx, in)
			obs(next.Name(), time.Since(start), err)
			return out, err
		})
	}
}
