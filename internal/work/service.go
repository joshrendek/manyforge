package work

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/manyforge/manyforge/internal/platform/audit"
	"github.com/manyforge/manyforge/internal/platform/auth"
	"github.com/manyforge/manyforge/internal/platform/db"
	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
	"github.com/manyforge/manyforge/internal/platform/errs"
)

const (
	// maxListTasks caps the board view; maxKeyAttempts bounds server-side key collisions.
	maxListTasks   = 2000
	maxKeyAttempts = 3
	// tokenPrefixLen is how much of the plaintext is kept for display ("mfw_" + 8 chars).
	tokenPrefixLen = 12
)

// Service is the Work Board surface. Management methods take the caller's principalID and the
// URL businessID and run inside db.WithPrincipal (RLS) with the tenant_root_id predicate also
// pushed into SQL. A {pid} project reference is a UUID or a slug.
type Service struct {
	DB *db.DB
}

// ListProjects returns every project in the business with its board counts.
func (s *Service) ListProjects(ctx context.Context, principalID, businessID uuid.UUID) ([]Project, error) {
	var out []Project
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		tenantRoot, err := resolveTenantRoot(ctx, q, businessID)
		if err != nil {
			return err
		}
		rows, err := q.ListWorkProjectsWithCounts(ctx, dbgen.ListWorkProjectsWithCountsParams{
			BusinessID: businessID, TenantRootID: tenantRoot,
		})
		if err != nil {
			return err
		}
		out = make([]Project, 0, len(rows))
		for _, r := range rows {
			out = append(out, Project{
				ID: r.ID, Slug: r.Slug, Name: r.Name, KeyPrefix: r.KeyPrefix, CreatedAt: r.CreatedAt.UTC(),
				Counts: Counts{
					Backlog: int(r.Backlog), Todo: int(r.Todo), InProgress: int(r.InProgress),
					Blocked: int(r.Blocked), OperatorOpen: int(r.OperatorOpen),
				},
			})
		}
		return nil
	})
	if err != nil {
		return nil, mapErr(err)
	}
	return out, nil
}

// CreateProject inserts a project; a duplicate slug in the business is ErrConflict.
func (s *Service) CreateProject(ctx context.Context, principalID, businessID uuid.UUID, in ProjectInput) (Project, error) {
	name, slug, prefix, err := normalizeProject(in)
	if err != nil {
		return Project{}, err
	}
	var out Project
	err = s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		tenantRoot, err := resolveTenantRoot(ctx, q, businessID)
		if err != nil {
			return err
		}
		row, err := q.InsertWorkProject(ctx, dbgen.InsertWorkProjectParams{
			ID: uuid.New(), BusinessID: businessID, TenantRootID: tenantRoot,
			Slug: slug, Name: name, KeyPrefix: prefix, CreatedBy: db.PGUUID(principalID),
		})
		if err != nil {
			return err
		}
		out = toProject(row, Counts{})
		return writeAudit(ctx, tx, row, principalID, "work.project.created", "work_project", row.ID,
			map[string]any{"slug": slug, "name": name, "key_prefix": prefix})
	})
	if err != nil {
		return Project{}, mapErr(err)
	}
	return out, nil
}

// GetProject loads one project (by id or slug) with its board counts.
func (s *Service) GetProject(ctx context.Context, principalID, businessID uuid.UUID, ref string) (Project, error) {
	var out Project
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		proj, err := loadProject(ctx, q, businessID, ref, false)
		if err != nil {
			return err
		}
		c, err := q.GetWorkProjectCounts(ctx, dbgen.GetWorkProjectCountsParams{
			ProjectID: proj.ID, TenantRootID: proj.TenantRootID,
		})
		if err != nil {
			return err
		}
		out = toProject(proj, Counts{
			Backlog: int(c.Backlog), Todo: int(c.Todo), InProgress: int(c.InProgress),
			Blocked: int(c.Blocked), OperatorOpen: int(c.OperatorOpen),
		})
		return nil
	})
	if err != nil {
		return Project{}, mapErr(err)
	}
	return out, nil
}

// ListTasks returns the board view: open tasks plus those closed in the last 14 days (all
// tasks when includeClosed), ordered by priority then age, capped at maxListTasks.
func (s *Service) ListTasks(ctx context.Context, principalID, businessID uuid.UUID, ref string, includeClosed bool) ([]Task, error) {
	var out []Task
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		proj, err := loadProject(ctx, q, businessID, ref, false)
		if err != nil {
			return err
		}
		rows, err := q.ListWorkTasks(ctx, dbgen.ListWorkTasksParams{
			ProjectID: proj.ID, TenantRootID: proj.TenantRootID, IncludeClosed: includeClosed, Lim: maxListTasks,
		})
		if err != nil {
			return err
		}
		out = make([]Task, 0, len(rows))
		for _, r := range rows {
			out = append(out, toTask(r))
		}
		return nil
	})
	if err != nil {
		return nil, mapErr(err)
	}
	return out, nil
}

// CreateTask inserts a task with a server-generated key, retrying a key collision (each attempt
// in its own savepoint so a unique violation does not poison the transaction).
func (s *Service) CreateTask(ctx context.Context, principalID, businessID uuid.UUID, ref string, f TaskFields) (Task, error) {
	f, err := normalizeFields(f)
	if err != nil {
		return Task{}, err
	}
	v, err := createValues(f)
	if err != nil {
		return Task{}, err
	}
	var out Task
	err = s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		proj, err := loadProject(ctx, dbgen.New(tx), businessID, ref, true)
		if err != nil {
			return err
		}
		var row dbgen.WorkTask
		for attempt := 1; ; attempt++ {
			key, kerr := newTaskKey(proj.KeyPrefix)
			if kerr != nil {
				return kerr
			}
			row, err = insertTaskSavepoint(ctx, tx, proj, key, v, principalID)
			if err == nil {
				break
			}
			if !isUniqueViolation(err, "work_task_key_uniq") || attempt == maxKeyAttempts {
				return err
			}
		}
		out = toTask(row)
		return writeAudit(ctx, tx, proj, principalID, "work.task.created", "work_task", row.ID,
			map[string]any{"key": row.Key, "title": row.Title, "lane": row.Lane, "status": row.Status})
	})
	if err != nil {
		return Task{}, mapErr(err)
	}
	return out, nil
}

// UpdateTask patches the sent fields of one task and optionally appends a note authored by the
// caller (display name, else email, else "operator").
func (s *Service) UpdateTask(ctx context.Context, principalID, businessID uuid.UUID, ref, key string, f TaskFields, note *string) (Task, error) {
	f, err := normalizeFields(f)
	if err != nil {
		return Task{}, err
	}
	if f.empty() && note == nil {
		return Task{}, errValidation("no fields to update")
	}
	var out Task
	err = s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		proj, err := loadProject(ctx, q, businessID, ref, true)
		if err != nil {
			return err
		}
		var entry *string
		if note != nil {
			author, aerr := operatorName(ctx, q, principalID)
			if aerr != nil {
				return aerr
			}
			e, ferr := formatNote(time.Now(), author, "operator", "", *note)
			if ferr != nil {
				return ferr
			}
			entry = &e
		}
		row, err := patchTask(ctx, q, proj, key, f, entry)
		if err != nil {
			return err
		}
		out = toTask(row)
		return writeAudit(ctx, tx, proj, principalID, "work.task.updated", "work_task", row.ID,
			auditPatch(row.Key, f, entry != nil))
	})
	if err != nil {
		return Task{}, mapErr(err)
	}
	return out, nil
}

// ListTokens lists a project's work tokens (never their hashes).
func (s *Service) ListTokens(ctx context.Context, principalID, businessID uuid.UUID, ref string) ([]Token, error) {
	var out []Token
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		proj, err := loadProject(ctx, q, businessID, ref, false)
		if err != nil {
			return err
		}
		rows, err := q.ListWorkTokens(ctx, dbgen.ListWorkTokensParams{ProjectID: proj.ID, TenantRootID: proj.TenantRootID})
		if err != nil {
			return err
		}
		out = make([]Token, 0, len(rows))
		for _, r := range rows {
			out = append(out, toToken(r.ID, r.Name, r.TokenPrefix, r.CreatedAt, r.LastUsedAt, r.RevokedAt))
		}
		return nil
	})
	if err != nil {
		return nil, mapErr(err)
	}
	return out, nil
}

// CreateToken mints an mfw_ work token issued by the caller. Only its SHA-256 hash is stored;
// the plaintext is returned exactly once.
func (s *Service) CreateToken(ctx context.Context, principalID, businessID uuid.UUID, ref, name string) (CreatedToken, error) {
	name, err := validTokenName(name)
	if err != nil {
		return CreatedToken{}, err
	}
	plaintext, err := newWorkToken()
	if err != nil {
		return CreatedToken{}, err
	}
	var out CreatedToken
	err = s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		proj, err := loadProject(ctx, q, businessID, ref, false)
		if err != nil {
			return err
		}
		row, err := q.InsertWorkToken(ctx, dbgen.InsertWorkTokenParams{
			ID: uuid.New(), BusinessID: proj.BusinessID, TenantRootID: proj.TenantRootID, ProjectID: proj.ID,
			Name: name, TokenHash: auth.HashToken(plaintext), TokenPrefix: plaintext[:tokenPrefixLen],
			PrincipalID: principalID,
		})
		if err != nil {
			return err
		}
		out = CreatedToken{ID: row.ID, Name: row.Name, Prefix: row.TokenPrefix, CreatedAt: row.CreatedAt.UTC(), Token: plaintext}
		return writeAudit(ctx, tx, proj, principalID, "work.token.created", "work_token", row.ID,
			map[string]any{"name": row.Name, "prefix": row.TokenPrefix})
	})
	if err != nil {
		return CreatedToken{}, mapErr(err)
	}
	return out, nil
}

// RevokeToken revokes a project's token (idempotent). Unknown/foreign → ErrNotFound.
func (s *Service) RevokeToken(ctx context.Context, principalID, businessID uuid.UUID, ref string, tokenID uuid.UUID) error {
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		proj, err := loadProject(ctx, q, businessID, ref, false)
		if err != nil {
			return err
		}
		row, err := q.RevokeWorkToken(ctx, dbgen.RevokeWorkTokenParams{ID: tokenID, ProjectID: proj.ID, TenantRootID: proj.TenantRootID})
		if err != nil {
			return err
		}
		return writeAudit(ctx, tx, proj, principalID, "work.token.revoked", "work_token", row.ID,
			map[string]any{"name": row.Name, "prefix": row.TokenPrefix})
	})
	return mapErr(err)
}

// Summary tallies open work across every project in the business (the nav badge).
func (s *Service) Summary(ctx context.Context, principalID, businessID uuid.UUID) (Summary, error) {
	var out Summary
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		tenantRoot, err := resolveTenantRoot(ctx, q, businessID)
		if err != nil {
			return err
		}
		r, err := q.GetWorkSummary(ctx, dbgen.GetWorkSummaryParams{BusinessID: businessID, TenantRootID: tenantRoot})
		if err != nil {
			return err
		}
		out = Summary{OpenOperator: int(r.OpenOperator), OpenAgent: int(r.OpenAgent), InProgress: int(r.InProgress)}
		return nil
	})
	if err != nil {
		return Summary{}, mapErr(err)
	}
	return out, nil
}

// --- shared helpers ---

// resolveTenantRoot loads the tenant_root_id for the URL business via the RLS-bound GetBusiness
// query — a business the principal cannot see collapses to ErrNoRows ⇒ ErrNotFound (no oracle).
func resolveTenantRoot(ctx context.Context, q *dbgen.Queries, businessID uuid.UUID) (uuid.UUID, error) {
	b, err := q.GetBusiness(ctx, businessID)
	if err != nil {
		return uuid.Nil, err
	}
	return b.TenantRootID, nil
}

// loadProject resolves a {pid} reference — a UUID first, else a slug — within the URL
// business. lock takes the project row FOR UPDATE, which every task write must hold.
func loadProject(ctx context.Context, q *dbgen.Queries, businessID uuid.UUID, ref string, lock bool) (dbgen.WorkProject, error) {
	tenantRoot, err := resolveTenantRoot(ctx, q, businessID)
	if err != nil {
		return dbgen.WorkProject{}, err
	}
	id, perr := uuid.Parse(ref)
	if perr != nil {
		if !slugRe.MatchString(ref) {
			return dbgen.WorkProject{}, pgx.ErrNoRows
		}
		p, err := q.GetWorkProjectBySlug(ctx, dbgen.GetWorkProjectBySlugParams{
			Slug: ref, BusinessID: businessID, TenantRootID: tenantRoot,
		})
		if err != nil {
			return dbgen.WorkProject{}, err
		}
		if !lock {
			return p, nil
		}
		id = p.ID
	}
	if lock {
		return q.LockWorkProject(ctx, dbgen.LockWorkProjectParams{ID: id, BusinessID: businessID, TenantRootID: tenantRoot})
	}
	return q.GetWorkProject(ctx, dbgen.GetWorkProjectParams{ID: id, BusinessID: businessID, TenantRootID: tenantRoot})
}

// insertTask inserts one fully-defaulted task. The caller holds the project lock.
func insertTask(ctx context.Context, q *dbgen.Queries, proj dbgen.WorkProject, key string, v taskValues, creator uuid.UUID) (dbgen.WorkTask, error) {
	return q.InsertWorkTask(ctx, dbgen.InsertWorkTaskParams{
		ID: uuid.New(), BusinessID: proj.BusinessID, TenantRootID: proj.TenantRootID, ProjectID: proj.ID,
		Key: key, Title: v.title, Description: v.description, Lane: v.lane, Status: v.status,
		Priority: v.priority, Type: v.typ, Assignee: v.assignee, Labels: v.labels, BlockedBy: v.blockedBy,
		CreatedByPrincipal: db.PGUUID(creator),
		CreatedAt:          pgTime(v.createdAt),
		ClosedAt:           pgTime(v.closedAt),
	})
}

func pgTime(t *time.Time) pgtype.Timestamptz {
	if t == nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: *t, Valid: true}
}

// insertTaskSavepoint runs insertTask inside a savepoint, so a key collision rolls back only
// this statement and the caller can retry with a fresh key.
func insertTaskSavepoint(ctx context.Context, tx pgx.Tx, proj dbgen.WorkProject, key string, v taskValues, creator uuid.UUID) (dbgen.WorkTask, error) {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return dbgen.WorkTask{}, err
	}
	row, err := insertTask(ctx, dbgen.New(sp), proj, key, v, creator)
	if err != nil {
		_ = sp.Rollback(ctx)
		return dbgen.WorkTask{}, err
	}
	return row, sp.Commit(ctx)
}

// patchTask is the single patch path shared by the web PATCH and the sync update/note ops: it
// applies already-normalized fields and an optional note entry to one task. The caller holds
// the project lock. An unknown key is pgx.ErrNoRows.
func patchTask(ctx context.Context, q *dbgen.Queries, proj dbgen.WorkProject, key string, f TaskFields, noteEntry *string) (dbgen.WorkTask, error) {
	if f.empty() && noteEntry == nil {
		return dbgen.WorkTask{}, errValidation("no fields to update")
	}
	p := dbgen.UpdateWorkTaskParams{
		Title: f.Title, Description: f.Description, Lane: f.Lane, Status: f.Status, Type: f.Type,
		Assignee: f.Assignee, NoteEntry: noteEntry,
		ProjectID: proj.ID, Key: key, TenantRootID: proj.TenantRootID,
	}
	if f.Priority != nil {
		p.Priority = ptr(int16(*f.Priority))
	}
	if f.Labels != nil {
		p.Labels = nonNil(*f.Labels)
	}
	if f.BlockedBy != nil {
		p.BlockedBy = nonNil(*f.BlockedBy)
	}
	return q.UpdateWorkTask(ctx, p)
}

// operatorName is the note author for a web-UI note: the caller's display name, else email,
// else "operator".
func operatorName(ctx context.Context, q *dbgen.Queries, principalID uuid.UUID) (string, error) {
	acc, err := q.GetAccountByPrincipal(ctx, principalID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "operator", nil
	}
	if err != nil {
		return "", err
	}
	if n := strings.TrimSpace(acc.DisplayName); n != "" {
		return n, nil
	}
	if acc.Email != "" {
		return acc.Email, nil
	}
	return "operator", nil
}

// auditPatch summarizes a patch for the audit log (field values, but never note text).
func auditPatch(key string, f TaskFields, noted bool) map[string]any {
	m := map[string]any{"key": key}
	set := func(name string, v any, sent bool) {
		if sent {
			m[name] = v
		}
	}
	set("title", f.Title, f.Title != nil)
	set("lane", f.Lane, f.Lane != nil)
	set("status", f.Status, f.Status != nil)
	set("priority", f.Priority, f.Priority != nil)
	set("type", f.Type, f.Type != nil)
	set("assignee", f.Assignee, f.Assignee != nil)
	set("labels", f.Labels, f.Labels != nil)
	set("blocked_by", f.BlockedBy, f.BlockedBy != nil)
	if f.Description != nil {
		m["description_changed"] = true
	}
	if noted {
		m["note_appended"] = true
	}
	return m
}

func writeAudit(ctx context.Context, tx pgx.Tx, proj dbgen.WorkProject, actor uuid.UUID, action, targetType string, targetID uuid.UUID, newValue any) error {
	return audit.Write(ctx, tx, audit.Entry{
		BusinessID:       &proj.BusinessID,
		TenantRootID:     &proj.TenantRootID,
		ActorPrincipalID: &actor,
		Action:           action,
		TargetType:       &targetType,
		TargetID:         &targetID,
		NewValue:         newValue,
	})
}

func isUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == constraint
}

// newWorkToken mints a plaintext work token: "mfw_" + 256 random bits, base64url.
func newWorkToken() (string, error) {
	raw, err := auth.NewOpaqueToken()
	if err != nil {
		return "", fmt.Errorf("work: token entropy: %w", err)
	}
	return "mfw_" + raw, nil
}

// errNotFound is the uniform not-found the handlers emit for malformed path params.
var errNotFound = fmt.Errorf("work: not found: %w", errs.ErrNotFound)
