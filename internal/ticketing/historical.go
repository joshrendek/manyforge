package ticketing

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/audit"
	"github.com/manyforge/manyforge/internal/platform/blob"
	"github.com/manyforge/manyforge/internal/platform/errs"
)

const MaxHistoricalAttachmentBytes = 128 << 20

// HistoricalTicket is an owner-supplied snapshot of one external conversation.
// Status and Priority are native values chosen by the caller. SourceStatus and
// SourceResolution preserve the original independently of that mapping.
type HistoricalTicket struct {
	Source           string              `json:"source"`
	ID               string              `json:"id"`
	Subject          string              `json:"subject"`
	Status           string              `json:"status"`
	Priority         string              `json:"priority"`
	SourceStatus     string              `json:"source_status"`
	SourceResolution *string             `json:"source_resolution"`
	CreatedAt        time.Time           `json:"created_at"`
	UpdatedAt        time.Time           `json:"updated_at"`
	RequesterEmail   string              `json:"requester_email"`
	RequesterName    string              `json:"requester_name"`
	Owner            *HistoricalOwner    `json:"owner,omitempty"`
	Metadata         json.RawMessage     `json:"metadata,omitempty"`
	Messages         []HistoricalMessage `json:"messages"`
}

// HistoricalOwner is an explicit mapping from a trusted source identity to a
// portal identity. Importing or verifying an email never creates this mapping.
type HistoricalOwner struct {
	Namespace string `json:"namespace"`
	Subject   string `json:"subject"`
}

type HistoricalMessage struct {
	ID          string                 `json:"id"`
	Direction   string                 `json:"direction"`
	Body        string                 `json:"body"`
	CreatedAt   time.Time              `json:"created_at"`
	UpdatedAt   time.Time              `json:"updated_at"`
	AuthorID    string                 `json:"author_id"`
	AuthorName  string                 `json:"author_name"`
	AuthorKind  string                 `json:"author_kind"`
	Metadata    json.RawMessage        `json:"metadata,omitempty"`
	Attachments []HistoricalAttachment `json:"attachments,omitempty"`
}

type HistoricalAttachment struct {
	AttachmentInput
	ID              string    `json:"id"`
	CreatedAt       time.Time `json:"created_at"`
	CustomerVisible bool      `json:"customer_visible"`
}

type HistoricalResult struct {
	TicketID uuid.UUID `json:"ticket_id"`
	Created  bool      `json:"created"`
}

func historicalJSON(v json.RawMessage) bool {
	if len(v) == 0 {
		return true
	}
	trimmed := bytes.TrimSpace(v)
	return len(v) <= 1<<20 && len(trimmed) > 0 && trimmed[0] == '{' && json.Valid(trimmed)
}

// ValidateHistoricalTicket checks a snapshot without opening the database or blob store.
func ValidateHistoricalTicket(in HistoricalTicket) error {
	bad := func(field string) error { return fmt.Errorf("%w: invalid historical %s", errs.ErrValidation, field) }
	if !portalNamespacePattern.MatchString(in.Source) || !portalSubjectPattern.MatchString(in.ID) {
		return bad("source identity")
	}
	if !portalText(in.Subject, 1200) || utf8.RuneCountInString(in.Subject) > 300 ||
		len(in.SourceStatus) > 255 || in.SourceStatus == "" ||
		(in.SourceResolution != nil && len(*in.SourceResolution) > 255) ||
		!isValidStatus(in.Status) || !isValidPriority(in.Priority) || in.CreatedAt.IsZero() ||
		in.UpdatedAt.Before(in.CreatedAt) || !validPortalDisplayName(in.RequesterName) || !historicalJSON(in.Metadata) {
		return bad("ticket fields")
	}
	for _, r := range in.Subject {
		if unicode.IsControl(r) {
			return bad("subject")
		}
	}
	if _, err := portalEmail(in.RequesterEmail); err != nil {
		return bad("requester email")
	}
	if in.Owner != nil {
		if _, valid := portalExternalID(in.Owner.Namespace, in.Owner.Subject); !valid {
			return bad("owner")
		}
	}
	if len(in.Messages) > 1000 {
		return bad("message count")
	}
	ids := make(map[string]bool, len(in.Messages))
	var totalBytes int64
	totalTextBytes := int64(len(in.Metadata))
	for _, m := range in.Messages {
		totalTextBytes += int64(len(m.Body) + len(m.Metadata))
		if !portalSubjectPattern.MatchString(m.ID) || ids[m.ID] ||
			(m.Direction != "inbound" && m.Direction != "outbound" && m.Direction != "note") ||
			m.CreatedAt.IsZero() || m.UpdatedAt.Before(m.CreatedAt) ||
			(m.Body != "" && !portalText(m.Body, 1<<20)) ||
			(m.Body == "" && len(m.Attachments) == 0) ||
			len(m.AuthorID) > 256 || !validPortalDisplayName(m.AuthorName) ||
			(m.AuthorKind != "customer" && m.AuthorKind != "staff" && m.AuthorKind != "automation" && m.AuthorKind != "unknown") ||
			!historicalJSON(m.Metadata) || totalTextBytes > 16<<20 || len(m.Attachments) > 100 {
			return bad("message fields")
		}
		ids[m.ID] = true
		fileIDs := make(map[string]bool, len(m.Attachments))
		for _, file := range m.Attachments {
			totalBytes += int64(len(file.Content))
			if !portalSubjectPattern.MatchString(file.ID) || fileIDs[file.ID] || !validAttachmentName(file.Filename) ||
				file.CreatedAt.IsZero() || len(file.Content) == 0 || len(file.Content) > maxHistoricalFileBytes ||
				totalBytes > MaxHistoricalAttachmentBytes {
				return bad("attachment")
			}
			fileIDs[file.ID] = true
		}
	}
	return nil
}

func historicalDigest(in HistoricalTicket) ([]byte, error) {
	email, _ := portalEmail(in.RequesterEmail)
	h := sha256.New()
	// Hash file bytes directly and omit them from the JSON encoding, which
	// keeps the encoded snapshot small even for a large attachment.
	digestInput := in
	digestInput.RequesterEmail = email
	digestInput.Messages = append([]HistoricalMessage(nil), in.Messages...)
	for i, m := range in.Messages {
		files := append([]HistoricalAttachment(nil), m.Attachments...)
		for j, file := range files {
			sum := sha256.Sum256(file.Content)
			files[j].Content = sum[:]
		}
		digestInput.Messages[i].Attachments = files
	}
	if err := json.NewEncoder(h).Encode(digestInput); err != nil {
		return nil, errs.ErrValidation
	}
	return h.Sum(nil), nil
}

// ImportHistoricalTicket creates a native thread and audit entry without emitting
// live ticket events. Exact replays return the original ID. Changed snapshots conflict.
func (s *PortalService) ImportHistoricalTicket(ctx context.Context, principal, business uuid.UUID, in HistoricalTicket) (HistoricalResult, error) {
	var out HistoricalResult
	if err := ValidateHistoricalTicket(in); err != nil {
		return out, err
	}
	if len(s.ReplyTokenKey) < 32 || s.SystemDomain == "" {
		return out, ErrPortalUnavailable
	}
	digest, err := historicalDigest(in)
	if err != nil {
		return out, err
	}
	in.RequesterEmail, _ = portalEmail(in.RequesterEmail)
	batch := attachmentBatch{store: s.Blob}
	err = s.DB.WithPrincipal(ctx, principal, func(tx pgx.Tx) error {
		if err := s.allowed(ctx, tx, principal, business, true); err != nil {
			return err
		}
		var root uuid.UUID
		// Serialize one business's imports through commit. The same lock is
		// used for other owner-level portal setup.
		if err := tx.QueryRow(ctx, `SELECT tenant_root_id FROM business WHERE id=$1 AND status='active' AND deleted_at IS NULL FOR UPDATE`, business).Scan(&root); err != nil {
			return portalNotFound(err)
		}
		var oldHash []byte
		err := tx.QueryRow(ctx, `SELECT id,historical_hash FROM ticket WHERE business_id=$1 AND tenant_root_id=$2 AND historical_source=$3 AND historical_id=$4`,
			business, root, in.Source, in.ID).Scan(&out.TicketID, &oldHash)
		if err == nil {
			if !bytes.Equal(oldHash, digest) {
				return errs.ErrConflict
			}
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err := s.insertHistoricalTicket(ctx, tx, &batch, principal, business, root, digest, in, &out); err != nil {
			batch.discard(ctx)
			return err
		}
		return nil
	})
	if err != nil {
		return HistoricalResult{}, err
	}
	return out, nil
}

func (s *PortalService) insertHistoricalTicket(ctx context.Context, tx pgx.Tx, batch *attachmentBatch, principal, business, root uuid.UUID, digest []byte, in HistoricalTicket, out *HistoricalResult) error {
	var requester uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT resolve_support_portal_requester($1,$2,$3,$4)`, business, root, in.RequesterEmail, in.RequesterName).Scan(&requester); err != nil {
		return err
	}
	var ownerID string
	if in.Owner != nil {
		ownerID, _ = portalExternalID(in.Owner.Namespace, in.Owner.Subject)
		if _, err := tx.Exec(ctx, `INSERT INTO support_portal_contact(business_id,tenant_root_id,external_id,display_name)
			VALUES($1,$2,$3,$4) ON CONFLICT(business_id,external_id) DO NOTHING`, business, root, ownerID, in.RequesterName); err != nil {
			return err
		}
		// A previously verified contact's current address is authoritative for
		// future notifications on that explicitly assigned thread.
		if err := tx.QueryRow(ctx, `SELECT COALESCE(requester_id,$4) FROM support_portal_contact
			WHERE business_id=$1 AND tenant_root_id=$2 AND external_id=$3 FOR UPDATE`, business, root, ownerID, requester).Scan(&requester); err != nil {
			return err
		}
	}
	lastMessageAt := in.CreatedAt
	for _, m := range in.Messages {
		if m.CreatedAt.After(lastMessageAt) {
			lastMessageAt = m.CreatedAt
		}
	}
	out.TicketID = uuid.New()
	ticketMeta, _ := json.Marshal(map[string]any{
		"status": in.SourceStatus, "resolution": in.SourceResolution, "metadata": in.Metadata,
		"requester_email": in.RequesterEmail, "requester_name": in.RequesterName,
	})
	if _, err := tx.Exec(ctx, `
		INSERT INTO ticket(id,business_id,tenant_root_id,requester_id,subject,status,priority,reply_token,last_message_at,created_at,updated_at,
			historical_source,historical_id,historical_hash,historical_metadata)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
		out.TicketID, business, root, requester, in.Subject, in.Status, in.Priority, SignReplyToken(out.TicketID, s.ReplyTokenKey),
		lastMessageAt, in.CreatedAt, in.UpdatedAt, in.Source, in.ID, digest, ticketMeta); err != nil {
		return err
	}
	if ownerID != "" {
		if _, err := tx.Exec(ctx, `INSERT INTO support_portal_ticket(ticket_id,business_id,tenant_root_id,external_id,created_at)
			VALUES($1,$2,$3,$4,$5)`, out.TicketID, business, root, ownerID, in.CreatedAt); err != nil {
			return err
		}
	}
	for _, m := range in.Messages {
		if err := s.insertHistoricalMessage(ctx, tx, batch, business, root, out.TicketID, m); err != nil {
			return err
		}
	}
	out.Created = true
	return audit.Write(ctx, tx, audit.Entry{
		BusinessID: &business, TenantRootID: &root, ActorPrincipalID: &principal,
		Action: "ticket.history_imported", TargetType: ptr("ticket"), TargetID: &out.TicketID,
	})
}

func (s *PortalService) insertHistoricalMessage(ctx context.Context, tx pgx.Tx, batch *attachmentBatch, business, root, ticket uuid.UUID, m HistoricalMessage) error {
	// Thread queries use the message ID to break timestamp ties. Use ordered IDs
	// so those messages follow their order in the source snapshot.
	message, err := uuid.NewV7()
	if err != nil {
		return err
	}
	meta, _ := json.Marshal(map[string]any{
		"author_id": m.AuthorID, "author_name": m.AuthorName, "author_kind": m.AuthorKind, "updated_at": m.UpdatedAt, "metadata": m.Metadata,
	})
	if _, err := tx.Exec(ctx, `
		INSERT INTO ticket_message(id,ticket_id,business_id,tenant_root_id,direction,message_id,body_text,created_at,historical_id,historical_metadata)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		message, ticket, business, root, m.Direction, message.String()+"@history."+s.SystemDomain, m.Body, m.CreatedAt, m.ID, meta); err != nil {
		return err
	}
	for _, file := range m.Attachments {
		prepared := prepareHistoricalAttachment(m.Direction, file)
		if _, err := batch.add(ctx, tx, root, business, ticket, message, prepared); err != nil {
			return err
		}
	}
	return nil
}

func prepareHistoricalAttachment(direction string, file HistoricalAttachment) preparedAttachment {
	contentType, err := blob.Sniff(file.Content)
	if err != nil {
		contentType = "application/octet-stream"
	}
	return preparedAttachment{
		AttachmentInput: file.AttachmentInput, contentType: contentType, createdAt: file.CreatedAt,
		staffOnly: !file.CustomerVisible || direction == "note" || err != nil, historicalID: &file.ID,
	}
}
