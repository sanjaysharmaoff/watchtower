// Package guard implements both policy guardrails: input redaction before text
// leaves the process, and command inspection before a human sees a shell line.
package guard

import (
	"regexp"
	"strings"

	"github.com/sanjaysharmaoff/watchtower/internal/ports"
	"github.com/sanjaysharmaoff/watchtower/internal/wire"
)

func init() {
	wire.Redactors.Register("secrets", func(e *wire.Env) (ports.Redactor, error) {
		return NewPattern("secrets", secretPatterns), nil
	})
	wire.Redactors.Register("pii", func(e *wire.Env) (ports.Redactor, error) {
		return NewPattern("pii", piiPatterns), nil
	})
	wire.Redactors.Register("custom", func(e *wire.Env) (ports.Redactor, error) {
		return NewRaw("custom", e.Cfg.Guard.CustomRedact)
	})
}

// Rule is one redaction pattern and the placeholder it leaves behind.
type Rule struct {
	Label string
	Re    *regexp.Regexp
}

// Pattern redacts by regular expression.
type Pattern struct {
	label string
	rules []Rule
}

// NewPattern builds a redactor from compiled rules.
func NewPattern(label string, rules []Rule) Pattern {
	return Pattern{label: label, rules: rules}
}

// NewRaw compiles user-supplied patterns, ignoring an empty list.
func NewRaw(label string, patterns []string) (Pattern, error) {
	rules := make([]Rule, 0, len(patterns))
	for _, p := range patterns {
		if strings.TrimSpace(p) == "" {
			continue
		}
		re, err := regexp.Compile(p)
		if err != nil {
			return Pattern{}, err
		}
		rules = append(rules, Rule{Label: "CUSTOM", Re: re})
	}
	return Pattern{label: label, rules: rules}, nil
}

// Name implements ports.Redactor.
func (p Pattern) Name() string { return p.label }

// Redact implements ports.Redactor.
func (p Pattern) Redact(text string) string {
	if text == "" {
		return text
	}
	for _, r := range p.rules {
		text = r.Re.ReplaceAllString(text, "[REDACTED:"+r.Label+"]")
	}
	return text
}

// secretPatterns covers the credential shapes that realistically show up in logs
// and alert annotations. Order matters: longer, more specific first.
var secretPatterns = []Rule{
	{Label: "AWS_KEY", Re: regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{Label: "AWS_SECRET", Re: regexp.MustCompile(`(?i)\baws_?secret_?access_?key\b\s*[:=]\s*\S+`)},
	{Label: "PRIVATE_KEY", Re: regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`)},
	{Label: "JWT", Re: regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b`)},
	{Label: "BEARER", Re: regexp.MustCompile(`(?i)\b(?:bearer|token)\s+[A-Za-z0-9._\-]{12,}`)},
	{Label: "GITHUB_PAT", Re: regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}\b`)},
	{Label: "SLACK", Re: regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}\b`)},
	{Label: "OPENAI_KEY", Re: regexp.MustCompile(`\bsk-[A-Za-z0-9_\-]{20,}\b`)},
	{Label: "DSN_PASSWORD", Re: regexp.MustCompile(`(?i)\b([a-z0-9+.\-]+)://([^:@\s/]+):([^@\s/]+)@`)},
	{Label: "ASSIGNMENT", Re: regexp.MustCompile(`(?i)\b(password|passwd|secret|api[_-]?key|access[_-]?key|auth[_-]?token|client[_-]?secret)\b\s*[:=]\s*("[^"]*"|'[^']*'|\S+)`)},
	{Label: "BASIC_AUTH", Re: regexp.MustCompile(`(?i)\bauthorization\s*[:=]\s*\S+`)},
}

// piiPatterns covers personal data that has no business in a prompt.
var piiPatterns = []Rule{
	{Label: "EMAIL", Re: regexp.MustCompile(`\b[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}\b`)},
	{Label: "CARD", Re: regexp.MustCompile(`\b(?:\d[ -]*?){13,16}\b`)},
	{Label: "SSN", Re: regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`)},
	{Label: "PHONE", Re: regexp.MustCompile(`\+\d{1,3}[\s\-]?\(?\d{2,4}\)?[\s\-]?\d{3,4}[\s\-]?\d{3,4}\b`)},
}

// Note: IP addresses are deliberately not redacted by default. They are usually
// the most useful token in a network incident. Add them via
// WT_INPUT_REDACT_PATTERNS if your policy requires it.
