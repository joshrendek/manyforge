package ticketing

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/audit"
	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
	"github.com/manyforge/manyforge/internal/platform/events"
)

// RedactTicket soft-deletes (redacts in place) a ticket the caller holds tickets.delete
// for. It NEVER hard-DELETEs (Principle VI / FR-014): in one tx under the caller's RLS
// context it blanks the ticket subject, every message body, and every attachment
// filename; stamps redacted_at (excluding the ticket from get/list/messages); writes a
// single ticket.redacted audit carrying SCOPE METADATA ONLY (message/attachment counts,
// no PII); and enqueues one attachment.purge per blob so the worker deletes the bytes
// out-of-band. The shared requester row is deliberately untouched — it is deduped across
// tickets, and requester/account erasure is the 001 path (out of scope; research R7).
//
// Idempotent + no-oracle: an unknown, foreign-tenant, or already-redacted ticket all
// match zero rows in RedactTicket and surface as ErrNotFound (one indistinguishable
// shape). A principal IS present here (unlike principal-less ingest), so the cross-table
// blanking runs under WithPrincipal — no SECURITY DEFINER needed (the ticket/
// ticket_message/attachment RLS policies authorize the holder's writes).
func (s *Service) RedactTicket(ctx context.Context, principalID, businessID, ticketID uuid.UUID) error {
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)

		// Soft-delete the ticket itself. Zero rows ⇒ pgx.ErrNoRows ⇒ ErrNotFound
		// (unknown / foreign / already-redacted — all one no-oracle shape).
		red, err := q.RedactTicket(ctx, dbgen.RedactTicketParams{ID: ticketID, BusinessID: businessID})
		if err != nil {
			return err
		}

		// Blank message bodies; rows affected is the audit scope count.
		msgCount, err := q.BlankTicketMessages(ctx, dbgen.BlankTicketMessagesParams{
			TicketID: ticketID, BusinessID: businessID,
		})
		if err != nil {
			return err
		}

		// Collect blob keys BEFORE blanking filenames (blanking leaves blob_key intact,
		// but collecting first keeps the intent obvious), then blank the filenames.
		blobs, err := q.ListTicketAttachmentBlobs(ctx, dbgen.ListTicketAttachmentBlobsParams{
			TicketID: ticketID, BusinessID: businessID,
		})
		if err != nil {
			return err
		}
		attCount, err := q.BlankTicketAttachments(ctx, dbgen.BlankTicketAttachmentsParams{
			TicketID: ticketID, BusinessID: businessID,
		})
		if err != nil {
			return err
		}

		// Audit in-tx (FR-014): scope metadata only — proves WHAT was redacted and
		// when, retains zero requester PII (no subject text, no hash).
		if aerr := audit.Write(ctx, tx, audit.Entry{
			BusinessID:       &businessID,
			TenantRootID:     &red.TenantRootID,
			ActorPrincipalID: &principalID,
			Action:           "ticket.redacted",
			TargetType:       ptr("ticket"),
			TargetID:         &ticketID,
			OldValue:         map[string]any{"message_count": msgCount, "attachment_count": attCount},
			NewValue:         map[string]any{"redacted_at": red.RedactedAt.Time},
		}); aerr != nil {
			return aerr
		}

		// Enqueue one purge per blob (at-least-once; the consumer is idempotent).
		for _, key := range blobs {
			if eerr := events.Enqueue(ctx, tx, red.TenantRootID, events.TopicAttachmentPurge, map[string]any{
				"blob_key": key,
			}); eerr != nil {
				return eerr
			}
		}
		return nil
	})
	return mapErr(err)
}
