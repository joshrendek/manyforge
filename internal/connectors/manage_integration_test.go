//go:build integration

package connectors

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/errs"
)

// startConn / newConnService / seed are the shared connectors integration helpers
// (see internal/connectors/inbound_definer_integration_test.go). jiraInput() builds a
// valid CreateConnectorInput. If a helper name differs, align to the existing harness.

func TestManageListAndGet(t *testing.T) {
	ctx, tdb, seed := startConn(t)
	svc := newConnService(t, tdb, nil) // nil Verifier = skip live verify

	id, err := svc.Create(ctx, seed.principalID, seed.businessID, jiraInput())
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// List returns the connector with health, no credential.
	views, err := svc.List(ctx, seed.principalID, seed.businessID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(views) != 1 || views[0].ID != id.String() {
		t.Fatalf("list: want [%s], got %+v", id, views)
	}
	if views[0].Health.State != "healthy" {
		t.Fatalf("list: want health=healthy, got %q", views[0].Health.State)
	}

	// Get returns the same view.
	v, err := svc.Get(ctx, seed.principalID, seed.businessID, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if v.ID != id.String() || v.Type != "jira" {
		t.Fatalf("get: unexpected view %+v", v)
	}

	// Get with an unknown id → ErrNotFound (no oracle).
	if _, err := svc.Get(ctx, seed.principalID, seed.businessID, uuid.New()); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("get unknown: want ErrNotFound, got %v", err)
	}
}

func TestManageUpdate(t *testing.T) {
	ctx, tdb, seed := startConn(t)
	svc := newConnService(t, tdb, nil)
	id, err := svc.Create(ctx, seed.principalID, seed.businessID, jiraInput())
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Rename + disable; config omitted (preserved).
	newName := "Acme Jira (prod)"
	disabled := "disabled"
	v, err := svc.Update(ctx, seed.principalID, seed.businessID, id, UpdateConnectorInput{
		DisplayName: &newName, Status: &disabled,
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if v.DisplayName != newName || v.Status != "disabled" {
		t.Fatalf("update: got name=%q status=%q", v.DisplayName, v.Status)
	}
	if v.Health.State != "disabled" {
		t.Fatalf("update: want health=disabled, got %q", v.Health.State)
	}

	// Empty display_name → validation error, nothing persisted.
	empty := ""
	if _, err := svc.Update(ctx, seed.principalID, seed.businessID, id, UpdateConnectorInput{DisplayName: &empty}); !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("update empty name: want ErrValidation, got %v", err)
	}

	// Bad status value → validation error.
	bad := "paused"
	if _, err := svc.Update(ctx, seed.principalID, seed.businessID, id, UpdateConnectorInput{Status: &bad}); !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("update bad status: want ErrValidation, got %v", err)
	}

	// Unknown id → ErrNotFound.
	if _, err := svc.Update(ctx, seed.principalID, seed.businessID, uuid.New(), UpdateConnectorInput{DisplayName: &newName}); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("update unknown: want ErrNotFound, got %v", err)
	}
}

func TestManageRotateCredential(t *testing.T) {
	ctx, tdb, seed := startConn(t)
	svc := newConnService(t, tdb, nil) // nil Verifier: rotation skips live verify (mirrors Create)
	id, err := svc.Create(ctx, seed.principalID, seed.businessID, jiraInput())
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Capture the original secret_ref to prove it is swapped + the old secret deleted.
	oldRef := connectorSecretRef(t, ctx, tdb, seed.businessID, id)

	if err := svc.RotateCredential(ctx, seed.principalID, seed.businessID, id, RotateCredentialInput{
		Email: "rotated@acme.test", APIToken: "new-token-xyz", WebhookSecret: "new-webhook-secret",
	}); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	newRef := connectorSecretRef(t, ctx, tdb, seed.businessID, id)
	if newRef == oldRef {
		t.Fatal("rotate: secret_ref was not swapped")
	}
	// Old secret row must be gone.
	if secretExists(t, ctx, tdb, oldRef) {
		t.Fatal("rotate: old secret was not deleted")
	}
	// The resolved credential must be the new one.
	rc, err := svc.Resolve(ctx, seed.principalID, seed.businessID, id)
	if err != nil {
		t.Fatalf("resolve after rotate: %v", err)
	}
	if rc.Credential.APIToken != "new-token-xyz" {
		t.Fatalf("rotate: resolved token = %q, want new-token-xyz", rc.Credential.APIToken)
	}

	// Empty api_token → validation, nothing changed.
	if err := svc.RotateCredential(ctx, seed.principalID, seed.businessID, id, RotateCredentialInput{Email: "x@y.z", APIToken: ""}); !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("rotate empty token: want ErrValidation, got %v", err)
	}

	// Unknown id → ErrNotFound.
	if err := svc.RotateCredential(ctx, seed.principalID, seed.businessID, uuid.New(), RotateCredentialInput{Email: "x@y.z", APIToken: "t"}); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("rotate unknown: want ErrNotFound, got %v", err)
	}
}

func connectorSecretRef(t *testing.T, ctx context.Context, tdb *testdb.TestDB, businessID, connectorID uuid.UUID) uuid.UUID {
	t.Helper()
	var ref uuid.UUID
	if err := tdb.Super.QueryRow(ctx, "SELECT secret_ref FROM connector WHERE id=$1 AND business_id=$2", connectorID, businessID).Scan(&ref); err != nil {
		t.Fatalf("read secret_ref: %v", err)
	}
	return ref
}

func secretExists(t *testing.T, ctx context.Context, tdb *testdb.TestDB, secretID uuid.UUID) bool {
	t.Helper()
	var n int
	if err := tdb.Super.QueryRow(ctx, "SELECT count(*) FROM secret WHERE id=$1", secretID).Scan(&n); err != nil {
		t.Fatalf("count secret: %v", err)
	}
	return n > 0
}

func TestManageTest(t *testing.T) {
	ctx, tdb, seed := startConn(t)

	// With a stub Verifier that returns nil → ok=true.
	okSvc := newConnService(t, tdb, stubVerifier{})
	id, err := okSvc.Create(ctx, seed.principalID, seed.businessID, jiraInput())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	res, err := okSvc.Test(ctx, seed.principalID, seed.businessID, id)
	if err != nil {
		t.Fatalf("test: %v", err)
	}
	if !res.OK {
		t.Fatalf("test: want ok=true, got %+v", res)
	}

	// Unknown id → ErrNotFound (the connector must resolve first).
	if _, err := okSvc.Test(ctx, seed.principalID, seed.businessID, uuid.New()); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("test unknown: want ErrNotFound, got %v", err)
	}
}

type stubVerifier struct{}

func (stubVerifier) Verify(ctx context.Context, t VerifyTarget) error { return nil }
