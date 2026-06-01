// Package tenancy owns the business hierarchy, membership, and isolation.
package tenancy

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/authz"
	"github.com/manyforge/manyforge/internal/platform/audit"
	"github.com/manyforge/manyforge/internal/platform/db"
	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/events"
)

// MaxDepth bounds hierarchy nesting (FR-004; configurable later).
const MaxDepth = 10

// Service implements the tenancy use cases.
type Service struct {
	DB *db.DB
}

// loadVisible returns a business the caller can see, or ErrNotFound (no oracle).
func loadVisible(ctx context.Context, q *dbgen.Queries, id uuid.UUID) (dbgen.Business, error) {
	b, err := q.GetBusiness(ctx, id)
	if err != nil {
		return dbgen.Business{}, errs.ErrNotFound
	}
	return b, nil
}

// requirePerm checks the caller holds perm at business, returning ErrNotFound if not.
func requirePerm(ctx context.Context, tx pgx.Tx, principalID, businessID uuid.UUID, perm string) error {
	perms, err := authz.Resolve(ctx, tx, principalID, businessID)
	if err != nil {
		return err
	}
	if !perms.Has(perm) {
		return errs.ErrNotFound
	}
	return nil
}

// Business is the API-facing view of a business node.
type Business struct {
	ID           uuid.UUID
	ParentID     *uuid.UUID
	TenantRootID uuid.UUID
	Name         string
	Status       string
}

// GetBusiness returns a single business the caller can see (RLS-scoped), or
// ErrNotFound for an unknown or unauthorized id — the same shape, no oracle.
func (s *Service) GetBusiness(ctx context.Context, principalID, id uuid.UUID) (Business, error) {
	var out Business
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		b, err := loadVisible(ctx, dbgen.New(tx), id)
		if err != nil {
			return err
		}
		var parent *uuid.UUID
		if b.ParentID.Valid {
			p := uuid.UUID(b.ParentID.Bytes)
			parent = &p
		}
		out = Business{ID: b.ID, ParentID: parent, TenantRootID: b.TenantRootID, Name: b.Name, Status: b.Status}
		return nil
	})
	return out, err
}

// ListBusinesses returns the businesses visible to the principal (RLS-scoped).
func (s *Service) ListBusinesses(ctx context.Context, principalID uuid.UUID) ([]Business, error) {
	var out []Business
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		rows, err := dbgen.New(tx).ListBusinesses(ctx)
		if err != nil {
			return err
		}
		out = make([]Business, 0, len(rows))
		for _, b := range rows {
			var parent *uuid.UUID
			if b.ParentID.Valid {
				p := uuid.UUID(b.ParentID.Bytes)
				parent = &p
			}
			out = append(out, Business{ID: b.ID, ParentID: parent, TenantRootID: b.TenantRootID, Name: b.Name, Status: b.Status})
		}
		return nil
	})
	return out, err
}

// CreateMasterBusiness creates a top-level business owned by the creator: the
// business, its self closure row, the creator's Owner membership, and an audit
// entry — all in one transaction. The creator's email must be verified (FR-002).
func (s *Service) CreateMasterBusiness(ctx context.Context, creatorPrincipalID uuid.UUID, name string) (Business, error) {
	if name == "" {
		return Business{}, fmt.Errorf("name is required: %w", errs.ErrValidation)
	}
	var out Business
	bizID := uuid.New()
	err := s.DB.WithPrincipal(ctx, creatorPrincipalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)

		verified, err := q.IsAccountVerifiedByPrincipal(ctx, creatorPrincipalID)
		if err != nil {
			return err
		}
		if !verified {
			return fmt.Errorf("email must be verified before creating a business: %w", errs.ErrValidation)
		}

		ownerRole, err := q.OwnerRoleID(ctx)
		if err != nil {
			return err
		}
		if err := q.CreateBusiness(ctx, dbgen.CreateBusinessParams{
			ID: bizID, ParentID: db.PGUUIDPtr(nil), TenantRootID: bizID, Name: name,
		}); err != nil {
			return err
		}
		if err := q.InsertClosureSelf(ctx, dbgen.InsertClosureSelfParams{
			AncestorID: bizID, TenantRootID: bizID,
		}); err != nil {
			return err
		}
		if err := q.CreateMembership(ctx, dbgen.CreateMembershipParams{
			ID: uuid.New(), PrincipalID: creatorPrincipalID, BusinessID: bizID,
			TenantRootID: bizID, RoleID: ownerRole, GrantedBy: db.PGUUID(creatorPrincipalID),
		}); err != nil {
			return err
		}
		targetType := "business"
		if err := audit.Write(ctx, tx, audit.Entry{
			BusinessID: &bizID, TenantRootID: &bizID, ActorPrincipalID: &creatorPrincipalID,
			Action: "business.created", TargetType: &targetType, TargetID: &bizID,
			NewValue: map[string]any{"name": name, "kind": "master"},
		}); err != nil {
			return err
		}
		// Emit business.created in the SAME tx so inbox auto-provisions the zero-config
		// system inbound address (FR-001). The outbox decouples the two modules —
		// tenancy does NOT import inbox; inbox subscribes to this topic.
		if err := events.Enqueue(ctx, tx, bizID, events.TopicBusinessCreated, map[string]any{
			"business_id":    bizID,
			"tenant_root_id": bizID,
		}); err != nil {
			return err
		}
		out = Business{ID: bizID, TenantRootID: bizID, Name: name, Status: "active"}
		return nil
	})
	return out, err
}

func auditBusiness(ctx context.Context, tx pgx.Tx, actor, business, tenantRoot uuid.UUID, action string, value any) error {
	tt := "business"
	return audit.Write(ctx, tx, audit.Entry{
		BusinessID: &business, TenantRootID: &tenantRoot, ActorPrincipalID: &actor,
		Action: action, TargetType: &tt, TargetID: &business, NewValue: value,
	})
}

// CreateSubBusiness nests a new business under parentID. Requires hierarchy.manage
// on the parent; runs under the tenant advisory lock (FR-004/R5).
func (s *Service) CreateSubBusiness(ctx context.Context, principalID, parentID uuid.UUID, name string) (Business, error) {
	if name == "" {
		return Business{}, fmt.Errorf("name is required: %w", errs.ErrValidation)
	}
	var out Business
	childID := uuid.New()
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		parent, err := loadVisible(ctx, q, parentID)
		if err != nil {
			return err
		}
		if err := q.AcquireTenantLock(ctx, parent.TenantRootID.String()); err != nil {
			return err
		}
		if err := requirePerm(ctx, tx, principalID, parentID, "hierarchy.manage"); err != nil {
			return err
		}
		depth, err := q.DepthFromRoot(ctx, dbgen.DepthFromRootParams{DescendantID: parentID, AncestorID: parent.TenantRootID})
		if err != nil {
			return err
		}
		if int(depth)+1 > MaxDepth {
			return fmt.Errorf("maximum nesting depth (%d) reached: %w", MaxDepth, errs.ErrConflict)
		}
		if err := q.CreateBusiness(ctx, dbgen.CreateBusinessParams{ID: childID, ParentID: db.PGUUID(parentID), TenantRootID: parent.TenantRootID, Name: name}); err != nil {
			return err
		}
		if err := q.InsertClosureSelf(ctx, dbgen.InsertClosureSelfParams{AncestorID: childID, TenantRootID: parent.TenantRootID}); err != nil {
			return err
		}
		if err := q.InsertChildClosure(ctx, dbgen.InsertChildClosureParams{DescendantID: childID, DescendantID_2: parentID, TenantRootID: parent.TenantRootID}); err != nil {
			return err
		}
		if err := auditBusiness(ctx, tx, principalID, childID, parent.TenantRootID, "business.created", map[string]any{"name": name, "parent_id": parentID.String()}); err != nil {
			return err
		}
		// Emit business.created in the SAME tx so inbox auto-provisions the sub-business's
		// zero-config system inbound address (FR-001); tenant_root_id is the parent's root.
		if err := events.Enqueue(ctx, tx, parent.TenantRootID, events.TopicBusinessCreated, map[string]any{
			"business_id":    childID,
			"tenant_root_id": parent.TenantRootID,
		}); err != nil {
			return err
		}
		p := parentID
		out = Business{ID: childID, ParentID: &p, TenantRootID: parent.TenantRootID, Name: name, Status: "active"}
		return nil
	})
	return out, err
}

// Move reparents nodeID under newParentID within the same tenant. Rejects cycles,
// cross-tenant moves, master moves, and depth overflow (FR-006/FR-031/R5).
func (s *Service) Move(ctx context.Context, principalID, nodeID, newParentID uuid.UUID) error {
	if nodeID == newParentID {
		return fmt.Errorf("a business cannot be moved under itself: %w", errs.ErrConflict)
	}
	return s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		node, err := loadVisible(ctx, q, nodeID)
		if err != nil {
			return err
		}
		newParent, err := loadVisible(ctx, q, newParentID)
		if err != nil {
			return err
		}
		if node.TenantRootID != newParent.TenantRootID {
			return fmt.Errorf("cannot move across tenants: %w", errs.ErrConflict)
		}
		if err := q.AcquireTenantLock(ctx, node.TenantRootID.String()); err != nil {
			return err
		}
		if err := requirePerm(ctx, tx, principalID, nodeID, "hierarchy.manage"); err != nil {
			return err
		}
		if err := requirePerm(ctx, tx, principalID, newParentID, "hierarchy.manage"); err != nil {
			return err
		}
		if !node.ParentID.Valid {
			return fmt.Errorf("cannot move a master business: %w", errs.ErrConflict)
		}
		isDesc, err := q.IsDescendant(ctx, dbgen.IsDescendantParams{AncestorID: nodeID, DescendantID: newParentID})
		if err != nil {
			return err
		}
		if isDesc {
			return fmt.Errorf("would create a cycle: %w", errs.ErrConflict)
		}
		npDepth, err := q.DepthFromRoot(ctx, dbgen.DepthFromRootParams{DescendantID: newParentID, AncestorID: node.TenantRootID})
		if err != nil {
			return err
		}
		height, err := q.SubtreeHeight(ctx, nodeID)
		if err != nil {
			return err
		}
		if int(npDepth)+1+int(height) > MaxDepth {
			return fmt.Errorf("move would exceed maximum nesting depth (%d): %w", MaxDepth, errs.ErrConflict)
		}
		// RLS-exempt structural rewrite (see migration 0009); authorization was
		// enforced by the checks above.
		if _, err := tx.Exec(ctx, "SELECT move_business($1, $2, $3)", nodeID, newParentID, node.TenantRootID); err != nil {
			return err
		}
		return auditBusiness(ctx, tx, principalID, nodeID, node.TenantRootID, "business.moved", map[string]any{"new_parent_id": newParentID.String()})
	})
}

// setStatus sets the subtree status under the tenant lock.
func (s *Service) setStatus(ctx context.Context, principalID, nodeID uuid.UUID, status, action string) error {
	return s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		node, err := loadVisible(ctx, q, nodeID)
		if err != nil {
			return err
		}
		if err := q.AcquireTenantLock(ctx, node.TenantRootID.String()); err != nil {
			return err
		}
		if err := requirePerm(ctx, tx, principalID, nodeID, "hierarchy.manage"); err != nil {
			return err
		}
		if err := q.SetSubtreeStatus(ctx, dbgen.SetSubtreeStatusParams{AncestorID: nodeID, Status: status}); err != nil {
			return err
		}
		return auditBusiness(ctx, tx, principalID, nodeID, node.TenantRootID, action, map[string]any{"status": status})
	})
}

// Archive archives a business and its descendants.
func (s *Service) Archive(ctx context.Context, principalID, nodeID uuid.UUID) error {
	return s.setStatus(ctx, principalID, nodeID, "archived", "business.archived")
}

// Restore reactivates an archived business and its descendants.
func (s *Service) Restore(ctx context.Context, principalID, nodeID uuid.UUID) error {
	return s.setStatus(ctx, principalID, nodeID, "active", "business.restored")
}

// RenameBusiness changes a business's name (requires hierarchy.manage).
func (s *Service) RenameBusiness(ctx context.Context, principalID, nodeID uuid.UUID, name string) error {
	if name == "" {
		return fmt.Errorf("name is required: %w", errs.ErrValidation)
	}
	return s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		node, err := loadVisible(ctx, q, nodeID)
		if err != nil {
			return err
		}
		if err := requirePerm(ctx, tx, principalID, nodeID, "hierarchy.manage"); err != nil {
			return err
		}
		if err := q.RenameBusiness(ctx, dbgen.RenameBusinessParams{ID: nodeID, Name: name}); err != nil {
			return err
		}
		return auditBusiness(ctx, tx, principalID, nodeID, node.TenantRootID, "business.renamed", map[string]any{"name": name})
	})
}

// Delete soft-deletes a business; requires business.delete and no active children (FR-017).
func (s *Service) Delete(ctx context.Context, principalID, nodeID uuid.UUID) error {
	return s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		node, err := loadVisible(ctx, q, nodeID)
		if err != nil {
			return err
		}
		if err := q.AcquireTenantLock(ctx, node.TenantRootID.String()); err != nil {
			return err
		}
		if err := requirePerm(ctx, tx, principalID, nodeID, "business.delete"); err != nil {
			return err
		}
		children, err := q.CountActiveChildren(ctx, db.PGUUID(nodeID))
		if err != nil {
			return err
		}
		if children > 0 {
			return fmt.Errorf("archive or move sub-businesses before deleting: %w", errs.ErrConflict)
		}
		if err := q.SoftDeleteBusiness(ctx, nodeID); err != nil {
			return err
		}
		return auditBusiness(ctx, tx, principalID, nodeID, node.TenantRootID, "business.deleted", nil)
	})
}
