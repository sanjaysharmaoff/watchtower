package notify

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/sanjaysharmaoff/watchtower/internal/domain"
	"github.com/sanjaysharmaoff/watchtower/internal/ports"
	"github.com/sanjaysharmaoff/watchtower/internal/wire"
)

func init() {
	wire.Notifiers.Register("stdout", func(e *wire.Env) (ports.Notifier, error) {
		return &Stdout{log: e.Log.With("component", "notify")}, nil
	})
	wire.Notifiers.Register("none", func(e *wire.Env) (ports.Notifier, error) {
		return Discard{}, nil
	})
	wire.Notifiers.Register("memory", func(e *wire.Env) (ports.Notifier, error) {
		return NewCollector(), nil
	})
}

// Stdout prints the plain-text part of a message. Default driver, so a fresh clone
// shows the full pipeline output without any mail configuration.
type Stdout struct {
	log ports.Logger
	mu  sync.Mutex
}

// Name implements ports.Notifier.
func (*Stdout) Name() string { return "stdout" }

// Notify implements ports.Notifier.
func (s *Stdout) Notify(_ context.Context, msg domain.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	fmt.Fprintln(os.Stdout, strings.Repeat("=", 78))
	fmt.Fprintln(os.Stdout, msg.Subject)
	fmt.Fprintln(os.Stdout, strings.Repeat("-", 78))
	if msg.Text != "" {
		fmt.Fprintln(os.Stdout, msg.Text)
	} else {
		fmt.Fprintln(os.Stdout, "(no text part)")
	}
	fmt.Fprintln(os.Stdout, strings.Repeat("=", 78))
	return nil
}

// Discard drops messages.
type Discard struct{}

// Name implements ports.Notifier.
func (Discard) Name() string { return "none" }

// Notify implements ports.Notifier.
func (Discard) Notify(context.Context, domain.Message) error { return nil }

// Collector keeps messages in memory, which makes assertions in tests trivial.
type Collector struct {
	mu   sync.Mutex
	sent []domain.Message
}

// NewCollector builds a collecting notifier.
func NewCollector() *Collector { return &Collector{} }

// Name implements ports.Notifier.
func (*Collector) Name() string { return "memory" }

// Notify implements ports.Notifier.
func (c *Collector) Notify(_ context.Context, msg domain.Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, msg)
	return nil
}

// Sent returns a copy of everything delivered so far.
func (c *Collector) Sent() []domain.Message {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]domain.Message(nil), c.sent...)
}
