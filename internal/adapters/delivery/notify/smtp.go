// Package notify delivers rendered messages.
package notify

import (
	"context"
	"crypto/tls"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"strings"
	"time"

	"github.com/sanjaysharmaoff/watchtower/internal/domain"
	"github.com/sanjaysharmaoff/watchtower/internal/ports"
	"github.com/sanjaysharmaoff/watchtower/internal/wire"
)

func init() {
	wire.Notifiers.Register("smtp", func(e *wire.Env) (ports.Notifier, error) {
		cfg := e.Cfg.Notify
		if cfg.SMTPHost == "" {
			return nil, fmt.Errorf("smtp: WT_SMTP_HOST is not set")
		}
		return &SMTP{
			host:     cfg.SMTPHost,
			port:     cfg.SMTPPort,
			username: cfg.SMTPUser,
			password: cfg.SMTPPassword,
			useTLS:   cfg.SMTPTLS,
			from:     cfg.From,
			to:       cfg.To,
			clock:    e.Clock,
			ids:      e.IDs,
			log:      e.Log.With("component", "smtp"),
		}, nil
	})
}

// SMTP delivers multipart/alternative email through a relay such as Amazon SES.
type SMTP struct {
	host     string
	port     int
	username string
	password string
	useTLS   bool
	from     string
	to       []string
	clock    ports.Clock
	ids      ports.IDGen
	log      ports.Logger
}

// Name implements ports.Notifier.
func (s *SMTP) Name() string { return "smtp" }

// Notify implements ports.Notifier.
func (s *SMTP) Notify(ctx context.Context, msg domain.Message) error {
	to := msg.To
	if len(to) == 0 {
		to = s.to
	}
	if len(to) == 0 {
		return fmt.Errorf("smtp: no recipients configured")
	}

	body := s.build(msg, to)
	addr := net.JoinHostPort(s.host, itoa(s.port))

	// Dial with the caller's deadline so a wedged relay cannot pin a worker.
	deadline := time.Now().Add(30 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	conn, err := (&net.Dialer{Deadline: deadline}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("smtp: dial %s: %w", addr, err)
	}
	_ = conn.SetDeadline(deadline)

	client, err := smtp.NewClient(conn, s.host)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("smtp: handshake: %w", err)
	}
	defer func() { _ = client.Quit() }()

	if s.useTLS {
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(&tls.Config{ServerName: s.host, MinVersion: tls.VersionTLS12}); err != nil {
				return fmt.Errorf("smtp: starttls: %w", err)
			}
		} else {
			return fmt.Errorf("smtp: relay does not offer STARTTLS but TLS is required")
		}
	}
	if s.username != "" {
		if err := client.Auth(smtp.PlainAuth("", s.username, s.password, s.host)); err != nil {
			return fmt.Errorf("smtp: auth: %w", err)
		}
	}
	if err := client.Mail(s.from); err != nil {
		return fmt.Errorf("smtp: mail from: %w", err)
	}
	for _, rcpt := range to {
		if err := client.Rcpt(rcpt); err != nil {
			return fmt.Errorf("smtp: rcpt %s: %w", rcpt, err)
		}
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("smtp: data: %w", err)
	}
	if _, err := w.Write([]byte(body)); err != nil {
		_ = w.Close()
		return fmt.Errorf("smtp: write body: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp: close body: %w", err)
	}

	s.log.Info("email delivered", "to", strings.Join(to, ","), "subject", msg.Subject)
	return nil
}

// build assembles a MIME multipart/alternative message with a plain-text part, so
// clients that refuse HTML still get the runbook.
func (s *SMTP) build(msg domain.Message, to []string) string {
	boundary := "wt-" + s.ids.New()
	text := msg.Text
	if text == "" {
		text = msg.Subject
	}

	var b strings.Builder
	b.WriteString("From: " + s.from + "\r\n")
	b.WriteString("To: " + strings.Join(to, ", ") + "\r\n")
	b.WriteString("Subject: " + mime.QEncoding.Encode("utf-8", msg.Subject) + "\r\n")
	b.WriteString("Date: " + s.clock.Now().Format(time.RFC1123Z) + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	for k, v := range msg.Meta {
		b.WriteString("X-WatchTower-" + strings.ToUpper(k) + ": " + sanitizeHeader(v) + "\r\n")
	}
	b.WriteString("Content-Type: multipart/alternative; boundary=\"" + boundary + "\"\r\n\r\n")

	b.WriteString("--" + boundary + "\r\n")
	b.WriteString("Content-Type: text/plain; charset=\"utf-8\"\r\n\r\n")
	b.WriteString(text + "\r\n\r\n")

	if msg.HTML != "" {
		b.WriteString("--" + boundary + "\r\n")
		b.WriteString("Content-Type: text/html; charset=\"utf-8\"\r\n\r\n")
		b.WriteString(msg.HTML + "\r\n\r\n")
	}
	b.WriteString("--" + boundary + "--\r\n")
	return b.String()
}

// sanitizeHeader strips CR/LF so metadata can never inject extra headers.
func sanitizeHeader(v string) string {
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(v)
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
