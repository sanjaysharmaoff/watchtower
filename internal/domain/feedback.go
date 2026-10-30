package domain

import "time"

// Feedback is what the SRE sent back after reviewing a delivered plan.
type Feedback struct {
	IncidentID string    `json:"incident_id"`
	Signature  string    `json:"signature,omitempty"`
	Decision   Decision  `json:"decision"`
	Reviewer   string    `json:"reviewer,omitempty"`
	Title      string    `json:"title,omitempty"`
	RootCause  string    `json:"root_cause,omitempty"`
	Commands   []Command `json:"commands,omitempty"`
	Notes      string    `json:"notes,omitempty"`
	ReceivedAt time.Time `json:"received_at"`
}

// ApprovedSummary is the durable record of a reviewed incident.
type ApprovedSummary struct {
	ID         string            `json:"id"`
	IncidentID string            `json:"incident_id"`
	Signature  string            `json:"signature,omitempty"`
	Title      string            `json:"title"`
	RootCause  string            `json:"root_cause,omitempty"`
	Commands   []Command         `json:"commands,omitempty"`
	Decision   Decision          `json:"decision"`
	Reviewer   string            `json:"reviewer,omitempty"`
	Notes      string            `json:"notes,omitempty"`
	Services   []string          `json:"services,omitempty"`
	Severity   Severity          `json:"severity,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
	CreatedAt  time.Time         `json:"created_at"`
}

// AsRunbook converts a reviewed summary into retrieval material.
func (s ApprovedSummary) AsRunbook() Runbook {
	return Runbook{
		ID:        s.ID,
		Title:     s.Title,
		Signature: s.Signature,
		RootCause: s.RootCause,
		Commands:  s.Commands,
		Tags:      s.Services,
		Labels:    s.Labels,
		Reviewer:  s.Reviewer,
		CreatedAt: s.CreatedAt,
	}
}

// Digest is the 24h executive summary produced by the daily pulse.
type Digest struct {
	From      time.Time         `json:"from"`
	To        time.Time         `json:"to"`
	Total     int               `json:"total"`
	Approved  int               `json:"approved"`
	Edited    int               `json:"edited"`
	Rejected  int               `json:"rejected"`
	Services  []string          `json:"services,omitempty"`
	Entries   []ApprovedSummary `json:"entries,omitempty"`
	Narrative string            `json:"narrative,omitempty"`
}

// Message is a rendered notification, transport agnostic.
type Message struct {
	To      []string          `json:"to,omitempty"`
	Subject string            `json:"subject"`
	Text    string            `json:"text,omitempty"`
	HTML    string            `json:"html,omitempty"`
	Meta    map[string]string `json:"meta,omitempty"`
}

// DeadLetter captures anything the system failed to persist or process so it is
// never silently lost.
type DeadLetter struct {
	ID      string    `json:"id"`
	Stage   string    `json:"stage"`
	Reason  string    `json:"reason"`
	RefID   string    `json:"ref_id,omitempty"`
	Payload []byte    `json:"payload,omitempty"`
	At      time.Time `json:"at"`
}
