package ports

import "time"

// Logger is a structured logger seam. Nothing in the codebase imports slog
// directly except the one adapter that implements this.
type Logger interface {
	Debug(msg string, kv ...any)
	Info(msg string, kv ...any)
	Warn(msg string, kv ...any)
	Error(msg string, kv ...any)
	With(kv ...any) Logger
}

// Clock keeps time injectable, which makes debouncing and retention testable.
type Clock interface {
	Now() time.Time
}

// IDGen hands out identifiers.
type IDGen interface {
	New() string
}

// ClockFunc adapts a function to Clock.
type ClockFunc func() time.Time

// Now implements Clock.
func (f ClockFunc) Now() time.Time { return f() }

// IDFunc adapts a function to IDGen.
type IDFunc func() string

// New implements IDGen.
func (f IDFunc) New() string { return f() }
