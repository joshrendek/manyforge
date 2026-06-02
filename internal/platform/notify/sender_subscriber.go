package notify

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/events"
)

// KeySealer is the send path's minimal point-of-use view of the at-rest secret
// sealer: it only ever needs to Open (decrypt) a sealed DKIM private-key ref. The
// production *crypto.Sealer satisfies it, as does the integration test stub. A nil
// Sealer disables custom-identity signing (the system fallback still works).
type KeySealer interface {
	Open(ref string) ([]byte, error)
}

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

	// Sealer decrypts a verified domain's sealed DKIM private-key ref so the reply
	// can be signed as that domain (US4/FR-013). nil ⇒ custom-identity signing is
	// disabled and every reply falls back to the system address (no per-message
	// DKIM). The send NEVER fails because signing was unavailable.
	Sealer KeySealer
}

// Handle is the events.Handler for the ticket.replied topic.
func (s SendSubscriber) Handle(ctx context.Context, tx pgx.Tx, e events.Event) error {
	var p repliedPayload
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return fmt.Errorf("notify: send: unmarshal ticket.replied payload: %w", err)
	}

	// One round-trip via the principal-less DEFINER: current delivery_state (for the
	// idempotency guard), the business's system inbound address (From / VERP Reply-To
	// base), and the reply body (authoritative in the message row, NOT the outbox
	// payload). The function self-asserts the message belongs to (business_id, tenant);
	// zero rows ⇒ message or system address not found. NULL delivery_state / body
	// columns scan into nil *string.
	var (
		deliveryState *string
		sysAddr       string
		bodyText      *string
		bodyHTML      *string
	)
	row := tx.QueryRow(ctx,
		"SELECT delivery_state, system_address, body_text, body_html FROM get_send_context($1, $2, $3)",
		p.MessageRowID, p.BusinessID, e.TenantRootID)
	if err := row.Scan(&deliveryState, &sysAddr, &bodyText, &bodyHTML); err != nil {
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
	if bodyText != nil {
		mail.BodyText = *bodyText
	}
	if bodyHTML != nil {
		mail.BodyHTML = *bodyHTML
	}
	if p.InReplyTo != nil {
		mail.InReplyTo = *p.InReplyTo
	}
	// Auto-Submitted is intentionally NOT set. Per RFC 3834 the header marks
	// machine-generated mail; a US2 reply is human-authored (an agent composed it), so
	// stamping "auto-submitted" would be incorrect and could cause well-behaved
	// recipients to suppress their own auto-responses. Loop cooperation instead rides
	// our stable Reply-To token (threads a reply back rather than spawning a new
	// ticket), and the inbound side's is_auto_reply guard bounds machine loops.

	// Outbound identity selection (FR-013): if this business owns a VERIFIED custom
	// email_domain with a generated DKIM key + a custom inbound_address, send FROM
	// that address and DKIM-sign as that domain. Otherwise keep the system From and
	// leave Mail.DKIM nil. get_send_identity (DEFINER 0023) bypasses RLS — this tx is
	// principal-less, exactly like the get_send_context read above. Custom signing is
	// strictly best-effort: a miss, a nil Sealer, a decrypt failure, or a malformed
	// key NEVER fails the send — it falls back to the always-available system identity.
	s.selectCustomIdentity(ctx, tx, &mail, p.BusinessID, e.TenantRootID)

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

// selectCustomIdentity sets mail.From + mail.DKIM to the business's verified
// custom domain identity when one is available, mutating mail in place. It is a
// no-op (leaving the system From and nil DKIM) on any miss or failure — custom
// signing must NEVER block delivery (FR-013). Failures are logged ONCE at warn,
// with NO secret material (never the sealed ref, the key bytes, or the plaintext).
func (s SendSubscriber) selectCustomIdentity(ctx context.Context, tx pgx.Tx, mail *Mail, businessID, tenantRootID uuid.UUID) {
	if s.Sealer == nil {
		return // no sealer wired ⇒ custom signing disabled; system fallback.
	}

	var (
		fromAddr, dkimDomain, dkimSelector, dkimRef string
	)
	row := tx.QueryRow(ctx,
		"SELECT from_address, dkim_domain, dkim_selector, dkim_private_key_ref FROM get_send_identity($1, $2)",
		businessID, tenantRootID)
	if err := row.Scan(&fromAddr, &dkimDomain, &dkimSelector, &dkimRef); err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			// A real query error (not "no verified domain"): fall back, log once.
			s.logger().WarnContext(ctx, "send: get_send_identity failed; using system identity",
				"business_id", businessID, "err", err)
		}
		return // no verified custom identity ⇒ system fallback (the common case).
	}

	keyBytes, err := s.Sealer.Open(dkimRef)
	if err != nil {
		// Decrypt failed (wrong master key / tampered ref). NEVER log key material.
		s.logger().WarnContext(ctx, "send: could not unseal custom DKIM key; using system identity",
			"business_id", businessID, "dkim_domain", dkimDomain)
		return
	}
	if len(keyBytes) != ed25519.PrivateKeySize {
		s.logger().WarnContext(ctx, "send: malformed custom DKIM key; using system identity",
			"business_id", businessID, "dkim_domain", dkimDomain)
		return
	}

	mail.From = fromAddr
	mail.DKIM = &DKIMConfig{
		Domain:     dkimDomain,
		Selector:   dkimSelector,
		PrivateKey: ed25519.PrivateKey(keyBytes),
	}
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
