package security_regression

import (
	"os"
	"strings"
	"testing"
)

// FINDING: manyforge-1kv — /agents/tools and /agents/models expose the tool and
// model catalogs and MUST stay behind the agents.configure gate (they live in the
// agents handler subtree, which main.go mounts under h.agentsConfigure).

func TestAgentMetadataRoutesRegistered(t *testing.T) {
	src, err := os.ReadFile("../agents/agent_handler.go")
	if err != nil {
		t.Fatalf("read handler: %v", err)
	}
	s := string(src)
	for _, want := range []string{`r.Get("/tools", h.listTools)`, `r.Get("/models", h.listModels)`} {
		if !strings.Contains(s, want) {
			t.Fatalf("agent metadata route missing: %s", want)
		}
	}
}

func TestAgentGroupStaysConfigureGated(t *testing.T) {
	src, err := os.ReadFile("../../cmd/manyforge/main.go")
	if err != nil {
		t.Fatalf("read main: %v", err)
	}
	s := string(src)
	if !strings.Contains(s, "ag.Use(h.agentsConfigure)") || !strings.Contains(s, "h.agents.ProtectedRoutes(ag)") {
		t.Fatal("agents handler group is no longer gated on agents.configure")
	}
}
