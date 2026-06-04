package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/manyforge/manyforge/internal/platform/mcp"
)

// mcpServerResolver is the subset of MCPServerService the MCPHost needs. It is
// a separate interface (not mcpValidator) so unit tests can inject a lightweight
// fake that only implements ListEnabledForAgent without a DB.
type mcpServerResolver interface {
	ListEnabledForAgent(ctx context.Context, principalID, businessID uuid.UUID, ids []uuid.UUID) ([]ResolvedMCPServer, error)
}

// MCPHost discovers tools from the agent's allowed MCP servers at run start,
// returning them classified as External with RequiredPerm "mcp.invoke". Per-server
// errors are fail-open: a failing server contributes zero tools; the run proceeds
// with whatever succeeded.
type MCPHost struct {
	Servers mcpServerResolver
	// Connect builds a ClientLike from a server URL and auth header. The prod
	// wiring passes a factory backed by the loopback-aware guarded transport;
	// tests inject a factory that returns a *mcp.MockClient.
	Connect mcp.ClientFactory
	Logger  *slog.Logger
}

// DiscoverTools connects to each enabled MCP server in ids, calls Initialize +
// ListTools, and returns the discovered tools namespaced as
//
//	"mcp:<server.Name>:<def.Name>"
//
// with Effect=EffectExternal and RequiredPerm="mcp.invoke". A server whose
// Initialize or ListTools call fails contributes zero tools and is recorded for
// audit; DiscoverTools never returns a non-nil error from a per-server failure
// (fail-open). The only non-nil error return is from the resolver itself
// (e.g. DB down at run start), which the caller treats as a discovery failure.
func (h *MCPHost) DiscoverTools(ctx context.Context, principalID, businessID uuid.UUID, ids []uuid.UUID) ([]Tool, error) {
	servers, err := h.Servers.ListEnabledForAgent(ctx, principalID, businessID, ids)
	if err != nil {
		return nil, fmt.Errorf("agents: mcp_host: list enabled servers: %w", err)
	}

	var tools []Tool
	for _, server := range servers {
		discovered, err := h.discoverServerTools(ctx, server)
		if err != nil {
			h.Logger.WarnContext(ctx, "agent.mcp.discovery_failed",
				"server_id", server.ID,
				"server_name", server.Name,
				"err", err,
			)
			// Fail-open: continue to next server.
			continue
		}
		tools = append(tools, discovered...)
	}
	return tools, nil
}

// discoverServerTools connects to a single server and lists its tools. Any error
// is returned to the caller (DiscoverTools) which decides whether to fail-open.
func (h *MCPHost) discoverServerTools(ctx context.Context, server ResolvedMCPServer) ([]Tool, error) {
	client := h.Connect(server.URL, server.AuthHeader)

	if err := client.Initialize(ctx); err != nil {
		return nil, fmt.Errorf("initialize: %w", err)
	}

	defs, err := client.ListTools(ctx)
	if err != nil {
		return nil, fmt.Errorf("list tools: %w", err)
	}

	tools := make([]Tool, 0, len(defs))
	for _, def := range defs {
		// Capture loop variables explicitly for closure correctness.
		capturedClient := client
		capturedDef := def
		capturedServer := server

		schemaJSON := ""
		if len(capturedDef.InputSchema) > 0 {
			schemaJSON = string(capturedDef.InputSchema)
		}

		tools = append(tools, Tool{
			Name:         "mcp:" + capturedServer.Name + ":" + capturedDef.Name,
			Description:  capturedDef.Description,
			SchemaJSON:   schemaJSON,
			Effect:       EffectExternal,
			RequiredPerm: "mcp.invoke",
			Invoke: func(ctx context.Context, pid, bid uuid.UUID, args json.RawMessage) (string, error) {
				idemHint := ""
				if k, ok := approvalKeyFrom(ctx); ok {
					idemHint = k.String()
				}
				res, err := capturedClient.CallTool(ctx, capturedDef.Name, args, idemHint)
				if err != nil {
					return "", fmt.Errorf("mcp tool %s/%s: %w", capturedServer.Name, capturedDef.Name, err)
				}
				if res.IsError {
					return "", fmt.Errorf("mcp tool %s/%s returned error: %s", capturedServer.Name, capturedDef.Name, res.Content)
				}
				return res.Content, nil
			},
		})
	}
	return tools, nil
}
