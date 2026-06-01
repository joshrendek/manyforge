package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/events"
)

// repliedPayload decodes the ticket.replied outbox payload. It is a CONSUMER-owned
// typed struct: the producer (ticketing.Service.Reply) enqueues a map[string]any, and
// this subscriber declares its own struct to decode it (the house pattern — see
// inbox.businessCreatedPayload). The json tags MUST match the keys Reply enqueues
// EXACTLY, or a decode silently yields zero values.
type repliedPayload struct {
	MessageRowID uuid.UUID `json:"message_row_id"`
	Recipient    string    `json:"recipient"`
	Subject      string    `json:"subject"`
	RFCMessageID string    `json:"rfc_message_id"`
	InReplyTo    *string   `json:"in_reply_to"`
	References   []string  `json:"references"`
	ReplyToken   string    `json:"reply_token"`
	BusinessID   uuid.UUID `json:"business_id"`
}

// SendSubscriber dispatches queued replies (FR-008): it drains a ticket.replied
// outbox event into a threaded Mail and hands it to the Sender, recording the
// delivery outcome. From/Reply-To are built on the business's system inbound address
// (US2: system identity only).
//
// It runs INSIDE the outbox worker's per-event SAVEPOINT tx (so the delivery_state
// write commits atomically with the event being marked processed). Crucially, that tx
// is PRINCIPAL-LESS — the worker holds no manyforge.principal_id GUC — so its reads
// and writes to the RLS-protected ticket_message / inbound_address tables go through
// the SECURITY DEFINER functions get_send_context + mark_message_delivery (migration
// 0019), NOT plain sqlc queries (which would silently match zero rows under RLS with
// no principal; manyforge-0fq).
//
// The handler MUST be idempotent: outbox delivery is at-least-once, so a message
// already 'sent' is skipped (no second Sender.Send). A suppressed recipient is a
// terminal failure (mark 'failed', return nil — no retry); any other Send error is
// returned so the worker reschedules with backoff.
type SendSubscriber struct {
	Sender Sender
	Logger *slog.Logger
}

// Handle is the events.Handler for the ticket.replied topic.
func (s SendSubscriber) Handle(ctx context.Context, tx pgx.Tx, e events.Event) error {
	var p repliedPayload
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return fmt.Errorf("notify: send: unmarshal ticket.replied payload: %w", err)
	}

	// One round-trip via the principal-less DEFINER: current delivery_state (for the
	// idempotency guard) + the business's system inbound address (From / VERP Reply-To
	// base). The function self-asserts the message belongs to (business_id, tenant);
	// zero rows ⇒ message or system address not found. A NULL delivery_state (column
	// nullable for inbound/note rows) scans into a nil *string.
	var (
		deliveryState *string
		sysAddr       string
	)
	row := tx.QueryRow(ctx,
		"SELECT delivery_state, system_address FROM get_send_context($1, $2, $3)",
		p.MessageRowID, p.BusinessID, e.TenantRootID)
	if err := row.Scan(&deliveryState, &sysAddr); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// No such outbound message for this business/tenant, or the business has no
			// system inbound address. Either is non-retryable (a replay would behave
			// identically); treat as a terminal no-op so the worker marks the event
			// processed rather than spinning on backoff.
			s.logger().WarnContext(ctx, "send: no send context for message; dropping",
				"message_row_id", p.MessageRowID, "business_id", p.BusinessID)
			return nil
		}
		return fmt.Errorf("notify: send: load send context: %w", err)
	}

	// Idempotency: a replayed event for an already-sent message is a no-op.
	if deliveryState != nil && *deliveryState == "sent" {
		return nil
	}

	replyTo := verpAddress(sysAddr, p.ReplyToken)
	mail := Mail{
		From:         sysAddr,
		To:           p.Recipient,
		Subject:      p.Subject,
		MessageID:    p.RFCMessageID,
		References:   p.References,
		ReplyTo:      replyTo,
		EnvelopeFrom: replyTo, // VERP return-path so DSNs/bounces are correlatable
	}
	if p.InReplyTo != nil {
		mail.InReplyTo = *p.InReplyTo
	}

	if serr := s.Sender.Send(ctx, mail); serr != nil {
		if errors.Is(serr, ErrSuppressed) {
			// Terminal: the recipient is hard-bounced/suppressed. Record the failure
			// and return nil so the worker does NOT retry (a retry would never succeed).
			if merr := s.mark(ctx, tx, p.MessageRowID, e.TenantRootID, "failed", "recipient suppressed"); merr != nil {
				return merr
			}
			return nil
		}
		// Transient (relay down, timeout, …): return the error so the worker
		// reschedules with backoff. delivery_state stays 'pending'.
		return fmt.Errorf("notify: send: dispatch: %w", serr)
	}

	return s.mark(ctx, tx, p.MessageRowID, e.TenantRootID, "sent", "")
}

// mark records the delivery outcome via the SECURITY DEFINER (RLS-bypassing) function.
// errReason is the empty string for success (passed to SQL as NULL).
func (s SendSubscriber) mark(ctx context.Context, tx pgx.Tx, messageID, tenantRootID uuid.UUID, state, errReason string) error {
	var errArg any
	if errReason != "" {
		errArg = errReason
	}
	if _, err := tx.Exec(ctx,
		"SELECT mark_message_delivery($1, $2, $3, $4)",
		messageID, tenantRootID, state, errArg); err != nil {
		return fmt.Errorf("notify: send: mark %s: %w", state, err)
	}
	return nil
}

func (s SendSubscriber) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

// verpAddress inserts +token before the '@' of the system address, producing the
// VERP Reply-To (support+<token>@<domain>) that threads an inbound reply back onto
// the ticket. The token's case is PRESERVED — the inbound normalizer preserves
// plus-token case (manyforge-btv), so a lowercased token here would never match.
// An address with no '@' is returned unchanged.
func verpAddress(sysAddr, token string) string {
	at := -1
	for i := len(sysAddr) - 1; i >= 0; i-- {
		if sysAddr[i] == '@' {
			at = i
			break
		}
	}
	if at < 0 {
		return sysAddr
	}
	return sysAddr[:at] + "+" + token + sysAddr[at:]
}
