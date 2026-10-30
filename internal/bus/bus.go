// Package bus is an in-process publish/subscribe hop.
//
// It exists so producers (the pipeline) and consumers (persistence, delivery,
// audit) never reference each other. Swapping this for NATS or SQS later means
// writing one adapter against Bus.
package bus

import (
	"context"
	"sync"
	"time"

	"github.com/sanjaysharmaoff/watchtower/internal/ports"
)

// Topics published by WatchTower.
const (
	TopicIncidentReceived  = "incident.received"
	TopicIncidentSanitized = "incident.sanitized"
	TopicIncidentTriaged   = "incident.triaged"
	TopicIncidentDropped   = "incident.dropped"
	TopicIncidentFailed    = "incident.failed"
	TopicFeedbackAccepted  = "feedback.accepted"
	TopicRunbookLearned    = "runbook.learned"
	TopicDigestReady       = "digest.ready"
)

// Event is a topic plus an opaque payload. Handlers type-assert what they care
// about and ignore the rest.
type Event struct {
	Topic   string
	At      time.Time
	Payload any
}

// Handler consumes events.
type Handler interface {
	Name() string
	Handle(ctx context.Context, e Event) error
}

// HandlerFunc adapts a function to Handler.
type HandlerFunc struct {
	Label string
	Fn    func(ctx context.Context, e Event) error
}

// Name implements Handler.
func (h HandlerFunc) Name() string { return h.Label }

// Handle implements Handler.
func (h HandlerFunc) Handle(ctx context.Context, e Event) error { return h.Fn(ctx, e) }

// Bus is the publish side, which is all producers ever need.
type Bus interface {
	Publish(ctx context.Context, topic string, payload any)
}

// Broker is a bus that also accepts subscriptions.
type Broker interface {
	Bus
	Subscribe(topic string, h Handler)
}

// Async is a buffered, worker-backed broker. Publishers never block: if the
// buffer is full the event is dropped with a warning, because dropping an audit
// event is better than stalling ingestion.
type Async struct {
	log     ports.Logger
	clock   ports.Clock
	queue   chan Event
	workers int

	mu   sync.RWMutex
	subs map[string][]Handler

	start sync.Once
	stop  sync.Once
	done  chan struct{}
	wg    sync.WaitGroup
}

// NewAsync builds an async broker.
func NewAsync(log ports.Logger, clock ports.Clock, buffer, workers int) *Async {
	if buffer <= 0 {
		buffer = 256
	}
	if workers <= 0 {
		workers = 2
	}
	return &Async{
		log:     log.With("component", "bus"),
		clock:   clock,
		queue:   make(chan Event, buffer),
		workers: workers,
		subs:    map[string][]Handler{},
		done:    make(chan struct{}),
	}
}

// Subscribe registers a handler for a topic. Subscribe before Start.
func (b *Async) Subscribe(topic string, h Handler) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subs[topic] = append(b.subs[topic], h)
}

// Publish enqueues an event for asynchronous fan-out.
func (b *Async) Publish(_ context.Context, topic string, payload any) {
	e := Event{Topic: topic, At: b.clock.Now(), Payload: payload}
	select {
	case b.queue <- e:
	default:
		b.log.Warn("event dropped, buffer full", "topic", topic)
	}
}

// Start spins up the fan-out workers.
func (b *Async) Start() {
	b.start.Do(func() {
		for i := 0; i < b.workers; i++ {
			b.wg.Add(1)
			go b.loop()
		}
	})
}

func (b *Async) loop() {
	defer b.wg.Done()
	for {
		select {
		case <-b.done:
			return
		case e := <-b.queue:
			b.dispatch(e)
		}
	}
}

func (b *Async) dispatch(e Event) {
	b.mu.RLock()
	handlers := append([]Handler(nil), b.subs[e.Topic]...)
	handlers = append(handlers, b.subs["*"]...)
	b.mu.RUnlock()

	for _, h := range handlers {
		func(h Handler) {
			defer func() {
				if r := recover(); r != nil {
					b.log.Error("handler panic", "topic", e.Topic, "handler", h.Name(), "panic", r)
				}
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			if err := h.Handle(ctx, e); err != nil {
				b.log.Error("handler failed", "topic", e.Topic, "handler", h.Name(), "err", err.Error())
			}
		}(h)
	}
}

// Close drains what it can and stops the workers.
func (b *Async) Close(ctx context.Context) error {
	b.stop.Do(func() {
		drain := time.NewTimer(2 * time.Second)
		defer drain.Stop()
	loop:
		for {
			select {
			case <-ctx.Done():
				break loop
			case <-drain.C:
				break loop
			default:
				if len(b.queue) == 0 {
					break loop
				}
				time.Sleep(10 * time.Millisecond)
			}
		}
		close(b.done)
		b.wg.Wait()
	})
	return nil
}

// Discard is a no-op bus, useful for tests and for disabling side effects.
type Discard struct{}

// Publish implements Bus.
func (Discard) Publish(context.Context, string, any) {}
