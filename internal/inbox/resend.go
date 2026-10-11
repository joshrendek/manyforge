package inbox

import (
	"context"
	"errors"
	"net/mail"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// maxResendCandidates bounds per-message recipient resolution work.
const maxResendCandidates = 50

// resendSource is the ingest source label suffix (p_source = "inbox:resend").
const resendSource = "resend"

// ReceiveResend ingests one raw message that Resend received for the business
// owning the receiving sending profile (spec 017 R2-R4).
//
// Recipients come from the raw message's To and Cc headers (R3). Only when none
// of them is owned does it consult envelopeRecipients — Resend's authenticated
// Receiving API received_for/to, never the webhook payload — so a Bcc'd support
// address still routes. Every candidate is resolved through
// resolve_inbound_address_for_business, so only custom addresses on verified
// domains owned by businessID can match (R4). The +token reply tag is read from
// the chosen address.
//
// It returns nil for ingested, duplicate, loop-suppressed, and dropped messages
// (a drop writes an audit entry). A non-nil error means the caller should ask
// Resend to retry.
func (s *Service) ReceiveResend(ctx context.Context, businessID uuid.UUID, eventID string, envelopeRecipients []string, raw []byte) error {
	parsed, perr := Parse(raw)
	if perr != nil {
		// Fixed reason only: parser errors can echo message content.
		s.logger.WarnContext(ctx, "inbox: resend parse degraded", "business_id", businessID)
	}
	// A malformed or oversized event id must never make the audit call fail
	// forever (permanent webhook retry); audit it without the id instead.
	eventID = auditableEventID(eventID)
	recipient, err := s.pickOwnedRecipient(ctx, businessID, resendCandidates(parsed.Recipients))
	if err != nil {
		return err
	}
	if recipient == "" {
		recipient, err = s.pickOwnedRecipient(ctx, businessID, resendCandidates(envelopeAddresses(envelopeRecipients)))
		if err != nil {
			return err
		}
	}
	if recipient == "" {
		return s.auditResendDrop(ctx, businessID, eventID, "no_owned_recipient")
	}
	return s.ingestResend(ctx, businessID, eventID, recipient, parsed.From.Address, raw)
}

// ingestResend ingests raw for an already-picked recipient. A route that
// vanished between pick and ingest is an audited drop, not a retry.
func (s *Service) ingestResend(ctx context.Context, businessID uuid.UUID, eventID, recipient, sender string, raw []byte) error {
	_, err := s.Ingest(ctx, RawMessage{
		Provider:           resendSource,
		EnvelopeRecipient:  recipient,
		EnvelopeSender:     sender,
		ReceivedAt:         time.Now().UTC(),
		Raw:                raw,
		RestrictToBusiness: businessID,
	})
	if IsNoRoute(err) {
		// The address or its verification disappeared between pick and ingest.
		return s.auditResendDrop(ctx, businessID, eventID, "route_lost")
	}
	return err
}

// maxEventIDLen mirrors the length guard in support_audit_inbound_dropped.
const maxEventIDLen = 500

// auditableEventID returns eventID when the audit function can store it, else "".
func auditableEventID(eventID string) string {
	if len(eventID) > maxEventIDLen || !utf8.ValidString(eventID) {
		return ""
	}
	for _, r := range eventID {
		if r < 0x20 || r == 0x7f {
			return ""
		}
	}
	return eventID
}

// envelopeAddresses extracts bare addresses from Receiving API entries, which
// may be "Name <addr>"; entries that do not parse are skipped.
func envelopeAddresses(entries []string) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		a, err := mail.ParseAddress(strings.TrimSpace(e))
		if err != nil {
			continue
		}
		out = append(out, a.Address)
	}
	return out
}

// resendCandidates trims, drops non-addresses, de-duplicates on (routing key,
// token) keeping first-seen order and the original token case, and bounds the
// list.
func resendCandidates(addrs []string) []string {
	seen := make(map[string]bool, len(addrs))
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		a = strings.TrimSpace(a)
		if a == "" || !strings.Contains(a, "@") {
			continue
		}
		normalized, token := normalizeRecipient(a)
		key := normalized + "\x00" + token
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, a)
		if len(out) == maxResendCandidates {
			break
		}
	}
	return out
}

// pickOwnedRecipient returns the first candidate that resolves for businessID,
// or "" when none does. One short principal-less read transaction.
func (s *Service) pickOwnedRecipient(ctx context.Context, businessID uuid.UUID, candidates []string) (string, error) {
	if len(candidates) == 0 {
		return "", nil
	}
	var picked string
	err := s.db.WithTx(ctx, func(tx pgx.Tx) error {
		for _, c := range candidates {
			normalized, _ := normalizeRecipient(c)
			_, rerr := resolveRecipientForBusiness(ctx, tx, businessID, normalized)
			if rerr == nil {
				picked = c
				return nil
			}
			if !errors.Is(rerr, errNoRoute) {
				return rerr
			}
		}
		return nil
	})
	return picked, err
}

// auditResendDrop records a dropped Resend-received message (R4). No recipient
// address is logged or audited.
func (s *Service) auditResendDrop(ctx context.Context, businessID uuid.UUID, eventID, reason string) error {
	s.logger.InfoContext(ctx, "inbox: resend message dropped", "business_id", businessID, "event_id", eventID, "reason", reason)
	return s.db.WithTx(ctx, func(tx pgx.Tx) error {
		var ok bool
		return tx.QueryRow(ctx, "SELECT support_audit_inbound_dropped($1, $2, $3, $4)",
			businessID, "inbox:"+resendSource, reason, eventID).Scan(&ok)
	})
}
