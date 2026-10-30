// Package decode turns webhook bodies into normalized alerts.
package decode

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/sanjaysharmaoff/watchtower/internal/domain"
	"github.com/sanjaysharmaoff/watchtower/internal/ports"
	"github.com/sanjaysharmaoff/watchtower/internal/wire"
)

func init() {
	wire.AlertDecoders.Register("alertmanager", func(e *wire.Env) (ports.AlertDecoder, error) {
		return Alertmanager{}, nil
	})
	wire.AlertDecoders.Register("generic", func(e *wire.Env) (ports.AlertDecoder, error) {
		return Generic{}, nil
	})
}

// amPayload is the subset of the Alertmanager webhook contract we rely on.
type amPayload struct {
	Version           string            `json:"version"`
	GroupKey          string            `json:"groupKey"`
	Status            string            `json:"status"`
	Receiver          string            `json:"receiver"`
	ExternalURL       string            `json:"externalURL"`
	CommonLabels      map[string]string `json:"commonLabels"`
	CommonAnnotations map[string]string `json:"commonAnnotations"`
	Alerts            []struct {
		Status       string            `json:"status"`
		Labels       map[string]string `json:"labels"`
		Annotations  map[string]string `json:"annotations"`
		StartsAt     time.Time         `json:"startsAt"`
		EndsAt       time.Time         `json:"endsAt"`
		Fingerprint  string            `json:"fingerprint"`
		GeneratorURL string            `json:"generatorURL"`
	} `json:"alerts"`
}

// Alertmanager decodes the Prometheus Alertmanager webhook shape.
type Alertmanager struct{}

// Name implements ports.AlertDecoder.
func (Alertmanager) Name() string { return "alertmanager" }

// Decode implements ports.AlertDecoder.
func (d Alertmanager) Decode(r io.Reader) ([]domain.Alert, error) {
	var p amPayload
	if err := json.NewDecoder(r).Decode(&p); err != nil {
		return nil, fmt.Errorf("decode alertmanager payload: %w", err)
	}
	if len(p.Alerts) == 0 {
		return nil, fmt.Errorf("payload contains no alerts")
	}

	out := make([]domain.Alert, 0, len(p.Alerts))
	for _, a := range p.Alerts {
		labels := merge(p.CommonLabels, a.Labels)
		anns := merge(p.CommonAnnotations, a.Annotations)

		status := a.Status
		if status == "" {
			status = p.Status
		}
		out = append(out, domain.Alert{
			Fingerprint: a.Fingerprint,
			Name:        pick(labels, "alertname", "alert", "rule"),
			Status:      status,
			Severity:    domain.ParseSeverity(pick(labels, "severity", "priority")),
			Service:     pick(labels, "service", "app", "job", "deployment", "container"),
			Namespace:   pick(labels, "namespace", "ns"),
			Cluster:     pick(labels, "cluster", "env", "environment"),
			Summary:     pick(anns, "summary", "title", "message"),
			Description: pick(anns, "description", "detail"),
			RunbookURL:  pick(anns, "runbook_url", "runbook"),
			Labels:      labels,
			Annotations: anns,
			StartsAt:    a.StartsAt,
			EndsAt:      zeroIfEpoch(a.EndsAt),
			Source:      "alertmanager",
		})
	}
	return out, nil
}

// Generic decodes either a single alert or an array of alerts in WatchTower's own
// normalized shape, which keeps curl-driven testing and internal producers simple.
type Generic struct{}

// Name implements ports.AlertDecoder.
func (Generic) Name() string { return "generic" }

// Decode implements ports.AlertDecoder.
func (Generic) Decode(r io.Reader) ([]domain.Alert, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil, fmt.Errorf("empty body")
	}

	if strings.HasPrefix(trimmed, "[") {
		var many []domain.Alert
		if err := json.Unmarshal(raw, &many); err != nil {
			return nil, fmt.Errorf("decode alert array: %w", err)
		}
		return normalizeAll(many), nil
	}

	var wrapper struct {
		Alerts []domain.Alert `json:"alerts"`
	}
	if err := json.Unmarshal(raw, &wrapper); err == nil && len(wrapper.Alerts) > 0 {
		return normalizeAll(wrapper.Alerts), nil
	}

	var one domain.Alert
	if err := json.Unmarshal(raw, &one); err != nil {
		return nil, fmt.Errorf("decode alert: %w", err)
	}
	return normalizeAll([]domain.Alert{one}), nil
}

func normalizeAll(in []domain.Alert) []domain.Alert {
	out := make([]domain.Alert, 0, len(in))
	for _, a := range in {
		if a.Severity == "" {
			a.Severity = domain.SeverityUnknown
		} else {
			a.Severity = domain.ParseSeverity(string(a.Severity))
		}
		if a.Source == "" {
			a.Source = "generic"
		}
		if a.StartsAt.IsZero() {
			a.StartsAt = time.Now().UTC()
		}
		if a.Name == "" {
			a.Name = pick(a.Labels, "alertname", "alert")
		}
		out = append(out, a)
	}
	return out
}

func merge(base, over map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(over))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		out[k] = v
	}
	return out
}

func pick(m map[string]string, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok && strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// zeroIfEpoch normalizes Alertmanager's "0001-01-01" style empty end time.
func zeroIfEpoch(t time.Time) time.Time {
	if t.Year() <= 1 {
		return time.Time{}
	}
	return t
}
