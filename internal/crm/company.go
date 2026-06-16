package crm

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/db"
	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
	"github.com/manyforge/manyforge/internal/platform/errs"
)

// CompanyService is the authenticated CRUD surface over tenant-wide companies. It mirrors
// ContactService: every method takes the caller's principalID and the target businessID,
// runs inside db.WithPrincipal (RLS scopes rows to the caller's authorized tenants), AND
// pushes the ownership predicate (tenant_root_id = $) into SQL — dual enforcement. Unknown
// / foreign-tenant ids collapse to ErrNotFound (no existence oracle). Companies carry no
// PII / soft-delete column, so Delete is a hard delete.
type CompanyService struct {
	DB *db.DB
}

// Create inserts a new company. Name is required (ErrValidation); Domain is optional. A
// second company with the same live (tenant_root_id, domain) violates the partial unique
// index → ErrConflict (no upsert here; that is ResolveOrCreateByDomain's job).
func (s *CompanyService) Create(ctx context.Context, principalID, businessID uuid.UUID, in CompanyInput) (Company, error) {
	if strings.TrimSpace(in.Name) == "" {
		return Company{}, fmt.Errorf("crm: name required: %w", errs.ErrValidation)
	}
	var out Company
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		tenantRoot, terr := resolveTenantRoot(ctx, q, businessID)
		if terr != nil {
			return terr
		}
		row, ierr := q.InsertCompany(ctx, dbgen.InsertCompanyParams{
			ID:           uuid.New(),
			TenantRootID: tenantRoot,
			Name:         in.Name,
			Domain:       in.Domain,
		})
		if ierr != nil {
			return ierr
		}
		out = toCompany(row)
		return nil
	})
	if err != nil {
		return Company{}, mapErr(err)
	}
	return out, nil
}

// Get loads a single company the caller can see, or ErrNotFound (no oracle).
func (s *CompanyService) Get(ctx context.Context, principalID, businessID, id uuid.UUID) (Company, error) {
	var out Company
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		tenantRoot, terr := resolveTenantRoot(ctx, q, businessID)
		if terr != nil {
			return terr
		}
		row, gerr := q.GetCompany(ctx, dbgen.GetCompanyParams{ID: id, TenantRootID: tenantRoot})
		if gerr != nil {
			return gerr
		}
		out = toCompany(row)
		return nil
	})
	if err != nil {
		return Company{}, mapErr(err)
	}
	return out, nil
}

// List returns a keyset page of the tenant's companies, ordered by name (then id). limit
// is clamped to [1,200] HERE (service boundary). NextCursor is minted from the last row's
// (name, id) when a further page exists.
func (s *CompanyService) List(ctx context.Context, principalID, businessID uuid.UUID, cursor string, limit int) (Page[Company], error) {
	lim := clampLimit(limit)
	var out Page[Company]
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		tenantRoot, terr := resolveTenantRoot(ctx, q, businessID)
		if terr != nil {
			return terr
		}

		var rows []dbgen.Company
		var qerr error
		if cursor == "" {
			rows, qerr = q.ListCompanies(ctx, dbgen.ListCompaniesParams{
				TenantRootID: tenantRoot, Limit: int32(lim + 1),
			})
		} else {
			k, perr := decodeCompanyCursor(cursor)
			if perr != nil {
				return perr
			}
			rows, qerr = q.ListCompaniesAfter(ctx, dbgen.ListCompaniesAfterParams{
				TenantRootID: tenantRoot, CurName: k.key, CurID: k.id, Lim: int32(lim + 1),
			})
		}
		if qerr != nil {
			return qerr
		}

		rows, next := trim(rows, lim)
		items := make([]Company, 0, len(rows))
		for _, r := range rows {
			items = append(items, toCompany(r))
		}
		out.Items = items
		if next {
			last := rows[len(rows)-1]
			out.NextCursor = ptr(encodeCompanyCursor(keyset{key: last.Name, id: last.ID}))
		}
		return nil
	})
	if err != nil {
		return Page[Company]{}, mapErr(err)
	}
	return out, nil
}

// Update applies a partial update via COALESCE nargs: an empty Name preserves the current
// value, a nil Domain preserves the current value. A foreign-tenant / unknown id matches
// zero rows ⇒ ErrNotFound (no oracle).
func (s *CompanyService) Update(ctx context.Context, principalID, businessID, id uuid.UUID, in CompanyInput) (Company, error) {
	var out Company
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		tenantRoot, terr := resolveTenantRoot(ctx, q, businessID)
		if terr != nil {
			return terr
		}
		var name *string // nil = COALESCE preserves current value
		if n := strings.TrimSpace(in.Name); n != "" {
			name = &n
		}
		row, uerr := q.UpdateCompany(ctx, dbgen.UpdateCompanyParams{
			Name:         name,
			Domain:       in.Domain,
			ID:           id,
			TenantRootID: tenantRoot,
		})
		if uerr != nil {
			return uerr
		}
		out = toCompany(row)
		return nil
	})
	if err != nil {
		return Company{}, mapErr(err)
	}
	return out, nil
}

// Delete hard-deletes a company (companies carry no soft-delete column). The
// contact.company_id → company FK is NO ACTION (restrict), so a company still referenced by
// contacts cannot be dropped (SQLSTATE 23503) until those contacts are detached. Delete
// therefore runs three steps in ONE WithPrincipal tx, atomically: (1) GetCompany — a
// vanished / foreign-tenant row yields pgx.ErrNoRows ⇒ ErrNotFound (no oracle, no silent
// success), mirroring ContactService.SoftDelete; (2) DetachContactsFromCompany — nulls out
// company_id on every contact pointing here so the FK no longer blocks the delete; (3)
// DeleteCompany. Sharing one tx means no TOCTOU window and no half-detached state on error.
func (s *CompanyService) Delete(ctx context.Context, principalID, businessID, id uuid.UUID) error {
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		tenantRoot, terr := resolveTenantRoot(ctx, q, businessID)
		if terr != nil {
			return terr
		}
		if _, gerr := q.GetCompany(ctx, dbgen.GetCompanyParams{ID: id, TenantRootID: tenantRoot}); gerr != nil {
			return gerr // ErrNoRows ⇒ ErrNotFound via mapErr
		}
		if derr := q.DetachContactsFromCompany(ctx, dbgen.DetachContactsFromCompanyParams{
			CompanyID: db.PGUUID(id), TenantRootID: tenantRoot,
		}); derr != nil {
			return derr
		}
		return q.DeleteCompany(ctx, dbgen.DeleteCompanyParams{ID: id, TenantRootID: tenantRoot})
	})
	return mapErr(err)
}

// ResolveOrCreateByDomain is the idempotent get-or-create by domain used by the
// principal-less inbound-email path (Task 9): it runs in the CALLER's tx (no WithPrincipal —
// the inbox seam already holds a tx and carries no principal) and takes tenantRootID
// explicitly rather than resolving it from a business. A new (tenant_root_id, domain)
// inserts with name defaulted to the domain (the user can rename later); a live duplicate
// returns the existing row (ResolveCompanyByDomain's ON CONFLICT against
// company_tenant_domain_uq), so the seam never creates a duplicate company for a recurring
// sender domain.
//
// Caller MUST have already excluded free-email domains via IsFreeEmailDomain (otherwise
// every gmail.com sender collapses into one bogus company), MUST pass a trusted
// tenantRootID (never derived from untrusted inbound headers), and MUST run on a tx whose
// RLS principal is already set or an RLS-exempt path; this method performs no tenant
// resolution or principal binding of its own.
func (s *CompanyService) ResolveOrCreateByDomain(ctx context.Context, tx pgx.Tx, tenantRootID uuid.UUID, domain string) (Company, error) {
	if domain == "" {
		return Company{}, fmt.Errorf("crm: domain required: %w", errs.ErrValidation)
	}
	q := dbgen.New(tx)
	row, err := q.ResolveCompanyByDomain(ctx, dbgen.ResolveCompanyByDomainParams{
		ID:           uuid.New(),
		TenantRootID: tenantRootID,
		Name:         domain, // default name = domain; user can rename later
		Domain:       &domain,
	})
	if err != nil {
		return Company{}, mapErr(err)
	}
	return toCompany(row), nil
}

// toCompany maps a dbgen.Company row onto the API view.
func toCompany(c dbgen.Company) Company {
	return Company{
		ID:           c.ID,
		TenantRootID: c.TenantRootID,
		Name:         c.Name,
		Domain:       c.Domain,
		CreatedAt:    c.CreatedAt,
		UpdatedAt:    c.UpdatedAt,
	}
}
