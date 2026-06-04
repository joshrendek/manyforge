package agents

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/manyforge/manyforge/internal/platform/mcp"
)

// fakeMCPServerResolver is a lightweight fake for mcpServerResolver that
// returns a pre-populated list without touching the database.
type fakeMCPServerResolver struct {
	servers []ResolvedMCPServer
	err     error
}

func (f *fakeMCPServerResolver) ListEnabledForAgent(_ context.Context, _, _ uuid.UUID, _ []uuid.UUID) ([]ResolvedMCPServer, error) {
	return f.servers, f.err
}

// ResolveEnabledByName returns the first server in the list whose Name matches,
// mirroring how the production MCPServerService would do an indexed lookup.
func (f *fakeMCPServerResolver) ResolveEnabledByName(_ context.Context, _, _ uuid.UUID, name string) (ResolvedMCPServer, error) {
	if f.err != nil {
		return ResolvedMCPServer{}, f.err
	}
	for _, s := range f.servers {
		if s.Name == name {
			return s, nil
		}
	}
	return ResolvedMCPServer{}, errors.New("mcp server not found: " + name)
}

// buildMCPHost constructs an MCPHost whose Connect factory maps serverURL →
// the provided MockClient. Servers not in the map get a nil return (callers
// must only request servers present in the map).
func buildMCPHost(clients map[string]*mcp.MockClient, resolver mcpServerResolver) *MCPHost {
	return &MCPHost{
		Servers: resolver,
		Connect: func(serverURL, _ string) mcp.ClientLike {
			return clients[serverURL]
		},
		Logger: slog.Default(),
	}
}

// TestMCPHost_DiscoverTools_Namespacing verifies that discovered tools carry
// the correct name, Effect, and RequiredPerm.
func TestMCPHost_DiscoverTools_Namespacing(t *testing.T) {
	serverID := uuid.New()
	server := ResolvedMCPServer{
		ID:   serverID,
		Name: "crm",
		URL:  "http://crm.local",
	}

	defs := []mcp.ToolDef{
		{Name: "get_contact", Description: "fetch a contact", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "update_contact", Description: "update a contact", InputSchema: json.RawMessage(`{"type":"object"}`)},
	}
	mockClient := mcp.NewMockClient(defs, map[string][]mcp.Result{})

	host := buildMCPHost(
		map[string]*mcp.MockClient{"http://crm.local": mockClient},
		&fakeMCPServerResolver{servers: []ResolvedMCPServer{server}},
	)

	tools, err := host.DiscoverTools(context.Background(), uuid.New(), uuid.New(), []uuid.UUID{serverID})
	if err != nil {
		t.Fatalf("DiscoverTools: %v", err)
	}
	if len(tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(tools))
	}

	for _, tool := range tools {
		// Namespacing: "mcp:<server>:<tool>"
		if tool.Name != "mcp:crm:get_contact" && tool.Name != "mcp:crm:update_contact" {
			t.Errorf("unexpected tool name %q", tool.Name)
		}
		// Classification
		if tool.Effect != EffectExternal {
			t.Errorf("tool %q: Effect=%v, want EffectExternal", tool.Name, tool.Effect)
		}
		if tool.RequiredPerm != "mcp.invoke" {
			t.Errorf("tool %q: RequiredPerm=%q, want mcp.invoke", tool.Name, tool.RequiredPerm)
		}
		// Schema preserved
		if tool.SchemaJSON == "" {
			t.Errorf("tool %q: SchemaJSON is empty", tool.Name)
		}
		// Invoke must be set
		if tool.Invoke == nil {
			t.Errorf("tool %q: Invoke is nil", tool.Name)
		}
	}
}

// TestMCPHost_DiscoverTools_InvokeCallsTool verifies that the Invoke closure
// calls MockClient.CallTool with the real (non-namespaced) tool name and
// the idempotency hint from ctx.
func TestMCPHost_DiscoverTools_InvokeCallsTool(t *testing.T) {
	serverID := uuid.New()
	server := ResolvedMCPServer{
		ID:   serverID,
		Name: "erp",
		URL:  "http://erp.local",
	}

	defs := []mcp.ToolDef{
		{Name: "lookup_order", Description: "look up order", InputSchema: json.RawMessage(`{}`)},
	}
	expectedResult := mcp.Result{Content: `{"order_id":"abc"}`, IsError: false}
	mockClient := mcp.NewMockClient(defs, map[string][]mcp.Result{
		"lookup_order": {expectedResult},
	})

	host := buildMCPHost(
		map[string]*mcp.MockClient{"http://erp.local": mockClient},
		&fakeMCPServerResolver{servers: []ResolvedMCPServer{server}},
	)

	tools, err := host.DiscoverTools(context.Background(), uuid.New(), uuid.New(), []uuid.UUID{serverID})
	if err != nil {
		t.Fatalf("DiscoverTools: %v", err)
	}
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}

	// Invoke with an idempotency key in context.
	idemKey := uuid.New()
	ctx := withApprovalKey(context.Background(), idemKey)
	args := json.RawMessage(`{"order_id":"123"}`)
	content, invokeErr := tools[0].Invoke(ctx, uuid.New(), uuid.New(), args)
	if invokeErr != nil {
		t.Fatalf("Invoke: %v", invokeErr)
	}
	if content != expectedResult.Content {
		t.Errorf("Invoke content=%q, want %q", content, expectedResult.Content)
	}

	// Verify CallTool was called with the REAL (non-namespaced) name and idem hint.
	calls := mockClient.Calls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 CallTool invocation, got %d", len(calls))
	}
	if calls[0].Name != "lookup_order" {
		t.Errorf("CallTool name=%q, want lookup_order", calls[0].Name)
	}
	if calls[0].IdemHint != idemKey.String() {
		t.Errorf("CallTool IdemHint=%q, want %q", calls[0].IdemHint, idemKey.String())
	}
}

// TestMCPHost_DiscoverTools_FailOpen verifies that a server whose Initialize
// or ListTools fails contributes zero tools; DiscoverTools continues to
// remaining servers and does NOT return an error.
func TestMCPHost_DiscoverTools_FailOpen(t *testing.T) {
	goodServerID := uuid.New()
	badServerID := uuid.New()

	goodServer := ResolvedMCPServer{
		ID:   goodServerID,
		Name: "good",
		URL:  "http://good.local",
	}
	badServer := ResolvedMCPServer{
		ID:   badServerID,
		Name: "bad",
		URL:  "http://bad.local",
	}

	// A client that returns error from Initialize.
	failingClient := &failingMCPClient{failInit: true}

	goodDefs := []mcp.ToolDef{
		{Name: "do_good", Description: "a good tool", InputSchema: json.RawMessage(`{}`)},
	}
	goodClient := mcp.NewMockClient(goodDefs, nil)

	resolver := &fakeMCPServerResolver{
		servers: []ResolvedMCPServer{goodServer, badServer},
	}

	host := &MCPHost{
		Servers: resolver,
		Connect: func(serverURL, _ string) mcp.ClientLike {
			switch serverURL {
			case "http://good.local":
				return goodClient
			case "http://bad.local":
				return failingClient
			}
			return nil
		},
		Logger: slog.Default(),
	}

	tools, err := host.DiscoverTools(context.Background(), uuid.New(), uuid.New(), []uuid.UUID{goodServerID, badServerID})
	if err != nil {
		t.Fatalf("DiscoverTools returned error on fail-open path: %v", err)
	}

	// Only the good server's tools should appear.
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool (from good server), got %d: %v", len(tools), tools)
	}
	if tools[0].Name != "mcp:good:do_good" {
		t.Errorf("unexpected tool name %q", tools[0].Name)
	}
}

// TestMCPHost_DiscoverTools_ListToolsFailOpen verifies fail-open for ListTools errors.
func TestMCPHost_DiscoverTools_ListToolsFailOpen(t *testing.T) {
	serverID := uuid.New()
	server := ResolvedMCPServer{
		ID:   serverID,
		Name: "flaky",
		URL:  "http://flaky.local",
	}

	failClient := &failingMCPClient{failList: true}
	resolver := &fakeMCPServerResolver{servers: []ResolvedMCPServer{server}}

	host := &MCPHost{
		Servers: resolver,
		Connect: func(_, _ string) mcp.ClientLike { return failClient },
		Logger:  slog.Default(),
	}

	tools, err := host.DiscoverTools(context.Background(), uuid.New(), uuid.New(), []uuid.UUID{serverID})
	if err != nil {
		t.Fatalf("DiscoverTools returned error on ListTools fail-open path: %v", err)
	}
	if len(tools) != 0 {
		t.Fatalf("expected 0 tools from failing server, got %d", len(tools))
	}
}

// failingMCPClient is a ClientLike that errors on Initialize or ListTools.
type failingMCPClient struct {
	failInit bool
	failList bool
}

func (f *failingMCPClient) Initialize(_ context.Context) error {
	if f.failInit {
		return errors.New("initialize: connection refused")
	}
	return nil
}

func (f *failingMCPClient) ListTools(_ context.Context) ([]mcp.ToolDef, error) {
	if f.failList {
		return nil, errors.New("list tools: timeout")
	}
	return nil, nil
}

func (f *failingMCPClient) CallTool(_ context.Context, _ string, _ json.RawMessage, _ string) (mcp.Result, error) {
	return mcp.Result{}, errors.New("not implemented")
}

// TestMCPHost_DiscoverTools_MultipleServers verifies tools from multiple servers
// are all returned, correctly namespaced.
func TestMCPHost_DiscoverTools_MultipleServers(t *testing.T) {
	idA := uuid.New()
	idB := uuid.New()

	srvA := ResolvedMCPServer{ID: idA, Name: "alpha", URL: "http://alpha.local"}
	srvB := ResolvedMCPServer{ID: idB, Name: "beta", URL: "http://beta.local"}

	clientA := mcp.NewMockClient([]mcp.ToolDef{
		{Name: "tool1", InputSchema: json.RawMessage(`{}`)},
	}, nil)
	clientB := mcp.NewMockClient([]mcp.ToolDef{
		{Name: "tool2", InputSchema: json.RawMessage(`{}`)},
		{Name: "tool3", InputSchema: json.RawMessage(`{}`)},
	}, nil)

	host := &MCPHost{
		Servers: &fakeMCPServerResolver{servers: []ResolvedMCPServer{srvA, srvB}},
		Connect: func(serverURL, _ string) mcp.ClientLike {
			switch serverURL {
			case "http://alpha.local":
				return clientA
			case "http://beta.local":
				return clientB
			}
			return nil
		},
		Logger: slog.Default(),
	}

	tools, err := host.DiscoverTools(context.Background(), uuid.New(), uuid.New(), []uuid.UUID{idA, idB})
	if err != nil {
		t.Fatalf("DiscoverTools: %v", err)
	}
	if len(tools) != 3 {
		t.Fatalf("expected 3 tools, got %d", len(tools))
	}

	names := map[string]bool{}
	for _, tool := range tools {
		names[tool.Name] = true
		if tool.Effect != EffectExternal {
			t.Errorf("tool %q not External", tool.Name)
		}
		if tool.RequiredPerm != "mcp.invoke" {
			t.Errorf("tool %q RequiredPerm=%q, want mcp.invoke", tool.Name, tool.RequiredPerm)
		}
	}
	for _, want := range []string{"mcp:alpha:tool1", "mcp:beta:tool2", "mcp:beta:tool3"} {
		if !names[want] {
			t.Errorf("missing expected tool %q; got %v", want, names)
		}
	}
}

// TestMCPHost_DiscoverTools_MultipleServers_InvokeBindsDistinctClient is a
// regression guard against loop-variable aliasing in the Invoke closures.
// Each server has its OWN distinct MockClient scripted with its OWN tool name
// and result; invoking each discovered tool must hit ITS OWN client with ITS OWN
// real (non-namespaced) tool name. A reintroduced shared-loop-variable capture
// would route every Invoke to the last server's client/def and fail this test —
// whereas the namespacing-only assertions above would still pass.
func TestMCPHost_DiscoverTools_MultipleServers_InvokeBindsDistinctClient(t *testing.T) {
	idA := uuid.New()
	idB := uuid.New()

	srvA := ResolvedMCPServer{ID: idA, Name: "alpha", URL: "http://alpha.local"}
	srvB := ResolvedMCPServer{ID: idB, Name: "beta", URL: "http://beta.local"}

	// Distinct tool names + distinct scripted results per client.
	clientA := mcp.NewMockClient(
		[]mcp.ToolDef{{Name: "alpha_tool", InputSchema: json.RawMessage(`{}`)}},
		map[string][]mcp.Result{"alpha_tool": {{Content: "from-alpha"}}},
	)
	clientB := mcp.NewMockClient(
		[]mcp.ToolDef{{Name: "beta_tool", InputSchema: json.RawMessage(`{}`)}},
		map[string][]mcp.Result{"beta_tool": {{Content: "from-beta"}}},
	)

	clientsByURL := map[string]*mcp.MockClient{
		"http://alpha.local": clientA,
		"http://beta.local":  clientB,
	}

	host := &MCPHost{
		Servers: &fakeMCPServerResolver{servers: []ResolvedMCPServer{srvA, srvB}},
		Connect: func(serverURL, _ string) mcp.ClientLike {
			return clientsByURL[serverURL]
		},
		Logger: slog.Default(),
	}

	tools, err := host.DiscoverTools(context.Background(), uuid.New(), uuid.New(), []uuid.UUID{idA, idB})
	if err != nil {
		t.Fatalf("DiscoverTools: %v", err)
	}
	if len(tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(tools))
	}

	// Index the discovered tools by their namespaced name so we can invoke each.
	byName := map[string]Tool{}
	for _, tool := range tools {
		byName[tool.Name] = tool
	}

	// Each case: invoke the namespaced tool, assert the expected content, then
	// assert the matching mock received exactly one call with the REAL tool name.
	cases := []struct {
		namespaced string
		realName   string
		wantBody   string
		client     *mcp.MockClient
	}{
		{"mcp:alpha:alpha_tool", "alpha_tool", "from-alpha", clientA},
		{"mcp:beta:beta_tool", "beta_tool", "from-beta", clientB},
	}

	for _, tc := range cases {
		tool, ok := byName[tc.namespaced]
		if !ok {
			t.Fatalf("discovered tool %q missing; got %v", tc.namespaced, byName)
		}
		out, invokeErr := tool.Invoke(context.Background(), uuid.New(), uuid.New(), json.RawMessage(`{}`))
		if invokeErr != nil {
			t.Fatalf("Invoke %q: %v", tc.namespaced, invokeErr)
		}
		if out != tc.wantBody {
			t.Errorf("Invoke %q content=%q, want %q (closure bound to wrong client?)", tc.namespaced, out, tc.wantBody)
		}
	}

	// Each client must have received exactly ITS OWN tool call — proving the
	// closures did not alias a shared loop variable to one server.
	callsA := clientA.Calls()
	if len(callsA) != 1 || callsA[0].Name != "alpha_tool" {
		t.Errorf("clientA calls = %+v, want exactly one call to alpha_tool", callsA)
	}
	callsB := clientB.Calls()
	if len(callsB) != 1 || callsB[0].Name != "beta_tool" {
		t.Errorf("clientB calls = %+v, want exactly one call to beta_tool", callsB)
	}
}

// TestMCPHost_InvokeMCPTool_Success verifies the happy path: parsing the
// namespace, resolving by name, connecting, calling the tool, and returning
// the content. The approval id is passed as idemHint.
func TestMCPHost_InvokeMCPTool_Success(t *testing.T) {
	serverID := uuid.New()
	server := ResolvedMCPServer{
		ID:   serverID,
		Name: "erp",
		URL:  "http://erp.local",
	}
	toolDefs := []mcp.ToolDef{{Name: "get_order", InputSchema: json.RawMessage(`{}`)}}
	mockClient := mcp.NewMockClient(toolDefs, map[string][]mcp.Result{
		"get_order": {{Content: `{"order":"42"}`, IsError: false}},
	})
	host := buildMCPHost(
		map[string]*mcp.MockClient{"http://erp.local": mockClient},
		&fakeMCPServerResolver{servers: []ResolvedMCPServer{server}},
	)

	idemHint := uuid.New().String()
	out, err := host.InvokeMCPTool(context.Background(), uuid.New(), uuid.New(), "mcp:erp:get_order", json.RawMessage(`{}`), idemHint)
	if err != nil {
		t.Fatalf("InvokeMCPTool: %v", err)
	}
	if out != `{"order":"42"}` {
		t.Errorf("InvokeMCPTool content=%q, want {\"order\":\"42\"}", out)
	}

	calls := mockClient.Calls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 CallTool call, got %d", len(calls))
	}
	if calls[0].Name != "get_order" {
		t.Errorf("CallTool name=%q, want get_order", calls[0].Name)
	}
	if calls[0].IdemHint != idemHint {
		t.Errorf("CallTool IdemHint=%q, want %q", calls[0].IdemHint, idemHint)
	}
}

// TestMCPHost_InvokeMCPTool_MalformedName verifies that a malformed tool name
// (missing server or tool segment) returns an error without touching the server.
func TestMCPHost_InvokeMCPTool_MalformedName(t *testing.T) {
	host := buildMCPHost(nil, &fakeMCPServerResolver{})

	cases := []string{
		"mcp:",         // no server or tool
		"mcp::",        // empty server
		"mcp:srv:",     // empty tool
		"notmcp:s:t",   // wrong prefix
		"mcp:only-one", // only one colon
	}
	for _, tc := range cases {
		_, err := host.InvokeMCPTool(context.Background(), uuid.New(), uuid.New(), tc, nil, "")
		if err == nil {
			t.Errorf("InvokeMCPTool(%q): want error for malformed name, got nil", tc)
		}
	}
}

// TestMCPHost_InvokeMCPTool_UnknownServer verifies that an unknown server name
// (not visible under RLS) returns an error.
func TestMCPHost_InvokeMCPTool_UnknownServer(t *testing.T) {
	host := buildMCPHost(nil, &fakeMCPServerResolver{servers: []ResolvedMCPServer{}})
	_, err := host.InvokeMCPTool(context.Background(), uuid.New(), uuid.New(), "mcp:nonexistent:some_tool", nil, "")
	if err == nil {
		t.Fatal("InvokeMCPTool with unknown server: want error, got nil")
	}
}

// TestMCPHost_InvokeMCPTool_IsError verifies that a tool call where IsError=true
// is surfaced as an error from InvokeMCPTool.
func TestMCPHost_InvokeMCPTool_IsError(t *testing.T) {
	server := ResolvedMCPServer{ID: uuid.New(), Name: "svc", URL: "http://svc.local"}
	toolDefs := []mcp.ToolDef{{Name: "fail_tool", InputSchema: json.RawMessage(`{}`)}}
	mockClient := mcp.NewMockClient(toolDefs, map[string][]mcp.Result{
		"fail_tool": {{Content: "something went wrong", IsError: true}},
	})
	host := buildMCPHost(
		map[string]*mcp.MockClient{"http://svc.local": mockClient},
		&fakeMCPServerResolver{servers: []ResolvedMCPServer{server}},
	)
	_, err := host.InvokeMCPTool(context.Background(), uuid.New(), uuid.New(), "mcp:svc:fail_tool", json.RawMessage(`{}`), "")
	if err == nil {
		t.Fatal("InvokeMCPTool with IsError tool result: want error, got nil")
	}
}
