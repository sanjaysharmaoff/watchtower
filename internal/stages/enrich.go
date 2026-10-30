package stages

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/sanjaysharmaoff/watchtower/internal/domain"
	"github.com/sanjaysharmaoff/watchtower/internal/ports"
)

// Enrich fans out to every configured context provider in parallel, with a hard
// timeout, and folds whatever came back into the envelope. A provider that fails
// or hangs costs a note in the trace, not the incident.
func Enrich(providers []ports.ContextProvider, timeout time.Duration) Stage {
	return mutator("enrich", func(ctx context.Context, inc *domain.Incident) error {
		if len(providers) == 0 {
			inc.Note("no context providers configured")
			return nil
		}
		if timeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, timeout)
			defer cancel()
		}

		var (
			mu        sync.Mutex
			fragments = make([]domain.ContextFragment, 0, len(providers))
			wg        sync.WaitGroup
		)

		for _, p := range providers {
			wg.Add(1)
			go func(p ports.ContextProvider) {
				defer wg.Done()
				defer func() {
					if r := recover(); r != nil {
						mu.Lock()
						fragments = append(fragments, domain.ContextFragment{Provider: p.Name(), Err: "panic during fetch"})
						mu.Unlock()
					}
				}()

				start := time.Now()
				frag, err := p.Fetch(ctx, inc)
				frag.Provider = p.Name()
				frag.Took = time.Since(start)
				if err != nil {
					frag.Err = err.Error()
				}
				mu.Lock()
				fragments = append(fragments, frag)
				mu.Unlock()
			}(p)
		}
		wg.Wait()

		// Deterministic order regardless of which provider answered first.
		sort.Slice(fragments, func(i, j int) bool { return fragments[i].Provider < fragments[j].Provider })
		for _, f := range fragments {
			inc.AddFragment(f)
			if f.Err != "" {
				inc.Note("provider %s failed: %s", f.Provider, f.Err)
				continue
			}
			inc.Note("provider %s: %d logs, %d events in %s", f.Provider, len(f.Logs), len(f.Events), f.Took.Round(time.Millisecond))
		}

		sort.SliceStable(inc.Logs, func(i, j int) bool { return inc.Logs[i].Timestamp.Before(inc.Logs[j].Timestamp) })
		sort.SliceStable(inc.Events, func(i, j int) bool { return inc.Events[i].Timestamp.Before(inc.Events[j].Timestamp) })
		return nil
	})
}

// Sanitize runs the log processor chain: dedupe, keep signal, compress, cap. Each
// link is a port, so the chain is configuration rather than code.
func Sanitize(processors []ports.LogProcessor) Stage {
	return mutator("sanitize", func(_ context.Context, inc *domain.Incident) error {
		before := len(inc.Logs)
		lines := inc.Logs
		for _, p := range processors {
			lines = p.Process(lines)
		}
		inc.Logs = lines
		inc.SetMeta("logs_in", itoa(before))
		inc.SetMeta("logs_out", itoa(len(lines)))
		if before > 0 {
			inc.Note("sanitizer kept %d of %d log lines", len(lines), before)
		}
		return nil
	})
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
