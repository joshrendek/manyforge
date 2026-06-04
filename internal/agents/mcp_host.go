package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"
	"github.com/manyforge/manyforge/internal/platform/mcp"
)

// mcpServerResolver is the subset of MCPServerService the MCPHost needs. It is
// a separate interface (not mcpValidator) so unit tests can inject a lightweight
// fake that only implements the required methods without a DB.
type mcpServerResolver interface {
	ListEnabledForAgent(ctx context.Context, principalID, businessID uuid.UUID, ids []uuid.UUID) ([]ResolvedMCPServer, error)
	// ResolveEnabledByName fetches a single enabled server by name under RLS.
	// Used by InvokeMCPTool to resolve the server for an approved mcp: tool call.
	ResolveEnabledByName(ctx context.Context, principalID, businessID uuid.UUID, name string) (ResolvedMCPServer, error)
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

// mcpInvoker is the narrow interface the ApprovalExecutor uses to invoke an MCP
// tool. It is satisfied by *MCPHost but can also be faked in unit tests.
type mcpInvoker interface {
	InvokeMCPTool(ctx context.Context, principalID, businessID uuid.UUID, namespacedTool string, args json.RawMessage, idemHint string) (string, error)
}

// InvokeMCPTool executes an approved mcp: tool call, resolving the server by name
// under RLS (tenant-scoped to principalID / businessID), connecting, and invoking
// the tool. namespacedTool must be of the form "mcp:<server>:<tool>" where server
// names must not contain ':' (enforced at server-creation time).
//
// At-least-once caveat (design §3.6): if the process crashes after CallTool returns
// but before the caller's MarkExecuted commits, the foreign side effect has already
// occurred and will be re-invoked on redelivery. Exactly-once for a foreign side
// effect requires the remote server to honour the idemHint as an idempotency key;
// this implementation passes the approval id as idemHint as a best-effort hint.
// Mark-first (at-most-once) is deliberately avoided — silently dropping an approved
// action is worse than a rare double-fire on a crash path.
func (h *MCPHost) InvokeMCPTool(ctx context.Context, principalID, businessID uuid.UUID, namespacedTool string, args json.RawMessage, idemHint string) (string, error) {
	parts := strings.SplitN(namespacedTool, ":", 3)
	if len(parts) != 3 || parts[0] != "mcp" || parts[1] == "" || parts[2] == "" {
		return "", fmt.Errorf("agents: mcp_host: malformed tool name %q (want mcp:<server>:<tool>)", namespacedTool)
	}
	serverName, toolName := parts[1], parts[2]

	server, err := h.Servers.ResolveEnabledByName(ctx, principalID, businessID, serverName)
	if err != nil {
		return "", fmt.Errorf("agents: mcp_host: resolve server %q: %w", serverName, err)
	}

	client := h.Connect(server.URL, server.AuthHeader)
	if err := client.Initialize(ctx); err != nil {
		return "", fmt.Errorf("agents: mcp_host: initialize %q: %w", serverName, err)
	}

	res, err := client.CallTool(ctx, toolName, args, idemHint)
	if err != nil {
		return "", fmt.Errorf("agents: mcp_host: call tool %s/%s: %w", serverName, toolName, err)
	}
	if res.IsError {
		return "", fmt.Errorf("agents: mcp_host: tool %s/%s returned error: %s", serverName, toolName, res.Content)
	}
	return res.Content, nil
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
