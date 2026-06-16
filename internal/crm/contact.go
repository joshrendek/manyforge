package crm

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/manyforge/manyforge/internal/platform/db"
	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
	"github.com/manyforge/manyforge/internal/platform/errs"
)

// ContactService is the authenticated CRUD surface over tenant-wide contacts. Every
// method takes the caller's principalID and the target businessID: the query runs
// inside db.WithPrincipal (RLS scopes rows to the caller's authorized tenants) AND
// pushes the ownership predicate (tenant_root_id = $) into SQL — dual enforcement.
// The tenant_root_id is resolved from businessID inside the same tx (the business in
// the URL is the tenant context; RLS already gates whether the principal can see it).
type ContactService struct {
	DB *db.DB
}

// Create inserts a new contact. PrimaryEmail is required (ErrValidation). A second
// contact with the same live (tenant_root_id, primary_email) violates the partial
// unique index → ErrConflict (no upsert here; that is InsertContactByEmail's job).
func (s *ContactService) Create(ctx context.Context, principalID, businessID uuid.UUID, in ContactInput) (Contact, error) {
	if in.PrimaryEmail == "" {
		return Contact{}, fmt.Errorf("crm: primary_email required: %w", errs.ErrValidation)
	}
	var out Contact
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		tenantRoot, terr := resolveTenantRoot(ctx, q, businessID)
		if terr != nil {
			return terr
		}
		row, ierr := q.InsertContact(ctx, dbgen.InsertContactParams{
			ID:           uuid.New(),
			TenantRootID: tenantRoot,
			PrimaryEmail: in.PrimaryEmail,
			DisplayName:  in.DisplayName,
			CompanyID:    db.PGUUIDPtr(in.CompanyID),
		})
		if ierr != nil {
			return ierr
		}
		out = toContact(row)
		return nil
	})
	if err != nil {
		return Contact{}, mapErr(err)
	}
	return out, nil
}

// Get loads a single live contact the caller can see, or ErrNotFound (no oracle).
func (s *ContactService) Get(ctx context.Context, principalID, businessID, id uuid.UUID) (Contact, error) {
	var out Contact
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		tenantRoot, terr := resolveTenantRoot(ctx, q, businessID)
		if terr != nil {
			return terr
		}
		row, gerr := q.GetContact(ctx, dbgen.GetContactParams{ID: id, TenantRootID: tenantRoot})
		if gerr != nil {
			return gerr
		}
		out = toContact(row)
		return nil
	})
	if err != nil {
		return Contact{}, mapErr(err)
	}
	return out, nil
}

// List returns a keyset page of the tenant's live contacts, ordered by primary_email.
// limit is clamped to [1,200] HERE (service boundary) so an absurd caller value never
// returns the whole table. NextCursor is minted from the last row's (primary_email, id)
// when a further page exists.
func (s *ContactService) List(ctx context.Context, principalID, businessID uuid.UUID, cursor string, limit int) (Page[Contact], error) {
	lim := clampLimit(limit)
	var out Page[Contact]
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		tenantRoot, terr := resolveTenantRoot(ctx, q, businessID)
		if terr != nil {
			return terr
		}

		var rows []dbgen.Contact
		var qerr error
		if cursor == "" {
			rows, qerr = q.ListContacts(ctx, dbgen.ListContactsParams{
				TenantRootID: tenantRoot, Limit: int32(lim + 1),
			})
		} else {
			k, perr := decodeContactCursor(cursor)
			if perr != nil {
				return perr
			}
			rows, qerr = q.ListContactsAfter(ctx, dbgen.ListContactsAfterParams{
				TenantRootID: tenantRoot, CurEmail: k.key, CurID: k.id, Lim: int32(lim + 1),
			})
		}
		if qerr != nil {
			return qerr
		}

		rows, next := trim(rows, lim)
		items := make([]Contact, 0, len(rows))
		for _, r := range rows {
			items = append(items, toContact(r))
		}
		out.Items = items
		if next {
			last := rows[len(rows)-1]
			out.NextCursor = ptr(encodeContactCursor(keyset{key: last.PrimaryEmail, id: last.ID}))
		}
		return nil
	})
	if err != nil {
		return Page[Contact]{}, mapErr(err)
	}
	return out, nil
}

// Update applies a partial update: a nil ContactInput field is preserved (COALESCE
// narg). PrimaryEmail is immutable here (UpdateContact does not touch it). A soft-deleted
// / foreign-tenant id matches zero rows ⇒ ErrNotFound (no oracle).
func (s *ContactService) Update(ctx context.Context, principalID, businessID, id uuid.UUID, in ContactInput) (Contact, error) {
	var out Contact
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		tenantRoot, terr := resolveTenantRoot(ctx, q, businessID)
		if terr != nil {
			return terr
		}
		row, uerr := q.UpdateContact(ctx, dbgen.UpdateContactParams{
			ID:           id,
			TenantRootID: tenantRoot,
			DisplayName:  in.DisplayName,
			CompanyID:    db.PGUUIDPtr(in.CompanyID),
		})
		if uerr != nil {
			return uerr
		}
		out = toContact(row)
		return nil
	})
	if err != nil {
		return Contact{}, mapErr(err)
	}
	return out, nil
}

// SoftDelete stamps deleted_at on a live contact. SoftDeleteContact is an :exec (no
// returned row), so to avoid silently succeeding on a missing / already-deleted /
// foreign-tenant id we GetContact first IN THE SAME TX: a vanished row yields
// pgx.ErrNoRows ⇒ ErrNotFound (no oracle); only an extant live row proceeds to the
// soft-delete. The Get + delete share one WithPrincipal tx so there is no TOCTOU window.
func (s *ContactService) SoftDelete(ctx context.Context, principalID, businessID, id uuid.UUID) error {
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		tenantRoot, terr := resolveTenantRoot(ctx, q, businessID)
		if terr != nil {
			return terr
		}
		if _, gerr := q.GetContact(ctx, dbgen.GetContactParams{ID: id, TenantRootID: tenantRoot}); gerr != nil {
			return gerr // ErrNoRows ⇒ ErrNotFound via mapErr
		}
		return q.SoftDeleteContact(ctx, dbgen.SoftDeleteContactParams{ID: id, TenantRootID: tenantRoot})
	})
	return mapErr(err)
}

// resolveTenantRoot loads the tenant_root_id for the business in the URL. It reuses the
// existing RLS-bound GetBusiness query, so a business the principal cannot see (foreign
// tenant / unknown / soft-deleted) yields pgx.ErrNoRows ⇒ ErrNotFound (no oracle) rather
// than leaking that the business exists. No new dbgen query is added.
func resolveTenantRoot(ctx context.Context, q *dbgen.Queries, businessID uuid.UUID) (uuid.UUID, error) {
	b, err := q.GetBusiness(ctx, businessID)
	if err != nil {
		return uuid.Nil, err
	}
	return b.TenantRootID, nil
}

// toContact maps a dbgen.Contact row onto the API view; deleted_at is dropped (reads
// already exclude soft-deleted rows).
func toContact(c dbgen.Contact) Contact {
	return Contact{
		ID:           c.ID,
		TenantRootID: c.TenantRootID,
		PrimaryEmail: c.PrimaryEmail,
		DisplayName:  c.DisplayName,
		CompanyID:    pgUUIDPtr(c.CompanyID),
		CreatedAt:    c.CreatedAt,
		UpdatedAt:    c.UpdatedAt,
	}
}

// mapErr converts a query/closure error into a stable service-layer sentinel.
// pgx.ErrNoRows → ErrNotFound (no oracle). SQLSTATE 23505 (unique violation, e.g. a
// duplicate live primary_email) → ErrConflict. ErrValidation (a malformed cursor or a
// missing email) is preserved. Everything else is wrapped so the HTTP layer logs it
// server-side and surfaces a generic 500.
func mapErr(err error) error {
	var pgErr *pgconn.PgError
	switch {
	case err == nil:
		return nil
	case errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("crm: not found: %w", errs.ErrNotFound)
	case errors.As(err, &pgErr) && pgErr.Code == "23505":
		return fmt.Errorf("crm: duplicate contact: %w", errs.ErrConflict)
	case errors.Is(err, errs.ErrValidation), errors.Is(err, errs.ErrNotFound),
		errors.Is(err, errs.ErrConflict):
		return err
	default:
		return fmt.Errorf("crm: query: %w", err)
	}
}

// pgUUIDPtr converts a nullable pgtype.UUID column into an optional uuid.UUID for the
// API view (NULL → nil). Mirrors ticketing.pgUUIDPtr.
func pgUUIDPtr(u pgtype.UUID) *uuid.UUID {
	if !u.Valid {
		return nil
	}
	v := uuid.UUID(u.Bytes)
	return &v
}

// clampLimit applies the service-boundary page cap: a non-positive request gets the
// default; an oversized request is silently capped (never the whole table).
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

// trim drops the sentinel (limit+1)th row used to detect a further page, returning the
// kept rows and whether a next page exists.
func trim[T any](rows []T, lim int) ([]T, bool) {
	if len(rows) > lim {
		return rows[:lim], true
	}
	return rows, false
}

func ptr[T any](v T) *T { return &v }
