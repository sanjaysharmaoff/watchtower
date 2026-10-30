package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/sanjaysharmaoff/watchtower/internal/bus"
	"github.com/sanjaysharmaoff/watchtower/internal/domain"
	"github.com/sanjaysharmaoff/watchtower/internal/ports"
)

// Feedback closes the learning loop: record what the SRE decided, and when they
// approved something, embed it so the next similar incident retrieves it.
type Feedback struct {
	incidents ports.IncidentStore
	summaries ports.SummaryStore
	vectors   ports.VectorStore
	embedder  ports.Embedder
	dlq       ports.DeadLetterQueue
	bus       bus.Bus
	ids       ports.IDGen
	clock     ports.Clock
	log       ports.Logger
}

// NewFeedback wires the service. The vector store and embedder are optional: with
// them absent, feedback is still recorded, just not learned from.
func NewFeedback(
	incidents ports.IncidentStore,
	summaries ports.SummaryStore,
	vectors ports.VectorStore,
	embedder ports.Embedder,
	dlq ports.DeadLetterQueue,
	b bus.Bus,
	ids ports.IDGen,
	clock ports.Clock,
	log ports.Logger,
) *Feedback {
	return &Feedback{
		incidents: incidents, summaries: summaries, vectors: vectors, embedder: embedder,
		dlq: dlq, bus: b, ids: ids, clock: clock, log: log.With("service", "feedback"),
	}
}

// Submit implements ports.FeedbackSink.
func (f *Feedback) Submit(ctx context.Context, fb domain.Feedback) error {
	if fb.IncidentID == "" {
		return fmt.Errorf("feedback requires an incident id")
	}
	if fb.Decision == "" {
		fb.Decision = domain.DecisionReject
	}
	if fb.ReceivedAt.IsZero() {
		fb.ReceivedAt = f.clock.Now()
	}

	summary := f.buildSummary(ctx, fb)

	if err := f.summaries.SaveSummary(ctx, summary); err != nil {
		// The decision is too valuable to drop: park it and keep going, because the
		// runbook can still be learned and the DLQ row can be replayed later.
		f.park(ctx, "save-summary", err.Error(), summary)
		f.log.Error("summary write failed, parked in DLQ", "incident", fb.IncidentID, "err", err.Error())
	}
	f.bus.Publish(ctx, bus.TopicFeedbackAccepted, summary)

	if !fb.Decision.Learnable() {
		f.log.Info("feedback recorded", "incident", fb.IncidentID, "decision", fb.Decision, "learned", false)
		return nil
	}
	if err := f.learn(ctx, summary); err != nil {
		f.log.Warn("runbook not learned", "incident", fb.IncidentID, "err", err.Error())
		return nil // recording succeeded; learning is best effort
	}
	f.log.Info("feedback recorded", "incident", fb.IncidentID, "decision", fb.Decision, "learned", true)
	return nil
}

// buildSummary merges what the reviewer sent with what the original incident knew,
// so a bare "approve" click still produces a complete record.
func (f *Feedback) buildSummary(ctx context.Context, fb domain.Feedback) domain.ApprovedSummary {
	summary := domain.ApprovedSummary{
		ID:         f.ids.New(),
		IncidentID: fb.IncidentID,
		Signature:  fb.Signature,
		Title:      fb.Title,
		RootCause:  fb.RootCause,
		Commands:   fb.Commands,
		Decision:   fb.Decision,
		Reviewer:   fb.Reviewer,
		Notes:      fb.Notes,
		CreatedAt:  fb.ReceivedAt,
	}

	inc, err := f.incidents.GetIncident(ctx, fb.IncidentID)
	if err != nil {
		if !errors.Is(err, domain.ErrNotFound) {
			f.log.Warn("incident lookup failed", "incident", fb.IncidentID, "err", err.Error())
		}
		if summary.Title == "" {
			summary.Title = "Incident " + fb.IncidentID
		}
		return summary
	}

	if summary.Title == "" {
		summary.Title = inc.Title()
	}
	if summary.Signature == "" {
		summary.Signature = inc.Signature
	}
	summary.Services = inc.Services
	summary.Severity = inc.Severity
	summary.Labels = inc.PrimaryAlert().Labels

	if inc.Plan != nil {
		if summary.RootCause == "" {
			summary.RootCause = inc.Plan.RootCause
		}
		if len(summary.Commands) == 0 {
			// Only the commands policy allowed through are eligible to be learned.
			summary.Commands = inc.Plan.Executable()
		}
	}
	return summary
}

// learn embeds the reviewed runbook and writes it to the vector corpus.
func (f *Feedback) learn(ctx context.Context, summary domain.ApprovedSummary) error {
	if f.embedder == nil || f.vectors == nil {
		return fmt.Errorf("retrieval corpus is not configured")
	}
	runbook := summary.AsRunbook()
	if len(runbook.Commands) == 0 && runbook.RootCause == "" {
		return fmt.Errorf("nothing worth embedding: no commands and no root cause")
	}

	vec, err := f.embedder.Embed(ctx, runbook.Text())
	if err != nil {
		return fmt.Errorf("embed runbook: %w", err)
	}
	runbook.Embedding = vec
	runbook.SourceText = runbook.Text()

	if err := f.vectors.Upsert(ctx, runbook); err != nil {
		f.park(ctx, "upsert-runbook", err.Error(), summary)
		return fmt.Errorf("upsert runbook: %w", err)
	}
	f.bus.Publish(ctx, bus.TopicRunbookLearned, runbook)
	return nil
}

func (f *Feedback) park(ctx context.Context, stage, reason string, summary domain.ApprovedSummary) {
	if f.dlq == nil {
		return
	}
	payload, _ := json.Marshal(summary)
	dl := domain.DeadLetter{
		ID:      f.ids.New(),
		Stage:   stage,
		Reason:  reason,
		RefID:   summary.IncidentID,
		Payload: payload,
		At:      f.clock.Now(),
	}
	if err := f.dlq.Park(ctx, dl); err != nil {
		f.log.Error("dead letter park failed", "incident", summary.IncidentID, "stage", stage, "err", err.Error())
	}
}
