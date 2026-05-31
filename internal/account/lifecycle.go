package account

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/audit"
	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
	"github.com/manyforge/manyforge/internal/platform/errs"
)

// erasureRetention is the grace window between a delete request and the
// irreversible PII purge (FR-028). Access is cut off immediately; the account is
// recoverable until purge_after, after which the purge worker (T085) anonymizes
// the PII while preserving the pseudonymized audit trail.
const erasureRetention = 30 * 24 * time.Hour

// Deactivate marks the account inactive (reversible). Login already denies any
// non-active account, so this takes effect immediately. Refused if the principal
// is the last Owner of any tenant — that would orphan it (FR-014/FR-024). Audited.
func (s *Service) Deactivate(ctx context.Context, principalID uuid.UUID) error {
	return s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		acc, err := q.GetAccountByPrincipal(ctx, principalID)
		if err != nil {
			return errs.ErrNotFound
		}
		if err := guardNotLastOwner(ctx, q, principalID); err != nil {
			return err
		}
		if err := q.DeactivateAccount(ctx, acc.ID); err != nil {
			return err
		}
		tt := "account"
		return audit.Write(ctx, tx, audit.Entry{
			ActorPrincipalID: &principalID, Action: "account.deactivated",
			TargetType: &tt, TargetID: &principalID,
		})
	})
}

// Delete soft-deletes the account and cuts off access immediately — deleted_at is
// set (Login filters it out), every live session is revoked — then schedules the
// irreversible PII purge for now()+retention (FR-028). Refused if the principal
// is the last Owner of any tenant. The audit trail is preserved; it becomes
// pseudonymized once the purge worker anonymizes the account, with principal_id
// as the stable pseudonym. Returns the scheduled purge time. Audited.
func (s *Service) Delete(ctx context.Context, principalID uuid.UUID) (time.Time, error) {
	var purgeAfter time.Time
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		acc, err := q.GetAccountByPrincipal(ctx, principalID)
		if err != nil {
			return errs.ErrNotFound
		}
		if err := guardNotLastOwner(ctx, q, principalID); err != nil {
			return err
		}
		if err := q.SoftDeleteAccount(ctx, acc.ID); err != nil {
			return err
		}
		if err := q.RevokeAllRefreshForPrincipal(ctx, principalID); err != nil {
			return err
		}
		purgeAfter = s.now().Add(erasureRetention)
		if err := q.ScheduleErasure(ctx, dbgen.ScheduleErasureParams{AccountID: acc.ID, PurgeAfter: purgeAfter}); err != nil {
			return err
		}
		tt := "account"
		return audit.Write(ctx, tx, audit.Entry{
			ActorPrincipalID: &principalID, Action: "account.deletion_scheduled",
			TargetType: &tt, TargetID: &principalID,
			NewValue: map[string]any{"purge_after": purgeAfter.UTC().Format(time.RFC3339)},
		})
	})
	if err != nil {
		return time.Time{}, err
	}
	return purgeAfter, nil
}

// ExportMembership is one grant in a personal-data export.
type ExportMembership struct {
	BusinessID   string
	BusinessName string
	TenantRootID string
	RoleKey      string
	GrantedAt    time.Time
}

// Export is the account's portable personal data (FR-028).
type Export struct {
	Account     Profile
	Memberships []ExportMembership
}

// Export returns the account's personal data and its direct memberships.
func (s *Service) Export(ctx context.Context, principalID uuid.UUID) (Export, error) {
	var out Export
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		acc, err := q.GetAccountByPrincipal(ctx, principalID)
		if err != nil {
			return errs.ErrNotFound
		}
		out.Account = toProfile(acc)
		rows, err := q.ExportMembershipsForPrincipal(ctx, principalID)
		if err != nil {
			return err
		}
		out.Memberships = make([]ExportMembership, 0, len(rows))
		for _, m := range rows {
			out.Memberships = append(out.Memberships, ExportMembership{
				BusinessID:   m.BusinessID.String(),
				BusinessName: m.BusinessName,
				TenantRootID: m.TenantRootID.String(),
				RoleKey:      m.RoleKey,
				GrantedAt:    m.GrantedAt,
			})
		}
		return nil
	})
	return out, err
}

// guardNotLastOwner refuses the operation if the principal is the sole direct
// Owner of any tenant root — deactivating/deleting them would leave that tenant
// ownerless (FR-014/FR-024). Runs under the caller's own principal context, where
// their owner memberships are visible and, as an Owner, the tenant's owner count
// is readable.
func guardNotLastOwner(ctx context.Context, q *dbgen.Queries, principalID uuid.UUID) error {
	roots, err := q.ListOwnerRootMembershipsForPrincipal(ctx, principalID)
	if err != nil {
		return err
	}
	for _, root := range roots {
		owners, err := q.CountDirectOwners(ctx, root)
		if err != nil {
			return err
		}
		if owners <= 1 {
			return fmt.Errorf("cannot deactivate or delete the last Owner of a tenant: %w", errs.ErrConflict)
		}
	}
	return nil
}
