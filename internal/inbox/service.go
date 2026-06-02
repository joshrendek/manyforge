package inbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/manyforge/manyforge/internal/platform/blob"
	"github.com/manyforge/manyforge/internal/platform/db"
	"github.com/manyforge/manyforge/internal/platform/events"
	"github.com/manyforge/manyforge/internal/ticketing"
)

// Config holds the ingestion service's runtime knobs. ReplyTokenKey is the HMAC
// server key used to mint/verify the threading reply token (ticketing.Sign/VerifyReplyToken);
// AttachmentMaxBytes caps a single stored attachment; InboundSystemDomain is the
// platform-hosted domain that auto-provisioned system inbound addresses live on.
type Config struct {
	ReplyTokenKey       []byte
	AttachmentMaxBytes  int64
	InboundSystemDomain string
	// LoopGuardMaxAutoReplies bounds auto-responder amplification (FR-018/SC-011):
	// the maximum auto-generated inbound messages one requester may produce within
	// the loop window before further ones are suppressed. <= 0 uses the default.
	LoopGuardMaxAutoReplies int
}

// IngestResult reports the outcome of ingesting one inbound message. TicketID and
// MessageID are the DB ids of the ticket and the ticket_message row. Created is
// true when a brand-new ticket was opened; Duplicate is true when the message was
// a replay of an already-ingested Message-ID (idempotent no-op, SC-002/FR-005).
type IngestResult struct {
	TicketID   uuid.UUID
	MessageID  uuid.UUID // DB id of the ticket_message row
	Created    bool      // a new ticket was created
	Duplicate  bool      // replay of an already-ingested message_id
	Suppressed bool      // dropped as a bounded mail-loop auto-reply (FR-018/SC-011)
}

// errNoRoute is the uniform sentinel for an inbound recipient that resolves to no
// business (FR-003/SC-006). The webhook/SMTP adapters (later tasks) map it to the
// SAME 202/250 success an actually-routed message gets — an unknown recipient
// writes ZERO rows and is byte-identical to a routable one, so the response is
// never an existence oracle. It is unexported: only this package distinguishes it.
var errNoRoute = errors.New("inbox: recipient does not resolve to a business")

// IsNoRoute reports whether err is the no-route sentinel, so an adapter can map a
// dropped (unroutable) message to the same uniform success ack as a routed one
// without importing the sentinel directly.
func IsNoRoute(err error) bool { return errors.Is(err, errNoRoute) }

// Service ingests inbound messages: it resolves the recipient to exactly one
// business, threads the message onto a ticket, upserts the requester, stores
// attachments, and enqueues the outbound notification — all through the audited,
// business-scoped SECURITY DEFINER path.
type Service struct {
	db     *db.DB
	blob   blob.Store
	cfg    Config
	logger *slog.Logger
}

// NewService constructs the ingestion Service.
func NewService(database *db.DB, store blob.Store, cfg Config, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{db: database, blob: store, cfg: cfg, logger: logger}
}

// defaultLoopGuardMaxAutoReplies is the FR-018/SC-011 per-requester auto-reply cap
// used when Config.LoopGuardMaxAutoReplies is unset (<= 0). Small enough to bound a
// runaway loop quickly, generous enough that an ordinary vacation responder never
// trips it.
const defaultLoopGuardMaxAutoReplies = 10

// loopGuardMax is the effective per-requester auto-reply cap (the configured value,
// or the default when unset). Passed to ingest_inbound_message as the loop bound.
func (s *Service) loopGuardMax() int {
	if s.cfg.LoopGuardMaxAutoReplies > 0 {
		return s.cfg.LoopGuardMaxAutoReplies
	}
	return defaultLoopGuardMaxAutoReplies
}

// sniffedAttachment is one attachment that passed the MIME-sniff allowlist and is
// staged for storage. content_type is the SNIFFED type (the declared header is
// never trusted, FR-007); blobKey is the tenant-scoped object key, assigned after
// the recipient resolves so an unknown recipient writes nothing.
type sniffedAttachment struct {
	id          uuid.UUID
	filename    string
	contentType string // sniffed, allowlisted
	size        int64
	content     []byte
	blobKey     string
}

// Ingest resolves, threads, and persists one inbound message.
//
// Security ordering (intentional):
//  1. Parse (degrades safely; soft errors ignored).
//  2. Synthesize a deterministic Message-ID for header-less mail (idempotency).
//  3. MIME-SNIFF every attachment BEFORE any DB/blob write; a single disallowed or
//     lying part rejects the WHOLE message with ZERO rows/blobs (FR-007/SC-007).
//  4. Verify the plus/VERP reply token → p_hint_ticket (HMAC, server-key only).
//  5. In ONE principal-less tx: resolve recipient (no-oracle on miss) → store
//     sniffed attachments → ingest_inbound_message (DEFINER: scope re-assertion,
//     requester upsert, threading, idempotent insert, attachments, audit) →
//     enqueue the outbox event(s) in the SAME tx.
//
// An unknown recipient returns errNoRoute (see IsNoRoute) after writing nothing.
func (s *Service) Ingest(ctx context.Context, msg RawMessage) (IngestResult, error) {
	parsed, perr := Parse(msg.Raw)
	if perr != nil {
		// Soft parse failure: enmime recovered a usable (possibly empty) view. We
		// proceed (FR: degrade safely) but record it for diagnostics.
		s.logger.WarnContext(ctx, "inbox: parse degraded", "err", perr, "provider", msg.Provider)
	}

	// MIME-sniff every attachment FIRST, before any DB or blob write. A disallowed
	// or lying part rejects the whole message; nothing is persisted (FR-007/SC-007).
	staged, err := s.sniffAttachments(parsed.Attachments)
	if err != nil {
		return IngestResult{}, err
	}

	// HMAC-verified reply-token hint (nil when absent/forged); the address token is
	// stripped during resolution below.
	normalizedAddr, plusToken := normalizeRecipient(msg.EnvelopeRecipient)
	hint := hintTicket(plusToken, s.cfg.ReplyTokenKey)

	// Sender identity for the requester upsert (done inside the DEFINER function).
	senderEmail := parsed.From.Address
	if senderEmail == "" {
		senderEmail = msg.EnvelopeSender
	}
	senderName := nilIfEmpty(parsed.From.Name)

	messageID := resolveMessageID(parsed, uuid.Nil, senderEmail) // tenant filled in after resolve

	var result IngestResult
	txErr := s.db.WithTx(ctx, func(tx pgx.Tx) error {
		r, err := resolveRecipient(ctx, tx, normalizedAddr)
		if err != nil {
			return err // errNoRoute (no rows written) or a real DB error
		}

		// Now that we know the tenant, the synthetic id must be tenant-scoped so the
		// same header-less message replayed into this tenant collides (idempotency).
		if parsed.MessageID == "" {
			messageID = resolveMessageID(parsed, r.tenantRootID, senderEmail)
		}

		// Assign each sniffed-OK attachment a tenant-scoped blob key and build the
		// metadata the DEFINER function inserts as attachment rows. We do NOT write
		// the bytes yet: a replay returns out_duplicate with no attachment rows, so
		// writing blobs here would orphan them on every (attacker-replayable) replay.
		// The bytes are Put only after a NON-duplicate result, below.
		attachmentsJSON, err := s.buildAttachmentMeta(r, staged)
		if err != nil {
			return err
		}

		authResults, err := json.Marshal(map[string]string{
			"spf":   parsed.Auth.SPF,
			"dkim":  parsed.Auth.DKIM,
			"dmarc": parsed.Auth.DMARC,
		})
		if err != nil {
			return fmt.Errorf("inbox: marshal auth_results: %w", err)
		}

		// A new ticket needs both its id and a reply token minted up front. We
		// generate the id here (uuid v7, so tickets stay time-ordered) and the
		// DEFINER inserts the new ticket with exactly this id — so the reply token
		// we sign over it is coherent with the row's id, and a later inbound reply
		// carrying that token recovers the SAME id via VerifyReplyToken. The token
		// is persisted only when the function actually opens a new ticket.
		// (manyforge-btv: previously a throwaway uuid.New() was signed, never the
		// row id, so the reply-token/VERP threading fallback matched no ticket.)
		ticketID, err := uuid.NewV7()
		if err != nil {
			return fmt.Errorf("inbox: mint ticket id: %w", err)
		}
		replyToken := ticketing.SignReplyToken(ticketID, s.cfg.ReplyTokenKey)

		var hintArg *uuid.UUID
		if hint != uuid.Nil {
			h := hint
			hintArg = &h
		}

		var out IngestResult
		// Nullable holders: a suppressed auto-reply (FR-018) returns NULL ticket/message.
		var scTicket, scMessage pgtype.UUID
		err = tx.QueryRow(ctx, `
			SELECT out_ticket_id, out_message_id, out_created, out_duplicate, out_suppressed
			FROM ingest_inbound_message(
				$1, $2, $3, $4, $5, $6, $7, $8, $9, $10,
				$11, $12, $13, $14, $15, $16, $17, $18, $19)`,
			r.businessID,                 // $1  p_business_id
			r.tenantRootID,               // $2  p_tenant_root_id
			normalizedAddr,               // $3  p_address
			senderEmail,                  // $4  p_sender_email
			senderName,                   // $5  p_sender_name
			parsed.Subject,               // $6  p_subject
			messageID,                    // $7  p_message_id
			nilIfEmpty(parsed.InReplyTo), // $8  p_in_reply_to
			parsed.References,            // $9  p_references (text[])
			parsed.TextBody,              // $10 p_body_text
			nilIfEmpty(parsed.HTMLBody),  // $11 p_body_html
			authResults,                  // $12 p_auth_results (jsonb)
			parsed.Auto.IsAutoReply,      // $13 p_is_auto_reply
			hintArg,                      // $14 p_hint_ticket (nullable uuid)
			ticketID,                     // $15 p_ticket_id (id for a new ticket)
			replyToken,                   // $16 p_reply_token
			attachmentsJSON,              // $17 p_attachments (jsonb)
			"inbox:"+msg.Provider,        // $18 p_source
			s.loopGuardMax(),             // $19 p_loop_max_auto_replies (FR-018/SC-011)
		).Scan(&scTicket, &scMessage, &out.Created, &out.Duplicate, &out.Suppressed)
		if err != nil {
			return fmt.Errorf("inbox: ingest_inbound_message: %w", err)
		}
		if scTicket.Valid {
			out.TicketID = uuid.UUID(scTicket.Bytes)
		}
		if scMessage.Valid {
			out.MessageID = uuid.UUID(scMessage.Bytes)
		}

		// FR-018/SC-011: a suppressed mail-loop auto-reply is an accepted no-op — the
		// DEFINER wrote the loop_suppressed audit and inserted no ticket/message, so
		// (like a replay) we store nothing further and skip the outbox fan-out.
		if out.Suppressed {
			result = out
			return nil
		}

		// A replay is an idempotent no-op: no new rows, no new outbox event, and —
		// critically — no blobs written (the DEFINER inserted no attachment rows, so
		// any blob would be orphaned). Return before storing anything.
		if out.Duplicate {
			result = out
			return nil
		}

		// Non-duplicate: NOW write the staged attachment bytes to object storage.
		// This runs inside the tx so a Put failure still rolls back the DB rows the
		// DEFINER just inserted, minimizing the dangling-reference window. The keys
		// were already fixed at metadata-build time, so the rows and blobs agree.
		if err := s.putAttachments(ctx, staged); err != nil {
			return err
		}

		// Enqueue the outbox event(s) in the SAME tx (the function does NOT touch the
		// outbox). A brand-new ticket fans out ticket.created; every fresh inbound
		// message fans out message.received so agents are notified.
		if out.Created {
			if err := events.Enqueue(ctx, tx, r.tenantRootID, "ticket.created", map[string]any{
				"ticket_id":   out.TicketID,
				"business_id": r.businessID,
				"message_id":  out.MessageID,
			}); err != nil {
				return err
			}
		}
		if err := events.Enqueue(ctx, tx, r.tenantRootID, "message.received", map[string]any{
			"ticket_id":   out.TicketID,
			"business_id": r.businessID,
			"message_id":  out.MessageID,
		}); err != nil {
			return err
		}

		result = out
		return nil
	})
	if txErr != nil {
		return IngestResult{}, txErr
	}
	return result, nil
}

// sniffAttachments MIME-sniffs every parsed attachment against the blob allowlist
// (FR-007) and enforces the per-attachment size cap, BEFORE any DB/blob write. A
// single disallowed/lying part (declared type ignored) or over-cap part fails the
// whole message so nothing is persisted (SC-007). Zero-byte parts are skipped (the
// attachment table CHECK requires size > 0).
func (s *Service) sniffAttachments(parts []ParsedAttachment) ([]sniffedAttachment, error) {
	if len(parts) == 0 {
		return nil, nil
	}
	out := make([]sniffedAttachment, 0, len(parts))
	for _, p := range parts {
		if len(p.Content) == 0 {
			continue // nothing to store; CHECK (size > 0) would reject anyway
		}
		if s.cfg.AttachmentMaxBytes > 0 && int64(len(p.Content)) > s.cfg.AttachmentMaxBytes {
			return nil, fmt.Errorf("inbox: attachment %q exceeds size cap: %w", p.FileName, blob.ErrUnsupportedType)
		}
		ct, err := blob.Sniff(p.Content) // declared type ignored; bytes decide
		if err != nil {
			return nil, fmt.Errorf("inbox: attachment %q rejected: %w", p.FileName, err)
		}
		out = append(out, sniffedAttachment{
			id:          uuid.New(),
			filename:    p.FileName,
			contentType: ct,
			size:        int64(len(p.Content)),
			content:     p.Content,
		})
	}
	return out, nil
}

// buildAttachmentMeta assigns each staged attachment its tenant-scoped blob key
// (so an object never crosses tenants) and returns the metadata JSON array the
// DEFINER function inserts as attachment rows — WITHOUT writing any bytes. Keys are
// fixed here, before ingest_inbound_message runs, so the rows it inserts and the
// blobs putAttachments later writes agree exactly. When no blob store is configured
// (attachments disabled) it returns an empty array so no attachment rows — and
// hence no orphaned blobs — are created.
func (s *Service) buildAttachmentMeta(r route, staged []sniffedAttachment) ([]byte, error) {
	if len(staged) == 0 || s.blob == nil {
		// No store ⇒ attachments disabled: emit no rows so nothing references a blob
		// that will never be written.
		return []byte("[]"), nil
	}
	meta := make([]map[string]any, 0, len(staged))
	for i := range staged {
		// The attachment id makes the key unique within the tenant; the ticket id is
		// not yet known (the DEFINER may create it), and the key must be fixed before
		// the call so the inserted rows reference exactly what we Put afterward.
		key := blob.Key(r.tenantRootID, r.businessID, staged[i].id, staged[i].id)
		staged[i].blobKey = key
		meta = append(meta, map[string]any{
			"blob_key":     key,
			"filename":     staged[i].filename,
			"content_type": staged[i].contentType,
			"size":         staged[i].size,
		})
	}
	raw, err := json.Marshal(meta)
	if err != nil {
		return nil, fmt.Errorf("inbox: marshal attachments: %w", err)
	}
	return raw, nil
}

// putAttachments writes each staged attachment's bytes to object storage under the
// key buildAttachmentMeta already assigned. It is called ONLY after a non-duplicate
// ingest_inbound_message result, so a replay (out_duplicate) writes zero blobs and
// nothing is orphaned. It runs inside the ingestion tx: a Put failure propagates so
// the DB rows roll back, keeping rows and blobs consistent. With no blob store the
// staged parts were never put into the attachment metadata, so this is a no-op.
func (s *Service) putAttachments(ctx context.Context, staged []sniffedAttachment) error {
	if s.blob == nil {
		return nil
	}
	for i := range staged {
		if err := s.blob.Put(ctx, staged[i].blobKey, staged[i].content, staged[i].contentType); err != nil {
			return fmt.Errorf("inbox: store attachment: %w", err)
		}
	}
	return nil
}

// nilIfEmpty maps "" to a nil *string so an absent header is stored as SQL NULL
// (not an empty string), matching the nullable columns' intent.
func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
