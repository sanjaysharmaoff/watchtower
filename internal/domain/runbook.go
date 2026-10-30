package domain

import (
	"strings"
	"time"
)

// RiskLevel is how dangerous a single command is judged to be.
type RiskLevel string

const (
	RiskRead      RiskLevel = "read"      // inspect only
	RiskMutate    RiskLevel = "mutate"    // changes state, reversible
	RiskDangerous RiskLevel = "dangerous" // destructive or irreversible
)

// Command is one executable step with its rationale.
type Command struct {
	Run     string    `json:"run"`
	Explain string    `json:"explain,omitempty"`
	Risk    RiskLevel `json:"risk,omitempty"`
	Blocked bool      `json:"blocked,omitempty"`
	Reason  string    `json:"reason,omitempty"`
}

// Binary is the first token of the command, used by policy inspectors.
func (c Command) Binary() string {
	fields := strings.Fields(strings.TrimSpace(c.Run))
	if len(fields) == 0 {
		return ""
	}
	return strings.ToLower(fields[0])
}

// Runbook is a verified remediation recipe. Once an SRE approves one it becomes
// retrieval material for future incidents.
type Runbook struct {
	ID         string            `json:"id"`
	Title      string            `json:"title"`
	Signature  string            `json:"signature,omitempty"`
	RootCause  string            `json:"root_cause,omitempty"`
	Commands   []Command         `json:"commands"`
	Tags       []string          `json:"tags,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
	Reviewer   string            `json:"reviewer,omitempty"`
	Embedding  []float32         `json:"-"`
	SourceText string            `json:"-"`
	CreatedAt  time.Time         `json:"created_at"`
}

// Text is the canonical string representation used for embedding and reranking.
func (r Runbook) Text() string {
	if r.SourceText != "" {
		return r.SourceText
	}
	var b strings.Builder
	b.WriteString(r.Title)
	if r.RootCause != "" {
		b.WriteString("\ncause: ")
		b.WriteString(r.RootCause)
	}
	for _, c := range r.Commands {
		b.WriteString("\n$ ")
		b.WriteString(c.Run)
		if c.Explain != "" {
			b.WriteString("  # ")
			b.WriteString(c.Explain)
		}
	}
	if len(r.Tags) > 0 {
		b.WriteString("\ntags: ")
		b.WriteString(strings.Join(r.Tags, ", "))
	}
	return b.String()
}

// RunbookMatch is a retrieval hit with its score and why it was kept.
type RunbookMatch struct {
	Runbook Runbook `json:"runbook"`
	Score   float64 `json:"score"`
	Stage   string  `json:"stage,omitempty"`
	Reason  string  `json:"reason,omitempty"`
}
