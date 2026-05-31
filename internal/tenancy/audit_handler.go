package tenancy

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/manyforge/manyforge/internal/authz"
	"github.com/manyforge/manyforge/internal/platform/db"
	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/httpx"
)

const (
	auditDefaultLimit = 50
	auditMaxLimit     = 200
)

// First-page keyset sentinel: every real row sorts before (year 9999, max uuid).
var (
	auditCursorStartTS = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
	auditCursorStartID = uuid.UUID{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
)

// AuditEntry is the metadata-only API view of an audit record (FR-016). It omits
// before/after values, which can carry sensitive state.
type AuditEntry struct {
	ID               string    `json:"id"`
	BusinessID       *string   `json:"business_id"`
	ActorPrincipalID *string   `json:"actor_principal_id"`
	Action           string    `json:"action"`
	TargetType       *string   `json:"target_type"`
	TargetID         *string   `json:"target_id"`
	CorrelationID    *string   `json:"correlation_id"`
	CreatedAt        time.Time `json:"created_at"`
}

// ListAudit returns a business's audit trail, newest first, keyset-paginated. The
// caller must hold audit.read at the business (FR-016); otherwise the business is
// reported as not-found (no oracle, FR-026). cursor is the opaque token from a
// prior page ("" for the first); the returned cursor is nil once exhausted.
func (s *Service) ListAudit(ctx context.Context, viewerID, businessID uuid.UUID, cursor string, limit int) ([]AuditEntry, *string, error) {
	if limit <= 0 || limit > auditMaxLimit {
		limit = auditDefaultLimit
	}
	beforeTS, beforeID, err := decodeAuditCursor(cursor)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid cursor: %w", errs.ErrValidation)
	}
	var out []AuditEntry
	var next *string
	err = s.DB.WithPrincipal(ctx, viewerID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		if _, err := loadVisible(ctx, q, businessID); err != nil {
			return err
		}
		perms, err := authz.Resolve(ctx, tx, viewerID, businessID)
		if err != nil {
			return err
		}
		if !perms.Has("audit.read") {
			return errs.ErrNotFound
		}
		rows, err := q.ListAuditEntries(ctx, dbgen.ListAuditEntriesParams{
			BusinessID: db.PGUUID(businessID), BeforeCreatedAt: beforeTS, BeforeID: beforeID, Lim: int32(limit + 1),
		})
		if err != nil {
			return err
		}
		hasMore := len(rows) > limit
		if hasMore {
			rows = rows[:limit]
		}
		out = make([]AuditEntry, 0, len(rows))
		for _, r := range rows {
			out = append(out, AuditEntry{
				ID: r.ID.String(), BusinessID: pgUUIDPtr(r.BusinessID), ActorPrincipalID: pgUUIDPtr(r.ActorPrincipalID),
				Action: r.Action, TargetType: r.TargetType, TargetID: pgUUIDPtr(r.TargetID),
				CorrelationID: r.CorrelationID, CreatedAt: r.CreatedAt,
			})
		}
		if hasMore && len(rows) > 0 {
			last := rows[len(rows)-1]
			c := encodeAuditCursor(last.CreatedAt, last.ID)
			next = &c
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return out, next, nil
}

func pgUUIDPtr(u pgtype.UUID) *string {
	if !u.Valid {
		return nil
	}
	s := uuid.UUID(u.Bytes).String()
	return &s
}

func encodeAuditCursor(ts time.Time, id uuid.UUID) string {
	return base64.RawURLEncoding.EncodeToString([]byte(ts.UTC().Format(time.RFC3339Nano) + "|" + id.String()))
}

func decodeAuditCursor(s string) (time.Time, uuid.UUID, error) {
	if s == "" {
		return auditCursorStartTS, auditCursorStartID, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return time.Time{}, uuid.Nil, err
	}
	parts := strings.SplitN(string(raw), "|", 2)
	if len(parts) != 2 {
		return time.Time{}, uuid.Nil, fmt.Errorf("malformed cursor")
	}
	ts, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return time.Time{}, uuid.Nil, err
	}
	id, err := uuid.Parse(parts[1])
	if err != nil {
		return time.Time{}, uuid.Nil, err
	}
	return ts, id, nil
}

// listAudit handles GET /businesses/{id}/audit.
func (h *Handler) listAudit(w http.ResponseWriter, r *http.Request) {
	pid, ok := h.principal(w, r)
	if !ok {
		return
	}
	id, err := pathID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	limit := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, e := strconv.Atoi(v); e == nil {
			limit = n
		}
	}
	entries, next, err := h.svc.ListAudit(r.Context(), pid, id, r.URL.Query().Get("cursor"), limit)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page[AuditEntry]{Items: entries, NextCursor: next})
}
