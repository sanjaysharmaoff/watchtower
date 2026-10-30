// Package loki fetches log context from Grafana Loki over its HTTP query API.
package loki

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/sanjaysharmaoff/watchtower/internal/config"
	"github.com/sanjaysharmaoff/watchtower/internal/domain"
	"github.com/sanjaysharmaoff/watchtower/internal/ports"
	"github.com/sanjaysharmaoff/watchtower/internal/wire"
)

func init() {
	wire.ContextProviders.Register("loki", func(e *wire.Env) (ports.ContextProvider, error) {
		if e.Cfg.Enrich.Loki.URL == "" {
			return nil, fmt.Errorf("loki: WT_LOKI_URL is not set")
		}
		return &Provider{
			cfg:      e.Cfg.Enrich.Loki,
			http:     e.HTTP,
			clock:    e.Clock,
			log:      e.Log.With("component", "loki"),
			limit:    e.Cfg.Enrich.LogLimit,
			lookback: e.Cfg.Enrich.Lookback,
		}, nil
	})
}

// Provider implements ports.ContextProvider against Loki.
type Provider struct {
	cfg      config.LokiConfig
	http     *http.Client
	clock    ports.Clock
	log      ports.Logger
	limit    int
	lookback time.Duration
}

// Name implements ports.ContextProvider.
func (p *Provider) Name() string { return "loki" }

// Fetch implements ports.ContextProvider.
func (p *Provider) Fetch(ctx context.Context, inc *domain.Incident) (domain.ContextFragment, error) {
	frag := domain.ContextFragment{Provider: "loki", Notes: map[string]string{}}

	query := p.buildQuery(inc)
	if query == "" {
		return frag, fmt.Errorf("no usable labels to build a LogQL selector")
	}
	frag.Notes["query"] = query

	end := p.clock.Now()
	start := end.Add(-p.lookback)
	limit := p.limit
	if limit <= 0 {
		limit = 100
	}

	endpoint := strings.TrimRight(p.cfg.URL, "/") + "/loki/api/v1/query_range"
	params := url.Values{}
	params.Set("query", query)
	params.Set("start", strconv.FormatInt(start.UnixNano(), 10))
	params.Set("end", strconv.FormatInt(end.UnixNano(), 10))
	params.Set("limit", strconv.Itoa(limit))
	params.Set("direction", "backward")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+params.Encode(), nil)
	if err != nil {
		return frag, err
	}
	if p.cfg.Tenant != "" {
		req.Header.Set("X-Scope-OrgID", p.cfg.Tenant)
	}
	if p.cfg.Username != "" {
		req.SetBasicAuth(p.cfg.Username, p.cfg.Password)
	}

	resp, err := p.http.Do(req)
	if err != nil {
		return frag, fmt.Errorf("loki query: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return frag, fmt.Errorf("loki query: status %d", resp.StatusCode)
	}

	var body struct {
		Data struct {
			Result []struct {
				Stream map[string]string `json:"stream"`
				Values [][2]string       `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return frag, fmt.Errorf("loki decode: %w", err)
	}

	for _, stream := range body.Data.Result {
		source := stream.Stream["pod"]
		if source == "" {
			source = stream.Stream["app"]
		}
		for _, pair := range stream.Values {
			ts := parseNanos(pair[0])
			line := strings.TrimSpace(pair[1])
			if line == "" {
				continue
			}
			frag.Logs = append(frag.Logs, domain.LogLine{
				Timestamp:   ts,
				Level:       detectLevel(line),
				Message:     line,
				Source:      "loki:" + source,
				Occurrences: 1,
				Labels:      stream.Stream,
			})
		}
	}
	frag.Notes["returned"] = strconv.Itoa(len(frag.Logs))
	return frag, nil
}

// buildQuery prefers an operator-supplied template and falls back to a selector
// derived from the alert's own labels.
func (p *Provider) buildQuery(inc *domain.Incident) string {
	primary := inc.PrimaryAlert()
	ns := primary.Label("namespace", "ns")
	svc := primary.Label("service", "app", "job", "deployment", "container")

	if p.cfg.Query != "" {
		q := p.cfg.Query
		q = strings.ReplaceAll(q, "{{namespace}}", ns)
		q = strings.ReplaceAll(q, "{{service}}", svc)
		q = strings.ReplaceAll(q, "{{cluster}}", primary.Cluster)
		return q
	}

	selectors := make([]string, 0, 2)
	if ns != "" {
		selectors = append(selectors, fmt.Sprintf(`namespace=%q`, ns))
	}
	if svc != "" {
		selectors = append(selectors, fmt.Sprintf(`app=%q`, svc))
	}
	if len(selectors) == 0 {
		return ""
	}
	return fmt.Sprintf(`{%s} |~ "(?i)(error|fatal|panic|fail|timeout|refused|exception|denied)"`, strings.Join(selectors, ", "))
}

func parseNanos(s string) time.Time {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(0, n).UTC()
}

// detectLevel sniffs a level out of an unstructured line so the sanitizer can rank
// it. Loki lines are frequently plain text with the level embedded.
func detectLevel(line string) string {
	lower := strings.ToLower(line)
	for _, level := range []string{"fatal", "panic", "error", "warn", "info", "debug"} {
		if strings.Contains(lower, level) {
			return level
		}
	}
	return ""
}
