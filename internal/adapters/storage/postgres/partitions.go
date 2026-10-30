package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sanjaysharmaoff/watchtower/internal/ports"
	"github.com/sanjaysharmaoff/watchtower/internal/wire"
)

func init() {
	wire.PartitionManagers.Register("postgres", func(e *wire.Env) (ports.PartitionManager, error) {
		pool, err := Pool(e)
		if err != nil {
			return nil, err
		}
		return &Partitions{pool: pool, clock: e.Clock, log: e.Log.With("component", "partitions")}, nil
	})
}

// Partitions owns the daily partition lifecycle of raw_anomalies: create ahead so
// writes never fail, drop behind so storage stays flat.
type Partitions struct {
	pool  *pgxpool.Pool
	clock ports.Clock
	log   ports.Logger
}

// EnsurePartitions creates today's partition plus daysAhead future ones.
func (p *Partitions) EnsurePartitions(ctx context.Context, daysAhead int) error {
	if daysAhead < 0 {
		daysAhead = 0
	}
	today := p.clock.Now().UTC().Truncate(24 * time.Hour)

	// Yesterday is included so late-arriving alerts with an older timestamp still
	// land somewhere.
	for offset := -1; offset <= daysAhead; offset++ {
		day := today.AddDate(0, 0, offset)
		if err := p.createFor(ctx, day); err != nil {
			return err
		}
	}
	return nil
}

func (p *Partitions) createFor(ctx context.Context, day time.Time) error {
	name := partitionName(day)
	stmt := fmt.Sprintf(
		`CREATE TABLE IF NOT EXISTS %s PARTITION OF raw_anomalies FOR VALUES FROM ('%s') TO ('%s')`,
		name,
		day.Format("2006-01-02 15:04:05-07"),
		day.AddDate(0, 0, 1).Format("2006-01-02 15:04:05-07"),
	)
	if _, err := p.pool.Exec(ctx, stmt); err != nil {
		return fmt.Errorf("create partition %s: %w", name, err)
	}
	return nil
}

// DropPartitionsOlderThan removes partitions whose whole range is past retention.
func (p *Partitions) DropPartitionsOlderThan(ctx context.Context, days int) error {
	if days <= 0 {
		return nil
	}
	cutoff := p.clock.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -days)

	const q = `
		SELECT c.relname
		FROM pg_class c
		JOIN pg_inherits i ON i.inhrelid = c.oid
		JOIN pg_class parent ON parent.oid = i.inhparent
		WHERE parent.relname = 'raw_anomalies'`

	rows, err := p.pool.Query(ctx, q)
	if err != nil {
		return fmt.Errorf("list partitions: %w", err)
	}
	names := make([]string, 0, 32)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		names = append(names, name)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	dropped := 0
	for _, name := range names {
		day, ok := dayFromPartition(name)
		if !ok || !day.Before(cutoff) {
			continue
		}
		if _, err := p.pool.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s`, name)); err != nil {
			p.log.Warn("drop partition failed", "partition", name, "err", err.Error())
			continue
		}
		dropped++
		p.log.Info("partition dropped", "partition", name)
	}
	if dropped > 0 {
		p.log.Info("retention enforced", "dropped", dropped, "cutoff", cutoff.Format(time.DateOnly))
	}
	return nil
}

func partitionName(day time.Time) string {
	return "raw_anomalies_" + day.Format("20060102")
}

func dayFromPartition(name string) (time.Time, bool) {
	const prefix = "raw_anomalies_"
	if len(name) != len(prefix)+8 || name[:len(prefix)] != prefix {
		return time.Time{}, false
	}
	day, err := time.Parse("20060102", name[len(prefix):])
	if err != nil {
		return time.Time{}, false
	}
	return day.UTC(), true
}
