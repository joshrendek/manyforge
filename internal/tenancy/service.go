// Package tenancy owns the business hierarchy, membership, and isolation.
package tenancy

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/audit"
	"github.com/manyforge/manyforge/internal/platform/db"
	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
	"github.com/manyforge/manyforge/internal/platform/errs"
)

// Service implements the tenancy use cases.
type Service struct {
	DB *db.DB
}

// Business is the API-facing view of a business node.
type Business struct {
	ID           uuid.UUID
	ParentID     *uuid.UUID
	TenantRootID uuid.UUID
	Name         string
	Status       string
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
		out = Business{ID: bizID, TenantRootID: bizID, Name: name, Status: "active"}
		return nil
	})
	return out, err
}
