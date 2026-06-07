//go:build integration

// us1_secret_vault_pin (spec 004 US1, manyforge-a7j.1): connector credentials are
// sealed at rest and never appear as plaintext in the secret column or the create
// audit entry.
//
// Adaptation note: seedAgentTenant returns agentTenant{master, child, agent,
// benignRole, ...}. The agent principal is seed.agent; the tenant-root business is
// seed.master; seed.benignRole is an existing business.read role. seedAgentTenant
// does NOT grant the agent a membership, so we grant that existing benign role via
// the package's grantAgentMembership helper to satisfy the RLS WithPrincipal path.
package security_regression

import (
	"context"
	"crypto/rand"
	"strings"
	"testing"
	"time"

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

	// Grant the agent its existing benign (business.read) role on the home business
	// so WithPrincipal (RLS) can scope it. seed.master is the home business AND the
	// tenant root, so it serves as both business and tenantRoot args.
	if err := grantAgentMembership(ctx, tdb, seed.agent, seed.master, seed.master, seed.benignRole); err != nil {
		t.Fatalf("grant agent membership: %v", err)
	}

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
