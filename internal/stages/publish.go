package stages

import (
	"context"

	"github.com/sanjaysharmaoff/watchtower/internal/bus"
	"github.com/sanjaysharmaoff/watchtower/internal/domain"
	"github.com/sanjaysharmaoff/watchtower/internal/pipeline"
)

// Publish emits the envelope on a topic and moves on. This is how the pipeline
// triggers persistence and delivery without importing a store or a mailer: the
// projectors subscribe, the pipeline just announces.
func Publish(b bus.Bus, topic string) Stage {
	return pipeline.Of("publish:"+topic, func(ctx context.Context, inc *domain.Incident) (*domain.Incident, error) {
		b.Publish(ctx, topic, inc)
		return inc, nil
	})
}

// Tap runs a side effect inline, for the rare case where ordering matters more
// than decoupling.
func Tap(name string, fn func(ctx context.Context, inc *domain.Incident)) Stage {
	return pipeline.Of("tap:"+name, func(ctx context.Context, inc *domain.Incident) (*domain.Incident, error) {
		fn(ctx, inc)
		return inc, nil
	})
}
