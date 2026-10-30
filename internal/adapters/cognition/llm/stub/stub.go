// Package stub is a dependency-free cognitive core.
//
// It exists so the whole pipeline can run locally with no API key, no network and
// no cost, and so tests are deterministic. It is rule based, not clever, and it
// says so in every plan it produces.
package stub

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/sanjaysharmaoff/watchtower/internal/domain"
	"github.com/sanjaysharmaoff/watchtower/internal/ports"
	"github.com/sanjaysharmaoff/watchtower/internal/wire"
)

func init() {
	wire.Planners.Register("stub", func(e *wire.Env) (ports.Planner, error) { return Planner{}, nil })
	wire.Critics.Register("stub", func(e *wire.Env) (ports.Critic, error) { return Critic{}, nil })
	wire.Narrators.Register("stub", func(e *wire.Env) (ports.Narrator, error) { return Narrator{}, nil })
}

// Planner produces a plausible diagnostic plan from keyword rules.
type Planner struct{}

// Name implements ports.Planner.
func (Planner) Name() string { return "stub:rules" }

var reTarget = regexp.MustCompile(`(?m)^target:\s*(.+)$`)

// Plan implements ports.Planner.
func (Planner) Plan(_ context.Context, p domain.Prompt) (domain.Plan, error) {
	text := strings.ToLower(p.User)
	ns, svc := targetFrom(p.User)

	plan := domain.Plan{
		RootCause:  "Rule-based triage (no model configured): " + guessCause(text),
		Impact:     "See the firing alerts; impact was not assessed by a model.",
		Confidence: 0.35,
	}

	plan.Steps = append(plan.Steps, domain.Command{
		Run:     fmt.Sprintf("kubectl -n %s get pods -l app=%s -o wide", ns, svc),
		Explain: "Establish current pod state, restarts and node placement.",
		Risk:    domain.RiskRead,
	})
	plan.Steps = append(plan.Steps, domain.Command{
		Run:     fmt.Sprintf("kubectl -n %s logs -l app=%s --tail=200 --since=15m", ns, svc),
		Explain: "Read recent application logs for the failing selector.",
		Risk:    domain.RiskRead,
	})

	switch {
	case containsAny(text, "oomkilled", "out of memory", "memory limit"):
		plan.Steps = append(plan.Steps,
			domain.Command{Run: fmt.Sprintf("kubectl -n %s describe pod -l app=%s | grep -A5 -i 'last state'", ns, svc), Explain: "Confirm the OOMKilled termination reason and previous exit code.", Risk: domain.RiskRead},
			domain.Command{Run: fmt.Sprintf("kubectl -n %s top pod -l app=%s", ns, svc), Explain: "Compare live usage against the configured limit.", Risk: domain.RiskRead},
		)
	case containsAny(text, "crashloop", "backoff", "restarting"):
		plan.Steps = append(plan.Steps,
			domain.Command{Run: fmt.Sprintf("kubectl -n %s logs -l app=%s --previous --tail=100", ns, svc), Explain: "Read the logs of the crashed container instance.", Risk: domain.RiskRead},
			domain.Command{Run: fmt.Sprintf("kubectl -n %s get events --sort-by=.lastTimestamp | tail -30", ns), Explain: "Correlate restarts with scheduler and kubelet events.", Risk: domain.RiskRead},
		)
	case containsAny(text, "disk", "no space", "filesystem", "inode"):
		plan.Steps = append(plan.Steps,
			domain.Command{Run: "df -h", Explain: "Identify which filesystem is full.", Risk: domain.RiskRead},
			domain.Command{Run: "du -xh --max-depth=2 /var | sort -h | tail -20", Explain: "Locate the largest consumers under /var.", Risk: domain.RiskRead},
		)
	case containsAny(text, "connection refused", "timeout", "unreachable", "dial tcp"):
		plan.Steps = append(plan.Steps,
			domain.Command{Run: fmt.Sprintf("kubectl -n %s get endpoints %s -o yaml", ns, svc), Explain: "Verify the service has ready endpoints.", Risk: domain.RiskRead},
			domain.Command{Run: fmt.Sprintf("kubectl -n %s describe svc %s", ns, svc), Explain: "Check selector and port mapping.", Risk: domain.RiskRead},
		)
	case containsAny(text, "certificate", "x509", "tls"):
		plan.Steps = append(plan.Steps,
			domain.Command{Run: fmt.Sprintf("kubectl -n %s get secret -o name | grep tls", ns), Explain: "List candidate TLS secrets.", Risk: domain.RiskRead},
			domain.Command{Run: fmt.Sprintf("echo | openssl s_client -connect %s:443 -servername %s 2>/dev/null | openssl x509 -noout -dates", svc, svc), Explain: "Read the served certificate validity window.", Risk: domain.RiskRead},
		)
	case containsAny(text, "latency", "p99", "slow", "saturation"):
		plan.Steps = append(plan.Steps,
			domain.Command{Run: fmt.Sprintf("kubectl -n %s top pod -l app=%s", ns, svc), Explain: "Check whether CPU or memory is saturated.", Risk: domain.RiskRead},
			domain.Command{Run: fmt.Sprintf("kubectl -n %s get hpa", ns), Explain: "Check whether autoscaling is at its ceiling.", Risk: domain.RiskRead},
		)
	}

	plan.Verification = []string{
		fmt.Sprintf("kubectl -n %s get pods -l app=%s should show all containers Ready with no new restarts", ns, svc),
		"The originating alert should resolve in Alertmanager within one evaluation interval",
	}
	plan.Rollback = []string{"No state was changed by these steps, so there is nothing to roll back."}
	return plan, nil
}

func guessCause(text string) string {
	switch {
	case containsAny(text, "oomkilled", "out of memory"):
		return "a container is being OOM killed, so the memory limit or a leak is the likely cause"
	case containsAny(text, "crashloop", "backoff"):
		return "a container is failing to start and being restarted repeatedly"
	case containsAny(text, "disk", "no space"):
		return "a filesystem is close to or at capacity"
	case containsAny(text, "connection refused", "dial tcp", "unreachable"):
		return "the dependency endpoint is not accepting connections"
	case containsAny(text, "certificate", "x509"):
		return "a TLS certificate is invalid or expired"
	case containsAny(text, "latency", "p99"):
		return "the service is saturated or a downstream dependency is slow"
	default:
		return "cause not determined from the available evidence"
	}
}

// targetFrom pulls namespace and service out of the "target: cluster/ns/svc"
// line the prompt builder writes.
func targetFrom(user string) (namespace, service string) {
	namespace, service = "default", "app"
	m := reTarget.FindStringSubmatch(user)
	if len(m) < 2 {
		return namespace, service
	}
	parts := strings.Split(strings.TrimSpace(m[1]), "/")
	switch len(parts) {
	case 1:
		if parts[0] != "" && parts[0] != "unknown" {
			service = parts[0]
		}
	case 2:
		namespace, service = parts[0], parts[1]
	default:
		namespace, service = parts[len(parts)-2], parts[len(parts)-1]
	}
	return namespace, service
}

// Critic rejects plans that are obviously unusable.
type Critic struct{}

// Name implements ports.Critic.
func (Critic) Name() string { return "stub:rules" }

// Review implements ports.Critic.
func (Critic) Review(_ context.Context, _ domain.Prompt, plan domain.Plan) (domain.Verdict, error) {
	var issues []string
	if len(plan.Steps) == 0 {
		issues = append(issues, "plan contains no steps")
	}
	if strings.TrimSpace(plan.RootCause) == "" {
		issues = append(issues, "root cause is empty")
	}
	for _, s := range plan.Steps {
		if strings.Contains(s.Run, "<") && strings.Contains(s.Run, ">") {
			issues = append(issues, "step still contains a placeholder: "+s.Run)
		}
		if s.Risk == domain.RiskDangerous {
			issues = append(issues, "step is self-declared dangerous: "+s.Run)
		}
	}
	mutating := false
	for _, s := range plan.Steps {
		if s.Risk == domain.RiskMutate {
			mutating = true
		}
	}
	if mutating && len(plan.Verification) == 0 {
		issues = append(issues, "state-changing plan has no verification step")
	}
	return domain.Verdict{Approved: len(issues) == 0, Issues: issues, Notes: "reviewed by the offline rule-based critic"}, nil
}

// Narrator writes a digest without a model.
type Narrator struct{}

// Name implements ports.Narrator.
func (Narrator) Name() string { return "stub:template" }

// Narrate implements ports.Narrator.
func (Narrator) Narrate(_ context.Context, p domain.Prompt) (string, error) {
	var b strings.Builder
	b.WriteString("## Summary\n\n")
	b.WriteString("Generated without a language model, so this is a factual roll-up of the recorded data rather than an analysis.\n\n")
	b.WriteString("```\n")
	for _, line := range strings.Split(p.User, "\n") {
		if strings.HasPrefix(line, "totals:") || strings.HasPrefix(line, "- [") {
			b.WriteString(line + "\n")
		}
	}
	b.WriteString("```\n")
	return b.String(), nil
}

func containsAny(haystack string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(haystack, n) {
			return true
		}
	}
	return false
}
