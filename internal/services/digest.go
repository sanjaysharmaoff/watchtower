package services

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/sanjaysharmaoff/watchtower/internal/adapters/cognition/prompt"
	"github.com/sanjaysharmaoff/watchtower/internal/bus"
	"github.com/sanjaysharmaoff/watchtower/internal/domain"
	"github.com/sanjaysharmaoff/watchtower/internal/ports"
)

// Digest builds and sends the daily pulse from reviewed incidents only, so the
// report reflects verified reality rather than model output.
type Digest struct {
	summaries ports.SummaryStore
	narrator  ports.Narrator
	renderer  ports.Renderer
	notifier  ports.Notifier
	bus       bus.Bus
	clock     ports.Clock
	window    time.Duration
	log       ports.Logger
}

// NewDigest wires the service.
func NewDigest(
	summaries ports.SummaryStore,
	narrator ports.Narrator,
	renderer ports.Renderer,
	notifier ports.Notifier,
	b bus.Bus,
	clock ports.Clock,
	window time.Duration,
	log ports.Logger,
) *Digest {
	if window <= 0 {
		window = 24 * time.Hour
	}
	return &Digest{
		summaries: summaries, narrator: narrator, renderer: renderer, notifier: notifier,
		bus: b, clock: clock, window: window, log: log.With("service", "digest"),
	}
}

// Run builds the digest for the trailing window and delivers it.
func (d *Digest) Run(ctx context.Context) error {
	to := d.clock.Now()
	from := to.Add(-d.window)

	entries, err := d.summaries.ListSummaries(ctx, from, to)
	if err != nil {
		return fmt.Errorf("list summaries: %w", err)
	}

	digest := build(from, to, entries)

	narrative, err := d.narrator.Narrate(ctx, prompt.Digest(digest))
	if err != nil {
		d.log.Warn("narrator failed, sending data-only digest", "err", err.Error())
		narrative = fallbackNarrative(digest)
	}
	digest.Narrative = narrative

	msg, err := d.renderer.RenderDigest(digest)
	if err != nil {
		return fmt.Errorf("render digest: %w", err)
	}
	if err := d.notifier.Notify(ctx, msg); err != nil {
		return fmt.Errorf("deliver digest: %w", err)
	}

	d.bus.Publish(ctx, bus.TopicDigestReady, digest)
	d.log.Info("daily pulse sent", "reviewed", digest.Total, "approved", digest.Approved, "window", d.window.String())
	return nil
}

// build aggregates the counters and the impacted service list.
func build(from, to time.Time, entries []domain.ApprovedSummary) domain.Digest {
	digest := domain.Digest{From: from, To: to, Total: len(entries), Entries: entries}

	seen := map[string]int{}
	for _, e := range entries {
		switch e.Decision {
		case domain.DecisionApprove:
			digest.Approved++
		case domain.DecisionEdit:
			digest.Edited++
		default:
			digest.Rejected++
		}
		for _, s := range e.Services {
			seen[s]++
		}
	}

	digest.Services = make([]string, 0, len(seen))
	for s := range seen {
		digest.Services = append(digest.Services, s)
	}
	// Most affected first, alphabetical as the tie-break.
	sort.Slice(digest.Services, func(i, j int) bool {
		a, b := digest.Services[i], digest.Services[j]
		if seen[a] != seen[b] {
			return seen[a] > seen[b]
		}
		return a < b
	})
	return digest
}

func fallbackNarrative(d domain.Digest) string {
	if d.Total == 0 {
		return "No incidents were reviewed in this window."
	}
	return fmt.Sprintf("%d incident(s) reviewed: %d approved, %d edited, %d rejected. Most affected: %v.",
		d.Total, d.Approved, d.Edited, d.Rejected, d.Services)
}
