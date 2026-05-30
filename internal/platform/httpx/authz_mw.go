package httpx

import (
	"context"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/db"
	"github.com/manyforge/manyforge/internal/platform/errs"
)

// BusinessIDFunc extracts the target business id from a request (e.g. a path param).
type BusinessIDFunc func(*http.Request) (uuid.UUID, error)

// Permissions is the minimal view of a resolved permission set the middleware
// needs. The authz package's PermissionSet satisfies it.
type Permissions interface{ Has(key string) bool }

// PermissionResolver resolves a principal's effective permissions at a business
// within an open transaction. Injected (rather than importing authz directly) so
// this transport package stays free of domain dependencies.
type PermissionResolver func(ctx context.Context, tx pgx.Tx, principalID, businessID uuid.UUID) (Permissions, error)

// RequirePermission gates a handler on the caller holding perm at the target
// business. Lacking the permission (or the business being invisible) returns
// 404 — never 403 — so authorization and existence are indistinguishable
// (FR-011/FR-026). Resolution runs inside the caller's RLS principal context.
func RequirePermission(database *db.DB, resolve PermissionResolver, perm string, businessID BusinessIDFunc) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			pid, ok := PrincipalFromContext(r.Context())
			if !ok {
				WriteError(w, r, errs.ErrNotFound)
				return
			}
			bid, err := businessID(r)
			if err != nil {
				WriteError(w, r, errs.ErrNotFound)
				return
			}
			var allowed bool
			err = database.WithPrincipal(r.Context(), pid, func(tx pgx.Tx) error {
				perms, rerr := resolve(r.Context(), tx, pid, bid)
				if rerr != nil {
					return rerr
				}
				allowed = perms.Has(perm)
				return nil
			})
			if err != nil {
				WriteError(w, r, err)
				return
			}
			if !allowed {
				WriteError(w, r, errs.ErrNotFound)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
