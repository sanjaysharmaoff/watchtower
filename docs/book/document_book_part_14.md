# Part 14 — The Event Bus and Projectors

The third architectural idea. Ports decouple the application from technologies; the pipeline
decouples steps from each other; the bus decouples **producers from consumers**.

## The problem

The pipeline finishes triaging an incident. Now three things should happen: save it, email
it, record a metric. The obvious code:

```go
// inside the last stage
store.SaveIncident(ctx, inc)
msg, _ := renderer.RenderIncident(inc)
notifier.Notify(ctx, msg)
metrics.IncrementTriaged()
```

Which means the pipeline now holds a store, a renderer, a notifier and a metrics client, and
every new consumer edits the pipeline. The stage that is supposed to be about *triage logic*
becomes a dispatch table.

## The solution: announce, do not call

```go
// internal/bus/bus.go
type Bus interface {
    Publish(ctx context.Context, topic string, payload any)
}
```

The pipeline says *what happened*. Whoever cares subscribes.

```go
stages.Publish(a.broker, bus.TopicIncidentSanitized),
...
stages.Publish(a.broker, bus.TopicIncidentTriaged),
```

That is the pipeline's entire relationship with persistence and email: two announcements. It
does not import a store, a renderer, a mailer or a metrics library.

Note the interface is **publish-only**. Producers cannot subscribe, because they have no
business knowing who is listening. Subscription lives on a wider interface:

```go
type Broker interface {
    Bus
    Subscribe(topic string, h Handler)
}
```

Handing out the narrowest interface a collaborator needs is the same discipline as everywhere
else in this codebase.

## Topics

```go
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
```

They are named as **facts in the past tense**. That is deliberate: an event is something that
happened, not an instruction. `incident.triaged` is a fact; `send_email` would be a command,
and a command implies the publisher knows what should happen next — which is exactly the
coupling being avoided.

## The async broker

```go
type Async struct {
    log     ports.Logger
    clock   ports.Clock
    queue   chan Event
    workers int

    mu   sync.RWMutex
    subs map[string][]Handler
    ...
}
```

A buffered channel plus N goroutines fanning events out to handlers.

### Publishers never block

```go
func (b *Async) Publish(_ context.Context, topic string, payload any) {
    e := Event{Topic: topic, At: b.clock.Now(), Payload: payload}
    select {
    case b.queue <- e:
    default:
        b.log.Warn("event dropped, buffer full", "topic", topic)
    }
}
```

`select` with a `default` makes the send non-blocking. If the buffer is full the event is
**dropped with a warning**.

That is a real tradeoff and interviewers will poke at it, so have the reasoning ready:

> Blocking a publisher means blocking the pipeline, which means blocking a worker, which
> eventually means rejecting webhooks. I would rather lose an audit event than stall
> ingestion. The buffer is 512 by default and handlers take milliseconds, so it should never
> fill. If it did, the warning tells me — and if events were business-critical rather than
> side effects, the right answer would be a real queue with persistence, not a bigger
> channel.

Note that persistence is *not* purely a side effect, which is why the important write also
happens at the `sanitized` topic while the incident is still in flight, and the DLQ catches
failures. But the strict guarantee "every incident is definitely stored even if the buffer
overflowed" is not there. Say so if asked; do not pretend.

### Handler isolation

```go
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
```

Each handler runs in its own recover scope with its own timeout. A panicking email renderer
cannot prevent the persistence handler from running. Handlers are **independent by
construction**.

Also note the handler list is copied under `RLock` before dispatch, so a slow handler does not
hold the subscription lock:

```go
b.mu.RLock()
handlers := append([]Handler(nil), b.subs[e.Topic]...)
handlers = append(handlers, b.subs["*"]...)
b.mu.RUnlock()
```

### The wildcard

`"*"` receives everything. Used by the audit projector, which is the cheapest possible way to
see the whole system's behaviour during development:

```
level=DEBUG msg=event projector=audit topic=incident.received  at=23:30:54.171
level=DEBUG msg=event projector=audit topic=incident.sanitized at=23:30:54.197
```

### Graceful shutdown

```go
func (b *Async) Close(ctx context.Context) error {
    b.stop.Do(func() {
        drain := time.NewTimer(2 * time.Second)
        ...
        for { /* wait until the queue is empty, ctx is done, or the drain timer fires */ }
        close(b.done)
        b.wg.Wait()
    })
    return nil
}
```

It tries to drain in-flight events before stopping, bounded by both a timer and the context.
`sync.Once` makes `Close` idempotent — a shutdown path that can be triggered twice is a
common source of "close of closed channel" panics.

## Projectors

A **projector** is a bus subscriber. The name comes from event-sourcing, where projections
build read models from an event stream; here they perform side effects.

### Persist

```go
func (p *Persist) Register(broker bus.Broker) {
    for _, topic := range []string{bus.TopicIncidentSanitized, bus.TopicIncidentTriaged, bus.TopicIncidentFailed} {
        broker.Subscribe(topic, bus.HandlerFunc{Label: "persist:" + topic, Fn: p.handle})
    }
}

func (p *Persist) handle(ctx context.Context, e bus.Event) error {
    inc, ok := e.Payload.(*domain.Incident)
    if !ok { return nil }
    if err := p.store.SaveIncident(ctx, inc); err != nil {
        p.park(ctx, "persist:"+e.Topic, err.Error(), inc)
        return err
    }
    return nil
}
```

Three topics, one handler, and the reason is a durability decision:

- **`sanitized`** — write early. The expensive, failure-prone part (retrieval, models) has not
  happened yet. If it all falls over, the *evidence* is already safe.
- **`triaged`** — write again with the plan, verdict and confidence. The SQL is an upsert
  (`ON CONFLICT DO UPDATE`), so this is an update, not a duplicate-key error.
- **`failed`** — write the partial incident, with its trace explaining how far it got.

And on a write failure, `park()` puts it in the dead letter queue with the full JSON payload.
**Nothing is lost.**

`e.Payload.(*domain.Incident)` with `if !ok { return nil }` is the price of `any` payloads:
handlers ignore what they do not recognise. The alternative — a typed bus with generics — was
not worth the complexity for eight topics, but it is a fair criticism to acknowledge.

### Deliver

```go
func (d *Deliver) handle(ctx context.Context, e bus.Event) error {
    inc, ok := e.Payload.(*domain.Incident)
    if !ok { return nil }
    if inc.Confidence < d.minConfidence {
        d.log.Info("delivery suppressed by confidence floor",
            "incident", inc.ID, "confidence", inc.Confidence, "floor", d.minConfidence)
        return nil
    }
    msg, err := d.renderer.RenderIncident(inc)
    if err != nil { d.park(ctx, "render", err.Error(), inc); return err }
    if err := d.notifier.Notify(ctx, msg); err != nil { d.park(ctx, "notify", err.Error(), inc); return err }
    return nil
}
```

The confidence floor (`WT_NOTIFY_MIN_CONFIDENCE`, default 0 = send everything) is an important
product knob: a team that gets tired of low-confidence emails can raise it, and **the incident
is still stored**, so nothing is lost and the threshold can be lowered again after inspecting
what was suppressed. That combination — suppress the notification, keep the data — is the
right way to build a noise control.

### Audit

Ten lines, subscribes to `"*"`, logs at debug. Demonstrates that adding an observer costs
nothing and touches nothing else.

## What this design makes easy

Because the pipeline only announces:

- **A metrics projector** — subscribe to all topics, increment counters. No pipeline change.
- **A Slack projector alongside email** — subscribe to `incident.triaged` too. Both run,
  independently, and one failing does not affect the other.
- **A "learned runbooks" audit log** — subscribe to `runbook.learned`.
- **Replacing the whole transport with NATS or SQS** — one adapter implementing `Bus` and
  `Broker`. Producers and consumers do not change, because they only ever reference the
  interface.

## Interview answers to have ready

**"Why an event bus inside a single process? Isn't that overkill?"**
It is one file and about 150 lines, and it buys inversion of control at the outbound edge: the
pipeline neither imports nor knows about persistence, email or metrics. It also gives a
natural upgrade path — swapping the in-process broker for NATS is one adapter behind the same
two interfaces, and no producer or consumer changes.

**"What if a handler fails?"**
It is logged with the handler name, isolated by its own recover and timeout, and other
handlers still run. Handlers that perform durable work park failures in the dead letter queue
with the full payload, so the work can be replayed rather than lost.

**"Events are dropped when the buffer is full. Isn't that a bug?"**
It is a deliberate tradeoff: I will not let an audit event stall ingestion. It is logged as a
warning so it is visible. For truly critical delivery I would use a persistent queue rather
than an in-memory channel — the interface is already the seam that would allow that without
touching producers.

**"Why not just call the store directly from the pipeline? It's fewer moving parts."**
It is, and for a small system that would be defensible. I chose the bus because there are
three consumers of the same fact today and I expect more (metrics, chat, ticketing), and
because it let me put persistence and delivery on *different* topics — evidence is stored
mid-pipeline, before the risky part, while delivery happens only at the end. Wiring that
directly would have put ordering logic inside the stages.

---

Next: **Part 15 — The Registry, the Composition Root and the Resource Pool.**
