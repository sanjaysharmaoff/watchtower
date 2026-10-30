package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"strconv"
	"strings"

	"github.com/sanjaysharmaoff/watchtower/internal/adapters/ingest/worker"
	"github.com/sanjaysharmaoff/watchtower/internal/domain"
)

// handleAlerts receives the webhook, decodes it and queues triage.
func (s *Server) handleAlerts(w http.ResponseWriter, r *http.Request) {
	body := http.MaxBytesReader(w, r.Body, s.maxBody())
	defer body.Close()

	alerts, err := s.deps.Decoder.Decode(body)
	if err != nil {
		s.log.Warn("alert decode failed", "decoder", s.deps.Decoder.Name(), "err", err.Error())
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	id, err := s.deps.Alerts.Submit(r.Context(), alerts)
	switch {
	case errors.Is(err, worker.ErrQueueFull):
		// Tell the sender to back off rather than pretending we accepted the work.
		w.Header().Set("Retry-After", "30")
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "pipeline saturated, retry shortly", "incident_id": id})
	case err != nil:
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
	default:
		writeJSON(w, http.StatusAccepted, map[string]any{"incident_id": id, "alerts": len(alerts), "status": "queued"})
	}
}

// feedbackRequest is the machine-facing feedback contract.
type feedbackRequest struct {
	IncidentID string           `json:"incident_id"`
	Signature  string           `json:"signature,omitempty"`
	Decision   string           `json:"decision"`
	Reviewer   string           `json:"reviewer,omitempty"`
	Title      string           `json:"title,omitempty"`
	RootCause  string           `json:"root_cause,omitempty"`
	Commands   []domain.Command `json:"commands,omitempty"`
	Notes      string           `json:"notes,omitempty"`
}

// handleFeedbackJSON is the API path, used by tooling and by the edit flow where
// the reviewer rewrites the commands before approving.
func (s *Server) handleFeedbackJSON(w http.ResponseWriter, r *http.Request) {
	body := http.MaxBytesReader(w, r.Body, s.maxBody())
	defer body.Close()

	var req feedbackRequest
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}
	if strings.TrimSpace(req.IncidentID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "incident_id is required"})
		return
	}

	fb := domain.Feedback{
		IncidentID: req.IncidentID,
		Signature:  req.Signature,
		Decision:   domain.ParseDecision(req.Decision),
		Reviewer:   req.Reviewer,
		Title:      req.Title,
		RootCause:  req.RootCause,
		Commands:   req.Commands,
		Notes:      req.Notes,
		ReceivedAt: s.deps.Clock.Now(),
	}
	if err := s.deps.Feedback.Submit(r.Context(), fb); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"status":      "recorded",
		"incident_id": fb.IncidentID,
		"decision":    string(fb.Decision),
	})
}

// handleFeedbackLink is the one-click path from the email. The HMAC signature and
// the expiry are the credentials here, so both are verified before anything is
// recorded.
func (s *Server) handleFeedbackLink(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	incidentID := q.Get("incident")
	decision := q.Get("decision")
	exp := q.Get("exp")
	sig := q.Get("sig")

	if incidentID == "" || decision == "" || exp == "" || sig == "" {
		writeHTML(w, http.StatusBadRequest, "Link is incomplete", "This approval link is missing parameters.")
		return
	}

	expUnix, err := strconv.ParseInt(exp, 10, 64)
	if err != nil {
		writeHTML(w, http.StatusBadRequest, "Link is malformed", "The expiry on this link could not be read.")
		return
	}
	payload := fmt.Sprintf("%s|%s|%s", incidentID, decision, exp)
	if !s.deps.Signer.Verify(payload, sig) {
		s.log.Warn("rejected approval link with bad signature", "incident", incidentID, "remote", clientIP(r))
		writeHTML(w, http.StatusForbidden, "Signature does not match", "This link was not issued by WatchTower, or it was altered in transit.")
		return
	}
	if s.deps.Clock.Now().Unix() > expUnix {
		writeHTML(w, http.StatusGone, "Link has expired",
			"Approval links are short lived. Use the API endpoint POST /runbook/approve to record your decision.")
		return
	}

	fb := domain.Feedback{
		IncidentID: incidentID,
		Decision:   domain.ParseDecision(decision),
		Reviewer:   q.Get("reviewer"),
		ReceivedAt: s.deps.Clock.Now(),
	}
	if err := s.deps.Feedback.Submit(r.Context(), fb); err != nil {
		writeHTML(w, http.StatusInternalServerError, "Could not record decision", err.Error())
		return
	}

	switch fb.Decision {
	case domain.DecisionApprove:
		writeHTML(w, http.StatusOK, "Approved",
			"The runbook was stored and embedded. The next incident with this signature will retrieve it.")
	case domain.DecisionEdit:
		writeHTML(w, http.StatusOK, "Marked for editing",
			"Send the corrected commands to POST /runbook/approve with decision \"edit\" to teach the corrected version.")
	default:
		writeHTML(w, http.StatusOK, "Rejected",
			"Recorded as rejected. This plan will not enter the retrieval corpus.")
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": s.deps.Version})
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	if s.deps.Ready == nil {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
		return
	}
	if err := s.deps.Ready(r.Context()); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not ready", "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	payload := map[string]any{"version": s.deps.Version, "time": s.deps.Clock.Now()}
	if s.deps.Stats != nil {
		payload["worker"] = s.deps.Stats()
	}
	writeJSON(w, http.StatusOK, payload)
}

func (s *Server) maxBody() int64 {
	if s.deps.Config.MaxBodyBytes > 0 {
		return s.deps.Config.MaxBodyBytes
	}
	return 2 << 20
}

// writeHTML renders a minimal, accessible confirmation page for email clicks.
func writeHTML(w http.ResponseWriter, status int, title, detail string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	page := `<!DOCTYPE html><html lang="en"><head><meta charset="utf-8">` +
		`<meta name="viewport" content="width=device-width, initial-scale=1"><title>` + html.EscapeString(title) + `</title></head>` +
		`<body style="font-family:-apple-system,Segoe UI,Roboto,Helvetica,Arial,sans-serif;max-width:640px;margin:48px auto;padding:0 16px;color:#1b1f23;">` +
		`<h1 style="font-size:22px;">` + html.EscapeString(title) + `</h1>` +
		`<p style="font-size:15px;line-height:1.6;color:#57606a;">` + html.EscapeString(detail) + `</p>` +
		`<p style="font-size:13px;color:#57606a;">WatchTower</p></body></html>`
	_, _ = w.Write([]byte(page))
}
