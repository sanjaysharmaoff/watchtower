// Package services holds the use cases: triage, feedback, digest, maintenance.
//
// A service owns orchestration and nothing else. It depends on ports and on the
// pipeline, never on a concrete adapter, which is why the same triage service runs
// against Postgres and OpenAI in production and against memory and stubs locally.
package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/sanjaysharmaoff/watchtower/internal/bus"
	"github.com/sanjaysharmaoff/watchtower/internal/domain"
	"github.com/sanjaysharmaoff/watchtower/internal/pipeline"
	"github.com/sanjaysharmaoff/watchtower/internal/ports"
)

// Triage accepts alerts and runs them through the incident pipeline off the
// request path.
type Triage struct {
	pipe  *pipeline.Pipeline[*domain.Incident]
	disp  ports.Dispatcher
	bus   bus.Bus
	dlq   ports.DeadLetterQueue
	ids   ports.IDGen
	clock ports.Clock
	log   ports.Logger
}

// NewTriage wires the service.
func NewTriage(
	pipe *pipeline.Pipeline[*domain.Incident],
	disp ports.Dispatcher,
	b bus.Bus,
	dlq ports.DeadLetterQueue,
	ids ports.IDGen,
	clock ports.Clock,
	log ports.Logger,
) *Triage {
	return &Triage{pipe: pipe, disp: disp, bus: b, dlq: dlq, ids: ids, clock: clock, log: log.With("service", "triage")}
}

// Submit implements ports.AlertSink. It returns as soon as the work is queued, so
// Alertmanager gets a fast 202 and never retries because of slow enrichment.
func (t *Triage) Submit(ctx context.Context, alerts []domain.Alert) (string, error) {
	if len(alerts) == 0 {
		return "", fmt.Errorf("no alerts to triage")
	}
	inc := domain.NewIncident(t.ids.New(), t.clock.Now(), alerts)
	t.bus.Publish(ctx, bus.TopicIncidentReceived, inc)

	err := t.disp.Submit(func(jobCtx context.Context) {
		t.Process(jobCtx, inc)
	})
	if err != nil {
		// Backpressure: park the payload so a saturated pipeline loses nothing.
		t.park(ctx, "dispatch", err.Error(), inc)
		return inc.ID, err
	}
	t.log.Info("incident queued", "incident", inc.ID, "alerts", len(inc.Alerts), "severity", inc.Severity)
	return inc.ID, nil
}

// Process runs the pipeline for one incident. Exported so a replay tool or a test
// can drive it directly without a queue.
func (t *Triage) Process(ctx context.Context, inc *domain.Incident) {
	out, err := t.pipe.Run(ctx, inc)
	switch {
	case err == nil:
		t.log.Info("incident triaged", "incident", out.ID, "confidence", out.Confidence, "attempts", out.Attempts)

	case domain.IsDrop(err):
		out.Note("dropped: %v", err)
		t.bus.Publish(ctx, bus.TopicIncidentDropped, out)
		t.log.Info("incident dropped", "incident", out.ID, "reason", err.Error())

	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		out.Note("timed out: %v", err)
		t.bus.Publish(ctx, bus.TopicIncidentFailed, out)
		t.park(ctx, "pipeline-timeout", err.Error(), out)
		t.log.Error("incident timed out", "incident", out.ID, "err", err.Error())

	default:
		out.Note("failed: %v", err)
		t.bus.Publish(ctx, bus.TopicIncidentFailed, out)
		t.park(ctx, "pipeline", err.Error(), out)
		t.log.Error("incident failed", "incident", out.ID, "err", err.Error())
	}
}

// park writes to the dead letter queue, which is the one place failures are
// allowed to end up.
func (t *Triage) park(ctx context.Context, stage, reason string, inc *domain.Incident) {
	if t.dlq == nil {
		return
	}
	payload, _ := json.Marshal(inc)
	dl := domain.DeadLetter{
		ID:      t.ids.New(),
		Stage:   stage,
		Reason:  reason,
		RefID:   inc.ID,
		Payload: payload,
		At:      t.clock.Now(),
	}
	if err := t.dlq.Park(ctx, dl); err != nil {
		t.log.Error("dead letter park failed", "incident", inc.ID, "stage", stage, "err", err.Error())
	}
}
