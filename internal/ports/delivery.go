package ports

import (
	"context"

	"github.com/sanjaysharmaoff/watchtower/internal/domain"
)

// Renderer turns domain objects into a deliverable message.
type Renderer interface {
	Name() string
	RenderIncident(inc *domain.Incident) (domain.Message, error)
	RenderDigest(d domain.Digest) (domain.Message, error)
}

// Notifier delivers a rendered message. SMTP, stdout, chat webhook: same seam.
type Notifier interface {
	Name() string
	Notify(ctx context.Context, msg domain.Message) error
}

// Scheduler runs named jobs on a cron spec.
type Scheduler interface {
	Schedule(name, spec string, job func(context.Context)) error
	Start()
	Stop(ctx context.Context) error
}

// Signer produces and verifies the tokens embedded in approval links, so email
// buttons cannot be forged.
type Signer interface {
	Sign(payload string) string
	Verify(payload, signature string) bool
}
