package stages

import (
	"context"
	"strings"

	"github.com/sanjaysharmaoff/watchtower/internal/domain"
	"github.com/sanjaysharmaoff/watchtower/internal/ports"
)

// InputGuard scrubs every piece of text that is about to leave the process for a
// model provider: alert annotations, log messages, event messages and retrieved
// runbooks. Adding a new class of secret means adding one ports.Redactor.
func InputGuard(redactors []ports.Redactor) Stage {
	return InputGuardNamed("input-guard", redactors)
}

// InputGuardNamed is InputGuard with an explicit stage name, so the same guard can
// appear more than once in a chain and still be told apart in the logs. Redaction
// is idempotent, which makes a second pass over freshly retrieved material safe.
func InputGuardNamed(name string, redactors []ports.Redactor) Stage {
	return mutator(name, func(_ context.Context, inc *domain.Incident) error {
		if len(redactors) == 0 {
			return nil
		}
		clean := func(s string) string {
			for _, r := range redactors {
				s = r.Redact(s)
			}
			return s
		}

		for i := range inc.Alerts {
			a := &inc.Alerts[i]
			a.Summary = clean(a.Summary)
			a.Description = clean(a.Description)
			for k, v := range a.Annotations {
				a.Annotations[k] = clean(v)
			}
			for k, v := range a.Labels {
				a.Labels[k] = clean(v)
			}
		}
		for i := range inc.Logs {
			inc.Logs[i].Message = clean(inc.Logs[i].Message)
		}
		for i := range inc.Events {
			inc.Events[i].Message = clean(inc.Events[i].Message)
		}
		for i := range inc.Matches {
			rb := &inc.Matches[i].Runbook
			rb.RootCause = clean(rb.RootCause)
			for j := range rb.Commands {
				rb.Commands[j].Run = clean(rb.Commands[j].Run)
				rb.Commands[j].Explain = clean(rb.Commands[j].Explain)
			}
		}

		names := make([]string, 0, len(redactors))
		for _, r := range redactors {
			names = append(names, r.Name())
		}
		inc.SetMeta("redactors", strings.Join(names, "+"))
		return nil
	})
}

// GuardMode decides what happens to a command that policy refuses.
type GuardMode string

const (
	// GuardStrip removes the offending command and keeps the rest of the plan,
	// clearly marked for the reviewer.
	GuardStrip GuardMode = "strip"
	// GuardHalt refuses the whole incident.
	GuardHalt GuardMode = "halt"
)

// OutputGuard runs every inspector over every proposed command. This is the last
// gate before a human sees a shell command, so it never trusts the model's own
// claim that a step is safe: the risk label is recomputed from the command text
// before the inspectors run.
func OutputGuard(inspectors []ports.CommandInspector, classifier ports.RiskClassifier, mode GuardMode) Stage {
	return mutator("output-guard", func(_ context.Context, inc *domain.Incident) error {
		if inc.Plan == nil {
			return nil
		}
		blocked, relabelled := 0, 0
		for i := range inc.Plan.Steps {
			if classifier != nil {
				actual := classifier.Classify(inc.Plan.Steps[i])
				if inc.Plan.Steps[i].Risk != actual {
					relabelled++
					inc.Plan.Steps[i].Risk = actual
				}
			}
			cmd := inc.Plan.Steps[i]
			for _, ins := range inspectors {
				if err := ins.Inspect(cmd); err != nil {
					inc.Plan.Steps[i].Blocked = true
					inc.Plan.Steps[i].Reason = ins.Name() + ": " + strings.TrimPrefix(err.Error(), domain.ErrBlocked.Error()+": ")
					blocked++
					break
				}
			}
		}
		inc.SetMeta("blocked_commands", itoa(blocked))
		if relabelled > 0 {
			inc.SetMeta("risk_relabelled", itoa(relabelled))
			inc.Note("risk classifier corrected %d self-declared risk level(s)", relabelled)
		}
		if blocked > 0 {
			inc.Note("output guard blocked %d of %d command(s)", blocked, len(inc.Plan.Steps))
		}
		if blocked > 0 && mode == GuardHalt {
			return domain.Block("%d command(s) violated policy", blocked)
		}
		if blocked > 0 && len(inc.Plan.Executable()) == 0 {
			inc.Note("no safe commands remain; delivering diagnosis only")
		}
		return nil
	})
}

// Score assigns the final confidence value used for delivery decisions.
func Score(s ports.Scorer) Stage {
	return mutator("score", func(_ context.Context, inc *domain.Incident) error {
		inc.Confidence = s.Score(inc)
		if inc.Plan != nil {
			inc.Plan.Confidence = inc.Confidence
		}
		inc.SetMeta("confidence", strings.TrimRight(strings.TrimRight(formatFloat(inc.Confidence), "0"), "."))
		return nil
	})
}

func formatFloat(f float64) string {
	// Two decimals, no strconv import churn elsewhere in the package.
	whole := int(f * 100)
	return itoa(whole/100) + "." + pad2(whole%100)
}

func pad2(n int) string {
	if n < 0 {
		n = -n
	}
	if n < 10 {
		return "0" + itoa(n)
	}
	return itoa(n)
}
