// Package cached decorates any embedder with a cache.
//
// This is composition instead of configuration: the OpenAI embedder knows nothing
// about caching, the cache knows nothing about embeddings, and the decorator
// knows nothing about either implementation.
package cached

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"math"
	"time"

	"github.com/sanjaysharmaoff/watchtower/internal/ports"
)

// Embedder wraps an embedder with a byte cache.
type Embedder struct {
	inner ports.Embedder
	cache ports.Cache
	ttl   time.Duration
	log   ports.Logger
}

// Wrap returns inner unchanged when there is nothing to cache with.
func Wrap(inner ports.Embedder, cache ports.Cache, ttl time.Duration, log ports.Logger) ports.Embedder {
	if inner == nil || cache == nil {
		return inner
	}
	return &Embedder{inner: inner, cache: cache, ttl: ttl, log: log.With("component", "embed-cache")}
}

// Name implements ports.Embedder.
func (e *Embedder) Name() string { return e.inner.Name() + "+cache" }

// Dimensions implements ports.Embedder.
func (e *Embedder) Dimensions() int { return e.inner.Dimensions() }

// Embed returns a cached vector when the exact text was embedded before.
func (e *Embedder) Embed(ctx context.Context, text string) ([]float32, error) {
	key := e.keyFor(text)

	if raw, ok, err := e.cache.Get(ctx, key); err == nil && ok {
		if vec, err := decode(raw); err == nil {
			e.log.Debug("embedding cache hit", "key", key)
			return vec, nil
		}
	} else if err != nil {
		e.log.Warn("embedding cache read failed", "err", err.Error())
	}

	vec, err := e.inner.Embed(ctx, text)
	if err != nil {
		return nil, err
	}
	if err := e.cache.Set(ctx, key, encode(vec), e.ttl); err != nil {
		e.log.Warn("embedding cache write failed", "err", err.Error())
	}
	return vec, nil
}

// keyFor namespaces by model name so switching models never serves stale vectors.
func (e *Embedder) keyFor(text string) string {
	sum := sha256.Sum256([]byte(e.inner.Name() + "\x00" + text))
	return "emb:" + hex.EncodeToString(sum[:16])
}

func encode(vec []float32) []byte {
	buf := make([]byte, 4*len(vec))
	for i, v := range vec {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(v))
	}
	return buf
}

func decode(raw []byte) ([]float32, error) {
	if len(raw)%4 != 0 || len(raw) == 0 {
		return nil, errBadPayload
	}
	vec := make([]float32, len(raw)/4)
	for i := range vec {
		vec[i] = math.Float32frombits(binary.LittleEndian.Uint32(raw[i*4:]))
	}
	return vec, nil
}

type cacheError string

func (e cacheError) Error() string { return string(e) }

const errBadPayload cacheError = "cached embedding payload is malformed"
