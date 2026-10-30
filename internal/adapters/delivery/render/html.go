// Package render turns incidents and digests into deliverable messages.
package render

import (
	"bytes"
	"fmt"
	"html/template"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/sanjaysharmaoff/watchtower/internal/domain"
	"github.com/sanjaysharmaoff/watchtower/internal/platform/sign"
	"github.com/sanjaysharmaoff/watchtower/internal/ports"
	"github.com/sanjaysharmaoff/watchtower/internal/wire"
)

func init() {
	wire.Renderers.Register("html", func(e *wire.Env) (ports.Renderer, error) {
		return New(
			e.Cfg.HTTP.PublicBaseURL,
			sign.HMAC(e.Cfg.HTTP.ApprovalSecret),
			e.Cfg.HTTP.ApprovalTimeout,
			e.Clock,
			e.Cfg.Notify.To,
		)
	})
}

// HTML renders interactive email. The approval links are HMAC signed and expiring,
// so a forwarded email cannot be used to inject a runbook.
type HTML struct {
	baseURL string
	signer  ports.Signer
	ttl     time.Duration
	clock   ports.Clock
	to      []string
	incTpl  *template.Template
	digTpl  *template.Template
}

// New builds the renderer.
func New(baseURL string, signer ports.Signer, ttl time.Duration, clock ports.Clock, to []string) (*HTML, error) {
	funcs := template.FuncMap{
		"pct":   func(f float64) string { return strconv.Itoa(int(f*100+0.5)) + "%" },
		"upper": strings.ToUpper,
	}
	incTpl, err := template.New("incident").Funcs(funcs).Parse(incidentTemplate)
	if err != nil {
		return nil, err
	}
	digTpl, err := template.New("digest").Funcs(funcs).Parse(digestTemplate)
	if err != nil {
		return nil, err
	}
	return &HTML{
		baseURL: strings.TrimRight(baseURL, "/"),
		signer:  signer,
		ttl:     ttl,
		clock:   clock,
		to:      to,
		incTpl:  incTpl,
		digTpl:  digTpl,
	}, nil
}

// Name implements ports.Renderer.
func (*HTML) Name() string { return "html" }

type incidentView struct {
	Incident   *domain.Incident
	Plan       *domain.Plan
	Steps      []domain.Command
	Blocked    []domain.Command
	Logs       []domain.LogLine
	Events     []domain.ClusterEvent
	Matches    []domain.RunbookMatch
	Verdict    *domain.Verdict
	Confidence float64
	Badge      string
	Script     string
	Approve    string
	Edit       string
	Reject     string
	Trace      []string
}

// RenderIncident implements ports.Renderer.
func (h *HTML) RenderIncident(inc *domain.Incident) (domain.Message, error) {
	if inc == nil {
		return domain.Message{}, fmt.Errorf("nil incident")
	}
	view := incidentView{
		Incident:   inc,
		Plan:       inc.Plan,
		Logs:       tailLogs(inc.Logs, 25),
		Events:     tailEvents(inc.Events, 10),
		Matches:    inc.Matches,
		Verdict:    inc.Verdict,
		Confidence: inc.Confidence,
		Badge:      badge(inc.Confidence),
		Trace:      inc.Trace,
		Approve:    h.link(inc.ID, domain.DecisionApprove),
		Edit:       h.link(inc.ID, domain.DecisionEdit),
		Reject:     h.link(inc.ID, domain.DecisionReject),
	}
	if inc.Plan != nil {
		view.Steps = inc.Plan.Executable()
		view.Blocked = inc.Plan.Blocked()
		view.Script = inc.Plan.Script()
	}

	var buf bytes.Buffer
	if err := h.incTpl.Execute(&buf, view); err != nil {
		return domain.Message{}, fmt.Errorf("render incident: %w", err)
	}

	subject := fmt.Sprintf("[WatchTower][%s] %s", strings.ToUpper(string(inc.Severity)), inc.Title())
	return domain.Message{
		To:      h.to,
		Subject: subject,
		HTML:    buf.String(),
		Text:    incidentText(inc, view),
		Meta:    map[string]string{"incident_id": inc.ID, "kind": "incident"},
	}, nil
}

// RenderDigest implements ports.Renderer.
func (h *HTML) RenderDigest(d domain.Digest) (domain.Message, error) {
	var buf bytes.Buffer
	if err := h.digTpl.Execute(&buf, d); err != nil {
		return domain.Message{}, fmt.Errorf("render digest: %w", err)
	}
	subject := fmt.Sprintf("[WatchTower] Daily Pulse %s: %d reviewed, %d approved",
		d.To.Format("2006-01-02"), d.Total, d.Approved)
	return domain.Message{
		To:      h.to,
		Subject: subject,
		HTML:    buf.String(),
		Text:    d.Narrative,
		Meta:    map[string]string{"kind": "digest"},
	}, nil
}

// link builds a signed, expiring one-click decision URL.
func (h *HTML) link(incidentID string, decision domain.Decision) string {
	exp := h.clock.Now().Add(h.ttl).Unix()
	payload := fmt.Sprintf("%s|%s|%d", incidentID, decision, exp)

	q := url.Values{}
	q.Set("incident", incidentID)
	q.Set("decision", string(decision))
	q.Set("exp", strconv.FormatInt(exp, 10))
	q.Set("sig", h.signer.Sign(payload))
	return h.baseURL + "/runbook/approve?" + q.Encode()
}

func badge(confidence float64) string {
	switch {
	case confidence >= 0.75:
		return "high"
	case confidence >= 0.45:
		return "medium"
	default:
		return "low"
	}
}

func tailLogs(in []domain.LogLine, n int) []domain.LogLine {
	if len(in) <= n {
		return in
	}
	return in[len(in)-n:]
}

func tailEvents(in []domain.ClusterEvent, n int) []domain.ClusterEvent {
	if len(in) <= n {
		return in
	}
	return in[len(in)-n:]
}

// incidentText is the plain-text alternative part, which is what pagers, terminal
// mail clients and text-only readers actually get.
func incidentText(inc *domain.Incident, v incidentView) string {
	var b strings.Builder
	fmt.Fprintf(&b, "WatchTower incident %s\nSeverity: %s\nTarget: %s\nConfidence: %.0f%% (%s)\n\n",
		inc.ID, inc.Severity, inc.PrimaryAlert().Target(), inc.Confidence*100, v.Badge)

	if inc.Plan != nil {
		fmt.Fprintf(&b, "Root cause: %s\n", inc.Plan.RootCause)
		if inc.Plan.Impact != "" {
			fmt.Fprintf(&b, "Impact: %s\n", inc.Plan.Impact)
		}
		b.WriteString("\nProposed steps:\n")
		for i, s := range v.Steps {
			fmt.Fprintf(&b, "%d. %s\n   %s\n", i+1, s.Run, s.Explain)
		}
		for _, s := range v.Blocked {
			fmt.Fprintf(&b, "BLOCKED: %s (%s)\n", s.Run, s.Reason)
		}
		if len(inc.Plan.Verification) > 0 {
			b.WriteString("\nVerification:\n")
			for _, s := range inc.Plan.Verification {
				fmt.Fprintf(&b, "- %s\n", s)
			}
		}
	} else {
		b.WriteString("No plan was produced for this incident.\n")
	}

	if len(v.Matches) > 0 {
		b.WriteString("\nHistorical runbooks used:\n")
		for _, m := range v.Matches {
			fmt.Fprintf(&b, "- %s (similarity %.0f%%)", m.Runbook.Title, m.Score*100)
			if m.Reason != "" {
				fmt.Fprintf(&b, " [%s]", m.Reason)
			}
			b.WriteString("\n")
		}
	}

	fmt.Fprintf(&b, "\nApprove: %s\nEdit:    %s\nReject:  %s\n", v.Approve, v.Edit, v.Reject)
	return b.String()
}
