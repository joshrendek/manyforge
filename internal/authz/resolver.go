// Package authz computes effective permissions and gates actions. It is the
// single authorization vocabulary for both human members and AI-agent principals
// (Constitution Principle IV).
package authz

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
)

// PermissionSet is the set of permission keys a principal effectively holds at a business.
type PermissionSet map[string]struct{}

// Has reports whether the set contains key.
func (p PermissionSet) Has(key string) bool { _, ok := p[key]; return ok }

// Resolve computes the effective permission set for principalID at businessID
// using queries bound to tx (RLS-scoped). The union is taken over every grant
// the principal holds on the business or a non-archived ancestor; the locked
// Owner role resolves to the entire catalog so new permissions are covered
// automatically (research R3).
func Resolve(ctx context.Context, tx pgx.Tx, principalID, businessID uuid.UUID) (PermissionSet, error) {
	q := dbgen.New(tx)

	owner, err := q.HasOwnerRole(ctx, dbgen.HasOwnerRoleParams{
		PrincipalID:  principalID,
		DescendantID: businessID,
	})
	if err != nil {
		return nil, err
	}

	var keys []string
	if owner {
		keys, err = q.AllPermissionKeys(ctx)
	} else {
		keys, err = q.EffectivePermissions(ctx, dbgen.EffectivePermissionsParams{
			PrincipalID:  principalID,
			DescendantID: businessID,
		})
	}
	if err != nil {
		return nil, err
	}

	set := make(PermissionSet, len(keys))
	for _, k := range keys {
		set[k] = struct{}{}
	}
	return set, nil
}
