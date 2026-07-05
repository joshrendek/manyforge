package githubapp

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// InstallationContext is the principal-less resolution of a GitHub App
// installation to its linked business/agent (migrations/0084:
// github_installation_context). BusinessID/TenantRootID/AgentID/
// AgentPrincipalID are uuid.Nil when the installation is not yet linked to a
// business (see migrations/0082/0084 — LEFT JOIN agent).
type InstallationContext struct {
	BusinessID, TenantRootID, AgentID, AgentPrincipalID uuid.UUID
	AgentEnabled, Enabled, Suspended                    bool
}

// PRReviewInput is the resolved (installation, delivery, business/agent,
// repo/PR/head) tuple passed to IngestPRReview.
type PRReviewInput struct {
	InstallationID                                      int64
	DeliveryID, Repo                                    string
	PRNumber                                            int
	HeadSHA                                             string
	BusinessID, TenantRootID, AgentID, AgentPrincipalID uuid.UUID
}

// PRReviewEnqueuer is the principal-less (webhook receiver) entry point for
// the pull_request → code_review path: it resolves a GitHub App installation
// to its linked business/agent, then atomically ingests a PR review request
// (dedup, rate cap, ensure-connector, same-head skip, pending-supersede,
// insert) via the migrations/0084 SECURITY DEFINER functions.
type PRReviewEnqueuer struct{ DB txRunner }

// ResolveInstallation looks up an installation's linked business/agent via
// github_installation_context. found is false if there is no non-deleted
// installation row for installationID at all; a found-but-unlinked
// installation returns found=true with zero-value business/agent fields.
func (e *PRReviewEnqueuer) ResolveInstallation(ctx context.Context, installationID int64) (InstallationContext, bool, error) {
	var c InstallationContext
	found := false
	err := e.DB.WithTx(ctx, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT business_id, tenant_root_id, agent_id, agent_principal_id, agent_enabled, enabled, suspended
            FROM github_installation_context($1)`, installationID)
		var bid, trid, aid, apid pgtype.UUID // nullable business/agent when unlinked
		if err := row.Scan(&bid, &trid, &aid, &apid, &c.AgentEnabled, &c.Enabled, &c.Suspended); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}
		found = true
		c.BusinessID = fromPg(bid)
		c.TenantRootID = fromPg(trid)
		c.AgentID = fromPg(aid)
		c.AgentPrincipalID = fromPg(apid)
		return nil
	})
	if err != nil {
		return InstallationContext{}, false, fmt.Errorf("resolve installation: %w", err)
	}
	return c, found, nil
}

// IngestPRReview atomically ingests one pull_request webhook event via
// github_pr_review_ingest. ok=false (with a nil error) means the event was a
// no-op by design: a replayed delivery id, an installation over the hourly
// rate cap, or a duplicate (repo, pr, head_sha) already pending/running/
// succeeded — none of these are caller errors.
func (e *PRReviewEnqueuer) IngestPRReview(ctx context.Context, in PRReviewInput) (uuid.UUID, bool, error) {
	var id pgtype.UUID
	err := e.DB.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT github_pr_review_ingest($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			in.InstallationID, in.DeliveryID, in.BusinessID, in.TenantRootID, in.AgentID,
			in.AgentPrincipalID, in.Repo, in.PRNumber, in.HeadSHA).Scan(&id)
	})
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("ingest pr review: %w", err)
	}
	if !id.Valid {
		return uuid.Nil, false, nil // replay/rate/dup
	}
	return uuid.UUID(id.Bytes), true, nil
}

// fromPg converts a nullable pgtype.UUID to uuid.UUID, mapping SQL NULL to
// uuid.Nil (used for the business/agent columns that are NULL when an
// installation is not yet linked).
func fromPg(u pgtype.UUID) uuid.UUID {
	if u.Valid {
		return u.Bytes
	}
	return uuid.Nil
}
