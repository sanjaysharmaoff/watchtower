package projectors

import (
	"context"
	"encoding/json"

	"github.com/sanjaysharmaoff/watchtower/internal/bus"
	"github.com/sanjaysharmaoff/watchtower/internal/domain"
	"github.com/sanjaysharmaoff/watchtower/internal/ports"
)

// Deliver renders and sends the interactive triage email.
type Deliver struct {
	renderer      ports.Renderer
	notifier      ports.Notifier
	dlq           ports.DeadLetterQueue
	ids           ports.IDGen
	clock         ports.Clock
	minConfidence float64
	log           ports.Logger
}

// NewDeliver wires the projector.
func NewDeliver(
	renderer ports.Renderer,
	notifier ports.Notifier,
	dlq ports.DeadLetterQueue,
	ids ports.IDGen,
	clock ports.Clock,
	minConfidence float64,
	log ports.Logger,
) *Deliver {
	return &Deliver{
		renderer: renderer, notifier: notifier, dlq: dlq, ids: ids, clock: clock,
		minConfidence: minConfidence, log: log.With("projector", "deliver"),
	}
}

// Register subscribes to triaged incidents.
func (d *Deliver) Register(broker bus.Broker) {
	broker.Subscribe(bus.TopicIncidentTriaged, bus.HandlerFunc{Label: "deliver", Fn: d.handle})
}

func (d *Deliver) handle(ctx context.Context, e bus.Event) error {
	inc, ok := e.Payload.(*domain.Incident)
	if !ok {
		return nil
	}
	if inc.Confidence < d.minConfidence {
		// Below the floor the email is suppressed, but the incident is already
		// stored, so nothing is lost and the threshold can be lowered later.
		d.log.Info("delivery suppressed by confidence floor",
			"incident", inc.ID, "confidence", inc.Confidence, "floor", d.minConfidence)
		return nil
	}

	msg, err := d.renderer.RenderIncident(inc)
	if err != nil {
		d.park(ctx, "render", err.Error(), inc)
		return err
	}
	if err := d.notifier.Notify(ctx, msg); err != nil {
		d.park(ctx, "notify", err.Error(), inc)
		return err
	}
	d.log.Info("incident delivered", "incident", inc.ID, "via", d.notifier.Name())
	return nil
}

func (d *Deliver) park(ctx context.Context, stage, reason string, inc *domain.Incident) {
	if d.dlq == nil {
		return
	}
	payload, _ := json.Marshal(inc)
	if err := d.dlq.Park(ctx, domain.DeadLetter{
		ID:      d.ids.New(),
		Stage:   "deliver:" + stage,
		Reason:  reason,
		RefID:   inc.ID,
		Payload: payload,
		At:      d.clock.Now(),
	}); err != nil {
		d.log.Error("dead letter park failed", "incident", inc.ID, "err", err.Error())
	}
}

// Audit logs every event that crosses the bus at debug level, which is the cheapest
// possible way to see the whole system's behaviour during development.
type Audit struct{ log ports.Logger }

// NewAudit wires the projector.
func NewAudit(log ports.Logger) *Audit { return &Audit{log: log.With("projector", "audit")} }

// Register subscribes to the wildcard topic.
func (a *Audit) Register(broker bus.Broker) {
	broker.Subscribe("*", bus.HandlerFunc{Label: "audit", Fn: func(_ context.Context, e bus.Event) error {
		a.log.Debug("event", "topic", e.Topic, "at", e.At.Format("15:04:05.000"))
		return nil
	}})
}
