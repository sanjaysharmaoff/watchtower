// Package logging is the only place that knows about log/slog.
package logging

import (
	"log/slog"
	"os"
	"strings"

	"github.com/sanjaysharmaoff/watchtower/internal/ports"
)

type logger struct{ l *slog.Logger }

// New builds a ports.Logger. Format is text or json, level is debug|info|warn|error.
func New(level, format string) ports.Logger {
	opts := &slog.HandlerOptions{Level: parseLevel(level)}
	var h slog.Handler
	if strings.EqualFold(format, "json") {
		h = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		h = slog.NewTextHandler(os.Stdout, opts)
	}
	return logger{l: slog.New(h)}
}

func parseLevel(level string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func (g logger) Debug(msg string, kv ...any) { g.l.Debug(msg, kv...) }
func (g logger) Info(msg string, kv ...any)  { g.l.Info(msg, kv...) }
func (g logger) Warn(msg string, kv ...any)  { g.l.Warn(msg, kv...) }
func (g logger) Error(msg string, kv ...any) { g.l.Error(msg, kv...) }

func (g logger) With(kv ...any) ports.Logger { return logger{l: g.l.With(kv...)} }

// Discard is a logger that throws everything away.
func Discard() ports.Logger {
	return logger{l: slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError + 8}))}
}
