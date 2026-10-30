// Package prompt assembles model instructions from an incident envelope.
package prompt

import (
	"fmt"
	"sort"
	"strings"

	"github.com/sanjaysharmaoff/watchtower/internal/domain"
	"github.com/sanjaysharmaoff/watchtower/internal/ports"
	"github.com/sanjaysharmaoff/watchtower/internal/wire"
)

func init() {
	wire.PromptBuilders.Register("sre", func(e *wire.Env) (ports.PromptBuilder, error) {
		return SRE{MaxLogs: e.Cfg.Clean.MaxLines, MaxEvents: 15}, nil
	})
}

// PlanSchema is the contract the planner must satisfy. It lives next to the
// prompt so the instruction and the parser can never drift apart.
const PlanSchema = `{
  "root_cause": "string, one or two sentences, name the failing component",
  "impact": "string, what users or systems are affected right now",
  "steps": [
    {"run": "single shell command", "explain": "why this command", "risk": "read|mutate|dangerous"}
  ],
  "verification": ["command or observation that confirms the fix worked"],
  "rollback": ["command or observation that undoes the change"],
  "confidence": 0.0
}`

const systemPrompt = `You are a senior site reliability engineer triaging a production incident.

Rules you must follow:
- Diagnose from the evidence provided. Never invent log lines, metrics or resource names that are not in the context.
- Prefer read-only investigation commands first, then the smallest reversible change that addresses the cause.
- Never propose destructive operations: no recursive deletes, no namespace or volume deletion, no --force, no infrastructure teardown, no data-destroying SQL.
- One command per step. No placeholders like <pod-name> unless the context genuinely does not contain the value.
- If the evidence is insufficient, say so in root_cause and propose diagnostic steps only.
- Reuse a verified historical runbook when one matches, adapting resource names to this incident.
- Reply with a single JSON object matching the schema. No prose, no markdown fences.`

// SRE builds the incident triage prompt.
type SRE struct {
	MaxLogs   int
	MaxEvents int
}

// Build implements ports.PromptBuilder.
func (b SRE) Build(inc *domain.Incident) (domain.Prompt, error) {
	if inc == nil || len(inc.Alerts) == 0 {
		return domain.Prompt{}, fmt.Errorf("incident has no alerts")
	}
	maxLogs := b.MaxLogs
	if maxLogs <= 0 {
		maxLogs = 60
	}
	maxEvents := b.MaxEvents
	if maxEvents <= 0 {
		maxEvents = 15
	}

	var u strings.Builder
	primary := inc.PrimaryAlert()

	u.WriteString("# Incident " + inc.ID + "\n")
	u.WriteString(fmt.Sprintf("severity: %s\ntarget: %s\nservices: %s\ndetected: %s\n",
		inc.Severity, primary.Target(), joinOr(inc.Services, "unknown"), inc.CreatedAt.Format("2006-01-02 15:04:05 MST")))

	u.WriteString("\n## Firing alerts\n")
	for _, a := range inc.Alerts {
		u.WriteString(fmt.Sprintf("- [%s] %s on %s\n", a.Severity, a.Name, a.Target()))
		if a.Summary != "" {
			u.WriteString("  summary: " + a.Summary + "\n")
		}
		if a.Description != "" {
			u.WriteString("  detail: " + oneLine(a.Description, 400) + "\n")
		}
		if len(a.Labels) > 0 {
			u.WriteString("  labels: " + labelLine(a.Labels) + "\n")
		}
	}

	if len(inc.Events) > 0 {
		u.WriteString("\n## Orchestrator events\n")
		for _, e := range trimEvents(inc.Events, maxEvents) {
			u.WriteString("- " + oneLine(e.String(), 300) + "\n")
		}
	}

	if len(inc.Logs) > 0 {
		u.WriteString("\n## Log evidence (deduplicated and filtered)\n```\n")
		for _, l := range trimLogs(inc.Logs, maxLogs) {
			u.WriteString(oneLine(l.String(), 400) + "\n")
		}
		u.WriteString("```\n")
	}

	if len(inc.Matches) > 0 {
		u.WriteString("\n## Verified historical runbooks (highest similarity first)\n")
		for i, m := range inc.Matches {
			u.WriteString(fmt.Sprintf("### %d. %s (similarity %.2f)\n", i+1, m.Runbook.Title, m.Score))
			if m.Runbook.RootCause != "" {
				u.WriteString("cause: " + m.Runbook.RootCause + "\n")
			}
			for _, c := range m.Runbook.Commands {
				u.WriteString("$ " + c.Run + "\n")
			}
		}
	} else {
		u.WriteString("\n## Verified historical runbooks\nNone matched. Treat this as a new failure mode.\n")
	}

	u.WriteString("\n## Required output\nReturn exactly this JSON shape:\n")
	u.WriteString(PlanSchema)
	u.WriteString("\n")

	return domain.Prompt{
		System: systemPrompt,
		User:   u.String(),
		Meta: map[string]string{
			"incident_id": inc.ID,
			"signature":   inc.Signature,
			"severity":    string(inc.Severity),
		},
	}, nil
}

// Digest builds the daily executive summary prompt. Kept in the same package as
// the triage prompt so all model-facing wording lives in one place.
func Digest(d domain.Digest) domain.Prompt {
	var u strings.Builder
	u.WriteString(fmt.Sprintf("Reviewed incidents from %s to %s (UTC).\n\n",
		d.From.Format("2006-01-02 15:04"), d.To.Format("2006-01-02 15:04")))
	u.WriteString(fmt.Sprintf("totals: %d reviewed, %d approved, %d edited, %d rejected\n\n", d.Total, d.Approved, d.Edited, d.Rejected))

	for _, e := range d.Entries {
		u.WriteString(fmt.Sprintf("- [%s] %s | services: %s | decision: %s\n", e.Severity, e.Title, joinOr(e.Services, "unknown"), e.Decision))
		if e.RootCause != "" {
			u.WriteString("  cause: " + oneLine(e.RootCause, 300) + "\n")
		}
	}
	if len(d.Entries) == 0 {
		u.WriteString("No incidents were reviewed in this window.\n")
	}

	u.WriteString(`
Write a short executive summary in Markdown for an engineering leadership audience:
1. One paragraph on overall reliability in the window.
2. A bullet list of recurring themes, most frequent first, naming the services involved.
3. A bullet list of at most three concrete follow-up actions to reduce repeat incidents.
Be specific, use the data above only, and keep it under 300 words.`)

	return domain.Prompt{
		System: "You are an SRE lead writing a factual daily reliability report. No filler, no speculation beyond the data.",
		User:   u.String(),
		Meta:   map[string]string{"kind": "digest"},
	}
}

func trimLogs(in []domain.LogLine, max int) []domain.LogLine {
	if len(in) <= max {
		return in
	}
	return in[len(in)-max:]
}

func trimEvents(in []domain.ClusterEvent, max int) []domain.ClusterEvent {
	if len(in) <= max {
		return in
	}
	return in[len(in)-max:]
}

func oneLine(s string, max int) string {
	s = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(s, "\r", " "), "\n", " "))
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

func labelLine(m map[string]string) string {
	skip := map[string]bool{"severity": true, "alertname": true, "__name__": true}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic prompts keep the embedding cache useful
	parts := make([]string, 0, len(m))
	for _, k := range keys {
		if skip[k] || m[k] == "" {
			continue
		}
		parts = append(parts, k+"="+m[k])
	}
	if len(parts) > 12 {
		parts = parts[:12]
	}
	return strings.Join(parts, " ")
}

func joinOr(in []string, fallback string) string {
	if len(in) == 0 {
		return fallback
	}
	return strings.Join(in, ", ")
}
