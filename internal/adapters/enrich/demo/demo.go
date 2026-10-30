// Package demo synthesizes plausible context so the pipeline can be exercised
// end to end without Loki or a cluster. Every line it produces is tagged as
// synthetic, so it can never be mistaken for real evidence.
package demo

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/sanjaysharmaoff/watchtower/internal/domain"
	"github.com/sanjaysharmaoff/watchtower/internal/ports"
	"github.com/sanjaysharmaoff/watchtower/internal/wire"
)

func init() {
	wire.ContextProviders.Register("demo", func(e *wire.Env) (ports.ContextProvider, error) {
		return Provider{clock: e.Clock}, nil
	})
	wire.ContextProviders.Register("none", func(e *wire.Env) (ports.ContextProvider, error) {
		return Empty{}, nil
	})
}

// Provider generates synthetic logs and events shaped by the alert name.
type Provider struct{ clock ports.Clock }

// Name implements ports.ContextProvider.
func (Provider) Name() string { return "demo" }

// Fetch implements ports.ContextProvider.
func (p Provider) Fetch(_ context.Context, inc *domain.Incident) (domain.ContextFragment, error) {
	primary := inc.PrimaryAlert()
	now := p.clock.Now()
	service := primary.Service
	if service == "" {
		service = "app"
	}
	name := strings.ToLower(primary.Name + " " + primary.Summary)

	frag := domain.ContextFragment{
		Provider: "demo",
		Notes:    map[string]string{"synthetic": "true", "warning": "generated locally, not real telemetry"},
	}

	switch {
	case strings.Contains(name, "memory") || strings.Contains(name, "oom"):
		frag.Logs = repeat(now, service, "error", "fatal error: runtime: out of memory (heap 512MiB limit 512MiB)", 3)
		frag.Logs = append(frag.Logs, repeat(now, service, "warn", "gc pressure high: 92% of heap in use", 12)...)
		frag.Events = []domain.ClusterEvent{
			{Timestamp: now.Add(-3 * time.Minute), Type: "Warning", Reason: "OOMKilling", Object: "pod/" + service + "-7d9c", Message: "Memory cgroup out of memory: killed process 1", Source: "demo", Count: 3},
			{Timestamp: now.Add(-2 * time.Minute), Type: "Normal", Reason: "Started", Object: "pod/" + service + "-7d9c", Message: "Started container " + service, Source: "demo"},
		}
	case strings.Contains(name, "crash") || strings.Contains(name, "restart"):
		frag.Logs = repeat(now, service, "error", "panic: runtime error: invalid memory address or nil pointer dereference", 5)
		frag.Logs = append(frag.Logs, repeat(now, service, "info", "starting "+service+" version 4.2.1", 5)...)
		frag.Events = []domain.ClusterEvent{
			{Timestamp: now.Add(-4 * time.Minute), Type: "Warning", Reason: "BackOff", Object: "pod/" + service + "-5f8b", Message: "Back-off restarting failed container", Source: "demo", Count: 7},
		}
	case strings.Contains(name, "latency") || strings.Contains(name, "slow") || strings.Contains(name, "p99"):
		frag.Logs = repeat(now, service, "warn", "upstream request took 4.812s (budget 0.500s) endpoint=/api/v1/checkout", 40)
		frag.Logs = append(frag.Logs, repeat(now, service, "error", "context deadline exceeded calling payments-service:8080", 9)...)
		frag.Events = []domain.ClusterEvent{
			{Timestamp: now.Add(-6 * time.Minute), Type: "Warning", Reason: "FailedComputeMetricsReplicas", Object: "hpa/" + service, Message: "invalid metrics (1 invalid out of 1)", Source: "demo"},
		}
	case strings.Contains(name, "disk") || strings.Contains(name, "space"):
		frag.Logs = repeat(now, service, "error", "write /var/lib/data/segment-0192.log: no space left on device", 6)
		frag.Events = []domain.ClusterEvent{
			{Timestamp: now.Add(-8 * time.Minute), Type: "Warning", Reason: "FreeDiskSpaceFailed", Object: "node/ip-10-0-3-14", Message: "failed to garbage collect required amount of images", Source: "demo"},
		}
	default:
		frag.Logs = repeat(now, service, "error", "connection refused dialing "+service+"-db:5432", 4)
		frag.Logs = append(frag.Logs, repeat(now, service, "warn", "retrying request (attempt 3 of 5)", 15)...)
		frag.Events = []domain.ClusterEvent{
			{Timestamp: now.Add(-5 * time.Minute), Type: "Warning", Reason: "Unhealthy", Object: "pod/" + service + "-1a2b", Message: "Readiness probe failed: HTTP probe failed with statuscode: 503", Source: "demo", Count: 11},
		}
	}
	return frag, nil
}

// repeat emits n copies of a line with drifting timestamps, so the deduper and the
// compressor have something realistic to chew on.
func repeat(now time.Time, service, level, msg string, n int) []domain.LogLine {
	out := make([]domain.LogLine, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, domain.LogLine{
			Timestamp: now.Add(-time.Duration(n-i) * 7 * time.Second),
			Level:     level,
			Message:   fmt.Sprintf("%s req_id=%08x", msg, i*7919),
			Source:    "demo:" + service,
		})
	}
	return out
}

// Empty is a provider that returns nothing, handy for disabling enrichment while
// keeping the stage in place.
type Empty struct{}

// Name implements ports.ContextProvider.
func (Empty) Name() string { return "none" }

// Fetch implements ports.ContextProvider.
func (Empty) Fetch(context.Context, *domain.Incident) (domain.ContextFragment, error) {
	return domain.ContextFragment{Provider: "none"}, nil
}
