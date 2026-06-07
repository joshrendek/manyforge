//go:build integration

// us1_secret_vault_pin (spec 004 US1): connector credentials are sealed at rest and
// never appear as plaintext in the secret column or in the create audit entry.
//
// Adaptation note: seedAgentTenant returns agentTenant{master, child, agent, ...}.
// The agent principal is seed.agent; the tenant-root business is seed.master.
// seedAgentTenant does NOT grant the agent a membership, so we add one here
// (as seedConnectorTenant does) to satisfy the RLS WithPrincipal path.
package security_regression

import (
	"context"
	"crypto/rand"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/manyforge/manyforge/internal/connectors"
	"github.com/manyforge/manyforge/internal/platform/crypto"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/secrets"
)

func TestUS1ConnectorSecretSealedAndUnlogged(t *testing.T) {
	const token = "PIN-super-secret-jira-token-xyz"
	const email = "pin-user@x.test"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	seed := seedAgentTenant(ctx, t, tdb)

	// Grant the agent a benign membership so WithPrincipal (RLS) can scope it.
	grantAgentPinMembership(ctx, t, tdb, seed.agent, seed.master)

	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("key: %v", err)
	}
	sealer, err := crypto.NewSealer(key)
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	svc := &connectors.Service{DB: tdb.App, Vault: secrets.NewVault(sealer)}

	// seed.agent is the principal ID; seed.master is the business (tenant-root) ID.
	id, err := svc.Create(ctx, seed.agent, seed.master, connectors.CreateConnectorInput{
		Type: "jira", DisplayName: "Pin Jira", BaseURL: "https://pin.atlassian.net",
		Email: email, APIToken: token,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// 1. Encrypted at rest: sealed_value must not contain the raw token.
	var sealed string
	if err := tdb.Super.QueryRow(ctx,
		"SELECT s.sealed_value FROM secret s JOIN connector c ON c.secret_ref=s.id WHERE c.id=$1", id).Scan(&sealed); err != nil {
		t.Fatalf("read sealed: %v", err)
	}
	if strings.Contains(sealed, token) {
		t.Fatalf("PIN VIOLATION: raw token in secret.sealed_value")
	}

	// 2. No secret material in the create audit entry.
	var inputs []byte
	if err := tdb.Super.QueryRow(ctx,
		"SELECT inputs FROM audit_entry WHERE action='connector.created' AND target_id=$1", id).Scan(&inputs); err != nil {
		t.Fatalf("read audit: %v", err)
	}
	if strings.Contains(string(inputs), token) || strings.Contains(string(inputs), email) {
		t.Fatalf("PIN VIOLATION: secret material in audit_entry.inputs: %s", inputs)
	}
}

// grantAgentPinMembership creates a minimal role with business.read and grants it
// to the agent at the given business — satisfying the RLS WithPrincipal requirement
// without elevating the principal beyond read access.
func grantAgentPinMembership(ctx context.Context, t *testing.T, tdb *testdb.TestDB, agentID, businessID uuid.UUID) {
	t.Helper()
	roleID := uuid.New()
	tx, err := tdb.Super.Begin(ctx)
	if err != nil {
		t.Fatalf("begin grant: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	stmts := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO role (id,tenant_root_id,key,name,is_locked,created_at) VALUES ($1,$2,'pin-read','PinRead',false,now())`,
			[]any{roleID, businessID}},
		{`INSERT INTO role_permission (role_id,permission_key) VALUES ($1,'business.read')`,
			[]any{roleID}},
		{`INSERT INTO membership (principal_id,business_id,tenant_root_id,role_id,granted_at) VALUES ($1,$2,$2,$3,now())`,
			[]any{agentID, businessID, roleID}},
	}
	for _, s := range stmts {
		if _, err := tx.Exec(ctx, s.sql, s.args...); err != nil {
			t.Fatalf("grant exec: %v\nSQL: %s", err, s.sql)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit grant: %v", err)
	}
}
