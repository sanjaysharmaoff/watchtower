package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/sanjaysharmaoff/watchtower/internal/domain"
	"github.com/sanjaysharmaoff/watchtower/internal/ports"
	"github.com/sanjaysharmaoff/watchtower/internal/wire"
)

func init() {
	wire.Rerankers.Register("llm", func(e *wire.Env) (ports.Reranker, error) {
		c, err := Shared(e)
		if err != nil {
			return nil, err
		}
		return Reranker{c: c, model: e.Cfg.LLM.CriticModel}, nil
	})
}

const rerankSystem = `You score how useful each historical runbook is for resolving the described incident.

Judge on: does the runbook address the same failure mode, and would its commands plausibly apply to this target?
Score 0.0 (irrelevant) to 1.0 (same failure mode, directly applicable).
Reply with JSON only: {"scores":[{"index":1,"score":0.0,"reason":"..."}]}`

// Reranker is a model-scored reranker, the expensive but accurate option. It
// stands in for a dedicated cross-encoder and satisfies the same port, so it can
// be swapped in per environment.
type Reranker struct {
	c     *Client
	model string
}

// Name implements ports.Reranker.
func (r Reranker) Name() string { return "llm:" + r.model }

// Rerank implements ports.Reranker.
func (r Reranker) Rerank(ctx context.Context, query string, matches []domain.RunbookMatch, top int) ([]domain.RunbookMatch, error) {
	if len(matches) == 0 {
		return matches, nil
	}
	if top <= 0 {
		top = 2
	}

	var u strings.Builder
	u.WriteString("## Incident\n")
	u.WriteString(query)
	u.WriteString("\n\n## Candidate runbooks\n")
	for i, m := range matches {
		u.WriteString(fmt.Sprintf("### %d. %s\n%s\n", i+1, m.Runbook.Title, truncate(m.Runbook.Text(), 1200)))
	}

	raw, err := r.c.Chat(ctx, ChatOptions{Model: r.model, System: rerankSystem, User: u.String(), JSONOnly: true, MaxTokens: 600})
	if err != nil {
		return nil, err
	}
	body := extractJSON(raw)
	if body == "" {
		return nil, fmt.Errorf("reranker returned no JSON object")
	}

	var dto struct {
		Scores []struct {
			Index  int     `json:"index"`
			Score  float64 `json:"score"`
			Reason string  `json:"reason"`
		} `json:"scores"`
	}
	if err := json.Unmarshal([]byte(body), &dto); err != nil {
		return nil, fmt.Errorf("decode rerank JSON: %w", err)
	}

	out := make([]domain.RunbookMatch, 0, len(matches))
	for _, s := range dto.Scores {
		idx := s.Index - 1
		if idx < 0 || idx >= len(matches) {
			continue
		}
		m := matches[idx]
		m.Score = clamp01(s.Score)
		m.Stage = "rerank:" + r.Name()
		m.Reason = strings.TrimSpace(s.Reason)
		out = append(out, m)
	}
	if len(out) == 0 {
		return matches[:min(top, len(matches))], nil
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if len(out) > top {
		out = out[:top]
	}
	return out, nil
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
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

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
