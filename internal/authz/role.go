package authz

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/audit"
	"github.com/manyforge/manyforge/internal/platform/db"
	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
	"github.com/manyforge/manyforge/internal/platform/errs"
)

// Role is the API-facing view of a role and its permission set.
type Role struct {
	ID          string   `json:"id"`
	Key         string   `json:"key"`
	Name        string   `json:"name"`
	Builtin     bool     `json:"builtin"`
	Locked      bool     `json:"locked"`
	Permissions []string `json:"permissions"`
}

// requirePerm returns ErrNotFound (no oracle) unless the caller holds perm at business.
func requirePerm(ctx context.Context, tx pgx.Tx, principalID, businessID uuid.UUID, perm string) error {
	perms, err := Resolve(ctx, tx, principalID, businessID)
	if err != nil {
		return err
	}
	if !perms.Has(perm) {
		return errs.ErrNotFound
	}
	return nil
}

// loadTenantRoot returns the tenant root of a caller-visible business, or
// ErrNotFound if the business is unknown/invisible (RLS-scoped, no oracle).
func loadTenantRoot(ctx context.Context, q *dbgen.Queries, businessID uuid.UUID) (uuid.UUID, error) {
	b, err := q.GetBusiness(ctx, businessID)
	if err != nil {
		return uuid.Nil, errs.ErrNotFound
	}
	return b.TenantRootID, nil
}

// ListRoles returns the presets plus the business's tenant-custom roles, each
// with its permission set. Requires roles.read.
func (s *Service) ListRoles(ctx context.Context, principalID, businessID uuid.UUID) ([]Role, error) {
	var out []Role
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		root, err := loadTenantRoot(ctx, q, businessID)
		if err != nil {
			return err
		}
		if err := requirePerm(ctx, tx, principalID, businessID, "roles.read"); err != nil {
			return err
		}
		rows, err := q.ListTenantRoles(ctx, db.PGUUID(root))
		if err != nil {
			return err
		}
		out = make([]Role, 0, len(rows))
		for _, r := range rows {
			perms, err := q.GetRolePermissions(ctx, r.ID)
			if err != nil {
				return err
			}
			out = append(out, Role{
				ID: r.ID.String(), Key: r.Key, Name: r.Name,
				Builtin: !r.TenantRootID.Valid, Locked: r.IsLocked, Permissions: nonNil(perms),
			})
		}
		return nil
	})
	return out, err
}

// CreateRole creates a custom role scoped to the business's tenant. Requires
// roles.manage and that every requested permission is one the creator itself
// holds at the business (no escalation, FR-023).
func (s *Service) CreateRole(ctx context.Context, principalID, businessID uuid.UUID, name string, perms []string) (Role, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Role{}, fmt.Errorf("name is required: %w", errs.ErrValidation)
	}
	roleID := uuid.New()
	key := slugify(name) + "-" + shortHex()
	var out Role
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		root, err := loadTenantRoot(ctx, q, businessID)
		if err != nil {
			return err
		}
		if err := requirePerm(ctx, tx, principalID, businessID, "roles.manage"); err != nil {
			return err
		}
		clean, err := validateGrantable(ctx, q, tx, principalID, businessID, perms)
		if err != nil {
			return err
		}
		if err := q.CreateRole(ctx, dbgen.CreateRoleParams{ID: roleID, TenantRootID: db.PGUUID(root), Key: key, Name: name}); err != nil {
			return err
		}
		for _, p := range clean {
			if err := q.AddRolePermission(ctx, dbgen.AddRolePermissionParams{RoleID: roleID, PermissionKey: p}); err != nil {
				return err
			}
		}
		out = Role{ID: roleID.String(), Key: key, Name: name, Permissions: clean}
		return auditRole(ctx, tx, principalID, businessID, root, roleID, "role.created", map[string]any{"name": name, "permissions": clean})
	})
	if err != nil {
		return Role{}, err
	}
	return out, nil
}

// UpdateRole edits a custom role's name and/or permission set (both optional).
// Presets are not editable (treated as not-found). Permission changes re-check
// escalation and apply transactionally, effective immediately (FR-025).
func (s *Service) UpdateRole(ctx context.Context, principalID, businessID, roleID uuid.UUID, name *string, perms *[]string) (Role, error) {
	var out Role
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		root, err := loadTenantRoot(ctx, q, businessID)
		if err != nil {
			return err
		}
		if err := requirePerm(ctx, tx, principalID, businessID, "roles.manage"); err != nil {
			return err
		}
		role, err := q.GetCustomRole(ctx, dbgen.GetCustomRoleParams{ID: roleID, TenantRootID: db.PGUUID(root)})
		if err != nil {
			return errs.ErrNotFound // preset, other tenant, or unknown
		}
		newName := role.Name
		if name != nil {
			n := strings.TrimSpace(*name)
			if n == "" {
				return fmt.Errorf("name is required: %w", errs.ErrValidation)
			}
			if err := q.UpdateRoleName(ctx, dbgen.UpdateRoleNameParams{ID: roleID, Name: n}); err != nil {
				return err
			}
			newName = n
		}
		var clean []string
		if perms != nil {
			if clean, err = validateGrantable(ctx, q, tx, principalID, businessID, *perms); err != nil {
				return err
			}
			if err := q.ClearRolePermissions(ctx, roleID); err != nil {
				return err
			}
			for _, p := range clean {
				if err := q.AddRolePermission(ctx, dbgen.AddRolePermissionParams{RoleID: roleID, PermissionKey: p}); err != nil {
					return err
				}
			}
		} else {
			existing, err := q.GetRolePermissions(ctx, roleID)
			if err != nil {
				return err
			}
			clean = nonNil(existing)
		}
		out = Role{ID: roleID.String(), Key: role.Key, Name: newName, Locked: role.IsLocked, Permissions: clean}
		return auditRole(ctx, tx, principalID, businessID, root, roleID, "role.updated", map[string]any{"name": newName, "permissions": clean})
	})
	if err != nil {
		return Role{}, err
	}
	return out, nil
}

// DeleteRole removes a custom role. Presets are not deletable (not-found), and a
// role still assigned to any member is refused (FR-025).
func (s *Service) DeleteRole(ctx context.Context, principalID, businessID, roleID uuid.UUID) error {
	return s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		root, err := loadTenantRoot(ctx, q, businessID)
		if err != nil {
			return err
		}
		if err := requirePerm(ctx, tx, principalID, businessID, "roles.manage"); err != nil {
			return err
		}
		if _, err := q.GetCustomRole(ctx, dbgen.GetCustomRoleParams{ID: roleID, TenantRootID: db.PGUUID(root)}); err != nil {
			return errs.ErrNotFound
		}
		n, err := q.CountRoleMemberships(ctx, roleID)
		if err != nil {
			return err
		}
		if n > 0 {
			return fmt.Errorf("role is still assigned to %d member(s): %w", n, errs.ErrConflict)
		}
		if err := q.DeleteRole(ctx, dbgen.DeleteRoleParams{ID: roleID, TenantRootID: db.PGUUID(root)}); err != nil {
			return err
		}
		return auditRole(ctx, tx, principalID, businessID, root, roleID, "role.deleted", nil)
	})
}

// validateGrantable dedupes the requested keys, rejects unknown catalog keys
// (ErrValidation), and refuses any the actor does not itself hold (ErrConflict,
// FR-023). Returns the sorted, de-duplicated set.
func validateGrantable(ctx context.Context, q *dbgen.Queries, tx pgx.Tx, principalID, businessID uuid.UUID, perms []string) ([]string, error) {
	seen := map[string]bool{}
	uniq := make([]string, 0, len(perms))
	for _, p := range perms {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		uniq = append(uniq, p)
	}
	catalog, err := q.AllPermissionKeys(ctx)
	if err != nil {
		return nil, err
	}
	known := map[string]bool{}
	for _, k := range catalog {
		known[k] = true
	}
	for _, p := range uniq {
		if !known[p] {
			return nil, fmt.Errorf("unknown permission %q: %w", p, errs.ErrValidation)
		}
	}
	actor, err := Resolve(ctx, tx, principalID, businessID)
	if err != nil {
		return nil, err
	}
	for _, p := range uniq {
		if !actor.Has(p) {
			return nil, fmt.Errorf("cannot grant a permission you do not hold (%s): %w", p, errs.ErrConflict)
		}
	}
	sort.Strings(uniq)
	return uniq, nil
}

func auditRole(ctx context.Context, tx pgx.Tx, actor, business, tenantRoot, roleID uuid.UUID, action string, value any) error {
	tt := "role"
	return audit.Write(ctx, tx, audit.Entry{
		BusinessID: &business, TenantRootID: &tenantRoot, ActorPrincipalID: &actor,
		Action: action, TargetType: &tt, TargetID: &roleID, NewValue: value,
	})
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

func slugify(s string) string {
	s = strings.Trim(slugRe.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if s == "" {
		return "role"
	}
	if len(s) > 32 {
		s = strings.Trim(s[:32], "-")
	}
	return s
}

func shortHex() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
