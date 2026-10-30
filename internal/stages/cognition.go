package stages

import (
	"context"
	"fmt"
	"strings"

	"github.com/sanjaysharmaoff/watchtower/internal/domain"
	"github.com/sanjaysharmaoff/watchtower/internal/ports"
)

// BuildPrompt renders the model instruction from the envelope.
func BuildPrompt(b ports.PromptBuilder) Stage {
	return mutator("prompt", func(_ context.Context, inc *domain.Incident) error {
		p, err := b.Build(inc)
		if err != nil {
			return fmt.Errorf("build prompt: %w", err)
		}
		inc.Prompt = p
		return nil
	})
}

// PlanOptions configures the planner/critic loop.
type PlanOptions struct {
	MaxAttempts int
	CriticOn    bool
}

// Plan drafts a remediation and, when a critic is wired in, loops: draft,
// critique, redraft with the critique appended, up to MaxAttempts. The loop lives
// here rather than inside a model adapter, so any planner and any critic can be
// paired, including a real planner with a stub critic.
func Plan(planner ports.Planner, critic ports.Critic, opts PlanOptions) Stage {
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = 1
	}
	return mutator("plan", func(ctx context.Context, inc *domain.Incident) error {
		prompt := inc.Prompt
		var lastErr error

		for attempt := 1; attempt <= opts.MaxAttempts; attempt++ {
			inc.Attempts = attempt
			plan, err := planner.Plan(ctx, prompt)
			if err != nil {
				lastErr = err
				inc.Note("planner attempt %d failed: %v", attempt, err)
				continue
			}
			plan.Model = planner.Name()
			inc.Plan = &plan
			lastErr = nil

			if !opts.CriticOn || critic == nil {
				return nil
			}
			verdict, err := critic.Review(ctx, prompt, plan)
			if err != nil {
				inc.Note("critic unavailable on attempt %d, accepting draft: %v", attempt, err)
				return nil
			}
			verdict.Model = critic.Name()
			inc.Verdict = &verdict
			if verdict.Approved {
				inc.Note("critic approved draft on attempt %d", attempt)
				return nil
			}
			if len(verdict.Issues) == 0 && strings.TrimSpace(verdict.Notes) == "" {
				// A rejection with no stated objection gives the next attempt nothing to
				// correct, so retrying is pure token spend. Keep the draft, flag it, and
				// let the confidence score carry the doubt to the reviewer.
				verdict.Issues = []string{"reviewer rejected the plan without giving a reason, so it was not rewritten"}
				inc.Verdict = &verdict
				inc.Note("critic rejected draft %d without reasons, keeping draft", attempt)
				return nil
			}
			inc.Note("critic rejected draft %d: %s", attempt, strings.Join(verdict.Issues, "; "))
			if attempt == opts.MaxAttempts {
				return nil
			}
			prompt = withCritique(inc.Prompt, plan, verdict)
		}

		if lastErr != nil {
			return fmt.Errorf("planner exhausted %d attempt(s): %w", opts.MaxAttempts, lastErr)
		}
		return nil
	})
}

// withCritique appends the rejected draft and the critic's objections so the next
// attempt is a correction rather than a fresh guess.
func withCritique(base domain.Prompt, draft domain.Plan, v domain.Verdict) domain.Prompt {
	var b strings.Builder
	b.WriteString(base.User)
	b.WriteString("\n\n## Previous attempt (rejected)\n")
	b.WriteString("root_cause: " + draft.RootCause + "\n")
	for _, s := range draft.Steps {
		b.WriteString("- " + s.Run + "\n")
	}
	b.WriteString("\n## Reviewer objections\n")
	for _, issue := range v.Issues {
		b.WriteString("- " + issue + "\n")
	}
	if v.Notes != "" {
		b.WriteString("\n" + v.Notes + "\n")
	}
	b.WriteString("\nProduce a corrected plan that resolves every objection. Do not repeat the rejected commands verbatim unless they were not the subject of an objection.")

	out := domain.Prompt{System: base.System, User: b.String(), Meta: map[string]string{}}
	for k, v := range base.Meta {
		out.Meta[k] = v
	}
	out.Meta["retry"] = "true"
	return out
}
