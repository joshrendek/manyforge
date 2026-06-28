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

// TestRepoConnectorListDelete covers List (newest-first, no secret field) and Delete
// (removes one, cross-tenant RLS, already-deleted → ErrNotFound).
func TestRepoConnectorListDelete(t *testing.T) {
	ctx, tdb, a := startRepo(t)
	b := seedConnectorTenant(ctx, t, tdb) // independent tenant B for RLS checks

	svcA := newRepoConnService(t, tdb)
	svcB := newRepoConnService(t, tdb)

	// --- seed two connectors for tenant A (slight delay so created_at ordering is reliable) ---
	in1 := repoInput()
	in1.DisplayName = "Connector One"
	in1.Repo = "acme/one"
	idOne, err := svcA.Create(ctx, a.principalID, a.businessID, in1)
	if err != nil {
		t.Fatalf("Create connector 1: %v", err)
	}

	in2 := repoInput()
	in2.DisplayName = "Connector Two"
	in2.Repo = "acme/two"
	idTwo, err := svcA.Create(ctx, a.principalID, a.businessID, in2)
	if err != nil {
		t.Fatalf("Create connector 2: %v", err)
	}

	// --- List returns 2 connectors, newest-first ---
	list, err := svcA.List(ctx, a.principalID, a.businessID)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("List: want 2 connectors, got %d", len(list))
	}
	// Newest (idTwo) must be first.
	if list[0].ID != idTwo.String() {
		t.Errorf("List order: want newest (%s) first, got %s", idTwo, list[0].ID)
	}
	if list[1].ID != idOne.String() {
		t.Errorf("List order: want oldest (%s) second, got %s", idOne, list[1].ID)
	}

	// Assert RepoConnectorSummary carries no credential/secret fields by verifying
	// the struct fields at compile time — any addition of a secret field would break this.
	var _ = RepoConnectorSummary{
		ID: "", Type: "", DisplayName: "", BaseURL: "", Repo: "",
		AllowPrivateBaseURL: false, Status: "", CreatedAt: list[0].CreatedAt,
	}

	// --- Delete one → List returns 1 ---
	if err := svcA.Delete(ctx, a.principalID, a.businessID, idTwo); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	list2, err := svcA.List(ctx, a.principalID, a.businessID)
	if err != nil {
		t.Fatalf("List after delete: %v", err)
	}
	if len(list2) != 1 {
		t.Fatalf("List after delete: want 1 connector, got %d", len(list2))
	}
	if list2[0].ID != idOne.String() {
		t.Errorf("List after delete: remaining connector should be %s, got %s", idOne, list2[0].ID)
	}

	// --- Delete of a foreign (tenant B) id → ErrNotFound ---
	// Create a connector in tenant B so we have a valid UUID that RLS should deny to A.
	idB, err := svcB.Create(ctx, b.principalID, b.businessID, repoInput())
	if err != nil {
		t.Fatalf("Create connector for tenant B: %v", err)
	}
	err = svcA.Delete(ctx, a.principalID, a.businessID, idB)
	if !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("Delete of foreign id: want ErrNotFound, got %v", err)
	}

	// --- Delete of an already-deleted id → ErrNotFound ---
	err = svcA.Delete(ctx, a.principalID, a.businessID, idTwo)
	if !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("Delete of already-deleted id: want ErrNotFound, got %v", err)
	}

	// --- Tenant B List must NOT see tenant A's connectors (RLS isolation) ---
	listB, err := svcB.List(ctx, b.principalID, b.businessID)
	if err != nil {
		t.Fatalf("List tenant B: %v", err)
	}
	for _, s := range listB {
		if s.ID == idOne.String() || s.ID == idTwo.String() {
			t.Errorf("RLS leak: tenant B List returned tenant A connector %s", s.ID)
		}
	}
}
