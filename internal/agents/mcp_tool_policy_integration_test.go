//go:build integration

package agents

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/mcp"
)

// fixedServerResolver returns a single ResolvedMCPServer for discovery (no real network).
type fixedServerResolver struct{ server ResolvedMCPServer }

func (f fixedServerResolver) ListEnabledForAgent(_ context.Context, _, _ uuid.UUID, _ []uuid.UUID) ([]ResolvedMCPServer, error) {
	return []ResolvedMCPServer{f.server}, nil
}
func (f fixedServerResolver) ResolveEnabledByName(_ context.Context, _, _ uuid.UUID, _ string) (ResolvedMCPServer, error) {
	return f.server, nil
}
func (f fixedServerResolver) ResolveEnabledByID(_ context.Context, _, _, _ uuid.UUID) (ResolvedMCPServer, error) {
	return f.server, nil
}

// Discovery applies a Reversible policy to a tool while leaving an unclassified tool External.
func TestToolPolicy_DiscoveryAppliesEffect(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	s := seedRunTenant(ctx, t, tdb)
	serverID := seedMCPServer(ctx, t, tdb, s, "acme", true)

	// Promote "safe_read" → Reversible via the real service (under the owner principal).
	policySvc := &MCPToolPolicyService{DB: tdb.App}
	if _, err := policySvc.Upsert(ctx, s.ownerID, s.businessID, serverID, "safe_read", "reversible"); err != nil {
		t.Fatalf("upsert policy: %v", err)
	}

	// Discover two tools from a mock MCP client: "safe_read" (policied) + "do_thing" (unclassified).
	mockClient := mcp.NewMockClient(
		[]mcp.ToolDef{{Name: "safe_read", Description: "r"}, {Name: "do_thing", Description: "d"}},
		nil,
	)
	host := &MCPHost{
		Servers:  fixedServerResolver{server: ResolvedMCPServer{ID: serverID, Name: "acme", URL: "https://x", AuthHeader: ""}},
		Policies: policySvc,
		Connect:  func(_, _ string) mcp.ClientLike { return mockClient },
		Logger:   slog.Default(),
	}
	tools, _, err := host.DiscoverTools(ctx, s.ownerID, s.businessID, []uuid.UUID{serverID})
	if err != nil {
		t.Fatalf("DiscoverTools: %v", err)
	}
	got := map[string]EffectClass{}
	for _, tl := range tools {
		got[tl.Name] = tl.Effect
	}
	if got["mcp:acme:safe_read"] != EffectReversible {
		t.Errorf("safe_read effect = %d, want Reversible(%d)", got["mcp:acme:safe_read"], EffectReversible)
	}
	if got["mcp:acme:do_thing"] != EffectExternal {
		t.Errorf("do_thing effect = %d, want External(%d) (unclassified default)", got["mcp:acme:do_thing"], EffectExternal)
	}
}

// Deleting the mcp_server cascades its tool policies away.
func TestToolPolicy_CascadeOnServerDelete(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	s := seedRunTenant(ctx, t, tdb)
	serverID := seedMCPServer(ctx, t, tdb, s, "acme", true)
	policySvc := &MCPToolPolicyService{DB: tdb.App}
	if _, err := policySvc.Upsert(ctx, s.ownerID, s.businessID, serverID, "t", "read"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if _, err := tdb.Super.Exec(ctx, `DELETE FROM mcp_server WHERE id=$1`, serverID); err != nil {
		t.Fatalf("delete server: %v", err)
	}
	var n int
	if err := tdb.Super.QueryRow(ctx, `SELECT count(*) FROM mcp_tool_policy WHERE mcp_server_id=$1`, serverID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("policies after server delete = %d, want 0 (cascade)", n)
	}
}

// A foreign/unknown server id is a no-oracle 404 (errs.ErrNotFound) on upsert.
func TestToolPolicy_ForeignServerIsNotFound(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	s := seedRunTenant(ctx, t, tdb)
	policySvc := &MCPToolPolicyService{DB: tdb.App}
	_, err = policySvc.Upsert(ctx, s.ownerID, s.businessID, uuid.New() /* nonexistent */, "t", "read")
	if err == nil {
		t.Fatal("upsert against unknown server: want ErrNotFound, got nil")
	}
}
