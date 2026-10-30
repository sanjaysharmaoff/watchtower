// Package worker is the bounded dispatcher that keeps a webhook flood from
// becoming an outage of its own.
package worker

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sanjaysharmaoff/watchtower/internal/ports"
)

// ErrQueueFull is returned when the queue is saturated, so the caller can reply
// 429 instead of buffering without limit.
var ErrQueueFull = errors.New("worker queue is full")

// ErrClosed is returned after shutdown.
var ErrClosed = errors.New("worker pool is closed")

// Pool implements ports.Dispatcher with a fixed number of workers, a bounded
// queue, per-job timeouts and panic isolation.
type Pool struct {
	queue      chan func(context.Context)
	workers    int
	jobTimeout time.Duration
	log        ports.Logger

	wg       sync.WaitGroup
	closing  atomic.Bool
	closed   chan struct{}
	stopOnce sync.Once

	accepted atomic.Int64
	rejected atomic.Int64
	failed   atomic.Int64
	done     atomic.Int64
}

// New builds and starts a pool.
func New(workers, queueSize int, jobTimeout time.Duration, log ports.Logger) *Pool {
	if workers <= 0 {
		workers = 4
	}
	if queueSize <= 0 {
		queueSize = workers * 32
	}
	p := &Pool{
		queue:      make(chan func(context.Context), queueSize),
		workers:    workers,
		jobTimeout: jobTimeout,
		log:        log.With("component", "worker"),
		closed:     make(chan struct{}),
	}
	for i := 0; i < workers; i++ {
		p.wg.Add(1)
		go p.loop(i)
	}
	p.log.Info("worker pool started", "workers", workers, "queue", queueSize)
	return p
}

// Submit implements ports.Dispatcher. It never blocks the caller.
func (p *Pool) Submit(job func(context.Context)) error {
	if p.closing.Load() {
		return ErrClosed
	}
	select {
	case p.queue <- job:
		p.accepted.Add(1)
		return nil
	default:
		p.rejected.Add(1)
		return ErrQueueFull
	}
}

func (p *Pool) loop(id int) {
	defer p.wg.Done()
	for {
		select {
		case <-p.closed:
			// Drain whatever is already queued before exiting.
			for {
				select {
				case job := <-p.queue:
					p.run(job)
				default:
					return
				}
			}
		case job := <-p.queue:
			p.run(job)
		}
	}
}

func (p *Pool) run(job func(context.Context)) {
	defer func() {
		if r := recover(); r != nil {
			p.failed.Add(1)
			p.log.Error("job panicked", "panic", r)
		}
	}()

	ctx := context.Background()
	if p.jobTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.jobTimeout)
		defer cancel()
	}
	job(ctx)
	p.done.Add(1)
}

// Shutdown stops accepting work and waits for the queue to drain.
func (p *Pool) Shutdown(ctx context.Context) error {
	p.stopOnce.Do(func() {
		p.closing.Store(true)
		close(p.closed)
	})

	finished := make(chan struct{})
	go func() { p.wg.Wait(); close(finished) }()

	select {
	case <-finished:
		p.log.Info("worker pool drained", "accepted", p.accepted.Load(), "completed", p.done.Load(), "rejected", p.rejected.Load(), "panics", p.failed.Load())
		return nil
	case <-ctx.Done():
		p.log.Warn("worker pool shutdown timed out", "pending", len(p.queue))
		return ctx.Err()
	}
}

// Stats reports counters for the health endpoint.
func (p *Pool) Stats() map[string]int64 {
	return map[string]int64{
		"accepted":  p.accepted.Load(),
		"completed": p.done.Load(),
		"rejected":  p.rejected.Load(),
		"panics":    p.failed.Load(),
		"queued":    int64(len(p.queue)),
	}
}
