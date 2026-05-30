// Package mailer abstracts outbound transactional email (verification,
// invitations). The dev implementation logs; production wires SMTP. A
// suppression check skips hard-bounced addresses (research R6).
package mailer

import (
	"context"
	"log/slog"
)

// Message is a transactional email.
type Message struct {
	To      string
	Subject string
	Body    string
}

// Mailer sends transactional email.
type Mailer interface {
	Send(ctx context.Context, msg Message) error
}

// SuppressionChecker reports whether an address is hard-bounced/suppressed.
type SuppressionChecker interface {
	IsSuppressed(ctx context.Context, email string) (bool, error)
}

// LogMailer writes messages to the logger instead of sending them (dev default).
type LogMailer struct{ Logger *slog.Logger }

// Send logs the message.
func (m LogMailer) Send(ctx context.Context, msg Message) error {
	logger := m.Logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.InfoContext(ctx, "dev mailer: would send", "to", msg.To, "subject", msg.Subject)
	return nil
}

// Guarded wraps a Mailer with a suppression check: suppressed addresses are
// silently skipped (no send, no error) so a bounced address can't be hammered.
type Guarded struct {
	Mailer  Mailer
	Checker SuppressionChecker
}

// Send skips suppressed addresses, otherwise delegates.
func (g Guarded) Send(ctx context.Context, msg Message) error {
	if g.Checker != nil {
		suppressed, err := g.Checker.IsSuppressed(ctx, msg.To)
		if err != nil {
			return err
		}
		if suppressed {
			return nil
		}
	}
	return g.Mailer.Send(ctx, msg)
}
