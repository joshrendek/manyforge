// Package ticketing owns the authenticated support-desk read+write surface:
// tickets, their threaded messages, and tenant-scoped requesters. The inbound
// ingest path that creates these rows lives in internal/inbox (a principal-less
// SECURITY DEFINER function); this package is the agent-facing API over them.
package ticketing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/manyforge/manyforge/internal/platform/audit"
	"github.com/manyforge/manyforge/internal/platform/db"
	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/events"
	"github.com/manyforge/manyforge/internal/platform/mailer"
	"github.com/manyforge/manyforge/internal/platform/ratelimit"
)

// Service implements the ticketing read slice (US1, T031). Every method takes the
// caller's principalID and the target businessID: the query runs inside
// db.WithPrincipal (RLS scopes rows to the caller's authorized businesses) AND
// pushes the ownership predicate (business_id = $) into SQL — dual enforcement, so
// neither RLS nor the predicate alone is load-bearing. Unknown / other-business /
// unauthorized all collapse to ErrNotFound (→ 404, no existence oracle).
type Service struct {
	DB              *db.DB
	ReplyTokenKey   []byte                    // signs the VERP Reply-To token
	SystemDomain    string                    // outbound mail domain for minted Message-IDs
	OutboundLimiter ratelimit.Limiter         // nil ⇒ no limit (tests/dev)
	Suppression     mailer.SuppressionChecker // nil ⇒ no pre-check (worker still gates)
}

// ReplyInput is the validated reply payload for Reply.
type ReplyInput struct {
	BodyText string
	BodyHTML *string
}

// TriageInput is the partial-update payload for Triage. A nil field is preserved;
// a non-nil field is applied. Tags non-nil is a FULL replacement (empty slice
// clears all). AssigneeSet=false preserves the assignee; AssigneeSet=true with a
// non-nil Assignee assigns (eligibility-gated, T048) and with a nil Assignee unassigns.
type TriageInput struct {
	Status      *string    // nil=preserve; else one of new/open/pending/solved/closed
	Priority    *string    // nil=preserve; else low/normal/high/urgent
	Tags        *[]string  // nil=preserve; non-nil = FULL replacement (empty slice clears all)
	AssigneeSet bool       // false=preserve; true=apply Assignee (assign if non-nil, else unassign)
	Assignee    *uuid.UUID // when AssigneeSet: non-nil assigns (eligibility-gated), nil unassigns
}

// TicketFilter is the optional facet set for ListTickets. A nil/zero field
// disables that facet. Unassigned and Assignee are mutually exclusive at the API
// (the `unassigned` sentinel vs. a principal id); if both are set, Unassigned wins.
type TicketFilter struct {
	Status     *string    // ticket_status enum value
	Priority   *string    // ticket_priority enum value
	Unassigned bool       // assignee == "unassigned" sentinel
	Assignee   *uuid.UUID // filter to a specific assignee principal
	Tag        *string    // exact (case-insensitive) tag match
}

// Page is a keyset-paginated result. NextCursor is an opaque token (nil = last page).
type Page[T any] struct {
	Items      []T
	NextCursor *string
}

// Ticket is the API view of a ticket. It embeds the full Requester and exposes
// tags / message_count / last_message_at — but never reply_token (DB-only).
type Ticket struct {
	ID                  uuid.UUID
	BusinessID          uuid.UUID
	TenantRootID        uuid.UUID
	Subject             string
	Status              string
	Priority            string
	AssigneePrincipalID *uuid.UUID
	Requester           Requester
	Tags                []string
	MessageCount        int
	LastMessageAt       *time.Time
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// Attachment is the API view of a stored, MIME-sniffed attachment.
type Attachment struct {
	ID          uuid.UUID
	Filename    string
	ContentType string
	Size        int64
	BlobKey     string
}

// Message is the API view of one entry in a ticket thread, with SPF/DKIM/DMARC
// projected from the stored auth_results jsonb into three typed fields.
type Message struct {
	ID                uuid.UUID
	TicketID          uuid.UUID
	Direction         string
	MessageID         *string
	InReplyTo         *string
	References        []string
	AuthorPrincipalID *uuid.UUID
	BodyText          *string
	BodyHTML          *string
	Attachments       []Attachment
	SPFResult         string
	DKIMResult        string
	DMARCResult       string
	// DeliveryState is the outbound lifecycle (pending|sent|failed); nil for
	// inbound messages and notes. delivery_error is intentionally NEVER exposed.
	DeliveryState *string
	CreatedAt     time.Time
}

// ListTickets returns a keyset page of the business's tickets, newest activity
// first, optionally filtered. limit is clamped to [1,100] HERE (service boundary)
// so an absurd caller value never returns the whole table.
func (s *Service) ListTickets(ctx context.Context, principalID, businessID uuid.UUID, f TicketFilter, cursor string, limit int) (Page[Ticket], error) {
	lim := clampLimit(limit)
	var out Page[Ticket]
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)

		var rows []dbgen.Ticket
		var qerr error
		if cursor == "" {
			rows, qerr = q.ListTickets(ctx, dbgen.ListTicketsParams{
				BusinessID:          businessID,
				Status:              nullStatus(f.Status),
				Priority:            nullPriority(f.Priority),
				AssigneeUnassigned:  f.Unassigned,
				AssigneePrincipalID: assigneeArg(f),
				Tag:                 f.Tag,
				Lim:                 int32(lim + 1),
			})
		} else {
			k, perr := decodeTicketCursor(cursor)
			if perr != nil {
				return perr
			}
			rows, qerr = q.ListTicketsAfter(ctx, dbgen.ListTicketsAfterParams{
				BusinessID:          businessID,
				Status:              nullStatus(f.Status),
				Priority:            nullPriority(f.Priority),
				AssigneeUnassigned:  f.Unassigned,
				AssigneePrincipalID: assigneeArg(f),
				Tag:                 f.Tag,
				CurLastMessageAt:    k.ts,
				CurID:               k.id,
				Lim:                 int32(lim + 1),
			})
		}
		if qerr != nil {
			return qerr
		}

		rows, next := trim(rows, lim)
		items := make([]Ticket, 0, len(rows))
		for _, r := range rows {
			tk, terr := assembleTicket(ctx, q, r)
			if terr != nil {
				return terr
			}
			items = append(items, tk)
		}
		out.Items = items
		if next {
			last := rows[len(rows)-1]
			out.NextCursor = ptr(encodeTicketCursor(keyset{ts: last.LastMessageAt, id: last.ID}))
		}
		return nil
	})
	if err != nil {
		return Page[Ticket]{}, mapErr(err)
	}
	return out, nil
}

// GetTicket loads a single ticket the caller can see, or ErrNotFound (no oracle).
func (s *Service) GetTicket(ctx context.Context, principalID, businessID, ticketID uuid.UUID) (Ticket, error) {
	var out Ticket
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		row, qerr := q.GetTicket(ctx, dbgen.GetTicketParams{ID: ticketID, BusinessID: businessID})
		if qerr != nil {
			return qerr
		}
		tk, terr := assembleTicket(ctx, q, row)
		if terr != nil {
			return terr
		}
		out = tk
		return nil
	})
	if err != nil {
		return Ticket{}, mapErr(err)
	}
	return out, nil
}

// Reply sends an outbound reply on a ticket (FR-008). One transaction: own-check
// the ticket, pre-check recipient suppression, apply the outbound rate limit,
// insert the outbound message (delivery_state='pending') threaded to the latest
// message, bump last_message_at, write an in-tx audit entry (FR-014), and enqueue
// the 'ticket.replied' outbox event the notify worker drains to actually send mail.
// Dual-enforced (WithPrincipal RLS + business_id predicate); unknown/foreign/
// unauthorized ticket ⇒ ErrNotFound (no oracle). Suppressed recipient ⇒ ErrConflict;
// rate-limited ⇒ ErrRateLimited; empty body ⇒ ErrValidation.
func (s *Service) Reply(ctx context.Context, principalID, businessID, ticketID uuid.UUID, in ReplyInput) (Message, error) {
	if len(in.BodyText) == 0 {
		return Message{}, fmt.Errorf("ticketing: empty reply: %w", errs.ErrValidation)
	}
	var out Message
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)

		// Load + own-check the ticket (ErrNoRows ⇒ ErrNotFound, no oracle).
		tk, terr := q.GetTicket(ctx, dbgen.GetTicketParams{ID: ticketID, BusinessID: businessID})
		if terr != nil {
			return terr
		}

		// yqi: a reply on a `new` ticket advances it to `open` (same tx, audited).
		if yerr := advanceNewToOpen(ctx, q, tx, tk, principalID, businessID); yerr != nil {
			return yerr
		}

		// Recipient email: the ticket's requester (same business/tenant scope).
		req, rerr := q.GetRequesterForTicket(ctx, dbgen.GetRequesterForTicketParams{ID: ticketID, BusinessID: businessID})
		if rerr != nil {
			return rerr
		}
		recipient := req.Email

		// Recipient suppression pre-check (the worker re-checks at send time).
		if s.Suppression != nil {
			suppressed, serr := s.Suppression.IsSuppressed(ctx, recipient)
			if serr != nil {
				return serr
			}
			if suppressed {
				return fmt.Errorf("ticketing: recipient suppressed: %w", errs.ErrConflict)
			}
		}

		// Outbound rate limit (FR-020/T041): per-business AND per-recipient. The
		// recipient key is tenant-scoped so the same email on two tenants is independent.
		if s.OutboundLimiter != nil {
			// Note: with ||, the biz token is spent even if the rcpt check then denies —
			// a benign, intentional over-count (we don't pre-peek to avoid a second code
			// path); both denials return the same 429 (no oracle).
			if !s.OutboundLimiter.Allow("ob:biz:"+businessID.String()) ||
				!s.OutboundLimiter.Allow("ob:rcpt:"+tk.TenantRootID.String()+":"+recipient) {
				return fmt.Errorf("ticketing: outbound rate limit: %w", errs.ErrRateLimited)
			}
		}

		// Threading headers from the latest message on the ticket.
		parent, perr := q.GetThreadingParent(ctx, dbgen.GetThreadingParentParams{
			TicketID: ticketID, BusinessID: businessID, TenantRootID: tk.TenantRootID,
		})
		if perr != nil && !errors.Is(perr, pgx.ErrNoRows) {
			return perr
		}
		var inReplyTo *string
		refs := []string{}
		if perr == nil {
			pid := parent.MessageID
			inReplyTo = &pid
			refs = append(append([]string{}, parent.References...), parent.MessageID)
		}

		msgID, gerr := uuid.NewV7()
		if gerr != nil {
			return gerr
		}
		rfcMessageID := msgID.String() + "@" + s.SystemDomain

		row, ierr := q.InsertOutboundMessage(ctx, dbgen.InsertOutboundMessageParams{
			ID: msgID, TicketID: ticketID, BusinessID: businessID, TenantRootID: tk.TenantRootID,
			AuthorPrincipalID: db.PGUUID(principalID), MessageID: rfcMessageID,
			InReplyTo: inReplyTo, References: refs,
			BodyText: &in.BodyText, BodyHtml: in.BodyHTML,
		})
		if ierr != nil {
			return ierr
		}
		if berr := q.BumpTicketActivity(ctx, dbgen.BumpTicketActivityParams{
			ID: ticketID, BusinessID: businessID, TenantRootID: tk.TenantRootID,
		}); berr != nil {
			return berr
		}

		// Audit-in-tx (FR-014) via the shared helper.
		targetType := "ticket_message"
		if aerr := audit.Write(ctx, tx, audit.Entry{
			BusinessID:       &businessID,
			TenantRootID:     &tk.TenantRootID,
			ActorPrincipalID: &principalID,
			Action:           "ticket.replied",
			TargetType:       &targetType,
			TargetID:         &msgID,
			NewValue:         map[string]any{"ticket_id": ticketID, "direction": "outbound"},
		}); aerr != nil {
			return aerr
		}

		// Outbox event — the worker builds the threaded Mail and dispatches it. The
		// reply_token signs the TICKET id so an inbound reply threads back (R4).
		replyToken := SignReplyToken(ticketID, s.ReplyTokenKey)
		if eerr := events.Enqueue(ctx, tx, tk.TenantRootID, events.TopicTicketReplied, map[string]any{
			"message_row_id": msgID,
			"ticket_id":      ticketID,
			"business_id":    businessID,
			"recipient":      recipient,
			"subject":        replySubject(tk.Subject),
			"rfc_message_id": rfcMessageID,
			"in_reply_to":    inReplyTo,
			"references":     refs,
			"reply_token":    replyToken,
		}); eerr != nil {
			return eerr
		}

		out = toMessage(row, nil)
		return nil
	})
	if err != nil {
		return Message{}, mapErr(err)
	}
	return out, nil
}

// NoteInput is an internal note's payload (plain text per the NoteBody contract).
type NoteInput struct {
	BodyText string
}

// AddNote records an internal note on a ticket (FR-009): a note-direction message,
// attributed to the acting member and audited in the same tx. It is NEVER delivered
// to the requester and NEVER enqueues outbound mail. Dual-enforced (WithPrincipal +
// business_id predicate); unknown/foreign ticket ⇒ ErrNotFound (no oracle).
func (s *Service) AddNote(ctx context.Context, principalID, businessID, ticketID uuid.UUID, in NoteInput) (Message, error) {
	if len(in.BodyText) == 0 {
		return Message{}, fmt.Errorf("ticketing: empty note: %w", errs.ErrValidation)
	}
	var out Message
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)

		// Load + own-check the ticket (ErrNoRows ⇒ ErrNotFound, no oracle).
		tk, terr := q.GetTicket(ctx, dbgen.GetTicketParams{ID: ticketID, BusinessID: businessID})
		if terr != nil {
			return terr
		}

		// yqi: a note on a `new` ticket advances it to `open` (same tx, audited).
		// A note never bumps last_message_at — the status UPDATE only touches
		// status/updated_at, so that invariant is preserved.
		if yerr := advanceNewToOpen(ctx, q, tx, tk, principalID, businessID); yerr != nil {
			return yerr
		}

		msgID, gerr := uuid.NewV7()
		if gerr != nil {
			return gerr
		}

		row, ierr := q.InsertNoteMessage(ctx, dbgen.InsertNoteMessageParams{
			ID: msgID, TicketID: ticketID, BusinessID: businessID, TenantRootID: tk.TenantRootID,
			AuthorPrincipalID: db.PGUUID(principalID),
			MessageID:         msgID.String() + "@note." + s.SystemDomain, // internal id; never sent
			BodyText:          &in.BodyText, BodyHtml: nil,
		})
		if ierr != nil {
			return ierr
		}

		// Audit-in-tx (FR-014) via the shared helper.
		targetType := "ticket_message"
		if aerr := audit.Write(ctx, tx, audit.Entry{
			BusinessID:       &businessID,
			TenantRootID:     &tk.TenantRootID,
			ActorPrincipalID: &principalID,
			Action:           "ticket.noted",
			TargetType:       &targetType,
			TargetID:         &msgID,
			NewValue:         map[string]any{"ticket_id": ticketID, "direction": "note"},
		}); aerr != nil {
			return aerr
		}

		out = toMessage(row, nil)
		return nil
	})
	if err != nil {
		return Message{}, mapErr(err)
	}
	return out, nil
}

// advanceNewToOpen implements the yqi lifecycle rule (data-model L438): the first
// reply or note on a `new` ticket transitions it to `open`. It UPDATEs only
// status/updated_at (never last_message_at) and writes the pinned
// ticket.status_changed audit in the SAME tx, attributed to the acting principal.
// A non-`new` ticket is a no-op (no update, no audit).
func advanceNewToOpen(ctx context.Context, q *dbgen.Queries, tx pgx.Tx, tk dbgen.Ticket, principalID, businessID uuid.UUID) error {
	if string(tk.Status) != "new" {
		return nil
	}
	if err := q.UpdateTicketStatus(ctx, dbgen.UpdateTicketStatusParams{
		ID: tk.ID, BusinessID: businessID, TenantRootID: tk.TenantRootID,
		Status: dbgen.TicketStatus("open"),
	}); err != nil {
		return err
	}
	return writeStatusAudit(ctx, tx, businessID, tk.TenantRootID, principalID, tk.ID, "new", "open")
}

// writeStatusAudit writes a pinned ticket.status_changed audit row (old→new) in tx.
func writeStatusAudit(ctx context.Context, tx pgx.Tx, businessID, tenantRootID, principalID, ticketID uuid.UUID, oldStatus, newStatus string) error {
	targetType := "ticket"
	return audit.Write(ctx, tx, audit.Entry{
		BusinessID:       &businessID,
		TenantRootID:     &tenantRootID,
		ActorPrincipalID: &principalID,
		Action:           "ticket.status_changed",
		TargetType:       &targetType,
		TargetID:         &ticketID,
		OldValue:         map[string]any{"status": oldStatus},
		NewValue:         map[string]any{"status": newStatus},
	})
}

// Triage applies a partial status/priority/tags/assignee update to a ticket (US3,
// FR-010..012). One transaction: own-check the ticket, validate enums, then for each
// CHANGED field apply the update + write one pinned audit row (old→new) in the same tx.
// A field equal to its current value is a no-op (no update, no audit). Tags non-nil is
// a full set replacement (empty clears all). Assignee is eligibility-gated (the
// assignee must be a member of the ticket's business or an authorized non-archived
// ancestor — verified in-tx via the is_eligible_assignee DEFINER, no TOCTOU); an
// ineligible-but-existing principal and a nonexistent uuid both ⇒ the same ErrConflict
// (no oracle); unassign is always allowed. NEVER touches last_message_at.
// Dual-enforced (WithPrincipal RLS + business_id predicate); unknown/foreign ticket ⇒
// ErrNotFound (no oracle); invalid status/priority ⇒ ErrValidation.
func (s *Service) Triage(ctx context.Context, principalID, businessID, ticketID uuid.UUID, in TriageInput) (Ticket, error) {
	// Validate enums up front so a rejected triage mutates nothing.
	if in.Status != nil && !isValidStatus(*in.Status) {
		return Ticket{}, fmt.Errorf("ticketing: invalid status %q: %w", *in.Status, errs.ErrValidation)
	}
	if in.Priority != nil && !isValidPriority(*in.Priority) {
		return Ticket{}, fmt.Errorf("ticketing: invalid priority %q: %w", *in.Priority, errs.ErrValidation)
	}
	// Tags content: reject empty/whitespace-only tags at the service boundary (the
	// authoritative clamp — covers callers that bypass the handler). A rejected
	// triage mutates nothing (validation runs before WithPrincipal).
	if in.Tags != nil {
		for _, tag := range *in.Tags {
			if strings.TrimSpace(tag) == "" {
				return Ticket{}, fmt.Errorf("ticketing: invalid tag: %w", errs.ErrValidation)
			}
		}
	}

	var out Ticket
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)

		// Load + own-check the ticket (ErrNoRows ⇒ ErrNotFound, no oracle).
		tk, terr := q.GetTicket(ctx, dbgen.GetTicketParams{ID: ticketID, BusinessID: businessID})
		if terr != nil {
			return terr
		}

		// --- status (changed only) ---
		if in.Status != nil && *in.Status != string(tk.Status) {
			if uerr := q.UpdateTicketStatus(ctx, dbgen.UpdateTicketStatusParams{
				ID: ticketID, BusinessID: businessID, TenantRootID: tk.TenantRootID,
				Status: dbgen.TicketStatus(*in.Status),
			}); uerr != nil {
				return uerr
			}
			if aerr := writeStatusAudit(ctx, tx, businessID, tk.TenantRootID, principalID, ticketID, string(tk.Status), *in.Status); aerr != nil {
				return aerr
			}
		}

		// --- priority (changed only) ---
		if in.Priority != nil && *in.Priority != string(tk.Priority) {
			if uerr := q.UpdateTicketPriority(ctx, dbgen.UpdateTicketPriorityParams{
				ID: ticketID, BusinessID: businessID, TenantRootID: tk.TenantRootID,
				Priority: dbgen.TicketPriority(*in.Priority),
			}); uerr != nil {
				return uerr
			}
			targetType := "ticket"
			if aerr := audit.Write(ctx, tx, audit.Entry{
				BusinessID:       &businessID,
				TenantRootID:     &tk.TenantRootID,
				ActorPrincipalID: &principalID,
				Action:           "ticket.priority_changed",
				TargetType:       &targetType,
				TargetID:         &ticketID,
				OldValue:         map[string]any{"priority": string(tk.Priority)},
				NewValue:         map[string]any{"priority": *in.Priority},
			}); aerr != nil {
				return aerr
			}
		}

		// --- tags (full replacement; changed only) ---
		if in.Tags != nil {
			cur, cerr := q.ListTicketTags(ctx, dbgen.ListTicketTagsParams{TicketID: ticketID, BusinessID: businessID})
			if cerr != nil {
				return cerr
			}
			next := dedupTags(*in.Tags)
			if !sameTagSet(cur, next) {
				if derr := q.DeleteTicketTags(ctx, dbgen.DeleteTicketTagsParams{TicketID: ticketID, BusinessID: businessID}); derr != nil {
					return derr
				}
				for _, tag := range next {
					if ierr := q.InsertTicketTag(ctx, dbgen.InsertTicketTagParams{
						TicketID: ticketID, Tag: tag, BusinessID: businessID, TenantRootID: tk.TenantRootID,
					}); ierr != nil {
						return ierr
					}
				}
				targetType := "ticket"
				if aerr := audit.Write(ctx, tx, audit.Entry{
					BusinessID:       &businessID,
					TenantRootID:     &tk.TenantRootID,
					ActorPrincipalID: &principalID,
					Action:           "ticket.tags_changed",
					TargetType:       &targetType,
					TargetID:         &ticketID,
					OldValue:         map[string]any{"tags": sortedTags(cur)},
					NewValue:         map[string]any{"tags": sortedTags(next)},
				}); aerr != nil {
					return aerr
				}
			}
		}

		// --- assignee (changed only; eligibility-gated) ---
		// AssigneeSet means the PATCH carried the field. Assign (non-nil) requires the
		// principal be a member of the ticket's business or an authorized non-archived
		// ancestor — verified inside this same tx (no TOCTOU) via the is_eligible_assignee
		// SECURITY DEFINER (the acting principal's RLS would HIDE an eligible
		// ancestor-member's membership, so a plain query can't evaluate it). An
		// ineligible-but-existing principal AND a nonexistent uuid both fail the predicate
		// → the SAME ErrConflict (no existence oracle). Unassign (nil) is always allowed.
		if in.AssigneeSet {
			cur := pgUUIDPtr(tk.AssigneePrincipalID)
			if !sameAssignee(cur, in.Assignee) {
				if in.Assignee != nil {
					eligible, eerr := isEligibleAssignee(ctx, tx, principalID, businessID, *in.Assignee)
					if eerr != nil {
						return eerr
					}
					if !eligible {
						return fmt.Errorf("ticketing: ineligible assignee: %w", errs.ErrConflict)
					}
				}
				if uerr := q.UpdateTicketAssignee(ctx, dbgen.UpdateTicketAssigneeParams{
					ID: ticketID, BusinessID: businessID, TenantRootID: tk.TenantRootID,
					AssigneePrincipalID: db.PGUUIDPtr(in.Assignee),
				}); uerr != nil {
					return uerr
				}
				targetType := "ticket"
				if aerr := audit.Write(ctx, tx, audit.Entry{
					BusinessID:       &businessID,
					TenantRootID:     &tk.TenantRootID,
					ActorPrincipalID: &principalID,
					Action:           "ticket.assigned",
					TargetType:       &targetType,
					TargetID:         &ticketID,
					OldValue:         map[string]any{"assignee_principal_id": uuidJSON(cur)},
					NewValue:         map[string]any{"assignee_principal_id": uuidJSON(in.Assignee)},
				}); aerr != nil {
					return aerr
				}
			}
		}

		// Re-assemble the updated ticket from the now-current row.
		updated, gerr := q.GetTicket(ctx, dbgen.GetTicketParams{ID: ticketID, BusinessID: businessID})
		if gerr != nil {
			return gerr
		}
		tkOut, aerr := assembleTicket(ctx, q, updated)
		if aerr != nil {
			return aerr
		}
		out = tkOut
		return nil
	})
	if err != nil {
		return Ticket{}, mapErr(err)
	}
	return out, nil
}

// isValidStatus / isValidPriority pin the closed enum sets for triage validation
// (mirrors the handler's validStatus/validPriority for the service boundary).
func isValidStatus(s string) bool {
	switch s {
	case "new", "open", "pending", "solved", "closed":
		return true
	}
	return false
}

func isValidPriority(p string) bool {
	switch p {
	case "low", "normal", "high", "urgent":
		return true
	}
	return false
}

// dedupTags returns the input tags CASE-FOLD deduped, preserving the first-seen
// ORIGINAL casing for storage. ticket_tag.tag is citext with PK (ticket_id, tag), so
// "Bug" and "bug" are the same key: folding here collapses them to one row (no
// unique-violation 500) while still persisting the first-seen casing ("Bug").
func dedupTags(tags []string) []string {
	seen := make(map[string]struct{}, len(tags))
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		k := strings.ToLower(t)
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, t)
	}
	return out
}

// sortedTags returns a sorted copy (for a stable audit trail).
func sortedTags(tags []string) []string {
	out := append([]string(nil), tags...)
	sort.Strings(out)
	return out
}

// sameTagSet reports whether two tag slices represent the same CASE-FOLDED set
// (order/dups/casing ignored) — matching the citext column so a case-only-different
// submit is detected as a no-op (no delete+reinsert, no spurious tags audit).
func sameTagSet(a, b []string) bool {
	da, dbb := dedupTags(a), dedupTags(b)
	if len(da) != len(dbb) {
		return false
	}
	set := make(map[string]struct{}, len(da))
	for _, t := range da {
		set[strings.ToLower(t)] = struct{}{}
	}
	for _, t := range dbb {
		if _, ok := set[strings.ToLower(t)]; !ok {
			return false
		}
	}
	return true
}

// isEligibleAssignee asks the is_eligible_assignee SECURITY DEFINER (migration 0021)
// whether p_assignee may be assigned a ticket in businessID, evaluated as the acting
// principal. The DEFINER bypasses RLS (so an eligible ANCESTOR-member's membership —
// which the acting principal's RLS view would hide — is still seen) AND self-asserts
// the acting principal's authority over the business. It is called via raw pgx because
// sqlc cannot resolve DEFINER functions (same pattern as resolve_inbound_address /
// mark_bounced_message). An ineligible-but-existing principal and a nonexistent uuid
// both return false (no oracle); the caller maps false → ErrConflict.
func isEligibleAssignee(ctx context.Context, tx pgx.Tx, actingPrincipal, businessID, assignee uuid.UUID) (bool, error) {
	var ok bool
	if err := tx.QueryRow(ctx,
		"SELECT is_eligible_assignee($1, $2, $3)",
		actingPrincipal, businessID, assignee,
	).Scan(&ok); err != nil {
		return false, err
	}
	return ok, nil
}

// sameAssignee reports whether two optional assignee ids are equal (both nil, or the
// same uuid) — the no-op guard so an unchanged assignee writes no update and no audit.
func sameAssignee(a, b *uuid.UUID) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// uuidJSON renders an optional uuid for an audit value: the string, or nil so the
// JSON serializes as a literal null (matching the NULL assignee column).
func uuidJSON(u *uuid.UUID) any {
	if u == nil {
		return nil
	}
	return u.String()
}

// replySubject ensures a "Re: " prefix so the reply continues the customer thread.
func replySubject(s string) string {
	if !strings.HasPrefix(strings.ToLower(s), "re:") {
		return "Re: " + s
	}
	return s
}

// ListMessages returns a keyset page of a ticket's thread, oldest first, with
// attachments and projected auth results. A ticket id from another business
// yields an empty page (RLS + the ticket_id/business_id predicate), never a leak.
func (s *Service) ListMessages(ctx context.Context, principalID, businessID, ticketID uuid.UUID, cursor string, limit int) (Page[Message], error) {
	lim := clampLimit(limit)
	var out Page[Message]
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)

		var rows []dbgen.TicketMessage
		var qerr error
		if cursor == "" {
			rows, qerr = q.ListMessages(ctx, dbgen.ListMessagesParams{
				TicketID: ticketID, BusinessID: businessID, Limit: int32(lim + 1),
			})
		} else {
			k, perr := decodeMessageCursor(cursor)
			if perr != nil {
				return perr
			}
			rows, qerr = q.ListMessagesAfter(ctx, dbgen.ListMessagesAfterParams{
				TicketID: ticketID, BusinessID: businessID,
				CurCreatedAt: k.ts, CurID: k.id, Lim: int32(lim + 1),
			})
		}
		if qerr != nil {
			return qerr
		}

		rows, next := trim(rows, lim)
		atts, aerr := loadAttachments(ctx, q, businessID, rows)
		if aerr != nil {
			return aerr
		}
		items := make([]Message, 0, len(rows))
		for _, r := range rows {
			items = append(items, toMessage(r, atts[r.ID]))
		}
		out.Items = items
		if next {
			last := rows[len(rows)-1]
			out.NextCursor = ptr(encodeMessageCursor(keyset{ts: last.CreatedAt, id: last.ID}))
		}
		return nil
	})
	if err != nil {
		return Page[Message]{}, mapErr(err)
	}
	return out, nil
}

// ListRequesters returns a keyset page of the business's requesters, with an
// optional exact (case-insensitive) email filter for lookup/dedup.
func (s *Service) ListRequesters(ctx context.Context, principalID, businessID uuid.UUID, email *string, cursor string, limit int) (Page[Requester], error) {
	lim := clampLimit(limit)
	var out Page[Requester]
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)

		var rows []dbgen.Requester
		var qerr error
		if cursor == "" {
			rows, qerr = q.ListRequesters(ctx, dbgen.ListRequestersParams{
				BusinessID: businessID, Email: email, Lim: int32(lim + 1),
			})
		} else {
			k, perr := decodeRequesterCursor(cursor)
			if perr != nil {
				return perr
			}
			rows, qerr = q.ListRequestersAfter(ctx, dbgen.ListRequestersAfterParams{
				BusinessID: businessID, Email: email,
				CurFirstSeenAt: k.ts, CurID: k.id, Lim: int32(lim + 1),
			})
		}
		if qerr != nil {
			return qerr
		}

		rows, next := trim(rows, lim)
		items := make([]Requester, 0, len(rows))
		for _, r := range rows {
			items = append(items, toRequester(r))
		}
		out.Items = items
		if next {
			last := rows[len(rows)-1]
			out.NextCursor = ptr(encodeRequesterCursor(keyset{ts: last.FirstSeenAt, id: last.ID}))
		}
		return nil
	})
	if err != nil {
		return Page[Requester]{}, mapErr(err)
	}
	return out, nil
}

// GetRequester loads a single requester the caller can see, or ErrNotFound.
func (s *Service) GetRequester(ctx context.Context, principalID, businessID, requesterID uuid.UUID) (Requester, error) {
	var out Requester
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		row, qerr := dbgen.New(tx).GetRequester(ctx, dbgen.GetRequesterParams{ID: requesterID, BusinessID: businessID})
		if qerr != nil {
			return qerr
		}
		out = toRequester(row)
		return nil
	})
	if err != nil {
		return Requester{}, mapErr(err)
	}
	return out, nil
}

// --- assembly helpers ---

// assembleTicket fills the embedded requester, tags, and message_count for a
// ticket row. All sub-queries are scoped to the same business_id (predicate) and
// run in the same RLS-bound tx — defense in depth on every projection.
func assembleTicket(ctx context.Context, q *dbgen.Queries, r dbgen.Ticket) (Ticket, error) {
	req, err := q.GetRequesterForTicket(ctx, dbgen.GetRequesterForTicketParams{ID: r.ID, BusinessID: r.BusinessID})
	if err != nil {
		return Ticket{}, err
	}
	tags, err := q.ListTicketTags(ctx, dbgen.ListTicketTagsParams{TicketID: r.ID, BusinessID: r.BusinessID})
	if err != nil {
		return Ticket{}, err
	}
	if tags == nil {
		tags = []string{}
	}
	count, err := q.CountTicketMessages(ctx, dbgen.CountTicketMessagesParams{TicketID: r.ID, BusinessID: r.BusinessID})
	if err != nil {
		return Ticket{}, err
	}
	var lastMsg *time.Time
	if !r.LastMessageAt.IsZero() {
		lm := r.LastMessageAt
		lastMsg = &lm
	}
	return Ticket{
		ID:                  r.ID,
		BusinessID:          r.BusinessID,
		TenantRootID:        r.TenantRootID,
		Subject:             r.Subject,
		Status:              string(r.Status),
		Priority:            string(r.Priority),
		AssigneePrincipalID: pgUUIDPtr(r.AssigneePrincipalID),
		Requester:           toRequester(req),
		Tags:                tags,
		MessageCount:        int(count),
		LastMessageAt:       lastMsg,
		CreatedAt:           r.CreatedAt,
		UpdatedAt:           r.UpdatedAt,
	}, nil
}

// loadAttachments fetches all attachments for a page of messages in one query and
// groups them by ticket_message_id.
func loadAttachments(ctx context.Context, q *dbgen.Queries, businessID uuid.UUID, msgs []dbgen.TicketMessage) (map[uuid.UUID][]Attachment, error) {
	if len(msgs) == 0 {
		return nil, nil
	}
	ids := make([]uuid.UUID, 0, len(msgs))
	for _, m := range msgs {
		ids = append(ids, m.ID)
	}
	rows, err := q.ListAttachmentsForMessages(ctx, dbgen.ListAttachmentsForMessagesParams{BusinessID: businessID, MessageIds: ids})
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID][]Attachment, len(msgs))
	for _, a := range rows {
		out[a.TicketMessageID] = append(out[a.TicketMessageID], Attachment{
			ID:          a.ID,
			Filename:    deref(a.Filename),
			ContentType: a.ContentType,
			Size:        a.Size,
			BlobKey:     a.BlobKey,
		})
	}
	return out, nil
}

func toRequester(r dbgen.Requester) Requester {
	return Requester{
		ID:           r.ID,
		TenantRootID: r.TenantRootID,
		Email:        r.Email,
		DisplayName:  r.DisplayName,
		ContactID:    pgUUIDPtr(r.ContactID),
		FirstSeenAt:  r.FirstSeenAt,
		LastSeenAt:   r.LastSeenAt,
	}
}

func toMessage(m dbgen.TicketMessage, atts []Attachment) Message {
	spf, dkim, dmarc := projectAuthResults(m.AuthResults)
	refs := m.References
	if refs == nil {
		refs = []string{}
	}
	if atts == nil {
		atts = []Attachment{}
	}
	var mid *string
	if m.MessageID != "" {
		v := m.MessageID
		mid = &v
	}
	return Message{
		ID:                m.ID,
		TicketID:          m.TicketID,
		Direction:         string(m.Direction),
		MessageID:         mid,
		InReplyTo:         m.InReplyTo,
		References:        refs,
		AuthorPrincipalID: pgUUIDPtr(m.AuthorPrincipalID),
		BodyText:          m.BodyText,
		BodyHTML:          m.BodyHtml,
		Attachments:       atts,
		SPFResult:         spf,
		DKIMResult:        dkim,
		DMARCResult:       dmarc,
		DeliveryState:     deliveryStatePtr(m.DeliveryState),
		CreatedAt:         m.CreatedAt,
	}
}

// deliveryStatePtr maps the nullable delivery_state enum onto an optional string
// for the API view (NULL ⇒ nil). delivery_error is never surfaced.
func deliveryStatePtr(s dbgen.NullMessageDeliveryState) *string {
	if !s.Valid {
		return nil
	}
	v := string(s.MessageDeliveryState)
	return &v
}

// projectAuthResults maps the stored {spf,dkim,dmarc} verdict triple onto the
// DnsRecordState enum [unknown, pending, pass, fail]. Only "pass"/"fail"/"pending"
// pass through; everything else (none, softfail, empty, missing, malformed) is
// "unknown" — flagged, never trusted as a verdict (FR-019).
func projectAuthResults(raw []byte) (spf, dkim, dmarc string) {
	spf, dkim, dmarc = "unknown", "unknown", "unknown"
	if len(raw) == 0 {
		return
	}
	var v struct {
		SPF   string `json:"spf"`
		DKIM  string `json:"dkim"`
		DMARC string `json:"dmarc"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return
	}
	return normState(v.SPF), normState(v.DKIM), normState(v.DMARC)
}

func normState(s string) string {
	switch s {
	case "pass", "fail", "pending", "unknown":
		return s
	default:
		return "unknown"
	}
}

// --- arg/null helpers ---

func nullStatus(s *string) dbgen.NullTicketStatus {
	if s == nil {
		return dbgen.NullTicketStatus{}
	}
	return dbgen.NullTicketStatus{TicketStatus: dbgen.TicketStatus(*s), Valid: true}
}

func nullPriority(p *string) dbgen.NullTicketPriority {
	if p == nil {
		return dbgen.NullTicketPriority{}
	}
	return dbgen.NullTicketPriority{TicketPriority: dbgen.TicketPriority(*p), Valid: true}
}

// assigneeArg yields the specific-assignee filter UUID, or NULL when the
// unassigned sentinel is set or no assignee facet was requested.
func assigneeArg(f TicketFilter) pgtype.UUID {
	if f.Unassigned || f.Assignee == nil {
		return db.PGUUIDPtr(nil)
	}
	return db.PGUUID(*f.Assignee)
}

// pgUUIDPtr converts a nullable pgtype.UUID column into an optional uuid.UUID for
// the API view (NULL → nil).
func pgUUIDPtr(u pgtype.UUID) *uuid.UUID {
	if !u.Valid {
		return nil
	}
	v := uuid.UUID(u.Bytes)
	return &v
}

// --- misc helpers ---

// clampLimit applies the service-boundary page cap. A non-positive request gets
// the default; an oversized request is silently capped at the max (never the
// whole table).
func clampLimit(requested int) int {
	const def, max = 50, 100
	switch {
	case requested <= 0:
		return def
	case requested > max:
		return max
	default:
		return requested
	}
}

// trim drops the sentinel (limit+1)th row used to detect a further page, returning
// the kept rows and whether a next page exists.
func trim[T any](rows []T, lim int) ([]T, bool) {
	if len(rows) > lim {
		return rows[:lim], true
	}
	return rows, false
}

// mapErr converts a query/closure error into a stable service-layer sentinel.
// pgx.ErrNoRows (single-row lookups) → ErrNotFound (no oracle). ErrValidation
// (a malformed cursor) is preserved. Everything else is returned wrapped so the
// HTTP layer logs it server-side and surfaces a generic 500.
func mapErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("ticketing: not found: %w", errs.ErrNotFound)
	case errors.Is(err, errs.ErrValidation), errors.Is(err, errs.ErrNotFound),
		errors.Is(err, errs.ErrConflict), errors.Is(err, errs.ErrRateLimited):
		return err
	default:
		return fmt.Errorf("ticketing: query: %w", err)
	}
}

func ptr[T any](v T) *T { return &v }

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
