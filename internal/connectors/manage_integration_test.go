//go:build integration

package connectors

import (
	"errors"
	"testing"

	"github.com/google/uuid"

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
