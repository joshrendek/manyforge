//go:build integration

package connectors

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

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

// a7j.8: suppress_native_notifications round-trips through Create + Get, and is updatable
// via PATCH with COALESCE preserve semantics (an omitted flag keeps the stored value).
func TestManageSuppressNativeNotifications(t *testing.T) {
	ctx, tdb, seed := startConn(t)
	svc := newConnService(t, tdb, nil)

	// Create with the flag on → Get reflects it.
	in := jiraInput()
	in.SuppressNativeNotifications = true
	id, err := svc.Create(ctx, seed.principalID, seed.businessID, in)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	v, err := svc.Get(ctx, seed.principalID, seed.businessID, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !v.SuppressNativeNotifications {
		t.Fatal("create with suppress=true: Get returned false")
	}

	// PATCH that omits the flag (display_name only) preserves it (COALESCE narg).
	name := "Renamed"
	v, err = svc.Update(ctx, seed.principalID, seed.businessID, id, UpdateConnectorInput{DisplayName: &name})
	if err != nil {
		t.Fatalf("update name: %v", err)
	}
	if !v.SuppressNativeNotifications {
		t.Fatal("update omitting suppress: flag was not preserved")
	}

	// PATCH that sets the flag off → reflected.
	off := false
	v, err = svc.Update(ctx, seed.principalID, seed.businessID, id, UpdateConnectorInput{SuppressNativeNotifications: &off})
	if err != nil {
		t.Fatalf("update suppress off: %v", err)
	}
	if v.SuppressNativeNotifications {
		t.Fatal("update suppress=false: flag still true")
	}

	// Default (omitted at create) is false.
	id2, err := svc.Create(ctx, seed.principalID, seed.businessID, jiraInputForHost("https://other.atlassian.net"))
	if err != nil {
		t.Fatalf("create default: %v", err)
	}
	v2, err := svc.Get(ctx, seed.principalID, seed.businessID, id2)
	if err != nil {
		t.Fatalf("get default: %v", err)
	}
	if v2.SuppressNativeNotifications {
		t.Fatal("default suppress should be false")
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

func TestManageDeleteDetaches(t *testing.T) {
	ctx, tdb, seed := startConn(t)
	svc := newConnService(t, tdb, nil)
	id, err := svc.Create(ctx, seed.principalID, seed.businessID, jiraInput())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	oldRef := connectorSecretRef(t, ctx, tdb, seed.businessID, id)

	// Link a ticket to the connector (simulate a synced ticket) via the inbound DEFINER fn.
	externalID := "JIRA-77"
	var ticketID uuid.UUID
	if err := tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, syncIssueSQL,
			id, externalID, "https://acme.atlassian.net/browse/JIRA-77", "Linked issue",
			"open", "high", "reporter@example.com", "Reporter", timeMinus5(), []byte(`{"key":"JIRA-77"}`),
		).Scan(&ticketID)
	}); err != nil {
		t.Fatalf("seed linked ticket: %v", err)
	}

	// Delete the connector.
	if err := svc.Delete(ctx, seed.principalID, seed.businessID, id); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// Connector row gone.
	var connCount int
	tdb.Super.QueryRow(ctx, "SELECT count(*) FROM connector WHERE id=$1", id).Scan(&connCount)
	if connCount != 0 {
		t.Fatalf("delete: connector row still present")
	}
	// Secret deleted.
	if secretExists(t, ctx, tdb, oldRef) {
		t.Fatalf("delete: sealed secret not removed")
	}
	// Ticket SURVIVES, connector_id NULL, external_id/external_url PRESERVED.
	var connID *uuid.UUID
	var extID, extURL *string
	if err := tdb.Super.QueryRow(ctx, "SELECT connector_id, external_id, external_url FROM ticket WHERE id=$1", ticketID).Scan(&connID, &extID, &extURL); err != nil {
		t.Fatalf("read detached ticket: %v", err)
	}
	if connID != nil {
		t.Fatalf("delete: ticket.connector_id not nulled")
	}
	if extID == nil || *extID != externalID {
		t.Fatalf("delete: ticket.external_id not preserved, got %v", extID)
	}
	if extURL == nil || *extURL == "" {
		t.Fatalf("delete: ticket.external_url not preserved")
	}
	// Sync-state bookkeeping gone.
	var ssCount int
	tdb.Super.QueryRow(ctx, "SELECT count(*) FROM connector_sync_state WHERE ticket_id=$1", ticketID).Scan(&ssCount)
	if ssCount != 0 {
		t.Fatalf("delete: connector_sync_state not cascaded")
	}

	// Delete unknown id → ErrNotFound.
	if err := svc.Delete(ctx, seed.principalID, seed.businessID, uuid.New()); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("delete unknown: want ErrNotFound, got %v", err)
	}
}

func timeMinus5() time.Time { return time.Now().UTC().Add(-5 * time.Minute) }

// seedOther inserts a second, fully independent tenant (account → business → owner principal
// + membership) into the test DB using the same pattern as seedConnectorTenant in
// testsupport_integration_test.go. It returns a connSeed with the new tenant's principalID
// (the owner human principal) + businessID.
func seedOther(t *testing.T, ctx context.Context, tdb *testdb.TestDB) connSeed {
	t.Helper()

	var ownerRole uuid.UUID
	if err := tdb.Super.QueryRow(ctx,
		"SELECT id FROM role WHERE tenant_root_id IS NULL AND key='owner'").Scan(&ownerRole); err != nil {
		t.Fatalf("seedOther: preset owner role: %v", err)
	}

	masterID := uuid.New()
	ownerAcctID := uuid.New()
	ownerHumanID := uuid.New()
	ownerEmail := "other-owner-" + masterID.String() + "@x.test"

	tx, err := tdb.Super.Begin(ctx)
	if err != nil {
		t.Fatalf("seedOther: begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	stmts := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO account (id,email,display_name,status,created_at,updated_at,email_verified_at) VALUES ($1,$2,'OtherOwner','active',now(),now(),now())`,
			[]any{ownerAcctID, ownerEmail}},
		{`INSERT INTO principal (id,kind,account_id,created_at) VALUES ($1,'human',$2,now())`,
			[]any{ownerHumanID, ownerAcctID}},
		{`INSERT INTO business (id,parent_id,tenant_root_id,name,status,created_at,updated_at) VALUES ($1,NULL,$1,'OtherCo','active',now(),now())`,
			[]any{masterID}},
		{`INSERT INTO business_closure (ancestor_id,descendant_id,depth,tenant_root_id) VALUES ($1,$1,0,$1)`,
			[]any{masterID}},
		{`INSERT INTO membership (principal_id,business_id,tenant_root_id,role_id,granted_at) VALUES ($1,$2,$2,$3,now())`,
			[]any{ownerHumanID, masterID, ownerRole}},
	}
	for _, s := range stmts {
		if _, err := tx.Exec(ctx, s.sql, s.args...); err != nil {
			t.Fatalf("seedOther exec: %v\nSQL: %s", err, s.sql)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("seedOther: commit: %v", err)
	}
	return connSeed{businessID: masterID, principalID: ownerHumanID}
}

// TestManageCrossTenantIsolation: a connector created by tenant A is invisible to tenant B —
// Get/Update/Delete by B's principal all return ErrNotFound (RLS + business predicate).
func TestManageCrossTenantIsolation(t *testing.T) {
	ctx, tdb, seed := startConn(t)
	svc := newConnService(t, tdb, nil)
	idA, err := svc.Create(ctx, seed.principalID, seed.businessID, jiraInput())
	if err != nil {
		t.Fatalf("create A: %v", err)
	}

	// seedOther builds a second tenant (principal + business). If the connectors harness lacks
	// it, mirror the multi-tenant seed used by the ticketing/agents integration tests.
	other := seedOther(t, ctx, tdb)

	if _, err := svc.Get(ctx, other.principalID, other.businessID, idA); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("cross-tenant Get: want ErrNotFound, got %v", err)
	}
	name := "hijack"
	if _, err := svc.Update(ctx, other.principalID, other.businessID, idA, UpdateConnectorInput{DisplayName: &name}); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("cross-tenant Update: want ErrNotFound, got %v", err)
	}
	if err := svc.Delete(ctx, other.principalID, other.businessID, idA); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("cross-tenant Delete: want ErrNotFound, got %v", err)
	}
}
