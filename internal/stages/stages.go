// Package stages contains the pipeline steps.
//
// Each constructor takes only ports, returns a pipeline.Stage, and never
// references another stage. The order lives in the composition root.
package stages

import (
	"context"

	"github.com/sanjaysharmaoff/watchtower/internal/domain"
	"github.com/sanjaysharmaoff/watchtower/internal/pipeline"
)

// Stage is the incident-shaped pipeline stage used throughout WatchTower.
type Stage = pipeline.Stage[*domain.Incident]

// Chain is a pipeline over incidents.
type Chain = pipeline.Pipeline[*domain.Incident]

// mutator wraps the common "read the envelope, enrich it in place" shape so each
// stage constructor stays a few lines long.
func mutator(name string, fn func(ctx context.Context, inc *domain.Incident) error) Stage {
	return pipeline.Of(name, func(ctx context.Context, inc *domain.Incident) (*domain.Incident, error) {
		if inc == nil {
			return nil, domain.Drop("nil incident")
		}
		if err := fn(ctx, inc); err != nil {
			return inc, err
		}
		return inc, nil
	})
}
