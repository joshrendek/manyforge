package githubapp

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/errs"
)

// InstallationService owns the lifecycle of github_app_installation rows
// (migrations/0082): mapping a GitHub App installation to a business/agent.
// The table is RLS-scoped (business_id must match the caller's authorized
// businesses), but every mutation here goes through a SECURITY DEFINER
// function since these calls are principal-less (webhook receiver) or must
// cross the RLS boundary to link an as-yet-unlinked (business_id IS NULL)
// row — see migrations/0082 for the guard logic.
type InstallationService struct{ DB txRunner }

// UpsertFromEvent records (or refreshes) an installation's identity from a
// GitHub webhook/setup callback. It never links the installation to a
// business — that only happens via Link.
func (s *InstallationService) UpsertFromEvent(ctx context.Context, id int64, login, accountType string) error {
	return s.DB.WithTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "SELECT github_upsert_installation($1, $2, $3)", id, login, accountType)
		return err
	})
}

// MarkDeleted records that the GitHub App was uninstalled: the installation
// is soft-deleted and disabled, but its row (and any business link) is
// retained for audit purposes.
func (s *InstallationService) MarkDeleted(ctx context.Context, id int64) error {
	return s.DB.WithTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "SELECT github_mark_installation_deleted($1)", id)
		return err
	})
}

// SetSuspended records a GitHub App suspend/unsuspend event for the
// installation.
func (s *InstallationService) SetSuspended(ctx context.Context, id int64, suspended bool) error {
	return s.DB.WithTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "SELECT github_set_installation_suspended($1, $2)", id, suspended)
		return err
	})
}

// Link binds an unlinked installation to a business and one of that
// business's agents. It returns errs.ErrNotFound if there is no eligible
// unlinked installation row for id, or if agentID does not belong to
// businessID — the two cases are indistinguishable to the caller (no
// existence oracle).
func (s *InstallationService) Link(ctx context.Context, id int64, businessID, agentID uuid.UUID) error {
	return s.DB.WithTx(ctx, func(tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, "SELECT github_link_installation($1, $2, $3)", id, businessID, agentID).Scan(&n); err != nil {
			return fmt.Errorf("link installation: %w", err)
		}
		if n == 0 {
			return errs.ErrNotFound // no unlinked row, or agent not in business
		}
		return nil
	})
}
