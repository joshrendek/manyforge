// Package feedback owns the feedback / feature-request boards surface (spec 006). A business
// runs one or more boards; users submit posts, move them through a status workflow, vote (one
// vote per identity), and a post can be converted to a support ticket (spec 002).
//
// Two ingress paths share the schema:
//   - Authenticated (this file + board.go/post.go/ingestkey.go/handler.go): services take
//     (ctx, principalID, businessID, …); the businessID is the tenant context from the URL
//     (RLS gates the principal's visibility of it), the tenant_root_id is resolved inside the
//     WithPrincipal tx, and every dbgen query also filters tenant_root_id (dual enforcement) —
//     unknown / foreign-tenant / soft-deleted collapse to ErrNotFound (no existence oracle).
//   - Public SDK / portal (public.go): principal-less, authenticated by a publishable board
//     key, all DB access through the SECURITY DEFINER functions of migration 0102.
package feedback

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/manyforge/manyforge/internal/platform/errs"
)

// Board is the API view of a feedback board.
type Board struct {
	ID           uuid.UUID `json:"id"`
	BusinessID   uuid.UUID `json:"business_id"`
	TenantRootID uuid.UUID `json:"tenant_root_id"`
	Slug         string    `json:"slug"`
	Name         string    `json:"name"`
	Description  *string   `json:"description,omitempty"`
	IsPublic     bool      `json:"is_public"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// BoardInput is the create payload. Slug is optional (derived from Name when empty).
type BoardInput struct {
	Slug        string
	Name        string
	Description *string
	IsPublic    bool
}

// BoardUpdate is the partial-update payload; a nil field is preserved (COALESCE narg).
type BoardUpdate struct {
	Name        *string
	Description *string
	IsPublic    *bool
}

// Post is the API view of a feedback post. AuthorPrincipalID/AuthorIdentity/TicketID are
// optional; deleted posts are never surfaced.
type Post struct {
	ID                uuid.UUID  `json:"id"`
	BusinessID        uuid.UUID  `json:"business_id"`
	TenantRootID      uuid.UUID  `json:"tenant_root_id"`
	BoardID           uuid.UUID  `json:"board_id"`
	Title             string     `json:"title"`
	Body              *string    `json:"body,omitempty"`
	Status            string     `json:"status"`
	VoteCount         int        `json:"vote_count"`
	AuthorKind        string     `json:"author_kind"`
	AuthorPrincipalID *uuid.UUID `json:"author_principal_id,omitempty"`
	AuthorIdentity    *string    `json:"author_identity,omitempty"`
	TicketID          *uuid.UUID `json:"ticket_id,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

// PostInput is the internal (principal-authored) submission payload.
type PostInput struct {
	Title string
	Body  *string
}

// IngestKey is the API view of a publishable ingest key. PublishableKey is the public client
// token embedded in an SDK (safe to expose); it is not a secret.
type IngestKey struct {
	ID             uuid.UUID  `json:"id"`
	BusinessID     uuid.UUID  `json:"business_id"`
	TenantRootID   uuid.UUID  `json:"tenant_root_id"`
	BoardID        uuid.UUID  `json:"board_id"`
	PublishableKey string     `json:"publishable_key"`
	Label          *string    `json:"label,omitempty"`
	Status         string     `json:"status"`
	CreatedAt      time.Time  `json:"created_at"`
	RevokedAt      *time.Time `json:"revoked_at,omitempty"`
}

// Page is a keyset-paginated result. NextCursor is an opaque token (nil = last page).
type Page[T any] struct {
	Items      []T     `json:"items"`
	NextCursor *string `json:"next_cursor,omitempty"`
}

// mapErr converts a query/closure error into a stable service-layer sentinel. pgx.ErrNoRows →
// ErrNotFound (no oracle). 23505 (unique) → ErrConflict (duplicate board slug / publishable
// key). 23503 (FK) / 23514 (check) → ErrConflict / ErrValidation. ErrValidation/ErrNotFound/
// ErrConflict are preserved; everything else is wrapped for a generic server-side-logged 500.
func mapErr(err error) error {
	var pgErr *pgconn.PgError
	switch {
	case err == nil:
		return nil
	case errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("feedback: not found: %w", errs.ErrNotFound)
	case errors.As(err, &pgErr) && pgErr.Code == "23505":
		return fmt.Errorf("feedback: duplicate: %w", errs.ErrConflict)
	case errors.As(err, &pgErr) && pgErr.Code == "23503":
		return fmt.Errorf("feedback: foreign key violation: %w", errs.ErrConflict)
	case errors.As(err, &pgErr) && pgErr.Code == "23514":
		return fmt.Errorf("feedback: check violation: %w", errs.ErrValidation)
	case errors.Is(err, errs.ErrValidation), errors.Is(err, errs.ErrNotFound),
		errors.Is(err, errs.ErrConflict):
		return err
	default:
		return fmt.Errorf("feedback: query: %w", err)
	}
}

// clampLimit applies the service-boundary page cap: non-positive → default; oversized → capped.
func clampLimit(requested int) int {
	const def, max = 50, 200
	switch {
	case requested <= 0:
		return def
	case requested > max:
		return max
	default:
		return requested
	}
}

// trim drops the sentinel (limit+1)th row used to detect a further page.
func trim[T any](rows []T, lim int) ([]T, bool) {
	if len(rows) > lim {
		return rows[:lim], true
	}
	return rows, false
}

func ptr[T any](v T) *T { return &v }

// pgUUIDPtr converts a nullable pgtype.UUID column into an optional uuid.UUID (NULL → nil).
func pgUUIDPtr(u pgtype.UUID) *uuid.UUID {
	if !u.Valid {
		return nil
	}
	v := uuid.UUID(u.Bytes)
	return &v
}

// pgTimePtr converts a nullable pgtype.Timestamptz into an optional time.Time (NULL → nil).
func pgTimePtr(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time
	return &v
}
