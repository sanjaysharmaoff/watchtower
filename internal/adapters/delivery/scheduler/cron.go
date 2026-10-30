// Package scheduler runs the maintenance jobs on a cron schedule.
package scheduler

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
	"github.com/sanjaysharmaoff/watchtower/internal/ports"
	"github.com/sanjaysharmaoff/watchtower/internal/wire"
)

func init() {
	wire.Schedulers.Register("cron", func(e *wire.Env) (ports.Scheduler, error) {
		loc, err := time.LoadLocation(e.Cfg.Cron.Timezone)
		if err != nil {
			e.Log.Warn("unknown cron timezone, falling back to UTC", "tz", e.Cfg.Cron.Timezone)
			loc = time.UTC
		}
		return NewCron(loc, e.Log.With("component", "cron")), nil
	})
	wire.Schedulers.Register("none", func(e *wire.Env) (ports.Scheduler, error) {
		return Noop{}, nil
	})
}

// Cron wraps robfig/cron with panic recovery and per-run context cancellation.
type Cron struct {
	c   *cron.Cron
	log ports.Logger

	mu      sync.Mutex
	running sync.WaitGroup
	cancels []context.CancelFunc
}

// NewCron builds a scheduler in the given location.
func NewCron(loc *time.Location, log ports.Logger) *Cron {
	return &Cron{c: cron.New(cron.WithLocation(loc)), log: log}
}

// Schedule implements ports.Scheduler.
func (s *Cron) Schedule(name, spec string, job func(context.Context)) error {
	if spec == "" {
		s.log.Info("job disabled, empty schedule", "job", name)
		return nil
	}
	_, err := s.c.AddFunc(spec, func() {
		defer func() {
			if r := recover(); r != nil {
				s.log.Error("scheduled job panicked", "job", name, "panic", r)
			}
		}()

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		s.mu.Lock()
		s.cancels = append(s.cancels, cancel)
		s.mu.Unlock()

		s.running.Add(1)
		defer s.running.Done()
		defer cancel()

		start := time.Now()
		s.log.Info("job started", "job", name)
		job(ctx)
		s.log.Info("job finished", "job", name, "took", time.Since(start).Round(time.Millisecond).String())
	})
	if err != nil {
		return fmt.Errorf("schedule %s (%q): %w", name, spec, err)
	}
	s.log.Info("job scheduled", "job", name, "spec", spec)
	return nil
}

// Start implements ports.Scheduler.
func (s *Cron) Start() { s.c.Start() }

// Stop implements ports.Scheduler, waiting for in-flight jobs within the context.
func (s *Cron) Stop(ctx context.Context) error {
	stopped := s.c.Stop()
	select {
	case <-stopped.Done():
	case <-ctx.Done():
	}

	done := make(chan struct{})
	go func() { s.running.Wait(); close(done) }()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		s.mu.Lock()
		for _, cancel := range s.cancels {
			cancel()
		}
		s.mu.Unlock()
		return ctx.Err()
	}
}

// Noop is a scheduler that never runs anything.
type Noop struct{}

// Schedule implements ports.Scheduler.
func (Noop) Schedule(string, string, func(context.Context)) error { return nil }

// Start implements ports.Scheduler.
func (Noop) Start() {}

// Stop implements ports.Scheduler.
func (Noop) Stop(context.Context) error { return nil }
