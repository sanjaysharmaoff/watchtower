package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Prompt is the provider-agnostic instruction pair handed to a model.
type Prompt struct {
	System string            `json:"system"`
	User   string            `json:"user"`
	Meta   map[string]string `json:"meta,omitempty"`
}

// Incident is the single envelope that travels through the whole pipeline.
// Every stage reads what it needs and writes what it produced; no stage knows
// which stage runs before or after it.
type Incident struct {
	ID             string            `json:"id"`
	CorrelationKey string            `json:"correlation_key"`
	Signature      string            `json:"signature"`
	Severity       Severity          `json:"severity"`
	Services       []string          `json:"services,omitempty"`
	Alerts         []Alert           `json:"alerts"`
	Logs           []LogLine         `json:"logs,omitempty"`
	Events         []ClusterEvent    `json:"events,omitempty"`
	Fragments      []ContextFragment `json:"fragments,omitempty"`
	Matches        []RunbookMatch    `json:"matches,omitempty"`
	Prompt         Prompt            `json:"-"`
	Plan           *Plan             `json:"plan,omitempty"`
	Verdict        *Verdict          `json:"verdict,omitempty"`
	Attempts       int               `json:"attempts,omitempty"`
	Confidence     float64           `json:"confidence"`
	Meta           map[string]string `json:"meta,omitempty"`
	Trace          []string          `json:"trace,omitempty"`
	CreatedAt      time.Time         `json:"created_at"`
}

// NewIncident builds an incident envelope from freshly received alerts.
func NewIncident(id string, at time.Time, alerts []Alert) *Incident {
	inc := &Incident{
		ID:        id,
		Alerts:    append([]Alert(nil), alerts...),
		Meta:      map[string]string{},
		CreatedAt: at,
	}
	inc.Recompute()
	return inc
}

// Recompute refreshes the derived fields after alerts change.
func (i *Incident) Recompute() {
	i.Severity = TopSeverity(i.Alerts)
	i.Services = ServicesOf(i.Alerts)
	i.Signature = i.computeSignature()
	if i.CorrelationKey == "" {
		i.CorrelationKey = i.Signature
	}
}

func (i *Incident) computeSignature() string {
	names := make([]string, 0, len(i.Alerts))
	for _, a := range i.Alerts {
		names = append(names, strings.ToLower(a.Name))
	}
	sort.Strings(names)
	seed := strings.Join(names, ",") + "|" + strings.Join(i.Services, ",")
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:])[:16]
}

// MergeAlerts folds extra alerts in, skipping ones already present. It returns
// how many were actually added.
func (i *Incident) MergeAlerts(extra []Alert) int {
	known := make(map[string]struct{}, len(i.Alerts))
	for _, a := range i.Alerts {
		known[a.Identity()] = struct{}{}
	}
	added := 0
	for _, a := range extra {
		if _, dup := known[a.Identity()]; dup {
			continue
		}
		known[a.Identity()] = struct{}{}
		i.Alerts = append(i.Alerts, a)
		added++
	}
	if added > 0 {
		i.Recompute()
	}
	return added
}

// PrimaryAlert is the highest severity, earliest alert in the group.
func (i *Incident) PrimaryAlert() Alert {
	if len(i.Alerts) == 0 {
		return Alert{Name: "unknown", Severity: SeverityUnknown}
	}
	best := i.Alerts[0]
	for _, a := range i.Alerts[1:] {
		if a.Severity.Rank() > best.Severity.Rank() {
			best = a
		}
	}
	return best
}

// Title is the subject line for humans.
func (i *Incident) Title() string {
	p := i.PrimaryAlert()
	if len(i.Alerts) > 1 {
		return fmt.Sprintf("%s (+%d related)", p.Title(), len(i.Alerts)-1)
	}
	return p.Title()
}

// SetMeta records a small key/value fact about the run.
func (i *Incident) SetMeta(key, value string) {
	if i.Meta == nil {
		i.Meta = map[string]string{}
	}
	i.Meta[key] = value
}

// Note appends to the processing trace, which is cheap observability that
// travels with the payload instead of living in a logger somewhere.
func (i *Incident) Note(format string, args ...any) {
	i.Trace = append(i.Trace, fmt.Sprintf(format, args...))
}

// AddFragment attaches provider output and folds its telemetry into the top
// level log and event slices.
func (i *Incident) AddFragment(f ContextFragment) {
	i.Fragments = append(i.Fragments, f)
	i.Logs = append(i.Logs, f.Logs...)
	i.Events = append(i.Events, f.Events...)
}

// TopMatch returns the best retrieved runbook, if any.
func (i *Incident) TopMatch() (RunbookMatch, bool) {
	if len(i.Matches) == 0 {
		return RunbookMatch{}, false
	}
	return i.Matches[0], true
}
