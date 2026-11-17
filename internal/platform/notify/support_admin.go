package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/events"
	"github.com/manyforge/manyforge/internal/platform/mailer"
)

// SupportAdminSubscriber queues a separate delivery for each administrator so a
// failed recipient does not resend alerts already accepted for other admins.
type SupportAdminSubscriber struct {
	Sender        Sender
	From          string
	BaseURL       string
	ProfileSender SupportProfileSender
	Suppression   mailer.SuppressionChecker
}

// Queue consumes message.received and atomically creates deduplicated alerts and
// delivery events for all eligible admins. The database validates message scope.
func (s SupportAdminSubscriber) Queue(ctx context.Context, tx pgx.Tx, e events.Event) error {
	var p struct {
		BusinessID uuid.UUID `json:"business_id"`
		TicketID   uuid.UUID `json:"ticket_id"`
		MessageID  uuid.UUID `json:"message_id"`
	}
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return fmt.Errorf("support admin: invalid inbound event")
	}
	_, err := tx.Exec(ctx, "SELECT queue_support_admin_notifications($1,$2,$3,$4)", e.TenantRootID, p.BusinessID, p.TicketID, p.MessageID)
	if err != nil {
		return fmt.Errorf("support admin: queue: %w", err)
	}
	return nil
}

// Send rechecks access and delivers one alert using the business support profile
// or, when absent, the shared transport. Failures retry up to the event limit.
func (s SupportAdminSubscriber) Send(ctx context.Context, tx pgx.Tx, e events.Event) error {
	var p struct {
		NotificationID uuid.UUID `json:"notification_id"`
	}
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return fmt.Errorf("support admin: invalid notification event")
	}
	var recipient, business, subject string
	var businessID, ticketID uuid.UUID
	err := tx.QueryRow(ctx, "SELECT recipient, business_id, ticket_id, business_name, subject FROM get_support_admin_notification($1,$2)", e.TenantRootID, p.NotificationID).Scan(&recipient, &businessID, &ticketID, &business, &subject)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("support admin: context: %w", err)
	}
	identity, hasProfile, err := (SendSubscriber{}).resendSupportIdentity(ctx, tx, businessID, e.TenantRootID)
	if err != nil {
		return fmt.Errorf("support admin: identity: %w", err)
	}
	fromAddress := s.From
	if hasProfile {
		fromAddress = identity.fromAddress
	}
	from, err := mail.ParseAddress(fromAddress)
	if err != nil || (!hasProfile && s.Sender == nil) || (hasProfile && (s.ProfileSender == nil || identity.profileStatus != "verified")) {
		return s.deliveryFailure(ctx, tx, e, p.NotificationID)
	}
	if s.Suppression != nil {
		suppressed, err := s.Suppression.IsSuppressed(ctx, recipient)
		if err != nil {
			return s.deliveryFailure(ctx, tx, e, p.NotificationID)
		}
		if suppressed {
			return finishAdminNotification(ctx, tx, e.TenantRootID, p.NotificationID, "suppressed")
		}
	}
	domain := from.Address[strings.LastIndexByte(from.Address, '@')+1:]
	link := strings.TrimRight(s.BaseURL, "/") + "/support/" + businessID.String() + "/" + ticketID.String()
	message := Mail{
		From: fromAddress, To: recipient,
		Subject:       "Support email: " + strings.Join(strings.Fields(subject), " "),
		BodyText:      fmt.Sprintf("A customer sent a support message to %s.\n\nSubject: %s\n\nView and reply in ManyForge:\n%s\n", business, subject, link),
		MessageID:     "support-admin-" + p.NotificationID.String() + "@" + domain,
		AutoSubmitted: "auto-generated",
		ExtraHeaders:  map[string]string{"X-Auto-Response-Suppress": "All", "Auto-Submitted": "auto-generated"},
	}
	if hasProfile {
		err = s.ProfileSender.SendSupportReply(ctx, identity.profileID, message)
	} else {
		err = s.Sender.Send(ctx, message)
	}
	state := "sent"
	if errors.Is(err, ErrSuppressed) {
		state = "suppressed"
	} else if err != nil {
		return s.deliveryFailure(ctx, tx, e, p.NotificationID)
	}
	return finishAdminNotification(ctx, tx, e.TenantRootID, p.NotificationID, state)
}

func (s SupportAdminSubscriber) deliveryFailure(ctx context.Context, tx pgx.Tx, e events.Event, id uuid.UUID) error {
	if e.IsFinalAttempt() {
		return finishAdminNotification(ctx, tx, e.TenantRootID, id, "failed")
	}
	// Transport errors can contain credentials, addresses or content.
	return errors.New("support admin: email delivery not accepted")
}

func finishAdminNotification(ctx context.Context, tx pgx.Tx, tenant, id uuid.UUID, state string) error {
	_, err := tx.Exec(ctx, "SELECT finish_support_admin_notification($1,$2,$3)", tenant, id, state)
	return err
}
