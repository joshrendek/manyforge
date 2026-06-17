// Finding: manyforge-xxe / US3 Task 4 (M2) — permission keys are referenced from Go (the
// RBAC middleware wiring in cmd/manyforge and the agent tool registry in internal/agents)
// AND seeded by SQL migrations. internal/authz now exports Perm* constants so a Go-side
// rename fails at COMPILE time instead of silently mis-gating. This source-level pin (no
// build tag → runs under `make test`/`make sec-test`) asserts each constant's string value
// is actually seeded in a migration, so a Go-side rename that drifts from the SQL catalog
// fails CI loudly rather than producing a permission that gates nothing.
package security_regression

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/manyforge/manyforge/internal/authz"
)

func TestPin_PermConstantsMatchSeededCatalog(t *testing.T) {
	const dir = "../../migrations"
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}
	var sb strings.Builder
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".up.sql") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		sb.Write(b)
	}
	migrations := sb.String()

	// Every exported permission constant must be a seeded permission.key ('<key>').
	keys := []string{
		authz.PermTicketsRead, authz.PermTicketsReply, authz.PermTicketsWrite,
		authz.PermTicketsAssign, authz.PermTicketsDelete, authz.PermInboxManage,
		authz.PermAgentsConfigure, authz.PermAgentsRun, authz.PermAgentsApprove,
		authz.PermConnectorsRead, authz.PermConnectorsWrite, authz.PermConnectorsManage,
		authz.PermCRMRead, authz.PermCRMWrite,
	}
	for _, k := range keys {
		if !strings.Contains(migrations, "'"+k+"'") {
			t.Errorf("permission key %q is not seeded in any migration — authz.Perm* drifted from the SQL catalog (manyforge-xxe)", k)
		}
	}
}
