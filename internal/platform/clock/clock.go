// Package clock provides time and identity sources as injectable ports.
package clock

import (
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/sanjaysharmaoff/watchtower/internal/ports"
)

// System is the real clock, in UTC so every stored timestamp is comparable.
func System() ports.Clock {
	return ports.ClockFunc(func() time.Time { return time.Now().UTC() })
}

// Fixed is a frozen clock for tests.
func Fixed(at time.Time) ports.Clock {
	return ports.ClockFunc(func() time.Time { return at })
}

// UUID generates v4 identifiers.
func UUID() ports.IDGen {
	return ports.IDFunc(func() string { return uuid.NewString() })
}

// Sequence generates predictable identifiers for tests.
func Sequence(prefix string) ports.IDGen {
	var mu sync.Mutex
	n := 0
	return ports.IDFunc(func() string {
		mu.Lock()
		defer mu.Unlock()
		n++
		return prefix + "-" + time.Now().UTC().Format("20060102") + "-" + itoa(n)
	})
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
