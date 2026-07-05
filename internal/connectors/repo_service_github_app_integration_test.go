//go:build integration

package connectors

// TestResolveGithubAppReturnsMetadataNoMint: an app-backed repo connector (type=
// 'github_app', secret_ref NULL, config carries installation_id) is seeded directly
// via the super pool — Create requires a PAT and can't produce this shape. Resolve
// must return connector metadata WITHOUT ever calling Vault.Open (Vault is nil here;
// a nil-deref would fail the test if the github_app branch fell through to the
// github credential-unseal path). Delete must reject github_app connectors —
// they're managed by the GitHub App install, not by this API.

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/errs"
)

func TestResolveGithubAppReturnsMetadataNoMint(t *testing.T) {
	ctx, tdb, seed := startRepo(t)

	connID := uuid.New()
	mustExecSuper(t, ctx, tdb.Super, `
		INSERT INTO repo_connector (id, business_id, tenant_root_id, type, display_name, base_url,
			repo, secret_ref, config, status)
		SELECT $1, b.id, b.tenant_root_id, 'github_app', 'owner/name', 'https://api.github.com',
			'owner/name', NULL, '{"installation_id": 77}'::jsonb, 'enabled'
		FROM business b WHERE b.id = $2`,
		connID, seed.businessID)

	// Vault is deliberately nil: the github_app Resolve branch must never call it.
	svc := &RepoConnectorService{DB: tdb.App, Vault: nil}

	rc, err := svc.Resolve(ctx, seed.principalID, seed.businessID, connID)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if rc.Type != "github_app" {
		t.Errorf("Type = %q, want github_app", rc.Type)
	}
	if rc.Credential.APIToken != "" {
		t.Errorf("Credential.APIToken = %q, want empty (no stored PAT)", rc.Credential.APIToken)
	}
	if rc.Config["installation_id"] == nil {
		t.Fatalf("Config missing installation_id: %+v", rc.Config)
	}

	if err := svc.Delete(ctx, seed.principalID, seed.businessID, connID); !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("Delete(github_app) = %v, want ErrValidation", err)
	}
}
