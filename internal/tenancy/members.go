package tenancy

import (
	"context"
	"fmt"
	"net/http"

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
		return fmt.Errorf("cannot demote the last Owner; transfer ownership first: %w", errs.ErrConflict)
	}
	return nil
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
