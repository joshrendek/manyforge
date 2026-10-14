package work

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/authz"
	"github.com/manyforge/manyforge/internal/platform/audit"
	"github.com/manyforge/manyforge/internal/platform/auth"
	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
	"github.com/manyforge/manyforge/internal/platform/errs"
)

const (
	// MaxSyncOps caps the ops in one sync request.
	MaxSyncOps = 1000
	// maxSyncTasks caps the tasks pulled per sync; the client re-syncs from rev while more.
	maxSyncTasks = 5000
)

// errTokenUnauthorized is every token failure — unknown, revoked, project gone, issuer lost
// access. The handler renders all of them as the same 401 (no oracle).
var errTokenUnauthorized = errors.New("work: token unauthorized")

// TokenAuth is what a work token resolves to.
type TokenAuth struct {
	TokenID      uuid.UUID
	ProjectID    uuid.UUID
	BusinessID   uuid.UUID
	TenantRootID uuid.UUID
	PrincipalID  uuid.UUID // the issuer; the sync runs as this principal
}

// SyncOp is one queued client mutation. Fields is decoded strictly per op (unknown field →
// op error) so a typo cannot be silently dropped.
type SyncOp struct {
	Op     string          `json:"op"`
	Key    string          `json:"key"`
	Fields json.RawMessage `json:"fields,omitempty"`
	Text   string          `json:"text,omitempty"`
	Author string          `json:"author,omitempty"`
}

// SyncRequest pushes ops and pulls every task with rev > Since.
type SyncRequest struct {
	Since int64    `json:"since"`
	Ops   []SyncOp `json:"ops"`
}

// OpResult reports one op's outcome; a failed op was skipped, the rest still applied.
type OpResult struct {
	Key   string `json:"key"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// SyncResponse returns the new cursor, the project, full snapshots of every changed task, and
// per-op results. More is true when the task page cap was hit (sync again from Rev).
type SyncResponse struct {
	Rev     int64       `json:"rev"`
	Project SyncProject `json:"project"`
	Tasks   []Task      `json:"tasks"`
	Results []OpResult  `json:"results"`
	More    bool        `json:"more"`
}

// ResolveToken authenticates an mfw_ plaintext through the SECURITY DEFINER
// work_resolve_token(): only the SHA-256 hash is sent to the database. Any failure is
// errTokenUnauthorized.
func (s *Service) ResolveToken(ctx context.Context, plaintext string) (TokenAuth, error) {
	if !tokenRe.MatchString(plaintext) {
		return TokenAuth{}, errTokenUnauthorized
	}
	hash := auth.HashToken(plaintext)
	var a TokenAuth
	err := s.DB.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT token_id, project_id, business_id, tenant_root_id, principal_id FROM work_resolve_token($1)`, hash,
		).Scan(&a.TokenID, &a.ProjectID, &a.BusinessID, &a.TenantRootID, &a.PrincipalID)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return TokenAuth{}, errTokenUnauthorized
	}
	if err != nil {
		return TokenAuth{}, mapErr(err)
	}
	return a, nil
}

// Sync applies the ops in order — each in its own savepoint, so a bad op is reported and
// skipped — then returns every task with rev > since. It runs as the token's issuer: RLS
// scopes it, and the issuer must still be an active account holding work.write on the
// business, otherwise errTokenUnauthorized. The project row lock serializes it with every
// other writer, so the returned rev is a safe cursor.
func (s *Service) Sync(ctx context.Context, a TokenAuth, req SyncRequest) (SyncResponse, error) {
	if req.Since < 0 {
		return SyncResponse{}, errValidation("since must be >= 0")
	}
	if len(req.Ops) > MaxSyncOps {
		return SyncResponse{}, errValidation("too many ops")
	}
	var out SyncResponse
	err := s.DB.WithPrincipal(ctx, a.PrincipalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		if err := requireIssuer(ctx, tx, q, a); err != nil {
			return err
		}
		proj, err := q.LockWorkProject(ctx, dbgen.LockWorkProjectParams{
			ID: a.ProjectID, BusinessID: a.BusinessID, TenantRootID: a.TenantRootID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return errTokenUnauthorized
		}
		if err != nil {
			return err
		}

		now := time.Now()
		out.Results = make([]OpResult, 0, len(req.Ops))
		applied := 0
		for _, op := range req.Ops {
			res, err := applyOp(ctx, tx, proj, a.PrincipalID, op, now)
			if err != nil {
				return err
			}
			if res.OK {
				applied++
			}
			out.Results = append(out.Results, res)
		}
		if len(req.Ops) > 0 {
			target := "work_token"
			if err := audit.Write(ctx, tx, audit.Entry{
				BusinessID: &proj.BusinessID, TenantRootID: &proj.TenantRootID, ActorPrincipalID: &a.PrincipalID,
				Action: "work.sync", TargetType: &target, TargetID: &a.TokenID,
				NewValue: map[string]any{
					"project_id": proj.ID, "ops": len(req.Ops), "applied": applied, "failed": len(req.Ops) - applied,
				},
			}); err != nil {
				return err
			}
		}

		rows, err := q.ListWorkTasksSince(ctx, dbgen.ListWorkTasksSinceParams{
			ProjectID: proj.ID, TenantRootID: proj.TenantRootID, Since: req.Since, Lim: maxSyncTasks + 1,
		})
		if err != nil {
			return err
		}
		if len(rows) > maxSyncTasks {
			rows, out.More = rows[:maxSyncTasks], true
		}
		out.Rev = req.Since
		out.Tasks = make([]Task, 0, len(rows))
		for _, r := range rows {
			out.Tasks = append(out.Tasks, toTask(r))
			out.Rev = r.Rev
		}
		out.Project = SyncProject{Slug: proj.Slug, Name: proj.Name, KeyPrefix: proj.KeyPrefix}
		return nil
	})
	if err != nil {
		return SyncResponse{}, mapErr(err)
	}
	return out, nil
}

// requireIssuer re-checks, on every sync, that the token's issuer is still an active account
// holding work.write on the token's business. Any miss is errTokenUnauthorized.
func requireIssuer(ctx context.Context, tx pgx.Tx, q *dbgen.Queries, a TokenAuth) error {
	acc, err := q.GetAccountByPrincipal(ctx, a.PrincipalID)
	if errors.Is(err, pgx.ErrNoRows) {
		return errTokenUnauthorized
	}
	if err != nil {
		return err
	}
	if acc.Status != "active" {
		return errTokenUnauthorized
	}
	perms, err := authz.Resolve(ctx, tx, a.PrincipalID, a.BusinessID)
	if err != nil {
		return err
	}
	if !perms.Has(authz.PermWorkWrite) {
		return errTokenUnauthorized
	}
	return nil
}

// applyOp runs one op in a savepoint. A client-attributable failure (validation, duplicate
// key, unknown key) rolls back just that op and is reported in the result; anything else
// (connection loss, tenant maintenance) is returned and aborts the whole sync.
func applyOp(ctx context.Context, tx pgx.Tx, proj dbgen.WorkProject, issuer uuid.UUID, op SyncOp, now time.Time) (OpResult, error) {
	res := OpResult{Key: op.Key}
	sp, err := tx.Begin(ctx)
	if err != nil {
		return res, err
	}
	opErr := runOp(ctx, dbgen.New(sp), proj, issuer, op, now)
	if opErr == nil {
		if err := sp.Commit(ctx); err != nil {
			return res, err
		}
		res.OK = true
		return res, nil
	}
	if err := sp.Rollback(ctx); err != nil {
		return res, err
	}
	msg, ok := opErrorMessage(opErr)
	if !ok {
		return res, opErr
	}
	res.Error = msg
	return res, nil
}

func runOp(ctx context.Context, q *dbgen.Queries, proj dbgen.WorkProject, issuer uuid.UUID, op SyncOp, now time.Time) error {
	switch op.Op {
	case "create":
		if !validKey(op.Key, proj.KeyPrefix) {
			return errValidation("key must be " + proj.KeyPrefix + "-<id>, e.g. " + proj.KeyPrefix + "-k3x9q2 or " + proj.KeyPrefix + "-k3x9.1")
		}
		var cf createFields
		if err := decodeStrict(op.Fields, &cf); err != nil {
			return err
		}
		f, err := normalizeFields(cf.TaskFields)
		if err != nil {
			return err
		}
		v, err := createValues(f)
		if err != nil {
			return err
		}
		if v.createdAt, v.closedAt, err = importTimes(cf, now); err != nil {
			return err
		}
		_, err = insertTask(ctx, q, proj, op.Key, v, issuer)
		return err
	case "update":
		if !keyRe.MatchString(op.Key) {
			return errValidation("invalid key")
		}
		f, err := decodeFields(op.Fields)
		if err != nil {
			return err
		}
		if f, err = normalizeFields(f); err != nil {
			return err
		}
		_, err = patchTask(ctx, q, proj, op.Key, f, nil)
		return err
	case "note":
		if !keyRe.MatchString(op.Key) {
			return errValidation("invalid key")
		}
		entry, err := formatNote(now, op.Author, "agent", "mf", op.Text)
		if err != nil {
			return err
		}
		_, err = patchTask(ctx, q, proj, op.Key, TaskFields{}, &entry)
		return err
	default:
		return errValidation("op must be create, update or note")
	}
}

// decodeFields strictly decodes an update op's fields object.
func decodeFields(raw json.RawMessage) (TaskFields, error) {
	var f TaskFields
	if err := decodeStrict(raw, &f); err != nil {
		return TaskFields{}, err
	}
	return f, nil
}

// decodeStrict decodes an op's fields object into v, rejecting unknown fields. An unknown field
// is an op error naming the field (client-supplied, safe to echo); any other decode failure is
// a generic message. An absent or null object leaves v zero.
func decodeStrict(raw json.RawMessage, v any) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if rest, ok := strings.CutPrefix(err.Error(), "json: unknown field "); ok {
			return errValidation("unknown field " + rest)
		}
		return errValidation("invalid fields")
	}
	return nil
}

// importTimes validates the timestamps an `mf` import may carry on create: neither may be in
// the future, closed_at may not precede created_at, and closed_at only applies to a task
// created done/canceled (it is otherwise ignored, matching the closed_at invariant).
func importTimes(cf createFields, now time.Time) (createdAt, closedAt *time.Time, err error) {
	createdAt = cf.CreatedAt
	if cf.Status != nil && (*cf.Status == "done" || *cf.Status == "canceled") {
		closedAt = cf.ClosedAt
	}
	limit := now.Add(time.Minute)
	if createdAt != nil && createdAt.After(limit) {
		return nil, nil, errValidation("created_at must not be in the future")
	}
	if closedAt != nil && closedAt.After(limit) {
		return nil, nil, errValidation("closed_at must not be in the future")
	}
	if createdAt != nil && closedAt != nil && closedAt.Before(*createdAt) {
		return nil, nil, errValidation("closed_at must not precede created_at")
	}
	return createdAt, closedAt, nil
}

// opErrorMessage maps a client-attributable op failure to its safe result message.
func opErrorMessage(err error) (string, bool) {
	mapped := mapErr(err)
	var ve *validationError
	switch {
	case errors.As(mapped, &ve):
		return ve.msg, true
	case errors.Is(mapped, errs.ErrConflict):
		return "key already exists", true
	case errors.Is(mapped, errs.ErrNotFound):
		return "not found", true
	}
	return "", false
}
