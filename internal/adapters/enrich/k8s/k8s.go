// Package k8s reads recent Kubernetes events through the API server's REST
// interface. Deliberately no client-go: one JSON endpoint is all this needs, and
// the dependency would dwarf the rest of the service.
package k8s

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sanjaysharmaoff/watchtower/internal/config"
	"github.com/sanjaysharmaoff/watchtower/internal/domain"
	"github.com/sanjaysharmaoff/watchtower/internal/ports"
	"github.com/sanjaysharmaoff/watchtower/internal/wire"
)

const (
	inClusterHost      = "https://kubernetes.default.svc"
	serviceAccountDir  = "/var/run/secrets/kubernetes.io/serviceaccount"
	serviceAccountFile = serviceAccountDir + "/token"
	serviceAccountCA   = serviceAccountDir + "/ca.crt"
)

func init() {
	wire.ContextProviders.Register("k8s", func(e *wire.Env) (ports.ContextProvider, error) {
		cfg := e.Cfg.Enrich.K8s
		if cfg.APIServer == "" {
			cfg.APIServer = inClusterHost
		}
		if cfg.Token == "" {
			if raw, err := os.ReadFile(serviceAccountFile); err == nil {
				cfg.Token = strings.TrimSpace(string(raw))
			}
		}
		if cfg.CAFile == "" {
			if _, err := os.Stat(serviceAccountCA); err == nil {
				cfg.CAFile = serviceAccountCA
			}
		}
		if cfg.Token == "" {
			return nil, fmt.Errorf("k8s: no token configured and no service account mounted")
		}
		client, err := httpClient(cfg)
		if err != nil {
			return nil, err
		}
		return &Provider{cfg: cfg, http: client, log: e.Log.With("component", "k8s")}, nil
	})
}

// Provider implements ports.ContextProvider against the Kubernetes events API.
type Provider struct {
	cfg  config.K8sConfig
	http *http.Client
	log  ports.Logger
}

// Name implements ports.ContextProvider.
func (*Provider) Name() string { return "k8s" }

// Fetch implements ports.ContextProvider.
func (p *Provider) Fetch(ctx context.Context, inc *domain.Incident) (domain.ContextFragment, error) {
	frag := domain.ContextFragment{Provider: "k8s", Notes: map[string]string{}}

	namespace := inc.PrimaryAlert().Label("namespace", "ns")
	if namespace == "" {
		return frag, fmt.Errorf("incident has no namespace label")
	}
	limit := p.cfg.Limit
	if limit <= 0 {
		limit = 40
	}

	params := url.Values{}
	params.Set("limit", strconv.Itoa(limit))
	endpoint := fmt.Sprintf("%s/api/v1/namespaces/%s/events?%s",
		strings.TrimRight(p.cfg.APIServer, "/"), url.PathEscape(namespace), params.Encode())

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return frag, err
	}
	req.Header.Set("Authorization", "Bearer "+p.cfg.Token)
	req.Header.Set("Accept", "application/json")

	resp, err := p.http.Do(req)
	if err != nil {
		return frag, fmt.Errorf("k8s events: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized {
		return frag, fmt.Errorf("k8s events: status %d, the service account needs get/list on events", resp.StatusCode)
	}
	if resp.StatusCode >= 400 {
		return frag, fmt.Errorf("k8s events: status %d", resp.StatusCode)
	}

	var body struct {
		Items []struct {
			Type           string `json:"type"`
			Reason         string `json:"reason"`
			Message        string `json:"message"`
			Count          int    `json:"count"`
			LastTimestamp  string `json:"lastTimestamp"`
			FirstTimestamp string `json:"firstTimestamp"`
			EventTime      string `json:"eventTime"`
			InvolvedObject struct {
				Kind string `json:"kind"`
				Name string `json:"name"`
			} `json:"involvedObject"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return frag, fmt.Errorf("k8s decode: %w", err)
	}

	service := inc.PrimaryAlert().Label("service", "app", "deployment", "container")
	for _, item := range body.Items {
		object := strings.ToLower(item.InvolvedObject.Kind) + "/" + item.InvolvedObject.Name
		// Keep warnings always; keep normal events only when they concern the
		// service the alert is about, otherwise the context fills with noise.
		if !strings.EqualFold(item.Type, "Warning") && service != "" && !strings.Contains(item.InvolvedObject.Name, service) {
			continue
		}
		frag.Events = append(frag.Events, domain.ClusterEvent{
			Timestamp: firstTime(item.LastTimestamp, item.EventTime, item.FirstTimestamp),
			Type:      item.Type,
			Reason:    item.Reason,
			Object:    object,
			Message:   strings.TrimSpace(item.Message),
			Source:    "k8s",
			Count:     item.Count,
		})
	}
	sort.SliceStable(frag.Events, func(i, j int) bool { return frag.Events[i].Timestamp.Before(frag.Events[j].Timestamp) })
	frag.Notes["namespace"] = namespace
	frag.Notes["returned"] = strconv.Itoa(len(frag.Events))
	return frag, nil
}

// httpClient builds a client that trusts the cluster CA, or explicitly does not
// verify when the operator opted in.
func httpClient(cfg config.K8sConfig) (*http.Client, error) {
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}

	switch {
	case cfg.Insecure:
		// Only reachable when WT_K8S_INSECURE=true is set on purpose.
		tlsCfg.InsecureSkipVerify = true
	case cfg.CAFile != "":
		pem, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("k8s: read CA file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("k8s: CA file %s contains no certificates", cfg.CAFile)
		}
		tlsCfg.RootCAs = pool
	}

	return &http.Client{
		Timeout:   15 * time.Second,
		Transport: &http.Transport{TLSClientConfig: tlsCfg},
	}, nil
}

func firstTime(values ...string) time.Time {
	for _, v := range values {
		if v == "" || v == "null" {
			continue
		}
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}
