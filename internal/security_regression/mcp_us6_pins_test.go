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

	"gopkg.in/yaml.v3"
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

// TestPin_MCPSealedAuthNotInOpenAPIResponse checks the response schema graph,
// including referenced and composed schemas, without depending on YAML layout.
func TestPin_MCPSealedAuthNotInOpenAPIResponse(t *testing.T) {
	raw := mustRead(t, "../../api/openapi.yaml")
	var doc struct {
		Components struct {
			Schemas map[string]any `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("parse canonical OpenAPI: %v", err)
	}
	visited := map[string]bool{}
	var inspect func(any)
	inspect = func(value any) {
		switch node := value.(type) {
		case map[string]any:
			if ref, ok := node["$ref"].(string); ok {
				const prefix = "#/components/schemas/"
				if !strings.HasPrefix(ref, prefix) {
					t.Fatalf("unexpected schema reference %q", ref)
				}
				name := strings.TrimPrefix(ref, prefix)
				schema, ok := doc.Components.Schemas[name]
				if !ok {
					t.Fatalf("unresolved response schema %q", ref)
				}
				if !visited[name] {
					visited[name] = true
					inspect(schema)
				}
			}
			if properties, ok := node["properties"].(map[string]any); ok {
				for _, forbidden := range []string{"auth_token", "sealed_auth_ref"} {
					if _, exists := properties[forbidden]; exists {
						t.Errorf("MCP response schema exposes secret field %q", forbidden)
					}
				}
			}
			for _, child := range node {
				inspect(child)
			}
		case []any:
			for _, child := range node {
				inspect(child)
			}
		}
	}
	for _, name := range []string{"MCPServer", "MCPServerList"} {
		schema, ok := doc.Components.Schemas[name]
		if !ok {
			t.Fatalf("canonical OpenAPI omits response schema %s", name)
		}
		inspect(schema)
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
