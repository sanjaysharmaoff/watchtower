package gate

import (
	"context"
	"fmt"
	"time"

	"github.com/sanjaysharmaoff/watchtower/internal/platform/redisx"
	"github.com/sanjaysharmaoff/watchtower/internal/ports"
	"github.com/sanjaysharmaoff/watchtower/internal/wire"
)

func init() {
	wire.Gates.Register("redis", func(e *wire.Env) (ports.Gate, error) {
		client, err := redisx.Shared(e)
		if err != nil {
			return nil, err
		}
		return &Redis{client: client, prefix: "wt:gate:"}, nil
	})
}

// Redis is a cluster-wide gate using SET NX with a TTL, so several WatchTower
// replicas debounce the same alert storm exactly once.
type Redis struct {
	client redisx.Client
	prefix string
}

// Allow implements ports.Gate.
func (r *Redis) Allow(ctx context.Context, key string, window time.Duration) (bool, error) {
	if window <= 0 {
		return true, nil
	}
	ok, err := r.client.SetNX(ctx, r.prefix+key, []byte("1"), window)
	if err != nil {
		return false, fmt.Errorf("redis gate: %w", err)
	}
	return ok, nil
}
