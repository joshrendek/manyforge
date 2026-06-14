// MCP per-tool effect policy (manyforge-k0d). An admin promotes specific MCP tools to
// Read/Reversible so they auto-execute mode-dependently; External (the fail-closed default) is
// the absence of a row. Mirrors MCPServerService: every method runs in the caller's RLS
// principal context AND pushes the ownership predicate into SQL.
package agents

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/audit"
	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
	"github.com/manyforge/manyforge/internal/platform/errs"
)

// MCPToolPolicy is one persisted reclassification returned to admin callers.
type MCPToolPolicy struct {
	ServerID uuid.UUID
	ToolName string
	Effect   string // "read" | "reversible"
}

type mcpPolicyDB interface {
	WithPrincipal(ctx context.Context, principalID uuid.UUID, fn func(pgx.Tx) error) error
}

// MCPToolPolicyService is the admin CRUD over mcp_tool_policy AND the run-path resolver the
// MCPHost consults at discovery.
type MCPToolPolicyService struct{ DB mcpPolicyDB }

// effectFromString maps the API effect token to the assignable EffectClass. Only Read/Reversible
// are assignable (the table CHECK enforces this too); anything else is a validation error.
func effectFromString(s string) (EffectClass, error) {
	switch s {
	case "read":
		return EffectRead, nil
	case "reversible":
		return EffectReversible, nil
	default:
		return 0, fmt.Errorf("agents: effect %q not assignable (want read|reversible): %w", s, errs.ErrValidation)
	}
}

func effectToString(e EffectClass) string {
	switch e {
	case EffectRead:
		return "read"
	case EffectReversible:
		return "reversible"
	default:
		return "external"
	}
}

// Upsert sets (or replaces) the policy for one (server, tool). Audits old→new in the same tx.
func (s *MCPToolPolicyService) Upsert(ctx context.Context, principalID, businessID, serverID uuid.UUID, toolName, effectStr string) (MCPToolPolicy, error) {
	if toolName == "" {
		return MCPToolPolicy{}, fmt.Errorf("agents: tool_name required: %w", errs.ErrValidation)
	}
	eff, err := effectFromString(effectStr)
	if err != nil {
		return MCPToolPolicy{}, err
	}
	var out MCPToolPolicy
	err = s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		// Old value for the audit (best-effort: no row => "external").
		oldStr := "external"
		if prev, gerr := q.GetMCPToolPolicy(ctx, dbgen.GetMCPToolPolicyParams{
			McpServerID: serverID, ToolName: toolName, BusinessID: businessID,
		}); gerr == nil {
			oldStr = effectToString(EffectClass(prev.Effect))
		} else if !errors.Is(gerr, pgx.ErrNoRows) {
			return gerr
		}
		row, ierr := q.UpsertMCPToolPolicy(ctx, dbgen.UpsertMCPToolPolicyParams{
			McpServerID: serverID, BusinessID: businessID, ToolName: toolName, Effect: int16(eff),
		})
		if ierr != nil {
			return ierr // ErrNoRows when the server is invisible/foreign → mapped to 404 below
		}
		out = MCPToolPolicy{ServerID: serverID, ToolName: toolName, Effect: effectToString(eff)}
		tt := "mcp_tool_policy"
		return audit.Write(ctx, tx, audit.Entry{
			BusinessID: &businessID, TenantRootID: &row.TenantRootID, ActorPrincipalID: &principalID,
			Action: "mcp.tool_policy.set", TargetType: &tt, TargetID: &serverID,
			OldValue: map[string]any{"tool": toolName, "effect": oldStr},
			NewValue: map[string]any{"tool": toolName, "effect": effectToString(eff)},
		})
	})
	if err != nil {
		return MCPToolPolicy{}, mapMCPErr(err)
	}
	return out, nil
}

// List returns the persisted policies for one server (admin view).
func (s *MCPToolPolicyService) List(ctx context.Context, principalID, businessID, serverID uuid.UUID) ([]MCPToolPolicy, error) {
	var out []MCPToolPolicy
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		rows, qerr := dbgen.New(tx).ListMCPToolPolicies(ctx, dbgen.ListMCPToolPoliciesParams{
			McpServerID: serverID, BusinessID: businessID,
		})
		if qerr != nil {
			return qerr
		}
		for _, r := range rows {
			out = append(out, MCPToolPolicy{ServerID: r.McpServerID, ToolName: r.ToolName, Effect: effectToString(EffectClass(r.Effect))})
		}
		return nil
	})
	if err != nil {
		return nil, mapMCPErr(err)
	}
	return out, nil
}

// Delete clears a policy (revert to External default). Audits the removal in the same tx.
func (s *MCPToolPolicyService) Delete(ctx context.Context, principalID, businessID, serverID uuid.UUID, toolName string) error {
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		prev, gerr := q.GetMCPToolPolicy(ctx, dbgen.GetMCPToolPolicyParams{
			McpServerID: serverID, ToolName: toolName, BusinessID: businessID,
		})
		if errors.Is(gerr, pgx.ErrNoRows) {
			return errs.ErrNotFound
		}
		if gerr != nil {
			return gerr
		}
		n, derr := q.DeleteMCPToolPolicy(ctx, dbgen.DeleteMCPToolPolicyParams{
			McpServerID: serverID, ToolName: toolName, BusinessID: businessID,
		})
		if derr != nil {
			return derr
		}
		if n == 0 {
			return errs.ErrNotFound
		}
		tt := "mcp_tool_policy"
		return audit.Write(ctx, tx, audit.Entry{
			BusinessID: &businessID, TenantRootID: &prev.TenantRootID, ActorPrincipalID: &principalID,
			Action: "mcp.tool_policy.cleared", TargetType: &tt, TargetID: &serverID,
			OldValue: map[string]any{"tool": toolName, "effect": effectToString(EffectClass(prev.Effect))},
		})
	})
	if err != nil {
		return mapMCPErr(err)
	}
	return nil
}

// ListToolPoliciesByServer is the run-path resolver (MCPHost.Policies). Returns tool_name →
// EffectClass for one server, read under the AGENT principal (RLS-scoped to its business).
func (s *MCPToolPolicyService) ListToolPoliciesByServer(ctx context.Context, principalID, businessID, serverID uuid.UUID) (map[string]EffectClass, error) {
	out := map[string]EffectClass{}
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		rows, qerr := dbgen.New(tx).ListToolPoliciesByServer(ctx, serverID)
		if qerr != nil {
			return qerr
		}
		for _, r := range rows {
			out[r.ToolName] = EffectClass(r.Effect)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("agents: list tool policies: %w", err)
	}
	return out, nil
}
