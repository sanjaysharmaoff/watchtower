// Package projectors are the bus subscribers.
//
// They are the reason the pipeline has no idea a database or a mail relay exists:
// the pipeline announces facts, projectors react to them.
package projectors

import (
	"context"
	"encoding/json"

	"github.com/sanjaysharmaoff/watchtower/internal/bus"
	"github.com/sanjaysharmaoff/watchtower/internal/domain"
	"github.com/sanjaysharmaoff/watchtower/internal/ports"
)

// Persist writes incidents to the raw store, parking failures in the DLQ.
type Persist struct {
	store ports.IncidentStore
	dlq   ports.DeadLetterQueue
	ids   ports.IDGen
	clock ports.Clock
	log   ports.Logger
}

// NewPersist wires the projector.
func NewPersist(store ports.IncidentStore, dlq ports.DeadLetterQueue, ids ports.IDGen, clock ports.Clock, log ports.Logger) *Persist {
	return &Persist{store: store, dlq: dlq, ids: ids, clock: clock, log: log.With("projector", "persist")}
}

// Register subscribes to the topics this projector cares about. Sanitized writes
// the record early so evidence survives even if the model stage fails; triaged and
// failed update it with the outcome.
func (p *Persist) Register(broker bus.Broker) {
	for _, topic := range []string{bus.TopicIncidentSanitized, bus.TopicIncidentTriaged, bus.TopicIncidentFailed} {
		broker.Subscribe(topic, bus.HandlerFunc{Label: "persist:" + topic, Fn: p.handle})
	}
}

func (p *Persist) handle(ctx context.Context, e bus.Event) error {
	inc, ok := e.Payload.(*domain.Incident)
	if !ok {
		return nil
	}
	if err := p.store.SaveIncident(ctx, inc); err != nil {
		p.park(ctx, "persist:"+e.Topic, err.Error(), inc)
		return err
	}
	p.log.Debug("incident persisted", "incident", inc.ID, "topic", e.Topic)
	return nil
}

func (p *Persist) park(ctx context.Context, stage, reason string, inc *domain.Incident) {
	if p.dlq == nil {
		return
	}
	payload, _ := json.Marshal(inc)
	if err := p.dlq.Park(ctx, domain.DeadLetter{
		ID:      p.ids.New(),
		Stage:   stage,
		Reason:  reason,
		RefID:   inc.ID,
		Payload: payload,
		At:      p.clock.Now(),
	}); err != nil {
		p.log.Error("dead letter park failed", "incident", inc.ID, "err", err.Error())
	}
}
