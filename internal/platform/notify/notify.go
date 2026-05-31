package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
	"github.com/manyforge/manyforge/internal/platform/mailer"
)

// Notification is an in-app notification to a member principal (SL-D).
type Notification struct {
	TenantRootID uuid.UUID
	PrincipalID  uuid.UUID
	Kind         string         // e.g. "ticket.new", "ticket.assigned", "ticket.replied"
	Ref          map[string]any // deep-link payload: {ticket_id, business_id, …}
}

// InApp writes an in-app notification in the given transaction (so it commits
// atomically with the outbox row that triggered it). The recipient reads it back
// under their own RLS scope (notification policy: principal_id = current_principal()).
func InApp(ctx context.Context, tx pgx.Tx, n Notification) error {
	raw, err := json.Marshal(n.Ref)
	if err != nil {
		return fmt.Errorf("marshal notification ref: %w", err)
	}
	id, err := uuid.NewV7()
	if err != nil {
		return fmt.Errorf("notification id: %w", err)
	}
	return dbgen.New(tx).InsertNotification(ctx, dbgen.InsertNotificationParams{
		ID:           id,
		TenantRootID: n.TenantRootID,
		PrincipalID:  n.PrincipalID,
		Kind:         n.Kind,
		Ref:          raw,
	})
}

// Mail is a threaded, optionally domain-authenticated outbound message (FR-008/
// FR-013). It extends the foundation's minimal mailer.Message with the RFC822
// threading headers and the sending identity that make a support reply continue
// the customer's conversation and send as the business's brand.
type Mail struct {
	From       string   // verified custom identity, else the system address (FR-013)
	To         string   // the requester
	Subject    string   // carries the [#<reply_token>] tag
	BodyText   string
	BodyHTML   string
	MessageID  string   // RFC822 Message-ID we mint for this outbound message
	InReplyTo  string   // the message being replied to
	References []string // the thread chain
	ReplyTo    string   // support+<reply_token>@<domain> (VERP threading fallback)
}

// ErrSuppressed is returned when the recipient is hard-bounced/suppressed (FR-013).
var ErrSuppressed = errors.New("recipient suppressed")

// Sender dispatches threaded outbound mail, refusing suppressed recipients.
type Sender interface {
	Send(ctx context.Context, m Mail) error
}

// LogSender is the dev default: it logs the full threaded message (so reply/
// notification flows are completable without a real MTA) and honors suppression.
// Production wires a real SMTP+DKIM sender behind the same interface (US2/US4).
type LogSender struct {
	Logger      *slog.Logger
	Suppression mailer.SuppressionChecker // optional; nil skips the check
}

// Send logs the message after a suppression check.
func (s LogSender) Send(ctx context.Context, m Mail) error {
	if s.Suppression != nil {
		suppressed, err := s.Suppression.IsSuppressed(ctx, m.To)
		if err != nil {
			return fmt.Errorf("suppression check: %w", err)
		}
		if suppressed {
			return ErrSuppressed
		}
	}
	logger := s.Logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.InfoContext(ctx, "dev mailer: would send threaded reply",
		"from", m.From, "to", m.To, "subject", m.Subject,
		"message_id", m.MessageID, "in_reply_to", m.InReplyTo, "reply_to", m.ReplyTo,
		"body", m.BodyText)
	return nil
}
