// No build tag: these source-level pins run in `make test` and `make sec-test` with NO
// infrastructure. They make a refactor that silently drops a US6 protection fail the
// security gate loudly, complementing the behavioral integration tests in internal/agents/.
//
// US6 contract: Spec 003 design §3.5 — MCP host integration; per-business server registry
// with RLS; External-only tool classification; netsafe SSRF protection; loopback blocked by
// default; sealed bearer tokens never returned in API responses; mcp.invoke granted to
// agent_runtime (not in the agent-guard forbidden set); executor MCP routing.
//
// Finding ID: MF-003-US6-MCP

package security_regression

import (
	"strings"
	"testing"
)

// TestPin_MCPServerRLS pins the RLS invariant on the mcp_server table (migration 0036).
// Removing ROW LEVEL SECURITY or weakening the policy would expose one tenant's MCP server
// credentials to every other tenant in the database.
func TestPin_MCPServerRLS(t *testing.T) {
	mig := mustRead(t, "../../migrations/0036_mcp_server.up.sql")
	for _, frag := range []string{
		"ENABLE ROW LEVEL SECURITY",
		"CREATE POLICY mcp_server_rls",
		"authorized_businesses(current_principal())",
	} {
		if !strings.Contains(mig, frag) {
			t.Errorf("0036 up: missing RLS fragment %q — dropping this weakens tenant isolation on mcp_server", frag)
		}
	}
}

// TestPin_MCPToolsDefaultExternal pins the fail-closed default: a discovered MCP tool starts at
// EffectExternal and requires mcp.invoke. manyforge-k0d lets an explicit per-business policy
// promote a tool to Read/Reversible, so the assertion is now "default is External (the var init),
// override is explicit" — never "every MCP tool is unconditionally External".
func TestPin_MCPToolsDefaultExternal(t *testing.T) {
	host := mustRead(t, "../agents/mcp_host.go")
	for _, frag := range []string{
		`effect := EffectExternal`,   // the fail-closed default
		`RequiredPerm: "mcp.invoke"`, // stays RBAC-gated
		`Effect:       effect`,       // the tool takes the (defaulted/overridden) effect
	} {
		if !strings.Contains(host, frag) {
			t.Errorf("mcp_host.go: missing fragment %q — MCP tools must default to External and require mcp.invoke", frag)
		}
	}
}

// TestPin_MCPClientNetsafe pins that the MCP HTTP client in main.go is built via
// netsafe.NewClientWithOptions (not a bare http.Client). A bare client would allow
// agents to use MCP calls as an SSRF vector targeting loopback or cloud-metadata IPs.
func TestPin_MCPClientNetsafe(t *testing.T) {
	main := mustRead(t, "../../cmd/manyforge/main.go")
	if !strings.Contains(main, "netsafe.NewClientWithOptions") {
		t.Error("main.go: MCP HTTP client must be built via netsafe.NewClientWithOptions — a bare http.Client allows SSRF via MCP endpoints")
	}
}

// TestPin_MCPLoopbackDefaultOff pins that MANYFORGE_MCP_ALLOW_LOOPBACK defaults to false
// (config.go). A default of true would enable loopback connections to cloud-metadata and
// internal services in production unless the operator explicitly sets the env var.
func TestPin_MCPLoopbackDefaultOff(t *testing.T) {
	cfg := mustRead(t, "../../internal/platform/config/config.go")
	if !strings.Contains(cfg, `envBool("MANYFORGE_MCP_ALLOW_LOOPBACK", false)`) {
		t.Error(`config.go: MANYFORGE_MCP_ALLOW_LOOPBACK must default to false — removing the false default enables loopback SSRF in production`)
	}
}

// TestPin_MCPInvokeGrantedToAgentRuntime pins that migration 0037 inserts the mcp.invoke
// permission AND grants it to the agent_runtime preset role. Without this grant, agents
// cannot pass the RBAC gate and MCP tool calls fail even when properly approved.
func TestPin_MCPInvokeGrantedToAgentRuntime(t *testing.T) {
	mig := mustRead(t, "../../migrations/0037_mcp_invoke_perm.up.sql")
	for _, frag := range []string{
		`'mcp.invoke'`,
		`r.key = 'agent_runtime'`,
	} {
		if !strings.Contains(mig, frag) {
			t.Errorf("0037 up: missing fragment %q — mcp.invoke must be granted to agent_runtime so MCP tools pass RBAC", frag)
		}
	}
}

// TestPin_MCPInvokeNotForbidden pins that mcp.invoke is NOT in the agent-guard forbidden
// set (migration 0033). Adding mcp.invoke to the forbidden set would silently revoke MCP
// access from all agents by preventing agent principals from holding any role that includes
// it — this would be a silent regression, not a security improvement.
func TestPin_MCPInvokeNotForbidden(t *testing.T) {
	guard := mustRead(t, "../../migrations/0033_agent_guard_forbid_approve.up.sql")
	// The forbidden set is the IN (...) literal inside membership_agent_guard.
	// Assert mcp.invoke does NOT appear in that literal.
	if strings.Contains(guard, "'mcp.invoke'") {
		t.Error("0033 up: mcp.invoke must NOT appear in the agent-guard forbidden set — adding it would silently revoke MCP access from all agents")
	}
	// Confirm the guard is still present and the expected approved-perm is forbidden.
	if !strings.Contains(guard, "'agents.approve'") {
		t.Error("0033 up: agents.approve must be in the agent-guard forbidden set — this is the separation-of-duties gate")
	}
}

// TestPin_MCPSealedAuthNeverInResponse pins that the mcpServerResp struct and toMCPServerResp
// mapper in mcp_server_handler.go do NOT contain sealed_auth_ref or auth_token fields.
// Auth is write-only: leaking the sealed blob in a GET/LIST response would expose
// (sealed) bearer tokens to any caller with agents.configure, enabling token extraction.
func TestPin_MCPSealedAuthNeverInResponse(t *testing.T) {
	handler := mustRead(t, "../agents/mcp_server_handler.go")

	// Extract the mcpServerResp struct body (from the type declaration to the closing brace).
	structStart := strings.Index(handler, "type mcpServerResp struct {")
	if structStart < 0 {
		t.Fatal("mcp_server_handler.go: mcpServerResp struct not found")
	}
	structEnd := strings.Index(handler[structStart:], "\n}")
	if structEnd < 0 {
		t.Fatal("mcp_server_handler.go: could not locate end of mcpServerResp struct")
	}
	respStruct := handler[structStart : structStart+structEnd]

	// Extract the toMCPServerResp function body.
	mapperStart := strings.Index(handler, "func toMCPServerResp(")
	if mapperStart < 0 {
		t.Fatal("mcp_server_handler.go: toMCPServerResp not found")
	}
	mapperEnd := strings.Index(handler[mapperStart:], "\n}")
	if mapperEnd < 0 {
		t.Fatal("mcp_server_handler.go: could not locate end of toMCPServerResp")
	}
	respMapper := handler[mapperStart : mapperStart+mapperEnd]

	for _, forbidden := range []string{"sealed_auth_ref", "auth_token", "AuthToken", "SealedAuth"} {
		if strings.Contains(respStruct, forbidden) {
			t.Errorf("mcp_server_handler.go mcpServerResp struct: must NOT contain %q — leaking sealed auth in responses exposes bearer tokens", forbidden)
		}
		if strings.Contains(respMapper, forbidden) {
			t.Errorf("mcp_server_handler.go toMCPServerResp: must NOT contain %q — leaking sealed auth in responses exposes bearer tokens", forbidden)
		}
	}
}

// TestPin_MCPSealedAuthNotInOpenAPIResponse pins the SAME write-only-auth invariant at the
// API CONTRACT level: the OpenAPI MCPServer / MCPServerList RESPONSE schemas must not expose
// auth_token or sealed_auth_ref. The request schemas (CreateMCPServerRequest /
// UpdateMCPServerRequest) legitimately carry auth_token with writeOnly: true, so the
// extraction window is scoped strictly to the response schemas (from "    MCPServer:" up to
// the next sibling "    CreateMCPServerRequest:" key) — it must NOT span the request schemas.
func TestPin_MCPSealedAuthNotInOpenAPIResponse(t *testing.T) {
	spec := mustRead(t, "../../specs/003-agent-runtime/contracts/openapi.yaml")

	// Response schemas are MCPServer and MCPServerList (which $refs MCPServer). They are
	// declared consecutively under components.schemas, immediately followed by the first
	// REQUEST schema, CreateMCPServerRequest. Extract [MCPServer: , CreateMCPServerRequest:)
	// so the window covers both response schemas and excludes the writeOnly request schemas.
	respStart := strings.Index(spec, "\n    MCPServer:\n")
	if respStart < 0 {
		t.Fatal("openapi.yaml: '    MCPServer:' response schema not found")
	}
	reqStart := strings.Index(spec[respStart:], "\n    CreateMCPServerRequest:\n")
	if reqStart < 0 {
		t.Fatal("openapi.yaml: '    CreateMCPServerRequest:' (the sibling key that bounds the response block) not found")
	}
	respBlock := spec[respStart : respStart+reqStart]

	// Sanity-check the window actually contains the response schemas it claims to scope...
	if !strings.Contains(respBlock, "MCPServerList:") {
		t.Fatal("openapi.yaml: extraction window did not capture MCPServerList — schema layout changed; re-scope the pin")
	}
	// ...and that it stops BEFORE the request schema (so a writeOnly auth_token there can't
	// give this pin a false pass).
	if strings.Contains(respBlock, "CreateMCPServerRequest:") {
		t.Fatal("openapi.yaml: extraction window leaked into the request schemas — re-scope the pin")
	}

	for _, forbidden := range []string{"auth_token", "sealed_auth_ref"} {
		if strings.Contains(respBlock, forbidden) {
			t.Errorf("openapi.yaml MCPServer/MCPServerList response schema: must NOT contain %q — leaking the bearer token in the API contract exposes sealed auth to any reader of a GET/LIST response", forbidden)
		}
	}
}

// TestPin_ExecutorMCPRouting pins that approval_executor.go dispatches tool calls whose
// name starts with "mcp:" through the MCPHost (not the internal ToolRegistry). Removing
// this branch would cause approved MCP tool calls to silently fail as "unknown tool"
// rather than executing the intended external action.
func TestPin_ExecutorMCPRouting(t *testing.T) {
	exec := mustRead(t, "../agents/approval_executor.go")
	if !strings.Contains(exec, `strings.HasPrefix(p.Tool, "mcp:")`) {
		t.Error(`approval_executor.go: must route tool calls starting with "mcp:" through MCPHost — removing this makes approved MCP actions silently fail`)
	}
}
