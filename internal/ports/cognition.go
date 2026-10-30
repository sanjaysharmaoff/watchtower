package ports

import (
	"context"

	"github.com/sanjaysharmaoff/watchtower/internal/domain"
)

// Redactor scrubs one class of sensitive data from text. The input guardrail is
// just a list of these.
type Redactor interface {
	Name() string
	Redact(text string) string
}

// PromptBuilder assembles the model instruction from an incident.
type PromptBuilder interface {
	Build(inc *domain.Incident) (domain.Prompt, error)
}

// Planner drafts a structured remediation plan.
type Planner interface {
	Name() string
	Plan(ctx context.Context, p domain.Prompt) (domain.Plan, error)
}

// Critic reviews a draft plan and asks for a rewrite when it is wrong.
type Critic interface {
	Name() string
	Review(ctx context.Context, p domain.Prompt, plan domain.Plan) (domain.Verdict, error)
}

// CommandInspector is one output policy rule. Returning a domain.ErrBlocked
// error refuses the command.
type CommandInspector interface {
	Name() string
	Inspect(cmd domain.Command) error
}

// RiskClassifier judges how dangerous a command is from the command itself.
//
// This exists because a model's self-declared risk level cannot be trusted: small
// models routinely label a pod deletion as "read". The reviewer sees this verdict,
// not the model's claim.
type RiskClassifier interface {
	Name() string
	Classify(cmd domain.Command) domain.RiskLevel
}

// Scorer assigns final confidence from everything gathered on the envelope.
type Scorer interface {
	Score(inc *domain.Incident) float64
}

// Narrator produces free-form prose, used by the daily pulse.
type Narrator interface {
	Name() string
	Narrate(ctx context.Context, p domain.Prompt) (string, error)
}
