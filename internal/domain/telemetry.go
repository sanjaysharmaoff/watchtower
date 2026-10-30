package domain

import (
	"fmt"
	"strings"
	"time"
)

// LogLine is one normalized log record, regardless of whether it came from
// Loki, Elasticsearch, a file tail or a demo generator.
type LogLine struct {
	Timestamp   time.Time         `json:"ts"`
	Level       string            `json:"level,omitempty"`
	Message     string            `json:"message"`
	Source      string            `json:"source,omitempty"`
	Occurrences int               `json:"occurrences,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
}

// String renders the line for prompts and emails.
func (l LogLine) String() string {
	ts := ""
	if !l.Timestamp.IsZero() {
		ts = l.Timestamp.UTC().Format(time.RFC3339)
	}
	line := strings.TrimSpace(fmt.Sprintf("%s %s %s", ts, strings.ToUpper(l.Level), l.Message))
	if l.Occurrences > 1 {
		line = fmt.Sprintf("%s (x%d)", line, l.Occurrences)
	}
	return line
}

// LevelRank orders log levels so filters can keep only meaningful signal.
func (l LogLine) LevelRank() int {
	switch strings.ToLower(strings.Trim(l.Level, "[]")) {
	case "fatal", "panic", "critical", "crit":
		return 5
	case "error", "err", "eror":
		return 4
	case "warn", "warning":
		return 3
	case "info", "notice":
		return 2
	case "debug", "trace":
		return 1
	default:
		return 0
	}
}

// LevelRankOf is the level ordering for a bare level string.
func LevelRankOf(level string) int {
	return LogLine{Level: level}.LevelRank()
}

// ClusterEvent is a normalized orchestrator event (Kubernetes today, anything
// with a Reason/Object/Message shape tomorrow).
type ClusterEvent struct {
	Timestamp time.Time `json:"ts"`
	Type      string    `json:"type,omitempty"`
	Reason    string    `json:"reason,omitempty"`
	Object    string    `json:"object,omitempty"`
	Message   string    `json:"message"`
	Source    string    `json:"source,omitempty"`
	Count     int       `json:"count,omitempty"`
}

// String renders the event for prompts and emails.
func (e ClusterEvent) String() string {
	ts := ""
	if !e.Timestamp.IsZero() {
		ts = e.Timestamp.UTC().Format(time.RFC3339)
	}
	return strings.TrimSpace(fmt.Sprintf("%s %s %s %s: %s", ts, e.Type, e.Reason, e.Object, e.Message))
}

// ContextFragment is whatever one enrichment provider managed to collect. A
// provider that fails returns a fragment carrying Err instead of blowing up the
// pipeline.
type ContextFragment struct {
	Provider string            `json:"provider"`
	Logs     []LogLine         `json:"logs,omitempty"`
	Events   []ClusterEvent    `json:"events,omitempty"`
	Notes    map[string]string `json:"notes,omitempty"`
	Err      string            `json:"error,omitempty"`
	Took     time.Duration     `json:"took,omitempty"`
}
