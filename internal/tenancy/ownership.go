package tenancy

import (
	"context"
	"fmt"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/audit"
	"github.com/manyforge/manyforge/internal/platform/db"
	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/httpx"
)

// TransferOwnership moves Owner of a tenant to another member, atomically
// (FR-024). It is Owner-only and operates ONLY on the tenant root — a sub-business
// is a conflict. The new owner must be a human, direct member of the root; on
// success they are promoted to the Owner role and the outgoing owner steps down to
// Admin, in a single transaction under the tenant lock, with the deferred
// tenant_owner_guard as the zero-owner backstop. Audited in the same tx. A
// non-owner actor or unknown business is reported as not-found (no oracle, FR-026).
func (s *Service) TransferOwnership(ctx context.Context, actorID, businessID, toPrincipal uuid.UUID) error {
	if actorID == toPrincipal {
		return fmt.Errorf("cannot transfer ownership to yourself: %w", errs.ErrConflict)
	}
	return s.DB.WithPrincipal(ctx, actorID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		biz, err := loadVisible(ctx, q, businessID)
		if err != nil {
			return err
		}
		if businessID != biz.TenantRootID {
			return fmt.Errorf("ownership transfer operates on the tenant root: %w", errs.ErrConflict)
		}
		if err := q.AcquireTenantLock(ctx, biz.TenantRootID.String()); err != nil {
			return err
		}
		isOwner, err := q.HasOwnerRole(ctx, dbgen.HasOwnerRoleParams{PrincipalID: actorID, DescendantID: businessID})
		if err != nil {
			return err
		}
		if !isOwner {
			return errs.ErrNotFound // Owner-only; no oracle for non-owners
		}
		if _, err := q.GetMembershipAt(ctx, dbgen.GetMembershipAtParams{PrincipalID: toPrincipal, BusinessID: businessID}); err != nil {
			return fmt.Errorf("the new owner must be a member of this business: %w", errs.ErrConflict)
		}
		kind, err := q.GetPrincipalKind(ctx, toPrincipal)
		if err != nil {
			return errs.ErrNotFound
		}
		if kind != "human" {
			return fmt.Errorf("ownership can be held only by a human principal: %w", errs.ErrConflict)
		}
		ownerRole, err := q.PresetRoleID(ctx, "owner")
		if err != nil {
			return err
		}
		adminRole, err := q.PresetRoleID(ctx, "admin")
		if err != nil {
			return err
		}
		// Promote the new owner and step the outgoing owner down to Admin in one tx;
		// the deferred tenant_owner_guard verifies ≥1 Owner at commit.
		if err := q.UpdateMembershipRole(ctx, dbgen.UpdateMembershipRoleParams{
			PrincipalID: toPrincipal, BusinessID: businessID, RoleID: ownerRole, GrantedBy: db.PGUUID(actorID),
		}); err != nil {
			return err
		}
		if err := q.UpdateMembershipRole(ctx, dbgen.UpdateMembershipRoleParams{
			PrincipalID: actorID, BusinessID: businessID, RoleID: adminRole, GrantedBy: db.PGUUID(actorID),
		}); err != nil {
			return err
		}
		tt := "membership"
		return audit.Write(ctx, tx, audit.Entry{
			BusinessID: &businessID, TenantRootID: &biz.TenantRootID, ActorPrincipalID: &actorID,
			Action: "ownership.transferred", TargetType: &tt, TargetID: &toPrincipal,
			NewValue: map[string]any{"from": actorID.String(), "to": toPrincipal.String()},
		})
	})
}

// transferOwnership handles POST /businesses/{id}/transfer-ownership.
func (h *Handler) transferOwnership(w http.ResponseWriter, r *http.Request) {
	pid, ok := h.principal(w, r)
	if !ok {
		return
	}
	id, err := pathID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	var in struct {
		ToPrincipalID string `json:"to_principal_id"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	to, err := uuid.Parse(in.ToPrincipalID)
	if err != nil {
		httpx.WriteJSON(w, http.StatusBadRequest, httpx.ErrorBody{Code: "VALIDATION", Message: "invalid to_principal_id"})
		return
	}
	if err := h.svc.TransferOwnership(r.Context(), pid, id, to); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
