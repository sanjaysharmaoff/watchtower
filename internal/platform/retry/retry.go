// Package retry is a minimal backoff helper for flaky dependencies.
package retry

import (
	"context"
	"errors"
	"math"
	"time"
)

// Policy describes the backoff schedule.
type Policy struct {
	Attempts int
	Base     time.Duration
	Max      time.Duration
}

// Default is a sane policy for network calls.
func Default() Policy {
	return Policy{Attempts: 3, Base: 200 * time.Millisecond, Max: 2 * time.Second}
}

// Do runs fn until it succeeds, the context ends, or attempts run out. Returning
// a Permanent error stops retrying immediately.
func Do(ctx context.Context, p Policy, fn func(ctx context.Context) error) error {
	if p.Attempts <= 0 {
		p.Attempts = 1
	}
	var last error
	for attempt := 0; attempt < p.Attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return errors.Join(last, err)
		}
		last = fn(ctx)
		if last == nil {
			return nil
		}
		var perm permanent
		if errors.As(last, &perm) {
			return perm.err
		}
		if attempt == p.Attempts-1 {
			break
		}
		wait := time.Duration(float64(p.Base) * math.Pow(2, float64(attempt)))
		if p.Max > 0 && wait > p.Max {
			wait = p.Max
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return errors.Join(last, ctx.Err())
		case <-timer.C:
		}
	}
	return last
}

type permanent struct{ err error }

func (p permanent) Error() string { return p.err.Error() }
func (p permanent) Unwrap() error { return p.err }

// Permanent marks an error as not worth retrying.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanent{err: err}
}
