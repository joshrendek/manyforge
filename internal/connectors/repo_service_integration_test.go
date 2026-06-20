//go:build integration

package connectors

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/secrets"
)

func newRepoConnService(t *testing.T, tdb *testdb.TestDB) *RepoConnectorService {
	t.Helper()
	return &RepoConnectorService{
		DB:    tdb.App,
		Vault: secrets.NewVault(newTestSealer(t)),
	}
}

func startRepo(t *testing.T) (context.Context, *testdb.TestDB, connSeed) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	return ctx, tdb, seedConnectorTenant(ctx, t, tdb)
}

func repoInput() CreateRepoConnectorInput {
	return CreateRepoConnectorInput{
		Type:        "github",
		DisplayName: "Acme GitHub",
		BaseURL:     "https://github.com",
		Repo:        "acme/backend",
		APIToken:    "ghp_abc123",
	}
}

// TestRepoConnectorCreateReturnsID asserts Create returns a non-nil UUID.
func TestRepoConnectorCreateReturnsID(t *testing.T) {
	ctx, tdb, seed := startRepo(t)
	svc := newRepoConnService(t, tdb)

	id, err := svc.Create(ctx, seed.principalID, seed.businessID, repoInput())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if id.String() == "00000000-0000-0000-0000-000000000000" {
		t.Fatal("Create returned nil UUID")
	}
}

// TestRepoConnectorRoundTrip asserts Resolve returns the same Repo, BaseURL, and APIToken
// that were stored by Create.
func TestRepoConnectorRoundTrip(t *testing.T) {
	ctx, tdb, seed := startRepo(t)
	svc := newRepoConnService(t, tdb)

	in := repoInput()
	id, err := svc.Create(ctx, seed.principalID, seed.businessID, in)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	rc, err := svc.Resolve(ctx, seed.principalID, seed.businessID, id)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if rc.Repo != in.Repo {
		t.Errorf("Repo: want %q, got %q", in.Repo, rc.Repo)
	}
	if rc.BaseURL != in.BaseURL {
		t.Errorf("BaseURL: want %q, got %q", in.BaseURL, rc.BaseURL)
	}
	if rc.Credential.APIToken != in.APIToken {
		t.Errorf("APIToken: want %q, got %q", in.APIToken, rc.Credential.APIToken)
	}
	if rc.Type != in.Type {
		t.Errorf("Type: want %q, got %q", in.Type, rc.Type)
	}
}

// TestRepoConnectorRLSIsolation asserts that Resolve under a DIFFERENT business's
// principal returns ErrNotFound — RLS isolation, no 403/404 oracle.
func TestRepoConnectorRLSIsolation(t *testing.T) {
	ctx, tdb, a := startRepo(t)
	b := seedConnectorTenant(ctx, t, tdb) // independent tenant, same DB

	svcA := newRepoConnService(t, tdb)
	svcB := &RepoConnectorService{
		DB:    tdb.App,
		Vault: secrets.NewVault(newTestSealer(t)),
	}

	id, err := svcA.Create(ctx, a.principalID, a.businessID, repoInput())
	if err != nil {
		t.Fatalf("Create (tenant A): %v", err)
	}

	// Tenant B principal attempting to resolve tenant A's connector must get ErrNotFound.
	_, err = svcB.Resolve(ctx, b.principalID, b.businessID, id)
	if !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("want ErrNotFound for cross-tenant resolve, got %v", err)
	}
}
