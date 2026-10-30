package domain

import (
	"errors"
	"fmt"
)

var (
	// ErrDrop signals a deliberate, non-exceptional stop: a debounced duplicate,
	// a resolved alert, nothing worth spending tokens on.
	ErrDrop = errors.New("dropped")
	// ErrBlocked signals a policy refusal from a guardrail.
	ErrBlocked = errors.New("blocked by policy")
	// ErrNotFound is returned by stores when a record does not exist.
	ErrNotFound = errors.New("not found")
	// ErrUnsupported is returned by adapters asked to do something they cannot.
	ErrUnsupported = errors.New("unsupported")
	// ErrUnavailable is a retryable dependency failure.
	ErrUnavailable = errors.New("dependency unavailable")
)

// Drop wraps ErrDrop with a human reason.
func Drop(reason string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrDrop, fmt.Sprintf(reason, args...))
}

// Block wraps ErrBlocked with a human reason.
func Block(reason string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrBlocked, fmt.Sprintf(reason, args...))
}

// IsDrop reports whether the error is a deliberate stop.
func IsDrop(err error) bool { return errors.Is(err, ErrDrop) }

// IsBlocked reports whether the error is a policy refusal.
func IsBlocked(err error) bool { return errors.Is(err, ErrBlocked) }
