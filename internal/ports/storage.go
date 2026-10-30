package ports

import (
	"context"
	"time"

	"github.com/sanjaysharmaoff/watchtower/internal/domain"
)

// IncidentStore persists the sanitized raw record (raw_anomalies).
type IncidentStore interface {
	SaveIncident(ctx context.Context, inc *domain.Incident) error
	GetIncident(ctx context.Context, id string) (*domain.Incident, error)
}

// SummaryStore persists reviewed outcomes (approved_summaries).
type SummaryStore interface {
	SaveSummary(ctx context.Context, s domain.ApprovedSummary) error
	ListSummaries(ctx context.Context, from, to time.Time) ([]domain.ApprovedSummary, error)
}

// DeadLetterQueue is the catch-all for write and processing failures.
type DeadLetterQueue interface {
	Park(ctx context.Context, dl domain.DeadLetter) error
}

// PartitionManager owns the time-partition lifecycle of the raw table. Drivers
// without partitions (the in-memory store) implement it as a trim, so the
// maintenance service never has to ask which storage it is talking to.
type PartitionManager interface {
	EnsurePartitions(ctx context.Context, daysAhead int) error
	DropPartitionsOlderThan(ctx context.Context, days int) error
}
