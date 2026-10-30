package domain

import "strings"

// Plan is the model's proposed remediation, kept strictly structured so
// guardrails and renderers never have to parse prose.
type Plan struct {
	RootCause    string    `json:"root_cause"`
	Impact       string    `json:"impact,omitempty"`
	Steps        []Command `json:"steps"`
	Verification []string  `json:"verification,omitempty"`
	Rollback     []string  `json:"rollback,omitempty"`
	Confidence   float64   `json:"confidence,omitempty"`
	Model        string    `json:"model,omitempty"`
	Raw          string    `json:"-"`
}

// Executable returns the steps that survived the output guardrail.
func (p *Plan) Executable() []Command {
	if p == nil {
		return nil
	}
	out := make([]Command, 0, len(p.Steps))
	for _, s := range p.Steps {
		if !s.Blocked {
			out = append(out, s)
		}
	}
	return out
}

// Blocked returns the steps policy refused.
func (p *Plan) Blocked() []Command {
	if p == nil {
		return nil
	}
	out := make([]Command, 0)
	for _, s := range p.Steps {
		if s.Blocked {
			out = append(out, s)
		}
	}
	return out
}

// Script renders the executable steps as a shell snippet.
func (p *Plan) Script() string {
	var b strings.Builder
	for _, s := range p.Executable() {
		if s.Explain != "" {
			b.WriteString("# " + s.Explain + "\n")
		}
		b.WriteString(s.Run + "\n")
	}
	return b.String()
}

// Verdict is the critic's opinion on a draft plan.
type Verdict struct {
	Approved bool     `json:"approved"`
	Issues   []string `json:"issues,omitempty"`
	Notes    string   `json:"notes,omitempty"`
	Model    string   `json:"model,omitempty"`
}

// Feedback describes what an SRE decided about a delivered plan.
type Decision string

const (
	DecisionApprove Decision = "approve"
	DecisionEdit    Decision = "edit"
	DecisionReject  Decision = "reject"
)

// ParseDecision normalizes a decision string from email links or API calls.
func ParseDecision(v string) Decision {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "approve", "approved", "accept", "ok":
		return DecisionApprove
	case "edit", "edited", "amend":
		return DecisionEdit
	default:
		return DecisionReject
	}
}

// Learnable reports whether the decision should feed the retrieval corpus.
func (d Decision) Learnable() bool {
	return d == DecisionApprove || d == DecisionEdit
}
