// Package correlate groups related failing services into one incident.
package correlate

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sanjaysharmaoff/watchtower/internal/domain"
	"github.com/sanjaysharmaoff/watchtower/internal/ports"
	"github.com/sanjaysharmaoff/watchtower/internal/wire"
)

func init() {
	wire.Correlators.Register("window", func(e *wire.Env) (ports.Correlator, error) {
		return NewWindow(e.Clock, e.Cfg.Ingest.CorrelationKeys, e.Cfg.Ingest.CorrelationWin), nil
	})
	wire.Correlators.Register("none", func(e *wire.Env) (ports.Correlator, error) {
		return noop{}, nil
	})
}

type noop struct{}

func (noop) Correlate(context.Context, *domain.Incident) error { return nil }

// Window correlates by shared topology labels inside a sliding time window: if
// checkout and payments both start failing in the same namespace within five
// minutes, the SRE gets one incident with both alerts instead of two emails.
type Window struct {
	clock  ports.Clock
	keys   []string
	window time.Duration

	mu     sync.Mutex
	groups map[string]*group
}

type group struct {
	alerts   []domain.Alert
	lastSeen time.Time
}

// NewWindow builds a windowed correlator.
func NewWindow(c ports.Clock, keys []string, window time.Duration) *Window {
	if len(keys) == 0 {
		keys = []string{"cluster", "namespace", "service"}
	}
	if window <= 0 {
		window = 5 * time.Minute
	}
	return &Window{clock: c, keys: keys, window: window, groups: map[string]*group{}}
}

// Correlate implements ports.Correlator.
func (w *Window) Correlate(_ context.Context, inc *domain.Incident) error {
	key := w.keyOf(inc)
	now := w.clock.Now()

	w.mu.Lock()
	defer w.mu.Unlock()
	w.prune(now)

	g, ok := w.groups[key]
	if !ok {
		g = &group{}
		w.groups[key] = g
	}

	added := inc.MergeAlerts(g.alerts)
	g.alerts = dedupe(append(g.alerts, inc.Alerts...))
	g.lastSeen = now

	inc.CorrelationKey = key
	inc.SetMeta("correlation_key", key)
	inc.SetMeta("correlated_alerts", itoa(len(inc.Alerts)))
	if added > 0 {
		inc.Note("correlated %d recent alert(s) on %s", added, key)
	}
	inc.Recompute()
	inc.CorrelationKey = key
	return nil
}

// keyOf builds the grouping key from the configured label set, falling back to
// the alert target so unlabelled alerts still group sensibly.
func (w *Window) keyOf(inc *domain.Incident) string {
	primary := inc.PrimaryAlert()
	parts := make([]string, 0, len(w.keys))
	for _, k := range w.keys {
		if v := primary.Label(k); v != "" {
			parts = append(parts, k+"="+v)
		}
	}
	if len(parts) == 0 {
		return "target=" + primary.Target()
	}
	return strings.Join(parts, ",")
}

func (w *Window) prune(now time.Time) {
	for k, g := range w.groups {
		if now.Sub(g.lastSeen) > w.window {
			delete(w.groups, k)
		}
	}
}

func dedupe(in []domain.Alert) []domain.Alert {
	seen := make(map[string]struct{}, len(in))
	out := make([]domain.Alert, 0, len(in))
	for _, a := range in {
		id := a.Identity()
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, a)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Severity.Rank() > out[j].Severity.Rank() })
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
