//go:build integration

package connectors

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/secrets"
)

type fakeVerifier struct{ err error }

func (f fakeVerifier) Verify(ctx context.Context, t VerifyTarget) error { return f.err }

func newConnService(t *testing.T, tdb *testdb.TestDB, v Verifier) *Service {
	return &Service{DB: tdb.App, Vault: secrets.NewVault(newTestSealer(t)), Verify: v}
}

func startConn(t *testing.T) (context.Context, *testdb.TestDB, connSeed) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	return ctx, tdb, seedConnectorTenant(ctx, t, tdb)
}

func jiraInput() CreateConnectorInput {
	return CreateConnectorInput{
		Type: "jira", DisplayName: "Acme Jira", BaseURL: "https://acme.atlassian.net",
		Email: "ops@acme.test", APIToken: "tok-abc-123",
	}
}

func TestCreateRoundTripSealsAndAudits(t *testing.T) {
	ctx, tdb, seed := startConn(t)
	svc := newConnService(t, tdb, nil)

	id, err := svc.Create(ctx, seed.principalID, seed.businessID, jiraInput())
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	var sealed string
	if err := tdb.Super.QueryRow(ctx,
		"SELECT s.sealed_value FROM secret s JOIN connector c ON c.secret_ref=s.id WHERE c.id=$1", id).Scan(&sealed); err != nil {
		t.Fatalf("read sealed: %v", err)
	}
	if strings.Contains(sealed, "tok-abc-123") {
		t.Fatalf("plaintext token in sealed_value")
	}

	var action string
	var inputs []byte
	if err := tdb.Super.QueryRow(ctx,
		"SELECT action, inputs FROM audit_entry WHERE target_id=$1 AND action='connector.created'", id).Scan(&action, &inputs); err != nil {
		t.Fatalf("read audit: %v", err)
	}
	if strings.Contains(string(inputs), "tok-abc-123") || strings.Contains(string(inputs), "ops@acme.test") {
		t.Fatalf("secret material in audit inputs: %s", inputs)
	}
}

func TestCreateTrustGrantAudited(t *testing.T) {
	ctx, tdb, seed := startConn(t)
	svc := newConnService(t, tdb, nil)

	in := jiraInput()
	in.BaseURL = "http://10.1.2.3"
	in.AllowPrivateBaseURL = true
	id, err := svc.Create(ctx, seed.principalID, seed.businessID, in)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	var decision string
	if err := tdb.Super.QueryRow(ctx,
		"SELECT decision FROM audit_entry WHERE target_id=$1 AND action='connector.created'", id).Scan(&decision); err != nil {
		t.Fatalf("read audit: %v", err)
	}
	if decision != "trust_private_base_url" {
		t.Fatalf("want trust decision, got %q", decision)
	}
}

func TestCreateDuplicateConflict(t *testing.T) {
	ctx, tdb, seed := startConn(t)
	svc := newConnService(t, tdb, nil)
	if _, err := svc.Create(ctx, seed.principalID, seed.businessID, jiraInput()); err != nil {
		t.Fatalf("first create: %v", err)
	}
	_, err := svc.Create(ctx, seed.principalID, seed.businessID, jiraInput())
	if !errors.Is(err, errs.ErrConflict) {
		t.Fatalf("want ErrConflict, got %v", err)
	}
}

func TestCreateVerifierFailureNoRows(t *testing.T) {
	ctx, tdb, seed := startConn(t)
	svc := newConnService(t, tdb, fakeVerifier{err: errors.New("401 from jira")})

	_, err := svc.Create(ctx, seed.principalID, seed.businessID, jiraInput())
	if !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("want ErrValidation, got %v", err)
	}
	var n int
	if err := tdb.Super.QueryRow(ctx, "SELECT count(*) FROM connector").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 connectors after verifier failure, got %d", n)
	}
}

func TestCreateNormalizesBaseURL(t *testing.T) {
	ctx, tdb, seed := startConn(t)
	svc := newConnService(t, tdb, nil)
	in := jiraInput()
	in.BaseURL = "https://acme.atlassian.net/"
	id, err := svc.Create(ctx, seed.principalID, seed.businessID, in)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	var stored string
	if err := tdb.Super.QueryRow(ctx, "SELECT base_url FROM connector WHERE id=$1", id).Scan(&stored); err != nil {
		t.Fatalf("read base_url: %v", err)
	}
	if stored != "https://acme.atlassian.net" {
		t.Fatalf("base_url not normalized (trailing slash): %q", stored)
	}
}
