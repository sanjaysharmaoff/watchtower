// Package score turns everything the pipeline learned into a single number the
// reviewer can trust.
package score

import (
	"github.com/sanjaysharmaoff/watchtower/internal/domain"
	"github.com/sanjaysharmaoff/watchtower/internal/ports"
	"github.com/sanjaysharmaoff/watchtower/internal/wire"
)

func init() {
	wire.Scorers.Register("weighted", func(e *wire.Env) (ports.Scorer, error) {
		return Weighted{}, nil
	})
}

// Weighted is an explainable, dependency-free confidence model. Evidence and
// review outcomes move the number; the model's own self-assessment is capped so a
// confident hallucination cannot dominate.
type Weighted struct{}

// Score implements ports.Scorer.
func (Weighted) Score(inc *domain.Incident) float64 {
	if inc == nil || inc.Plan == nil {
		return 0
	}

	score := 0.15 // a parsed, schema-valid plan is worth something on its own

	// Retrieval support: a verified historical runbook is the strongest signal.
	if m, ok := inc.TopMatch(); ok {
		score += 0.3 * clamp01(m.Score)
		if m.Runbook.Signature != "" && m.Runbook.Signature == inc.Signature {
			score += 0.1 // same failure signature we have fixed before
		}
	}

	// Evidence depth.
	switch {
	case len(inc.Logs) >= 20:
		score += 0.15
	case len(inc.Logs) >= 5:
		score += 0.1
	case len(inc.Logs) > 0:
		score += 0.05
	}
	if len(inc.Events) > 0 {
		score += 0.05
	}

	// Critic outcome.
	if inc.Verdict != nil {
		if inc.Verdict.Approved {
			score += 0.15
			if inc.Attempts == 1 {
				score += 0.05 // approved without needing a rewrite
			}
		} else {
			score -= 0.2
		}
	}

	// Plan shape.
	if len(inc.Plan.Verification) > 0 {
		score += 0.05
	}
	if len(inc.Plan.Rollback) > 0 {
		score += 0.05
	}
	if len(inc.Plan.Executable()) == 0 {
		score -= 0.25 // diagnosis only, nothing actionable survived
	}
	if blocked := len(inc.Plan.Blocked()); blocked > 0 {
		score -= 0.1 * float64(blocked)
	}
	if inc.Plan.RootCause == "" {
		score -= 0.15
	}

	// The model's own claim, weighted lightly.
	score += 0.1 * clamp01(inc.Plan.Confidence)

	return round2(clamp01(score))
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

func round2(f float64) float64 {
	return float64(int(f*100+0.5)) / 100
}
