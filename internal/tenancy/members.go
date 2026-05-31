package tenancy

import (
	"context"
	"fmt"
	"net/http"
	"sort"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/authz"
	"github.com/manyforge/manyforge/internal/platform/audit"
	"github.com/manyforge/manyforge/internal/platform/db"
	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/httpx"
)

// ChangeMemberRole reassigns targetPrincipal's role at businessID. The caller
// must hold members.manage there (FR-013). It enforces no-escalation (FR-023):
// the locked Owner role may be assigned only by an Owner, and any other role only
// if the actor holds every permission it grants. The last direct Owner of a
// tenant is protected from demotion (FR-014/FR-024). The change is effective
// immediately and audited in the same transaction (FR-015). An unknown business,
// a non-member target, or an unauthorized actor are reported uniformly as
// not-found (FR-026, no existence oracle); an unknown or unassignable role is a
// conflict, as is a refused escalation or last-Owner demotion.
func (s *Service) ChangeMemberRole(ctx context.Context, actorID, businessID, targetPrincipal, newRoleID uuid.UUID) error {
	return s.DB.WithPrincipal(ctx, actorID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)

		biz, err := loadVisible(ctx, q, businessID)
		if err != nil {
			return err
		}
		// Serialize structural mutations within the tenant so the last-Owner
		// check and the deferred tenant_owner_guard cannot race (research R5).
		if err := q.AcquireTenantLock(ctx, biz.TenantRootID.String()); err != nil {
			return err
		}
		if err := requirePerm(ctx, tx, actorID, businessID, "members.manage"); err != nil {
			return err
		}

		newRole, err := q.GetRoleInTenant(ctx, dbgen.GetRoleInTenantParams{ID: newRoleID, TenantRootID: db.PGUUID(biz.TenantRootID)})
		if err != nil {
			return fmt.Errorf("unknown or unassignable role: %w", errs.ErrConflict)
		}
		current, err := q.GetMembershipAt(ctx, dbgen.GetMembershipAtParams{PrincipalID: targetPrincipal, BusinessID: businessID})
		if err != nil {
			return errs.ErrNotFound // not a member here (or invisible to the caller)
		}
		if current.RoleID == newRoleID {
			return nil // no-op: nothing to change, nothing to audit
		}

		if err := guardEscalation(ctx, tx, q, actorID, businessID, newRoleID, newRole.IsLocked); err != nil {
			return err
		}
		if err := guardLastOwner(ctx, q, biz, businessID, current.RoleID, newRole.IsLocked); err != nil {
			return err
		}

		if err := q.UpdateMembershipRole(ctx, dbgen.UpdateMembershipRoleParams{
			PrincipalID: targetPrincipal, BusinessID: businessID,
			RoleID: newRoleID, GrantedBy: db.PGUUID(actorID),
		}); err != nil {
			return err
		}
		tt := "membership"
		return audit.Write(ctx, tx, audit.Entry{
			BusinessID: &businessID, TenantRootID: &biz.TenantRootID, ActorPrincipalID: &actorID,
			Action: "membership.role_changed", TargetType: &tt, TargetID: &targetPrincipal,
			NewValue: map[string]any{"from_role": current.RoleID.String(), "to_role": newRoleID.String()},
		})
	})
}

// guardEscalation enforces FR-023. Assigning the locked Owner role is reserved to
// Owners; any other role requires the actor to already hold every permission it
// grants. (The locked role is special-cased rather than compared permission-wise
// so the rule holds even if a future catalog permission is not mirrored onto the
// Owner role's grant rows — the resolver treats Owner as the whole catalog.)
func guardEscalation(ctx context.Context, tx pgx.Tx, q *dbgen.Queries, actorID, businessID, newRoleID uuid.UUID, newRoleLocked bool) error {
	if newRoleLocked {
		isOwner, err := q.HasOwnerRole(ctx, dbgen.HasOwnerRoleParams{PrincipalID: actorID, DescendantID: businessID})
		if err != nil {
			return err
		}
		if !isOwner {
			return fmt.Errorf("assigning the Owner role is reserved to Owners: %w", errs.ErrConflict)
		}
		return nil
	}
	actorPerms, err := authz.Resolve(ctx, tx, actorID, businessID)
	if err != nil {
		return err
	}
	rolePerms, err := q.GetRolePermissions(ctx, newRoleID)
	if err != nil {
		return err
	}
	for _, p := range rolePerms {
		if !actorPerms.Has(p) {
			return fmt.Errorf("cannot assign a role granting %q, a permission you do not hold: %w", p, errs.ErrConflict)
		}
	}
	return nil
}

// guardLastOwner enforces FR-014/FR-024: a tenant root must always retain at
// least one direct Owner, so demoting the sole direct Owner is refused until
// ownership is transferred. Demotions at sub-businesses (optional delegated Owner
// grants) and promotions are unaffected. The deferred tenant_owner_guard trigger
// is the atomic backstop; this returns a clean error instead of a commit-time raise.
func guardLastOwner(ctx context.Context, q *dbgen.Queries, biz dbgen.Business, businessID, currentRoleID uuid.UUID, newRoleLocked bool) error {
	if newRoleLocked || businessID != biz.TenantRootID {
		return nil
	}
	ownerRoleID, err := q.OwnerRoleID(ctx)
	if err != nil {
		return err
	}
	if currentRoleID != ownerRoleID {
		return nil // target is not a direct Owner; nothing to protect
	}
	owners, err := q.CountDirectOwners(ctx, businessID)
	if err != nil {
		return err
	}
	if owners <= 1 {
		return fmt.Errorf("the tenant must retain at least one Owner; transfer ownership first: %w", errs.ErrConflict)
	}
	return nil
}

// RevokeMember removes targetPrincipal's DIRECT membership at businessID. Requires
// members.manage (FR-013); inherited access from ancestors is untouched. The last
// direct Owner of the tenant root cannot be revoked (FR-014/FR-024). Effective
// immediately, audited in-tx (FR-015). Unknown business/member or an unauthorized
// caller -> not-found (FR-026); a last-Owner removal -> conflict.
func (s *Service) RevokeMember(ctx context.Context, actorID, businessID, targetPrincipal uuid.UUID) error {
	return s.DB.WithPrincipal(ctx, actorID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		biz, err := loadVisible(ctx, q, businessID)
		if err != nil {
			return err
		}
		if err := q.AcquireTenantLock(ctx, biz.TenantRootID.String()); err != nil {
			return err
		}
		if err := requirePerm(ctx, tx, actorID, businessID, "members.manage"); err != nil {
			return err
		}
		current, err := q.GetMembershipAt(ctx, dbgen.GetMembershipAtParams{PrincipalID: targetPrincipal, BusinessID: businessID})
		if err != nil {
			return errs.ErrNotFound // no direct membership at this business
		}
		if err := guardLastOwner(ctx, q, biz, businessID, current.RoleID, false); err != nil {
			return err
		}
		if err := q.DeleteMembershipAt(ctx, dbgen.DeleteMembershipAtParams{PrincipalID: targetPrincipal, BusinessID: businessID}); err != nil {
			return err
		}
		return auditMembership(ctx, tx, actorID, businessID, biz.TenantRootID, targetPrincipal, "membership.revoked", current.RoleID)
	})
}

// LeaveBusiness lets the caller voluntarily give up their OWN direct membership at
// businessID (FR-018) — no management permission required. The tenant's last direct
// Owner cannot leave until ownership is transferred (FR-014/FR-024). A caller with
// only inherited access (no direct membership here) gets not-found.
func (s *Service) LeaveBusiness(ctx context.Context, callerID, businessID uuid.UUID) error {
	return s.DB.WithPrincipal(ctx, callerID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		biz, err := loadVisible(ctx, q, businessID)
		if err != nil {
			return err
		}
		if err := q.AcquireTenantLock(ctx, biz.TenantRootID.String()); err != nil {
			return err
		}
		current, err := q.GetMembershipAt(ctx, dbgen.GetMembershipAtParams{PrincipalID: callerID, BusinessID: businessID})
		if err != nil {
			return errs.ErrNotFound // nothing to leave (no direct membership)
		}
		if err := guardLastOwner(ctx, q, biz, businessID, current.RoleID, false); err != nil {
			return err
		}
		if err := q.DeleteMembershipAt(ctx, dbgen.DeleteMembershipAtParams{PrincipalID: callerID, BusinessID: businessID}); err != nil {
			return err
		}
		return auditMembership(ctx, tx, callerID, businessID, biz.TenantRootID, callerID, "membership.left", current.RoleID)
	})
}

// auditMembership records a membership mutation that removes a grant (revoke/leave),
// capturing the role that was held, in the same transaction as the change (FR-015).
func auditMembership(ctx context.Context, tx pgx.Tx, actor, business, tenantRoot, target uuid.UUID, action string, roleID uuid.UUID) error {
	tt := "membership"
	return audit.Write(ctx, tx, audit.Entry{
		BusinessID: &business, TenantRootID: &tenantRoot, ActorPrincipalID: &actor,
		Action: action, TargetType: &tt, TargetID: &target,
		NewValue: map[string]any{"role": roleID.String()},
	})
}

// changeMemberRole handles PATCH /businesses/{id}/members/{principalId}.
func (h *Handler) changeMemberRole(w http.ResponseWriter, r *http.Request) {
	pid, ok := h.principal(w, r)
	if !ok {
		return
	}
	id, err := pathID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	target, err := uuid.Parse(chi.URLParam(r, "principalId"))
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	var in struct {
		RoleID string `json:"role_id"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	roleID, err := uuid.Parse(in.RoleID)
	if err != nil {
		httpx.WriteJSON(w, http.StatusBadRequest, httpx.ErrorBody{Code: "VALIDATION", Message: "invalid role_id"})
		return
	}
	if err := h.svc.ChangeMemberRole(r.Context(), pid, id, target, roleID); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Member is a business's access-list entry: a principal's identity, the union of
// grants (direct + each inherited ancestor) conferring access here, and its
// effective permission set (FR-016). display_name is the only identity field
// surfaced — email and other PII are withheld (FR-030).
type Member struct {
	PrincipalID          string   `json:"principal_id"`
	Kind                 string   `json:"kind"`
	DisplayName          string   `json:"display_name"`
	Grants               []Grant  `json:"grants"`
	EffectivePermissions []string `json:"effective_permissions"`
}

// Grant is one contributing membership: a role held directly on this business or
// inherited from a named ancestor.
type Grant struct {
	Role             GrantRole `json:"role"`
	Source           string    `json:"source"` // direct | inherited
	SourceBusinessID string    `json:"source_business_id"`
}

// GrantRole is the API role view embedded in a grant.
type GrantRole struct {
	ID          string   `json:"id"`
	Key         string   `json:"key"`
	Name        string   `json:"name"`
	Builtin     bool     `json:"builtin"`
	Locked      bool     `json:"locked"`
	Permissions []string `json:"permissions"`
}

// ListMembers returns a business's access list with full provenance. The caller
// must hold members.manage or audit.read at the business (FR-016); otherwise the
// business is reported as not-found (no oracle, FR-026). Inherited grants live on
// ancestor businesses the caller may not see under RLS, so the grant set is read
// via the access_list SECURITY DEFINER function AFTER the authorization check.
func (s *Service) ListMembers(ctx context.Context, viewerID, businessID uuid.UUID) ([]Member, error) {
	var out []Member
	err := s.DB.WithPrincipal(ctx, viewerID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		if _, err := loadVisible(ctx, q, businessID); err != nil {
			return err
		}
		perms, err := authz.Resolve(ctx, tx, viewerID, businessID)
		if err != nil {
			return err
		}
		if !perms.Has("members.manage") && !perms.Has("audit.read") {
			return errs.ErrNotFound
		}
		catalog, err := q.AllPermissionKeys(ctx)
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx,
			`SELECT principal_id, kind, display_name, source_business, is_direct,
			        role_id, role_key, role_name, role_builtin, role_locked, permissions
			 FROM access_list($1)`, businessID)
		if err != nil {
			return err
		}
		defer rows.Close()

		idx := map[string]int{}
		for rows.Next() {
			var (
				pid, srcBiz, roleID            uuid.UUID
				kind, dname, roleKey, roleName string
				isDirect, builtin, locked      bool
				rolePerms                      []string
			)
			if err := rows.Scan(&pid, &kind, &dname, &srcBiz, &isDirect,
				&roleID, &roleKey, &roleName, &builtin, &locked, &rolePerms); err != nil {
				return err
			}
			source := "inherited"
			if isDirect {
				source = "direct"
			}
			g := Grant{
				Role: GrantRole{
					ID: roleID.String(), Key: roleKey, Name: roleName,
					Builtin: builtin, Locked: locked, Permissions: nonNilStrings(rolePerms),
				},
				Source: source, SourceBusinessID: srcBiz.String(),
			}
			i, ok := idx[pid.String()]
			if !ok {
				idx[pid.String()] = len(out)
				out = append(out, Member{PrincipalID: pid.String(), Kind: kind, DisplayName: dname})
				i = len(out) - 1
			}
			out[i].Grants = append(out[i].Grants, g)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		for i := range out {
			out[i].EffectivePermissions = effectivePermissions(out[i].Grants, catalog)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// effectivePermissions unions a member's grants. A locked (Owner) grant resolves
// to the entire catalog so future permissions are covered (mirrors authz.Resolve).
func effectivePermissions(grants []Grant, catalog []string) []string {
	for _, g := range grants {
		if g.Role.Locked {
			return append([]string(nil), catalog...)
		}
	}
	set := map[string]struct{}{}
	for _, g := range grants {
		for _, p := range g.Role.Permissions {
			set[p] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// listMembers handles GET /businesses/{id}/members.
func (h *Handler) listMembers(w http.ResponseWriter, r *http.Request) {
	pid, ok := h.principal(w, r)
	if !ok {
		return
	}
	id, err := pathID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	members, err := h.svc.ListMembers(r.Context(), pid, id)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page[Member]{Items: members, NextCursor: nil})
}

// revokeMember handles DELETE /businesses/{id}/members/{principalId}.
func (h *Handler) revokeMember(w http.ResponseWriter, r *http.Request) {
	pid, ok := h.principal(w, r)
	if !ok {
		return
	}
	id, err := pathID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	target, err := uuid.Parse(chi.URLParam(r, "principalId"))
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	if err := h.svc.RevokeMember(r.Context(), pid, id, target); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// leaveBusiness handles POST /businesses/{id}/leave.
func (h *Handler) leaveBusiness(w http.ResponseWriter, r *http.Request) {
	pid, ok := h.principal(w, r)
	if !ok {
		return
	}
	id, err := pathID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	if err := h.svc.LeaveBusiness(r.Context(), pid, id); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
