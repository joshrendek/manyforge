package ticketing

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/events"
)

type PortalTicket struct {
	ID        uuid.UUID `json:"id"`
	Subject   string    `json:"subject"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
type PortalMessage struct {
	ID        uuid.UUID `json:"id"`
	Direction string    `json:"direction"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}
type PortalThread struct {
	Ticket     PortalTicket    `json:"ticket"`
	Messages   []PortalMessage `json:"messages"`
	NextCursor string          `json:"next_cursor,omitempty"`
}

func portalText(v string, max int) bool {
	if !utf8.ValidString(v) || strings.TrimSpace(v) == "" || len(v) > max {
		return false
	}
	for _, r := range v {
		if unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t' {
			return false
		}
	}
	return true
}

func (s *PortalService) ListTickets(ctx context.Context, p PortalScope) ([]PortalTicket, error) {
	out := make([]PortalTicket, 0)
	err := s.withScope(ctx, p, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT t.id,t.subject,t.status::text,t.created_at,t.updated_at FROM ticket t JOIN support_portal_ticket pt ON pt.ticket_id=t.id AND pt.business_id=t.business_id AND pt.tenant_root_id=t.tenant_root_id WHERE t.business_id=$1 AND t.tenant_root_id=$2 AND pt.external_id=$3 AND t.redacted_at IS NULL ORDER BY t.created_at DESC,t.id DESC LIMIT 200`, p.BusinessID, p.TenantRootID, p.ExternalID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var t PortalTicket
			if err := rows.Scan(&t.ID, &t.Subject, &t.Status, &t.CreatedAt, &t.UpdatedAt); err != nil {
				return err
			}
			out = append(out, t)
		}
		return rows.Err()
	})
	return out, err
}

func portalReadTicket(ctx context.Context, tx pgx.Tx, p PortalScope, id uuid.UUID) (PortalTicket, error) {
	var t PortalTicket
	err := tx.QueryRow(ctx, `SELECT t.id,t.subject,t.status::text,t.created_at,t.updated_at FROM ticket t JOIN support_portal_ticket pt ON pt.ticket_id=t.id AND pt.business_id=t.business_id AND pt.tenant_root_id=t.tenant_root_id WHERE t.id=$4 AND t.business_id=$1 AND t.tenant_root_id=$2 AND pt.external_id=$3 AND t.redacted_at IS NULL`, p.BusinessID, p.TenantRootID, p.ExternalID, id).Scan(&t.ID, &t.Subject, &t.Status, &t.CreatedAt, &t.UpdatedAt)
	return t, portalNotFound(err)
}

func (s *PortalService) Thread(ctx context.Context, p PortalScope, id uuid.UUID, before string) (PortalThread, error) {
	out := PortalThread{Messages: make([]PortalMessage, 0)}
	var cursor *uuid.UUID
	if before != "" {
		u, err := portalUUID(before)
		if err != nil {
			return out, ErrPortalInvalid
		}
		cursor = &u
	}
	err := s.withScope(ctx, p, func(tx pgx.Tx) error {
		var err error
		out.Ticket, err = portalReadTicket(ctx, tx, p, id)
		if err != nil {
			return err
		}
		if cursor != nil {
			var exists bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM ticket_message WHERE id=$1 AND ticket_id=$2 AND business_id=$3 AND tenant_root_id=$4 AND direction IN('inbound','outbound'))`, cursor, id, p.BusinessID, p.TenantRootID).Scan(&exists); err != nil {
				return err
			}
			if !exists {
				return errs.ErrNotFound
			}
		}
		rows, err := tx.Query(ctx, `SELECT m.id,m.direction::text,CASE WHEN octet_length(COALESCE(m.body_text,''))<=1048576 THEN COALESCE(m.body_text,'') ELSE NULL END,m.created_at FROM ticket_message m WHERE m.ticket_id=$1 AND m.business_id=$2 AND m.tenant_root_id=$3 AND m.direction IN('inbound','outbound') AND ($4::uuid IS NULL OR (m.created_at,m.id)<(SELECT c.created_at,c.id FROM ticket_message c WHERE c.id=$4 AND c.ticket_id=$1 AND c.business_id=$2 AND c.tenant_root_id=$3 AND c.direction IN('inbound','outbound'))) ORDER BY m.created_at DESC,m.id DESC LIMIT 21`, id, p.BusinessID, p.TenantRootID, cursor)
		if err != nil {
			return err
		}
		defer rows.Close()
		// Preserve complete messages, including Unicode and native operator
		// replies. Bound each row in SQL before allocation, and each encoded
		// page before returning it. Oversized legacy messages fail explicitly.
		encodedBytes := 0
		for rows.Next() {
			var m PortalMessage
			var body *string
			if err := rows.Scan(&m.ID, &m.Direction, &body, &m.CreatedAt); err != nil {
				return err
			}
			if body == nil {
				return ErrPortalUnavailable
			}
			m.Body = *body
			raw, err := json.Marshal(m)
			if err != nil {
				return err
			}
			if len(out.Messages) == 20 || encodedBytes+len(raw) > (6<<20)+(4<<10) {
				out.NextCursor = out.Messages[len(out.Messages)-1].ID.String()
				break
			}
			out.Messages = append(out.Messages, m)
			encodedBytes += len(raw)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		return nil
	})
	return out, err
}

func (s *PortalService) CreateTicket(ctx context.Context, p PortalScope, subject, body string, requestID uuid.UUID) (PortalTicket, error) {
	var out PortalTicket
	if !portalText(subject, 1200) || utf8.RuneCountInString(subject) > 300 || strings.IndexFunc(subject, unicode.IsControl) >= 0 || !portalText(body, 20000) || requestID == uuid.Nil {
		return out, ErrPortalInvalid
	}
	if len(s.ReplyTokenKey) < 32 || s.SystemDomain == "" {
		return out, ErrPortalUnavailable
	}
	err := s.withScope(ctx, p, func(tx pgx.Tx) error {
		ticketID, _, _, err := s.portalWrite(ctx, tx, p, uuid.Nil, subject, body, requestID)
		if err != nil {
			return err
		}
		out, err = portalReadTicket(ctx, tx, p, ticketID)
		return err
	})
	return out, err
}

func (s *PortalService) AddMessage(ctx context.Context, p PortalScope, ticketID uuid.UUID, body string, requestID uuid.UUID) (PortalMessage, error) {
	var out PortalMessage
	if !portalText(body, 20000) || requestID == uuid.Nil || ticketID == uuid.Nil {
		return out, ErrPortalInvalid
	}
	err := s.withScope(ctx, p, func(tx pgx.Tx) error {
		_, mid, _, err := s.portalWrite(ctx, tx, p, ticketID, "", body, requestID)
		if err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT id,direction::text,body_text,created_at FROM ticket_message WHERE id=$1 AND ticket_id=$2 AND business_id=$3 AND tenant_root_id=$4 AND direction='inbound'`, mid, ticketID, p.BusinessID, p.TenantRootID).Scan(&out.ID, &out.Direction, &out.Body, &out.CreatedAt)
	})
	return out, err
}

// The contact row serializes all writes for this customer, including UUID dedup
// and the lifetime ticket cap. A request UUID identifies both route and exact body.
func (s *PortalService) portalWrite(ctx context.Context, tx pgx.Tx, p PortalScope, ticketID uuid.UUID, subject, body string, requestID uuid.UUID) (uuid.UUID, uuid.UUID, bool, error) {
	if ticketID != uuid.Nil {
		if _, err := portalReadTicket(ctx, tx, p, ticketID); err != nil {
			return uuid.Nil, uuid.Nil, false, err
		}
	}
	var requester *uuid.UUID
	err := tx.QueryRow(ctx, `SELECT requester_id FROM support_portal_contact WHERE business_id=$1 AND tenant_root_id=$2 AND external_id=$3 FOR UPDATE`, p.BusinessID, p.TenantRootID, p.ExternalID).Scan(&requester)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, uuid.Nil, false, ErrPortalConflict
	}
	if err != nil {
		return uuid.Nil, uuid.Nil, false, err
	}
	payload, _ := json.Marshal([]string{ticketID.String(), subject, body})
	hash := sha256.Sum256(payload)
	var oldHash []byte
	var oldTicket, oldMessage uuid.UUID
	err = tx.QueryRow(ctx, `SELECT body_hash,ticket_id,message_id FROM support_portal_request WHERE business_id=$1 AND tenant_root_id=$2 AND external_id=$3 AND request_id=$4`, p.BusinessID, p.TenantRootID, p.ExternalID, requestID).Scan(&oldHash, &oldTicket, &oldMessage)
	if err == nil {
		if !bytes.Equal(oldHash, hash[:]) {
			return uuid.Nil, uuid.Nil, false, ErrPortalConflict
		}
		if _, err := portalReadTicket(ctx, tx, p, oldTicket); err != nil {
			return uuid.Nil, uuid.Nil, false, err
		}
		return oldTicket, oldMessage, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, uuid.Nil, false, err
	}
	if requester == nil {
		return uuid.Nil, uuid.Nil, false, ErrPortalConflict
	}
	created := ticketID == uuid.Nil
	if created {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM support_portal_ticket WHERE business_id=$1 AND tenant_root_id=$2 AND external_id=$3`, p.BusinessID, p.TenantRootID, p.ExternalID).Scan(&n); err != nil {
			return uuid.Nil, uuid.Nil, false, err
		}
		if n >= 200 {
			return uuid.Nil, uuid.Nil, false, ErrPortalConflict
		}
		ticketID = uuid.New()
		_, err = tx.Exec(ctx, `INSERT INTO ticket(id,business_id,tenant_root_id,requester_id,subject,reply_token) VALUES($1,$2,$3,$4,$5,$6)`, ticketID, p.BusinessID, p.TenantRootID, requester, subject, SignReplyToken(ticketID, s.ReplyTokenKey))
		if err != nil {
			return uuid.Nil, uuid.Nil, false, err
		}
		_, err = tx.Exec(ctx, `INSERT INTO support_portal_ticket(ticket_id,business_id,tenant_root_id,external_id) VALUES($1,$2,$3,$4)`, ticketID, p.BusinessID, p.TenantRootID, p.ExternalID)
		if err != nil {
			return uuid.Nil, uuid.Nil, false, err
		}
	} else {
		if _, err := portalReadTicket(ctx, tx, p, ticketID); err != nil {
			return uuid.Nil, uuid.Nil, false, err
		}
		_, err = tx.Exec(ctx, `UPDATE ticket SET status=CASE WHEN status IN('pending','solved','closed') THEN 'open'::ticket_status ELSE status END,last_message_at=now(),updated_at=now() WHERE id=$1 AND business_id=$2 AND tenant_root_id=$3`, ticketID, p.BusinessID, p.TenantRootID)
		if err != nil {
			return uuid.Nil, uuid.Nil, false, err
		}
	}
	mid := uuid.New()
	_, err = tx.Exec(ctx, `INSERT INTO ticket_message(id,ticket_id,business_id,tenant_root_id,direction,message_id,body_text) VALUES($1,$2,$3,$4,'inbound',$5,$6)`, mid, ticketID, p.BusinessID, p.TenantRootID, mid.String()+"@portal."+s.SystemDomain, body)
	if err != nil {
		return uuid.Nil, uuid.Nil, false, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO support_portal_request(business_id,tenant_root_id,external_id,request_id,body_hash,ticket_id,message_id) VALUES($1,$2,$3,$4,$5,$6,$7)`, p.BusinessID, p.TenantRootID, p.ExternalID, requestID, hash[:], ticketID, mid)
	if err != nil {
		return uuid.Nil, uuid.Nil, false, err
	}
	action := "ticket.message.received"
	if created {
		action = "ticket.created"
	}
	if err := portalAudit(ctx, tx, p.PrincipalID, p.BusinessID, p.TenantRootID, action, ticketID); err != nil {
		return uuid.Nil, uuid.Nil, false, err
	}
	event := map[string]any{"ticket_id": ticketID, "business_id": p.BusinessID, "message_id": mid}
	if created {
		if err := events.Enqueue(ctx, tx, p.TenantRootID, events.TopicTicketCreated, event); err != nil {
			return uuid.Nil, uuid.Nil, false, err
		}
	}
	if err := events.Enqueue(ctx, tx, p.TenantRootID, events.TopicMessageReceived, event); err != nil {
		return uuid.Nil, uuid.Nil, false, err
	}
	return ticketID, mid, false, nil
}
