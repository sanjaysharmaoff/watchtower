// Package redisx owns the single shared Redis connection.
//
// The gate adapter and the cache adapter both need Redis but neither should
// create (or close) a client the other is using, so the handle lives in the Env
// resource pool behind a narrow interface.
package redisx

import (
	"context"
	"errors"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"github.com/sanjaysharmaoff/watchtower/internal/wire"
)

// Client is the slice of Redis WatchTower actually uses.
type Client interface {
	Get(ctx context.Context, key string) ([]byte, bool, error)
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
	SetNX(ctx context.Context, key string, value []byte, ttl time.Duration) (bool, error)
	Ping(ctx context.Context) error
	Close() error
}

type client struct{ rdb *goredis.Client }

// Shared returns the process-wide Redis client, creating it on first use.
func Shared(e *wire.Env) (Client, error) {
	return wire.Resource(e, "redis.client", func() (Client, error) {
		rdb := goredis.NewClient(&goredis.Options{
			Addr:         e.Cfg.RAG.RedisAddr,
			Password:     e.Cfg.RAG.RedisPassword,
			DB:           e.Cfg.RAG.RedisDB,
			DialTimeout:  5 * time.Second,
			ReadTimeout:  3 * time.Second,
			WriteTimeout: 3 * time.Second,
		})
		c := &client{rdb: rdb}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := c.Ping(ctx); err != nil {
			_ = rdb.Close()
			return nil, err
		}
		e.OnClose(c.Close)
		e.Log.Info("redis connected", "addr", e.Cfg.RAG.RedisAddr, "db", e.Cfg.RAG.RedisDB)
		return c, nil
	})
}

func (c *client) Get(ctx context.Context, key string) ([]byte, bool, error) {
	val, err := c.rdb.Get(ctx, key).Bytes()
	if errors.Is(err, goredis.Nil) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return val, true, nil
}

func (c *client) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	return c.rdb.Set(ctx, key, value, ttl).Err()
}

func (c *client) SetNX(ctx context.Context, key string, value []byte, ttl time.Duration) (bool, error) {
	return c.rdb.SetNX(ctx, key, value, ttl).Result()
}

func (c *client) Ping(ctx context.Context) error { return c.rdb.Ping(ctx).Err() }

func (c *client) Close() error { return c.rdb.Close() }
