package services

import (
	"context"
	"errors"
	"fmt"

	"github.com/sanjaysharmaoff/watchtower/internal/ports"
)

// Maintenance keeps storage bounded: create partitions ahead of writes, drop the
// ones past retention.
type Maintenance struct {
	partitions ports.PartitionManager
	ahead      int
	retention  int
	log        ports.Logger
}

// NewMaintenance wires the service.
func NewMaintenance(p ports.PartitionManager, ahead, retention int, log ports.Logger) *Maintenance {
	return &Maintenance{partitions: p, ahead: ahead, retention: retention, log: log.With("service", "maintenance")}
}

// Run creates upcoming partitions and enforces retention. Both halves are
// attempted even if the first fails, since a missing partition and stale data are
// independent problems.
func (m *Maintenance) Run(ctx context.Context) error {
	var errs []error

	if err := m.partitions.EnsurePartitions(ctx, m.ahead); err != nil {
		errs = append(errs, fmt.Errorf("ensure partitions: %w", err))
	} else {
		m.log.Info("partitions ensured", "days_ahead", m.ahead)
	}

	if err := m.partitions.DropPartitionsOlderThan(ctx, m.retention); err != nil {
		errs = append(errs, fmt.Errorf("drop old partitions: %w", err))
	}

	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}
