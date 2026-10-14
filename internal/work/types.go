// Package work owns the Work Board (spec 018): per-business work projects whose tasks live in
// an agent lane (picked up by coding agents through the `mf` CLI) or an operator lane (things
// only a human can do), moving through a backlog and an active board.
//
// Two ingress paths share the schema:
//   - Management (service.go + handler.go): session JWT + RBAC (work.read / work.write).
//     Services take (ctx, principalID, businessID, …); the tenant_root_id is resolved inside
//     the WithPrincipal tx and every dbgen query also filters tenant_root_id (dual enforcement)
//     — unknown / foreign-tenant collapse to ErrNotFound (no existence oracle).
//   - `mf sync` (sync.go + sync_handler.go): a project-scoped mfw_ work token, resolved through
//     the SECURITY DEFINER work_resolve_token(); the sync then runs WithPrincipal(issuer) and
//     re-checks work.write, so a token dies with its issuer's access.
//
// Every task write locks the parent project row first, so a project's rev order (the sync
// cursor) equals its commit order.
package work

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
	"github.com/manyforge/manyforge/internal/platform/errs"
)

// Counts are the per-project board tallies shown on the project list.
type Counts struct {
	Backlog      int `json:"backlog"`
	Todo         int `json:"todo"`
	InProgress   int `json:"in_progress"`
	Blocked      int `json:"blocked"`
	OperatorOpen int `json:"operator_open"`
}

// Project is the management view of a work project.
type Project struct {
	ID        uuid.UUID `json:"id"`
	Slug      string    `json:"slug"`
	Name      string    `json:"name"`
	KeyPrefix string    `json:"key_prefix"`
	CreatedAt time.Time `json:"created_at"`
	Counts    Counts    `json:"counts"`
}

// SyncProject is the project as `mf` sees it in a sync response.
type SyncProject struct {
	Slug      string `json:"slug"`
	Name      string `json:"name"`
	KeyPrefix string `json:"key_prefix"`
}

// Task is the wire shape shared by the management API and `mf sync`. Every field is always
// present: labels/blocked_by are [] (never null) and closed_at is null while open.
type Task struct {
	Key         string     `json:"key"`
	Title       string     `json:"title"`
	Description string     `json:"description"`
	Notes       string     `json:"notes"`
	Lane        string     `json:"lane"`
	Status      string     `json:"status"`
	Priority    int        `json:"priority"`
	Type        string     `json:"type"`
	Assignee    string     `json:"assignee"`
	Labels      []string   `json:"labels"`
	BlockedBy   []string   `json:"blocked_by"`
	Rev         int64      `json:"rev"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	ClosedAt    *time.Time `json:"closed_at"`
}

// Token is the management view of a work token. The plaintext exists only in CreatedToken.
type Token struct {
	ID         uuid.UUID  `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	RevokedAt  *time.Time `json:"revoked_at"`
}

// CreatedToken is the create-only response: the token view plus the plaintext, returned once.
type CreatedToken struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	Prefix    string    `json:"prefix"`
	CreatedAt time.Time `json:"created_at"`
	Token     string    `json:"token"`
}

// Summary is the business-wide nav badge tally.
type Summary struct {
	OpenOperator int `json:"open_operator"`
	OpenAgent    int `json:"open_agent"`
	InProgress   int `json:"in_progress"`
}

// ProjectInput is the project create payload. Slug/KeyPrefix are derived when empty.
type ProjectInput struct {
	Name      string
	Slug      string
	KeyPrefix string
}

// TaskFields is the task create/patch payload shared by the web API and sync ops. A nil field
// is "not sent" and preserves the stored value on update.
type TaskFields struct {
	Title       *string   `json:"title"`
	Description *string   `json:"description"`
	Lane        *string   `json:"lane"`
	Status      *string   `json:"status"`
	Priority    *int      `json:"priority"`
	Type        *string   `json:"type"`
	Assignee    *string   `json:"assignee"`
	Labels      *[]string `json:"labels"`
	BlockedBy   *[]string `json:"blocked_by"`
}

// createFields is a sync create op's fields: TaskFields plus the original timestamps an `mf`
// import preserves. They are create-only; an update op naming them is an unknown field.
type createFields struct {
	TaskFields
	CreatedAt *time.Time `json:"created_at"`
	ClosedAt  *time.Time `json:"closed_at"`
}

func (f TaskFields) empty() bool {
	return f.Title == nil && f.Description == nil && f.Lane == nil && f.Status == nil &&
		f.Priority == nil && f.Type == nil && f.Assignee == nil && f.Labels == nil && f.BlockedBy == nil
}

// mapErr converts a query/closure error into a stable service-layer sentinel. pgx.ErrNoRows →
// ErrNotFound (no oracle); 23505 → ErrConflict; 23514 → a safe validation message; typed
// sentinels are preserved; everything else is wrapped for a generic server-side-logged 500.
func mapErr(err error) error {
	var pgErr *pgconn.PgError
	switch {
	case err == nil:
		return nil
	case errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("work: not found: %w", errs.ErrNotFound)
	case errors.As(err, &pgErr) && pgErr.Code == "23505":
		return fmt.Errorf("work: duplicate: %w", errs.ErrConflict)
	case errors.As(err, &pgErr) && pgErr.Code == "23514":
		if pgErr.ConstraintName == "work_task_notes_len_chk" {
			return errValidation(fmt.Sprintf("notes would exceed %d characters", maxNotesLen))
		}
		return errValidation("invalid field value")
	case errors.Is(err, errs.ErrValidation), errors.Is(err, errs.ErrNotFound),
		errors.Is(err, errs.ErrConflict), errors.Is(err, errs.ErrTenantMaintenance),
		errors.Is(err, errTokenUnauthorized):
		return err
	default:
		return fmt.Errorf("work: query: %w", err)
	}
}

// errValidation builds a safe-to-surface 400 (mirrors feedback.errValidation).
func errValidation(msg string) error { return &validationError{msg: msg} }

type validationError struct{ msg string }

func (e *validationError) Error() string { return e.msg }
func (e *validationError) Is(target error) bool {
	return target == errs.ErrValidation
}

func ptr[T any](v T) *T { return &v }

func pgTimePtr(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time.UTC()
	return &v
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func toTask(t dbgen.WorkTask) Task {
	return Task{
		Key:         t.Key,
		Title:       t.Title,
		Description: t.Description,
		Notes:       t.Notes,
		Lane:        t.Lane,
		Status:      t.Status,
		Priority:    int(t.Priority),
		Type:        t.Type,
		Assignee:    t.Assignee,
		Labels:      nonNil(t.Labels),
		BlockedBy:   nonNil(t.BlockedBy),
		Rev:         t.Rev,
		CreatedAt:   t.CreatedAt.UTC(),
		UpdatedAt:   t.UpdatedAt.UTC(),
		ClosedAt:    pgTimePtr(t.ClosedAt),
	}
}

func toProject(p dbgen.WorkProject, c Counts) Project {
	return Project{
		ID:        p.ID,
		Slug:      p.Slug,
		Name:      p.Name,
		KeyPrefix: p.KeyPrefix,
		CreatedAt: p.CreatedAt.UTC(),
		Counts:    c,
	}
}

func toToken(id uuid.UUID, name, prefix string, createdAt time.Time, lastUsed, revoked pgtype.Timestamptz) Token {
	return Token{
		ID:         id,
		Name:       name,
		Prefix:     prefix,
		CreatedAt:  createdAt.UTC(),
		LastUsedAt: pgTimePtr(lastUsed),
		RevokedAt:  pgTimePtr(revoked),
	}
}
