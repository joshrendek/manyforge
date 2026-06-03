package agents

import (
	"context"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/authz"
	"github.com/manyforge/manyforge/internal/platform/ai"
	"github.com/manyforge/manyforge/internal/platform/audit"
	"github.com/manyforge/manyforge/internal/platform/db"
)

// dbAuditor writes per-run audit rows under the agent principal's RLS context.
type dbAuditor struct{ db *db.DB }

// NewDBAuditor builds the production runAuditor over audit_entry.
func NewDBAuditor(d *db.DB) *dbAuditor { return &dbAuditor{db: d} }

func (a *dbAuditor) Run(ctx context.Context, principalID uuid.UUID, run AgentRun, action string, inputs, outputs any, decision string) error {
	pid := principalID
	bid := run.BusinessID
	rid := run.ID
	corr := run.CorrelationID
	dec := decision
	tt := "agent_run"
	err := a.db.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		return audit.Write(ctx, tx, audit.Entry{
			BusinessID:       &bid,
			ActorPrincipalID: &pid,
			Action:           action,
			TargetType:       &tt,
			TargetID:         &rid,
			CorrelationID:    &corr,
			Inputs:           inputs,
			Outputs:          outputs,
			Decision:         &dec,
		})
	})
	if err != nil {
		// Audit is best-effort telemetry; the engine treats failures as non-fatal,
		// but never let it vanish silently.
		slog.WarnContext(ctx, "agent run: audit write failed", "run_id", run.ID, "action", action, "err", err)
	}
	return err
}

// authzChecker resolves the agent's effective permissions (same RBAC layer as a human).
type authzChecker struct{ db *db.DB }

// NewAuthzChecker builds the production permChecker over authz.Resolve.
func NewAuthzChecker(d *db.DB) *authzChecker { return &authzChecker{db: d} }

func (c *authzChecker) Has(ctx context.Context, principalID, businessID uuid.UUID, key string) (bool, error) {
	var ok bool
	err := c.db.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		set, e := authz.Resolve(ctx, tx, principalID, businessID)
		if e != nil {
			return e
		}
		ok = set.Has(key)
		return nil
	})
	return ok, err
}

// NewCredentialProviderFactory builds the production providerFactory: resolves the agent's
// BYO credential, then constructs the SSRF-guarded AI provider. Returns the resolved model id.
func NewCredentialProviderFactory(cs *CredentialService) providerFactory {
	return func(ctx context.Context, principalID, businessID uuid.UUID, provider string) (ai.Provider, string, error) {
		rc, err := cs.Resolve(ctx, principalID, businessID, provider)
		if err != nil {
			return nil, "", err
		}
		p, perr := ai.New(ai.Credential{Provider: rc.Provider, APIKey: rc.APIKey, BaseURL: rc.BaseURL, Model: rc.Model})
		if perr != nil {
			return nil, "", perr
		}
		return p, rc.Model, nil
	}
}

// Compile-time assertions: the production adapters satisfy the Engine's collaborator
// interfaces (wired in Task 8b).
var _ runAuditor = (*dbAuditor)(nil)
var _ permChecker = (*authzChecker)(nil)
