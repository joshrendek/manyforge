package githubapp

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/db"
	"github.com/manyforge/manyforge/internal/platform/httpx"
)

// DBPermChecker adapts the platform RLS permission resolver to the Handler's
// permChecker seam. It is the M-1 authorization gate for linkInstallation:
// resolving the caller's effective permissions at the state's business INSIDE
// the caller's RLS principal context, exactly as httpx.RequirePermission does,
// so a non-member of the business can never satisfy Has and hijack-link an
// installation.
//
// Exported (rather than an unexported adapter in cmd/manyforge) so the security
// gate is covered by a real-DB integration test (perms_integration_test.go)
// alongside the rest of the githubapp suite.
type DBPermChecker struct {
	DB      *db.DB
	Resolve httpx.PermissionResolver
}

// Has reports whether principalID holds perm at businessID. It mirrors
// httpx.RequirePermission's resolve+check: run under the caller's RLS principal
// via WithPrincipal, resolve the effective permission set at the target
// business, and test perm. Any resolution error propagates to the caller (which
// maps it to a generic 500 — never a false allow).
func (g DBPermChecker) Has(ctx context.Context, principalID, businessID uuid.UUID, perm string) (bool, error) {
	var has bool
	err := g.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		perms, rerr := g.Resolve(ctx, tx, principalID, businessID)
		if rerr != nil {
			return rerr
		}
		has = perms.Has(perm)
		return nil
	})
	return has, err
}
