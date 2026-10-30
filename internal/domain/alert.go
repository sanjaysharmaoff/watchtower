// Package domain holds the pure vocabulary of WatchTower.
//
// Rule for this package: zero dependencies outside the standard library and no
// knowledge of transports, databases, or model providers. Everything else in the
// codebase is allowed to depend on domain; domain depends on nobody.
package domain

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Severity is a normalized alert severity, decoupled from whatever the source
// system happens to call it.
type Severity string

const (
	SeverityCritical Severity = "critical"
	SeverityWarning  Severity = "warning"
	SeverityInfo     Severity = "info"
	SeverityUnknown  Severity = "unknown"
)

// Rank turns a severity into an orderable integer.
func (s Severity) Rank() int {
	switch strings.ToLower(string(s)) {
	case "critical", "crit", "fatal", "emergency", "page", "p1":
		return 3
	case "warning", "warn", "major", "p2":
		return 2
	case "info", "notice", "low", "p3":
		return 1
	default:
		return 0
	}
}

// ParseSeverity maps a free-form severity label onto the canonical set.
func ParseSeverity(v string) Severity {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "critical", "crit", "fatal", "emergency", "page", "p1":
		return SeverityCritical
	case "warning", "warn", "major", "p2":
		return SeverityWarning
	case "info", "notice", "low", "p3":
		return SeverityInfo
	default:
		return SeverityUnknown
	}
}

// Alert is a single normalized signal. Prometheus, Alertmanager, a synthetic
// probe or a manual curl all collapse into this shape at the edge.
type Alert struct {
	Fingerprint string            `json:"fingerprint,omitempty"`
	Name        string            `json:"name"`
	Status      string            `json:"status,omitempty"`
	Severity    Severity          `json:"severity"`
	Service     string            `json:"service,omitempty"`
	Namespace   string            `json:"namespace,omitempty"`
	Cluster     string            `json:"cluster,omitempty"`
	Summary     string            `json:"summary,omitempty"`
	Description string            `json:"description,omitempty"`
	RunbookURL  string            `json:"runbook_url,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
	StartsAt    time.Time         `json:"starts_at,omitempty"`
	EndsAt      time.Time         `json:"ends_at,omitempty"`
	Source      string            `json:"source,omitempty"`
}

// Identity is a stable per-alert key used for de-duplication.
func (a Alert) Identity() string {
	if a.Fingerprint != "" {
		return a.Fingerprint
	}
	return fmt.Sprintf("%s|%s|%s|%s", a.Name, a.Cluster, a.Namespace, a.Service)
}

// Title is a human readable one-liner.
func (a Alert) Title() string {
	if a.Summary != "" {
		return a.Summary
	}
	if a.Name != "" {
		return a.Name
	}
	return "unnamed alert"
}

// Target describes where the alert is burning.
func (a Alert) Target() string {
	parts := make([]string, 0, 3)
	for _, p := range []string{a.Cluster, a.Namespace, a.Service} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) == 0 {
		return "unknown"
	}
	return strings.Join(parts, "/")
}

// IsResolved reports whether the source says the alert has cleared.
func (a Alert) IsResolved() bool {
	return strings.EqualFold(a.Status, "resolved")
}

// Label returns the first non-empty value for the given keys.
func (a Alert) Label(keys ...string) string {
	for _, k := range keys {
		if v, ok := a.Labels[k]; ok && v != "" {
			return v
		}
		if v, ok := a.Annotations[k]; ok && v != "" {
			return v
		}
	}
	return ""
}

// TopSeverity returns the highest severity in a set of alerts.
func TopSeverity(alerts []Alert) Severity {
	top := SeverityUnknown
	for _, a := range alerts {
		if a.Severity.Rank() > top.Rank() {
			top = a.Severity
		}
	}
	return top
}

// ServicesOf returns the sorted unique set of impacted services.
func ServicesOf(alerts []Alert) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(alerts))
	for _, a := range alerts {
		key := a.Service
		if key == "" {
			key = a.Name
		}
		if key == "" {
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}
