package stages

import (
	"context"
	"time"

	"github.com/sanjaysharmaoff/watchtower/internal/domain"
	"github.com/sanjaysharmaoff/watchtower/internal/ports"
)

// AdmitOptions configures the cheap pre-filter that runs before anything
// expensive happens.
type AdmitOptions struct {
	DropResolved bool
	MinSeverity  domain.Severity
}

// Admit drops noise that is not worth a database write, let alone a token: fully
// resolved groups and anything below the severity floor.
func Admit(opts AdmitOptions) Stage {
	return mutator("admit", func(_ context.Context, inc *domain.Incident) error {
		if len(inc.Alerts) == 0 {
			return domain.Drop("no alerts in payload")
		}
		if opts.DropResolved {
			firing := inc.Alerts[:0]
			for _, a := range inc.Alerts {
				if !a.IsResolved() {
					firing = append(firing, a)
				}
			}
			inc.Alerts = firing
			if len(inc.Alerts) == 0 {
				return domain.Drop("all alerts resolved")
			}
			inc.Recompute()
		}
		if floor := opts.MinSeverity.Rank(); floor > 0 && inc.Severity.Rank() < floor {
			return domain.Drop("severity %s below floor %s", inc.Severity, opts.MinSeverity)
		}
		return nil
	})
}

// Debounce refuses an incident whose signature was already handled inside the
// window. The decision itself lives in a ports.Gate, so in-memory and Redis
// behave identically from here.
func Debounce(gate ports.Gate, window time.Duration, keyOf func(*domain.Incident) string) Stage {
	if keyOf == nil {
		keyOf = func(inc *domain.Incident) string { return inc.Signature }
	}
	return mutator("debounce", func(ctx context.Context, inc *domain.Incident) error {
		key := keyOf(inc)
		allowed, err := gate.Allow(ctx, key, window)
		if err != nil {
			// Fail open: a broken gate must never stop real incidents.
			inc.Note("debounce gate error, failing open: %v", err)
			return nil
		}
		if !allowed {
			return domain.Drop("duplicate signature %s within %s", key, window)
		}
		inc.SetMeta("debounce_key", key)
		return nil
	})
}

// Correlate groups the incident with recent related ones.
func Correlate(c ports.Correlator) Stage {
	return mutator("correlate", func(ctx context.Context, inc *domain.Incident) error {
		if err := c.Correlate(ctx, inc); err != nil {
			inc.Note("correlation skipped: %v", err)
		}
		return nil
	})
}
