// Package hashing is a deterministic, offline embedder.
//
// It hashes token n-grams into a fixed-width vector (the "hashing trick"). Not
// semantic, but stable, free and good enough for lexical similarity, which makes
// local development and tests behave like the real thing.
package hashing

import (
	"context"
	"hash/fnv"
	"math"
	"regexp"
	"strings"

	"github.com/sanjaysharmaoff/watchtower/internal/ports"
	"github.com/sanjaysharmaoff/watchtower/internal/wire"
)

func init() {
	wire.Embedders.Register("stub", func(e *wire.Env) (ports.Embedder, error) {
		return New(256), nil
	})
}

var reToken = regexp.MustCompile(`[a-z0-9_.\-/]+`)

// Embedder implements ports.Embedder without leaving the process.
type Embedder struct{ dims int }

// New builds a hashing embedder with the given width.
func New(dims int) Embedder {
	if dims <= 0 {
		dims = 256
	}
	return Embedder{dims: dims}
}

// Name implements ports.Embedder.
func (Embedder) Name() string { return "hashing" }

// Dimensions implements ports.Embedder.
func (e Embedder) Dimensions() int { return e.dims }

// Embed implements ports.Embedder.
func (e Embedder) Embed(_ context.Context, text string) ([]float32, error) {
	vec := make([]float32, e.dims)
	tokens := reToken.FindAllString(strings.ToLower(text), -1)

	add := func(term string, weight float32) {
		h := fnv.New32a()
		_, _ = h.Write([]byte(term))
		idx := int(h.Sum32()) % e.dims
		if idx < 0 {
			idx += e.dims
		}
		vec[idx] += weight
	}

	for i, t := range tokens {
		if len(t) < 2 || stopWords[t] {
			continue
		}
		add(t, 1)
		if i > 0 {
			add(tokens[i-1]+" "+t, 0.5) // bigram, so word order carries a little weight
		}
	}

	// L2 normalize so cosine similarity is a plain dot product.
	var norm float64
	for _, v := range vec {
		norm += float64(v) * float64(v)
	}
	if norm == 0 {
		return vec, nil
	}
	inv := float32(1 / math.Sqrt(norm))
	for i := range vec {
		vec[i] *= inv
	}
	return vec, nil
}

var stopWords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "that": true, "this": true,
	"from": true, "have": true, "has": true, "was": true, "are": true, "not": true,
	"but": true, "you": true, "all": true, "can": true, "will": true, "its": true,
}
