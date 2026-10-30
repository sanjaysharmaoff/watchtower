// Package rerank narrows retrieval candidates before they reach a prompt.
package rerank

import (
	"context"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/sanjaysharmaoff/watchtower/internal/domain"
	"github.com/sanjaysharmaoff/watchtower/internal/ports"
	"github.com/sanjaysharmaoff/watchtower/internal/wire"
)

func init() {
	wire.Rerankers.Register("heuristic", func(e *wire.Env) (ports.Reranker, error) {
		return Heuristic{Clock: e.Clock}, nil
	})
	wire.Rerankers.Register("none", func(e *wire.Env) (ports.Reranker, error) {
		return Passthrough{}, nil
	})
}

var reToken = regexp.MustCompile(`[a-z0-9_\-./]+`)

// Heuristic reranks with lexical overlap, signature identity and recency on top of
// the vector score. It costs nothing per call, which matters when the alternative
// is a model invocation on every incident.
type Heuristic struct {
	Clock ports.Clock
}

// Name implements ports.Reranker.
func (Heuristic) Name() string { return "heuristic" }

// Rerank implements ports.Reranker.
func (h Heuristic) Rerank(_ context.Context, query string, matches []domain.RunbookMatch, top int) ([]domain.RunbookMatch, error) {
	if len(matches) == 0 {
		return matches, nil
	}
	if top <= 0 {
		top = 2
	}
	qTokens := tokenSet(query)
	now := time.Now().UTC()
	if h.Clock != nil {
		now = h.Clock.Now()
	}

	scored := make([]domain.RunbookMatch, 0, len(matches))
	for _, m := range matches {
		overlap := jaccard(qTokens, tokenSet(m.Runbook.Text()))
		score := 0.6*m.Score + 0.4*overlap

		reasons := make([]string, 0, 3)
		if strings.Contains(strings.ToLower(query), strings.ToLower(firstWords(m.Runbook.Title, 3))) {
			score += 0.05
			reasons = append(reasons, "title appears in query")
		}
		if age := now.Sub(m.Runbook.CreatedAt); !m.Runbook.CreatedAt.IsZero() && age < 30*24*time.Hour {
			score += 0.05
			reasons = append(reasons, "verified in the last 30 days")
		}
		if len(m.Runbook.Commands) == 0 {
			score -= 0.2
			reasons = append(reasons, "no commands recorded")
		}

		m.Score = clamp01(score)
		m.Stage = "rerank:heuristic"
		m.Reason = strings.Join(append([]string{"lexical overlap " + pct(overlap)}, reasons...), "; ")
		scored = append(scored, m)
	}

	sort.SliceStable(scored, func(i, j int) bool { return scored[i].Score > scored[j].Score })
	if len(scored) > top {
		scored = scored[:top]
	}
	return scored, nil
}

// Passthrough keeps the vector order and only truncates.
type Passthrough struct{}

// Name implements ports.Reranker.
func (Passthrough) Name() string { return "none" }

// Rerank implements ports.Reranker.
func (Passthrough) Rerank(_ context.Context, _ string, matches []domain.RunbookMatch, top int) ([]domain.RunbookMatch, error) {
	if top > 0 && len(matches) > top {
		return matches[:top], nil
	}
	return matches, nil
}

func tokenSet(s string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, t := range reToken.FindAllString(strings.ToLower(s), -1) {
		if len(t) < 3 {
			continue
		}
		out[t] = struct{}{}
	}
	return out
}

func jaccard(a, b map[string]struct{}) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	inter := 0
	for t := range a {
		if _, ok := b[t]; ok {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

func firstWords(s string, n int) string {
	parts := strings.Fields(s)
	if len(parts) > n {
		parts = parts[:n]
	}
	return strings.Join(parts, " ")
}

func clamp01(f float64) float64 {
	switch {
	case f < 0:
		return 0
	case f > 1:
		return 1
	default:
		return f
	}
}

func pct(f float64) string {
	n := int(f*100 + 0.5)
	digits := ""
	if n == 0 {
		digits = "0"
	}
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}
	return digits + "%"
}
