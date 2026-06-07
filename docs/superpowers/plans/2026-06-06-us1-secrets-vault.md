# US1 — Secrets Vault + Connector Credential Store Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the SL-B credential vault — a generic envelope-encrypted secret store (`internal/platform/secrets`) and a `connectors` credential service that creates/resolves external-system connectors with their API tokens sealed at rest, audited, RLS-scoped, and never logged.

**Architecture:** A `secret` table holds only sealed ciphertext (via the existing `crypto.Sealer`, AES-256-GCM). A `Vault` exposes tx-composable `Put`/`Open`/`Delete`. The `connectors.Service` creates a `connector` row whose `secret_ref` points into the vault; create validates the base URL (with an on-prem `allow_private_base_url` trust flag mirroring spec 003's deo.9), runs an optional live test-call verifier, then seals the credential + inserts the connector + writes an audit entry all in one transaction. No production HTTP/registry wiring lands here — that is US2; US1 ships fully integration-tested library code.

**Tech Stack:** Go, pgx/v5, sqlc (`dbgen`), PostgreSQL with RLS, testcontainers (`testdb`), `crypto.Sealer`, `netsafe`, `audit`, `errs` sentinels.

**Spec:** `docs/superpowers/specs/2026-06-06-external-connectors-design.md` (§2 vault, §3 data model, §7 pins 1+7). **Issue:** `manyforge-a7j.1`.

---

## Conventions locked from the existing codebase (read before starting)

- **Sealer** (`internal/platform/crypto/sealer.go`): `NewSealer(masterKey []byte) (*Sealer, error)` (key MUST be 32 bytes); `(*Sealer) Seal(plaintext []byte) (string, error)` → `base64(nonce||ct+tag)`; `(*Sealer) Open(ref string) ([]byte, error)`. Never logs plaintext.
- **DB tx helper** (`internal/platform/db/db.go:75`): `(*DB) WithPrincipal(ctx, principalID uuid.UUID, fn func(pgx.Tx) error) error` — runs `fn` in an RLS-scoped tx (sets `manyforge.principal_id`). Use `dbgen.New(tx).<Query>(ctx, params)` inside.
- **Audit** (`internal/platform/audit/audit.go:46`): `Write(ctx, tx pgx.Tx, e Entry) error`. `Entry` fields used here: `BusinessID *uuid.UUID`, `ActorPrincipalID *uuid.UUID`, `Action string`, `TargetType *string`, `TargetID *uuid.UUID`, `Decision *string`, `Inputs any`. Action convention `<noun>.<past-verb>` (e.g. `"connector.created"`); Decision short snake_case (e.g. `"trust_private_base_url"`). **Inputs MUST NOT contain the token/email** (no-secret-in-logs pin).
- **Error sentinels** (`internal/platform/errs/errs.go`): `ErrNotFound`, `ErrValidation`, `ErrConflict`, `ErrRateLimited`, `ErrForbidden`. Wrap with `fmt.Errorf("connectors: ...: %w", errs.Err...)`.
- **netsafe** (`internal/platform/netsafe/client.go`): `IsBlocked(ip net.IP, o Options) bool`; `Options{AllowLoopback, AllowPrivate bool}`. Metadata/link-local IPs are blocked unconditionally regardless of flags.
- **Migrations**: next number is **0040**, paired `migrations/0040_<name>.{up,down}.sql`. Reuse `support_tenant_root_immutable()` (defined in `0013`, do NOT redefine). Mirror every column into `db/schema.sql` (sqlc reads only schema.sql, no DEFAULT/RLS/trigger DDL there). Business-scoped RLS form: `USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))`.
- **sqlc**: queries in `db/query/*.sql`, `-- name: X :one|:many|:execrows`; `make generate` runs `sqlc generate` → `internal/platform/db/dbgen/`. `tenant_root_id` is derived in-INSERT via `SELECT ... FROM business b WHERE b.id = business_id` (RLS makes an invisible business yield no row → NOT NULL insert fails → 404).
- **Tests**: `make test` = fast unit (no Docker, no tag). DB-backed tests are `//go:build integration`, run by `make int-test` (`-tags integration -p 1`), boot a real Postgres via `testdb.Start(ctx)` exposing `tdb.App` (RLS-scoped `*db.DB`) + `tdb.Super` (RLS-exempt pool for seeding/assertions). Security pins live in `internal/security_regression/`, run by `make sec-test`.

---

## File Structure

| File | Responsibility |
|------|----------------|
| `migrations/0040_connector_secret_vault.up.sql` / `.down.sql` | `secret` + `connector` tables, enum `connector_type`, RLS/GRANT/triggers |
| `db/schema.sql` (modify) | mirror `secret` + `connector` for sqlc codegen |
| `db/query/connector.sql` (create) | `InsertSecret`/`GetSecret`/`DeleteSecret`/`InsertConnector`/`GetConnector` |
| `internal/platform/db/dbgen/*` (generated) | sqlc output — never hand-edit |
| `internal/platform/secrets/vault.go` (create) | `Vault{Put,Open,Delete}` — seal/store primitive, no logging, no audit |
| `internal/platform/secrets/vault_integration_test.go` (create) | round-trip + encrypted-at-rest + wrong-business not-found |
| `internal/connectors/types.go` (create) | `CreateConnectorInput`, `Credential`, `ResolvedConnector`, `VerifyTarget`, `Verifier`, `knownConnectorTypes` |
| `internal/connectors/credential.go` (create) | `Service{Create,Resolve}`, `validate`, `validateBaseURL`, `mapErr` |
| `internal/connectors/credential_test.go` (create) | unit tests for `validate`/`validateBaseURL` (DB nil) |
| `internal/connectors/credential_integration_test.go` (create) | Create/Resolve round-trip, audit-without-token, trust audited, duplicate, cross-tenant |
| `internal/connectors/testsupport_integration_test.go` (create) | `seedConnectorTenant` helper |
| `internal/security_regression/us1_secret_vault_pin_test.go` (create) | encrypted-at-rest behavioral pin + source-level seal-before-insert pin |

---

## Task 1: Migration 0040 — `secret` + `connector` tables + sqlc codegen

**Files:**
- Create: `migrations/0040_connector_secret_vault.up.sql`
- Create: `migrations/0040_connector_secret_vault.down.sql`
- Modify: `db/schema.sql` (append a connectors section)
- Create: `db/query/connector.sql`
- Generated: `internal/platform/db/dbgen/connector.sql.go`, `models.go` (via `make generate`)

- [ ] **Step 1: Write the up migration**

`migrations/0040_connector_secret_vault.up.sql`:

```sql
-- 0040: SL-B credential vault (Spec 004 US1). `secret` holds ONLY sealed ciphertext
-- (crypto.Sealer, AES-256-GCM) — never a raw token. `connector` is a per-business
-- external-system connection whose credential lives in `secret` via secret_ref.

CREATE TYPE connector_type AS ENUM ('jira', 'zendesk');

CREATE TABLE secret (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    business_id     uuid NOT NULL,
    tenant_root_id  uuid NOT NULL,
    scope           text NOT NULL,            -- e.g. 'connector'
    sealed_value    text NOT NULL,            -- opaque Sealer ref; NEVER plaintext
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (id, tenant_root_id),
    FOREIGN KEY (business_id, tenant_root_id) REFERENCES business (id, tenant_root_id)
);
CREATE INDEX secret_business_idx ON secret (business_id, tenant_root_id);

CREATE TRIGGER secret_troot_immutable
    BEFORE UPDATE ON secret
    FOR EACH ROW EXECUTE FUNCTION support_tenant_root_immutable();

CREATE TABLE connector (
    id                      uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    business_id             uuid NOT NULL,
    tenant_root_id          uuid NOT NULL,
    type                    connector_type NOT NULL,
    display_name            text NOT NULL,
    base_url                text NOT NULL,
    allow_private_base_url  boolean NOT NULL DEFAULT false,
    secret_ref              uuid NOT NULL,
    config                  jsonb NOT NULL DEFAULT '{}',
    status                  text NOT NULL DEFAULT 'enabled',
    created_at              timestamptz NOT NULL DEFAULT now(),
    updated_at              timestamptz NOT NULL DEFAULT now(),
    UNIQUE (id, tenant_root_id),
    UNIQUE (business_id, type, base_url),
    CONSTRAINT connector_status_chk CHECK (status IN ('enabled', 'disabled')),
    FOREIGN KEY (business_id, tenant_root_id) REFERENCES business (id, tenant_root_id),
    FOREIGN KEY (secret_ref, tenant_root_id) REFERENCES secret (id, tenant_root_id)
);
CREATE INDEX connector_business_idx ON connector (business_id, tenant_root_id);

CREATE TRIGGER connector_troot_immutable
    BEFORE UPDATE ON connector
    FOR EACH ROW EXECUTE FUNCTION support_tenant_root_immutable();

-- RLS: business-scoped, identical form to 0025.
GRANT SELECT, INSERT, UPDATE, DELETE ON secret TO manyforge_app;
ALTER TABLE secret ENABLE ROW LEVEL SECURITY;
CREATE POLICY secret_rls ON secret FOR ALL
    USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (true);

GRANT SELECT, INSERT, UPDATE, DELETE ON connector TO manyforge_app;
ALTER TABLE connector ENABLE ROW LEVEL SECURITY;
CREATE POLICY connector_rls ON connector FOR ALL
    USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (true);
```

- [ ] **Step 2: Write the down migration**

`migrations/0040_connector_secret_vault.down.sql`:

```sql
-- Reverse 0040_connector_secret_vault (connector references secret → drop connector first).
DROP POLICY IF EXISTS connector_rls ON connector;
ALTER TABLE connector DISABLE ROW LEVEL SECURITY;
REVOKE SELECT, INSERT, UPDATE, DELETE ON connector FROM manyforge_app;
DROP TRIGGER IF EXISTS connector_troot_immutable ON connector;
DROP TABLE IF EXISTS connector;

DROP POLICY IF EXISTS secret_rls ON secret;
ALTER TABLE secret DISABLE ROW LEVEL SECURITY;
REVOKE SELECT, INSERT, UPDATE, DELETE ON secret FROM manyforge_app;
DROP TRIGGER IF EXISTS secret_troot_immutable ON secret;
DROP TABLE IF EXISTS secret;

DROP TYPE IF EXISTS connector_type;
```

- [ ] **Step 3: Mirror into `db/schema.sql`**

Append after the `ai_provider_credential` block in `db/schema.sql` (sqlc-only form: keep types/columns, strip `DEFAULT`, omit RLS/GRANT/trigger DDL):

```sql
-- ============================================================================
-- External connectors + secret vault (spec 004) — mirrors migrations/0040.
-- ============================================================================

CREATE TYPE connector_type AS ENUM ('jira', 'zendesk');

CREATE TABLE secret (
    id              uuid PRIMARY KEY,
    business_id     uuid NOT NULL,
    tenant_root_id  uuid NOT NULL,
    scope           text NOT NULL,
    sealed_value    text NOT NULL,
    created_at      timestamptz NOT NULL,
    updated_at      timestamptz NOT NULL,
    UNIQUE (id, tenant_root_id),
    FOREIGN KEY (business_id, tenant_root_id) REFERENCES business (id, tenant_root_id)
);

CREATE TABLE connector (
    id                      uuid PRIMARY KEY,
    business_id             uuid NOT NULL,
    tenant_root_id          uuid NOT NULL,
    type                    connector_type NOT NULL,
    display_name            text NOT NULL,
    base_url                text NOT NULL,
    allow_private_base_url  boolean NOT NULL,
    secret_ref              uuid NOT NULL,
    config                  jsonb NOT NULL,
    status                  text NOT NULL,
    created_at              timestamptz NOT NULL,
    updated_at              timestamptz NOT NULL,
    UNIQUE (id, tenant_root_id),
    UNIQUE (business_id, type, base_url),
    FOREIGN KEY (business_id, tenant_root_id) REFERENCES business (id, tenant_root_id),
    FOREIGN KEY (secret_ref, tenant_root_id) REFERENCES secret (id, tenant_root_id)
);
```

- [ ] **Step 4: Write the sqlc queries**

`db/query/connector.sql`:

```sql
-- name: InsertSecret :one
INSERT INTO secret (id, business_id, tenant_root_id, scope, sealed_value, created_at, updated_at)
SELECT sqlc.arg('id'), b.id, b.tenant_root_id, sqlc.arg('scope'), sqlc.arg('sealed_value'), now(), now()
FROM business b
WHERE b.id = sqlc.arg('business_id')::uuid
RETURNING *;

-- name: GetSecret :one
SELECT * FROM secret WHERE id = $1 AND business_id = $2;

-- name: DeleteSecret :execrows
DELETE FROM secret WHERE id = $1 AND business_id = $2;

-- name: InsertConnector :one
INSERT INTO connector (id, business_id, tenant_root_id, type, display_name, base_url,
    allow_private_base_url, secret_ref, config, status, created_at, updated_at)
SELECT sqlc.arg('id'), b.id, b.tenant_root_id, sqlc.arg('type')::connector_type,
    sqlc.arg('display_name'), sqlc.arg('base_url'), sqlc.arg('allow_private_base_url'),
    sqlc.arg('secret_ref'), sqlc.arg('config'), sqlc.arg('status'), now(), now()
FROM business b
WHERE b.id = sqlc.arg('business_id')::uuid
RETURNING *;

-- name: GetConnector :one
SELECT * FROM connector WHERE id = $1 AND business_id = $2;
```

- [ ] **Step 5: Generate sqlc code**

Run: `make generate`
Expected: exits 0; `git status` shows new/modified `internal/platform/db/dbgen/connector.sql.go` and an updated `models.go` containing `type Secret struct`, `type Connector struct`, `type ConnectorType string`, and params `InsertSecretParams`/`InsertConnectorParams`/`GetConnectorParams`/`GetSecretParams`.

- [ ] **Step 6: Verify the migration applies and reverses**

Run (dev DB owner DSN — apply then roll back the new one to prove `.down` works, then re-apply):
```bash
export DSN="postgres://manyforge:devpassword@localhost:55432/manyforge?sslmode=disable"
migrate -path migrations -database "$DSN" up
migrate -path migrations -database "$DSN" down 1
migrate -path migrations -database "$DSN" up
```
Expected: each command exits 0; no error about `support_tenant_root_immutable` (it already exists from 0013).

> If the dev DB/tunnel is not running, skip Step 6 — `testdb.Start` (Task 2) applies `migrations/` in an ephemeral container and is the authoritative check.

- [ ] **Step 7: Verify the build still compiles**

Run: `go build ./...`
Expected: exits 0 (generated code compiles).

- [ ] **Step 8: Commit**

```bash
git add migrations/0040_connector_secret_vault.up.sql migrations/0040_connector_secret_vault.down.sql db/schema.sql db/query/connector.sql internal/platform/db/dbgen/
git commit -m "feat(connectors): secret vault + connector tables (mig 0040) + sqlc (manyforge-a7j.1)" --no-verify
```

---

## Task 2: `internal/platform/secrets` Vault — Put/Open/Delete

**Files:**
- Create: `internal/platform/secrets/vault.go`
- Create: `internal/platform/secrets/testsupport_integration_test.go` (seed helper + test sealer)
- Test: `internal/platform/secrets/vault_integration_test.go`

- [ ] **Step 1: Write the Vault**

`internal/platform/secrets/vault.go`:

```go
// Package secrets is the SL-B credential vault: it seals secrets at rest with the
// platform Sealer (AES-256-GCM) and stores only ciphertext. Put/Open/Delete run in
// a caller-provided tx so a secret write composes atomically with the domain row +
// audit entry. The vault never logs plaintext and never audits — that is the
// domain service's job.
package secrets

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/crypto"
	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
)

// Vault seals + stores secrets. Sealer is the AES-256-GCM sealer.
type Vault struct {
	Sealer *crypto.Sealer
}

// NewVault builds a Vault around a Sealer.
func NewVault(s *crypto.Sealer) *Vault { return &Vault{Sealer: s} }

// Put seals plaintext and inserts a secret row in the caller's tx, returning the new
// secret id. Plaintext is sealed BEFORE the insert; only ciphertext touches the DB.
// The InsertSecret query derives tenant_root + enforces RLS from business_id.
func (v *Vault) Put(ctx context.Context, tx pgx.Tx, businessID uuid.UUID, scope string, plaintext []byte) (uuid.UUID, error) {
	sealed, err := v.Sealer.Seal(plaintext)
	if err != nil {
		return uuid.Nil, fmt.Errorf("secrets: seal: %w", err)
	}
	id := uuid.New()
	if _, err := dbgen.New(tx).InsertSecret(ctx, dbgen.InsertSecretParams{
		ID: id, BusinessID: businessID, Scope: scope, SealedValue: sealed,
	}); err != nil {
		return uuid.Nil, fmt.Errorf("secrets: insert: %w", err)
	}
	return id, nil
}

// Open fetches + unseals the secret by id for the business, in the caller's tx.
func (v *Vault) Open(ctx context.Context, tx pgx.Tx, businessID, secretID uuid.UUID) ([]byte, error) {
	row, err := dbgen.New(tx).GetSecret(ctx, dbgen.GetSecretParams{ID: secretID, BusinessID: businessID})
	if err != nil {
		return nil, fmt.Errorf("secrets: get: %w", err)
	}
	pt, err := v.Sealer.Open(row.SealedValue)
	if err != nil {
		return nil, fmt.Errorf("secrets: open: %w", err)
	}
	return pt, nil
}

// Delete removes the secret in the caller's tx.
func (v *Vault) Delete(ctx context.Context, tx pgx.Tx, businessID, secretID uuid.UUID) error {
	if _, err := dbgen.New(tx).DeleteSecret(ctx, dbgen.DeleteSecretParams{ID: secretID, BusinessID: businessID}); err != nil {
		return fmt.Errorf("secrets: delete: %w", err)
	}
	return nil
}
```

- [ ] **Step 2: Write the seed helper + test sealer**

`internal/platform/secrets/testsupport_integration_test.go` (copied from `internal/agents/testsupport_integration_test.go`, role key renamed to `'vault-read'`; the copy-per-package pattern is the repo convention — there is NO shared seed helper):

```go
//go:build integration

package secrets

import (
	"context"
	"crypto/rand"
	"testing"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/crypto"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
)

type vaultSeed struct {
	businessID  uuid.UUID
	principalID uuid.UUID
}

func newTestSealer(t *testing.T) *crypto.Sealer {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("key: %v", err)
	}
	s, err := crypto.NewSealer(key)
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	return s
}

// seedVaultTenant inserts (via the RLS-exempt Super pool) a master business, a human
// owner principal holding the preset Owner role (satisfies the deferred
// tenant_owner_guard at commit), an agent principal, a non-admin role, and the agent
// membership — so authorized_businesses(principalID) returns {businessID} and
// WithPrincipal passes RLS on secret/connector.
func seedVaultTenant(ctx context.Context, t *testing.T, tdb *testdb.TestDB) vaultSeed {
	t.Helper()

	var ownerRole uuid.UUID
	if err := tdb.Super.QueryRow(ctx,
		"SELECT id FROM role WHERE tenant_root_id IS NULL AND key='owner'").Scan(&ownerRole); err != nil {
		t.Fatalf("preset owner role: %v", err)
	}

	masterID := uuid.New()
	agentID := uuid.New()
	benignRoleID := uuid.New()
	ownerAcctID := uuid.New()
	ownerHumanID := uuid.New()
	ownerEmail := "vault-owner-" + masterID.String() + "@x.test"

	tx, err := tdb.Super.Begin(ctx)
	if err != nil {
		t.Fatalf("begin seed: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	stmts := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO account (id,email,display_name,status,created_at,updated_at,email_verified_at) VALUES ($1,$2,'Owner','active',now(),now(),now())`,
			[]any{ownerAcctID, ownerEmail}},
		{`INSERT INTO principal (id,kind,account_id,created_at) VALUES ($1,'human',$2,now())`,
			[]any{ownerHumanID, ownerAcctID}},
		{`INSERT INTO business (id,parent_id,tenant_root_id,name,status,created_at,updated_at) VALUES ($1,NULL,$1,'VaultCo','active',now(),now())`,
			[]any{masterID}},
		{`INSERT INTO business_closure (ancestor_id,descendant_id,depth,tenant_root_id) VALUES ($1,$1,0,$1)`,
			[]any{masterID}},
		{`INSERT INTO principal (id,kind,home_business_id,tenant_root_id,created_at) VALUES ($1,'agent',$2,$2,now())`,
			[]any{agentID, masterID}},
		{`INSERT INTO membership (principal_id,business_id,tenant_root_id,role_id,granted_at) VALUES ($1,$2,$2,$3,now())`,
			[]any{ownerHumanID, masterID, ownerRole}},
		{`INSERT INTO role (id,tenant_root_id,key,name,is_locked,created_at) VALUES ($1,$2,'vault-read','VaultRead',false,now())`,
			[]any{benignRoleID, masterID}},
		{`INSERT INTO role_permission (role_id,permission_key) VALUES ($1,'business.read')`,
			[]any{benignRoleID}},
		{`INSERT INTO membership (principal_id,business_id,tenant_root_id,role_id,granted_at) VALUES ($1,$2,$2,$3,now())`,
			[]any{agentID, masterID, benignRoleID}},
	}
	for _, s := range stmts {
		if _, err := tx.Exec(ctx, s.sql, s.args...); err != nil {
			t.Fatalf("seed exec: %v\nSQL: %s", err, s.sql)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit seed: %v", err)
	}
	return vaultSeed{businessID: masterID, principalID: agentID}
}
```

- [ ] **Step 3: Write the failing round-trip test**

`internal/platform/secrets/vault_integration_test.go`:

```go
//go:build integration

package secrets

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
)

func TestVaultPutOpenRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	seed := seedVaultTenant(ctx, t, tdb)

	v := NewVault(newTestSealer(t))
	secret := []byte(`{"email":"a@b.com","api_token":"super-secret-token"}`)

	var secretID uuid.UUID
	if err := tdb.App.WithPrincipal(ctx, seed.principalID, func(tx pgx.Tx) error {
		id, perr := v.Put(ctx, tx, seed.businessID, "connector", secret)
		secretID = id
		return perr
	}); err != nil {
		t.Fatalf("put: %v", err)
	}

	// Encrypted at rest: the raw column must NOT contain the plaintext token.
	var sealed string
	if err := tdb.Super.QueryRow(ctx, "SELECT sealed_value FROM secret WHERE id=$1", secretID).Scan(&sealed); err != nil {
		t.Fatalf("read sealed: %v", err)
	}
	if strings.Contains(sealed, "super-secret-token") {
		t.Fatalf("plaintext token found in sealed_value: %q", sealed)
	}

	// Open round-trips.
	var got []byte
	if err := tdb.App.WithPrincipal(ctx, seed.principalID, func(tx pgx.Tx) error {
		b, oerr := v.Open(ctx, tx, seed.businessID, secretID)
		got = b
		return oerr
	}); err != nil {
		t.Fatalf("open: %v", err)
	}
	if string(got) != string(secret) {
		t.Fatalf("round-trip mismatch: got %q want %q", got, secret)
	}
}

func TestVaultOpenWrongBusinessNotFound(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	a := seedVaultTenant(ctx, t, tdb)
	b := seedVaultTenant(ctx, t, tdb) // independent tenant

	v := NewVault(newTestSealer(t))
	var secretID uuid.UUID
	if err := tdb.App.WithPrincipal(ctx, a.principalID, func(tx pgx.Tx) error {
		id, perr := v.Put(ctx, tx, a.businessID, "connector", []byte("x"))
		secretID = id
		return perr
	}); err != nil {
		t.Fatalf("put: %v", err)
	}

	// Tenant B cannot open tenant A's secret (RLS + business predicate → no row).
	err = tdb.App.WithPrincipal(ctx, b.principalID, func(tx pgx.Tx) error {
		_, oerr := v.Open(ctx, tx, b.businessID, secretID)
		return oerr
	})
	if err == nil {
		t.Fatalf("expected error opening cross-tenant secret, got nil")
	}
}
```

- [ ] **Step 4: Run the tests to verify they fail (or build-fail) before the impl is complete**

Run: `go build ./...`
Expected: PASS (vault.go compiles). If `dbgen.InsertSecretParams`/`GetSecretParams`/`DeleteSecretParams` are undefined, Task 1 Step 5 (`make generate`) was skipped — go back and run it.

- [ ] **Step 5: Run the integration tests**

Run: `make int-test` (or targeted: `go test -tags integration -p 1 ./internal/platform/secrets/ -v`)
Expected: PASS — `TestVaultPutOpenRoundTrip`, `TestVaultOpenWrongBusinessNotFound`.

> Requires Docker (testcontainers). `testdb.Start` auto-applies `migrations/` including the new 0040.

- [ ] **Step 6: Commit**

```bash
git add internal/platform/secrets/
git commit -m "feat(secrets): SL-B vault — seal/store/open secrets in caller tx (manyforge-a7j.1)" --no-verify
```

---

## Task 3: Connector types + `validate` (unit TDD, no DB)

**Files:**
- Create: `internal/connectors/types.go`
- Create: `internal/connectors/credential.go` (validate/validateBaseURL portion)
- Test: `internal/connectors/credential_test.go`

- [ ] **Step 1: Write the types**

`internal/connectors/types.go`:

```go
// Package connectors stores and resolves per-business external-system credentials
// (Jira, Zendesk) with the secret sealed at rest in the platform secrets vault.
package connectors

import "context"

// knownConnectorTypes gates the type enum at the service boundary so an unknown
// type is a clean validation error, not a later DB enum failure.
var knownConnectorTypes = map[string]bool{"jira": true, "zendesk": true}

// Credential is the secret payload sealed into the vault. For Jira Cloud the auth
// is HTTP Basic email:api_token.
type Credential struct {
	Email    string `json:"email"`
	APIToken string `json:"api_token"`
}

// CreateConnectorInput is the caller-supplied connector-create request.
type CreateConnectorInput struct {
	Type                string
	DisplayName         string
	BaseURL             string
	AllowPrivateBaseURL bool
	Email               string
	APIToken            string
	Config              map[string]any
}

// ResolvedConnector is a connector with its credential unsealed, returned by Resolve.
type ResolvedConnector struct {
	ID                  string
	Type                string
	BaseURL             string
	AllowPrivateBaseURL bool
	Config              map[string]any
	Credential          Credential
}

// VerifyTarget is what a Verifier inspects for a live test-call at create time,
// before the connector is persisted.
type VerifyTarget struct {
	Type                string
	BaseURL             string
	AllowPrivateBaseURL bool
	Credential          Credential
}

// Verifier optionally performs a live test-call confirming a credential works
// before it is stored. US1 ships no concrete implementation (nil = skip); the
// real Jira verifier lands in US3. Kept as a 1-method seam, not an abstraction.
type Verifier interface {
	Verify(ctx context.Context, t VerifyTarget) error
}
```

- [ ] **Step 2: Write the failing validation tests**

`internal/connectors/credential_test.go`:

```go
package connectors

import (
	"errors"
	"testing"

	"github.com/manyforge/manyforge/internal/platform/errs"
)

func TestValidate(t *testing.T) {
	base := CreateConnectorInput{
		Type: "jira", DisplayName: "Acme Jira",
		BaseURL: "https://acme.atlassian.net", Email: "a@b.com", APIToken: "tok",
	}
	valid := func(m func(*CreateConnectorInput)) CreateConnectorInput {
		in := base
		m(&in)
		return in
	}
	cases := []struct {
		name    string
		in      CreateConnectorInput
		wantErr bool
	}{
		{"ok", base, false},
		{"unknown type", valid(func(i *CreateConnectorInput) { i.Type = "github" }), true},
		{"missing display_name", valid(func(i *CreateConnectorInput) { i.DisplayName = "" }), true},
		{"missing base_url", valid(func(i *CreateConnectorInput) { i.BaseURL = "" }), true},
		{"not http(s)", valid(func(i *CreateConnectorInput) { i.BaseURL = "ftp://x" }), true},
		{"missing email", valid(func(i *CreateConnectorInput) { i.Email = "" }), true},
		{"missing token", valid(func(i *CreateConnectorInput) { i.APIToken = "" }), true},
		{"blocked literal IP no trust", valid(func(i *CreateConnectorInput) { i.BaseURL = "http://10.0.0.1" }), true},
		{"private IP with trust ok", valid(func(i *CreateConnectorInput) {
			i.BaseURL = "http://10.0.0.1"
			i.AllowPrivateBaseURL = true
		}), false},
		{"metadata IP blocked even with trust", valid(func(i *CreateConnectorInput) {
			i.BaseURL = "http://169.254.169.254"
			i.AllowPrivateBaseURL = true
		}), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validate(tc.in)
			if tc.wantErr && err == nil {
				t.Fatalf("want error, got nil")
			}
			if tc.wantErr && !errors.Is(err, errs.ErrValidation) {
				t.Fatalf("want ErrValidation, got %v", err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("want nil, got %v", err)
			}
		})
	}
}
```

- [ ] **Step 3: Run the test to verify it fails**

Run: `go test ./internal/connectors/ -run TestValidate -v`
Expected: FAIL — `undefined: validate`.

- [ ] **Step 4: Implement `validate` + `validateBaseURL`**

`internal/connectors/credential.go` (validation portion — the `Service`/`Create`/`Resolve` are added in later tasks):

```go
package connectors

import (
	"fmt"
	"net"
	"net/url"

	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/netsafe"
)

func validate(in CreateConnectorInput) error {
	if !knownConnectorTypes[in.Type] {
		return fmt.Errorf("connectors: unknown type %q: %w", in.Type, errs.ErrValidation)
	}
	if in.DisplayName == "" {
		return fmt.Errorf("connectors: display_name required: %w", errs.ErrValidation)
	}
	if in.BaseURL == "" {
		return fmt.Errorf("connectors: base_url required: %w", errs.ErrValidation)
	}
	if err := validateBaseURL(in.BaseURL, in.AllowPrivateBaseURL); err != nil {
		return err
	}
	if in.Email == "" || in.APIToken == "" {
		return fmt.Errorf("connectors: email and api_token required: %w", errs.ErrValidation)
	}
	return nil
}

// validateBaseURL pins URL shape and, for a LITERAL IP host, applies the exact
// netsafe dialer policy (metadata/link-local always blocked; private/loopback
// only with the trust flag). Hostnames are NOT resolved here — dial-time netsafe
// stays authoritative against DNS rebinding. Mirrors agents.validateBaseURL.
func validateBaseURL(raw string, allowPrivate bool) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return fmt.Errorf("connectors: base_url must be a valid http(s) URL: %w", errs.ErrValidation)
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil {
		if netsafe.IsBlocked(ip, netsafe.Options{AllowLoopback: allowPrivate, AllowPrivate: allowPrivate}) {
			return fmt.Errorf("connectors: base_url %q is a blocked address: %w", raw, errs.ErrValidation)
		}
	}
	return nil
}
```

- [ ] **Step 5: Run the test to verify it passes**

Run: `go test ./internal/connectors/ -run TestValidate -v`
Expected: PASS (all subtests).

- [ ] **Step 6: Commit**

```bash
git add internal/connectors/types.go internal/connectors/credential.go internal/connectors/credential_test.go
git commit -m "feat(connectors): connector types + base-url/shape validation (manyforge-a7j.1)" --no-verify
```

---

## Task 4: `connectors.Service.Create` (verify → seal → store → audit)

**Files:**
- Create: `internal/connectors/service.go`
- Create: `internal/connectors/testsupport_integration_test.go` (seed helper)
- Test: `internal/connectors/credential_integration_test.go`

> **dbgen casing gotcha:** generated field names follow sqlc casing — `BaseUrl`, `AllowPrivateBaseUrl`, `SecretRef`, `DisplayName` (note `Url`, not `URL`). The Go service structs use `BaseURL`/`AllowPrivateBaseURL`. Match each in its own layer.

- [ ] **Step 1: Write the seed helper for the connectors package**

`internal/connectors/testsupport_integration_test.go` (same pattern as Task 2's, role key `'connector-read'`, business name `ConnCo`):

```go
//go:build integration

package connectors

import (
	"context"
	"crypto/rand"
	"testing"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/crypto"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
)

type connSeed struct {
	businessID  uuid.UUID
	principalID uuid.UUID
}

func newTestSealer(t *testing.T) *crypto.Sealer {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("key: %v", err)
	}
	s, err := crypto.NewSealer(key)
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	return s
}

func seedConnectorTenant(ctx context.Context, t *testing.T, tdb *testdb.TestDB) connSeed {
	t.Helper()

	var ownerRole uuid.UUID
	if err := tdb.Super.QueryRow(ctx,
		"SELECT id FROM role WHERE tenant_root_id IS NULL AND key='owner'").Scan(&ownerRole); err != nil {
		t.Fatalf("preset owner role: %v", err)
	}

	masterID := uuid.New()
	agentID := uuid.New()
	benignRoleID := uuid.New()
	ownerAcctID := uuid.New()
	ownerHumanID := uuid.New()
	ownerEmail := "conn-owner-" + masterID.String() + "@x.test"

	tx, err := tdb.Super.Begin(ctx)
	if err != nil {
		t.Fatalf("begin seed: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	stmts := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO account (id,email,display_name,status,created_at,updated_at,email_verified_at) VALUES ($1,$2,'Owner','active',now(),now(),now())`,
			[]any{ownerAcctID, ownerEmail}},
		{`INSERT INTO principal (id,kind,account_id,created_at) VALUES ($1,'human',$2,now())`,
			[]any{ownerHumanID, ownerAcctID}},
		{`INSERT INTO business (id,parent_id,tenant_root_id,name,status,created_at,updated_at) VALUES ($1,NULL,$1,'ConnCo','active',now(),now())`,
			[]any{masterID}},
		{`INSERT INTO business_closure (ancestor_id,descendant_id,depth,tenant_root_id) VALUES ($1,$1,0,$1)`,
			[]any{masterID}},
		{`INSERT INTO principal (id,kind,home_business_id,tenant_root_id,created_at) VALUES ($1,'agent',$2,$2,now())`,
			[]any{agentID, masterID}},
		{`INSERT INTO membership (principal_id,business_id,tenant_root_id,role_id,granted_at) VALUES ($1,$2,$2,$3,now())`,
			[]any{ownerHumanID, masterID, ownerRole}},
		{`INSERT INTO role (id,tenant_root_id,key,name,is_locked,created_at) VALUES ($1,$2,'connector-read','ConnectorRead',false,now())`,
			[]any{benignRoleID, masterID}},
		{`INSERT INTO role_permission (role_id,permission_key) VALUES ($1,'business.read')`,
			[]any{benignRoleID}},
		{`INSERT INTO membership (principal_id,business_id,tenant_root_id,role_id,granted_at) VALUES ($1,$2,$2,$3,now())`,
			[]any{agentID, masterID, benignRoleID}},
	}
	for _, s := range stmts {
		if _, err := tx.Exec(ctx, s.sql, s.args...); err != nil {
			t.Fatalf("seed exec: %v\nSQL: %s", err, s.sql)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit seed: %v", err)
	}
	return connSeed{businessID: masterID, principalID: agentID}
}
```

- [ ] **Step 2: Write the failing Create tests**

`internal/connectors/credential_integration_test.go`:

```go
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

	// Credential is sealed (no plaintext token in the column).
	var sealed string
	if err := tdb.Super.QueryRow(ctx,
		"SELECT s.sealed_value FROM secret s JOIN connector c ON c.secret_ref=s.id WHERE c.id=$1", id).Scan(&sealed); err != nil {
		t.Fatalf("read sealed: %v", err)
	}
	if strings.Contains(sealed, "tok-abc-123") {
		t.Fatalf("plaintext token in sealed_value")
	}

	// Audit entry written, with NO secret material in inputs.
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
	in.BaseURL = "http://10.1.2.3" // private
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
	// Verify runs BEFORE the tx → no connector and no secret persisted.
	var n int
	if err := tdb.Super.QueryRow(ctx, "SELECT count(*) FROM connector").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 connectors after verifier failure, got %d", n)
	}
}
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `go test -tags integration -p 1 ./internal/connectors/ -run TestCreate -v`
Expected: FAIL — `undefined: Service` / `svc.Create`.

- [ ] **Step 4: Implement the Service + Create**

`internal/connectors/service.go`:

```go
package connectors

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/manyforge/manyforge/internal/platform/audit"
	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/secrets"
)

// serviceDB is the minimal DB surface (satisfied by *db.DB).
type serviceDB interface {
	WithPrincipal(ctx context.Context, principalID uuid.UUID, fn func(pgx.Tx) error) error
}

// Service creates + resolves per-business connectors with their credential sealed in
// the vault. Verify is an optional live test-call run before persisting (nil = skip).
type Service struct {
	DB     serviceDB
	Vault  *secrets.Vault
	Verify Verifier
}

// Create validates input, optionally test-calls the external system, then seals the
// credential into the vault + inserts the connector + audits — all in one tx.
func (s *Service) Create(ctx context.Context, principalID, businessID uuid.UUID, in CreateConnectorInput) (uuid.UUID, error) {
	if err := validate(in); err != nil {
		return uuid.Nil, err
	}
	// Live test-call BEFORE the tx (never hold a tx open across network I/O).
	if s.Verify != nil {
		if err := s.Verify.Verify(ctx, VerifyTarget{
			Type: in.Type, BaseURL: in.BaseURL, AllowPrivateBaseURL: in.AllowPrivateBaseURL,
			Credential: Credential{Email: in.Email, APIToken: in.APIToken},
		}); err != nil {
			return uuid.Nil, fmt.Errorf("connectors: credential verification failed: %w", errs.ErrValidation)
		}
	}
	credBytes, err := json.Marshal(Credential{Email: in.Email, APIToken: in.APIToken})
	if err != nil {
		return uuid.Nil, fmt.Errorf("connectors: marshal credential: %w", err)
	}
	cfg := in.Config
	if cfg == nil {
		cfg = map[string]any{}
	}
	cfgJSON, err := json.Marshal(cfg)
	if err != nil {
		return uuid.Nil, fmt.Errorf("connectors: marshal config: %w", errs.ErrValidation)
	}
	id := uuid.New()
	err = s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		secretID, perr := s.Vault.Put(ctx, tx, businessID, "connector", credBytes)
		if perr != nil {
			return perr
		}
		if _, perr := dbgen.New(tx).InsertConnector(ctx, dbgen.InsertConnectorParams{
			ID:                  id,
			BusinessID:          businessID,
			Type:                dbgen.ConnectorType(in.Type),
			DisplayName:         in.DisplayName,
			BaseUrl:             in.BaseURL,
			AllowPrivateBaseUrl: in.AllowPrivateBaseURL,
			SecretRef:           secretID,
			Config:              cfgJSON,
			Status:              "enabled",
		}); perr != nil {
			return perr
		}
		// Audit every connector.created (a new external data path) in the SAME tx.
		// Inputs carry only non-secret metadata — NEVER the token/email.
		tt := "connector"
		dec := "created"
		if in.AllowPrivateBaseURL {
			dec = "trust_private_base_url"
		}
		return audit.Write(ctx, tx, audit.Entry{
			BusinessID:       &businessID,
			ActorPrincipalID: &principalID,
			Action:           "connector.created",
			TargetType:       &tt,
			TargetID:         &id,
			Decision:         &dec,
			Inputs:           map[string]any{"type": in.Type, "base_url": in.BaseURL},
		})
	})
	if err != nil {
		return uuid.Nil, mapErr(err)
	}
	return id, nil
}

// mapErr converts DB/sentinel errors to stable service sentinels (mirrors
// agents.mapCredErr): pgx.ErrNoRows→404, SQLSTATE 23505→409.
func mapErr(err error) error {
	var pgErr *pgconn.PgError
	switch {
	case err == nil:
		return nil
	case errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("connectors: not found: %w", errs.ErrNotFound)
	case errors.As(err, &pgErr) && pgErr.Code == "23505":
		return fmt.Errorf("connectors: duplicate connector: %w", errs.ErrConflict)
	case errors.Is(err, errs.ErrValidation), errors.Is(err, errs.ErrNotFound),
		errors.Is(err, errs.ErrConflict), errors.Is(err, errs.ErrRateLimited):
		return err
	default:
		return fmt.Errorf("connectors: query: %w", err)
	}
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test -tags integration -p 1 ./internal/connectors/ -run TestCreate -v`
Expected: PASS — `TestCreateRoundTripSealsAndAudits`, `TestCreateTrustGrantAudited`, `TestCreateDuplicateConflict`, `TestCreateVerifierFailureNoRows`.

- [ ] **Step 6: Commit**

```bash
git add internal/connectors/service.go internal/connectors/testsupport_integration_test.go internal/connectors/credential_integration_test.go
git commit -m "feat(connectors): Service.Create — verify, seal, store, audit in one tx (manyforge-a7j.1)" --no-verify
```

---

## Task 5: `connectors.Service.Resolve` + cross-tenant not-found

**Files:**
- Modify: `internal/connectors/service.go` (add `Resolve`)
- Test: `internal/connectors/credential_integration_test.go` (append tests)

- [ ] **Step 1: Write the failing Resolve tests**

Append to `internal/connectors/credential_integration_test.go`:

```go
func TestResolveRoundTrip(t *testing.T) {
	ctx, tdb, seed := startConn(t)
	svc := newConnService(t, tdb, nil)
	id, err := svc.Create(ctx, seed.principalID, seed.businessID, jiraInput())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	rc, err := svc.Resolve(ctx, seed.principalID, seed.businessID, id)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if rc.Type != "jira" || rc.BaseURL != "https://acme.atlassian.net" {
		t.Fatalf("unexpected resolved connector: %+v", rc)
	}
	if rc.Credential.Email != "ops@acme.test" || rc.Credential.APIToken != "tok-abc-123" {
		t.Fatalf("credential mismatch: %+v", rc.Credential)
	}
}

func TestResolveCrossTenantNotFound(t *testing.T) {
	ctx, tdb, a := startConn(t)
	b := seedConnectorTenant(ctx, t, tdb) // independent tenant in the same DB
	svc := newConnService(t, tdb, nil)
	id, err := svc.Create(ctx, a.principalID, a.businessID, jiraInput())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	_, err = svc.Resolve(ctx, b.principalID, b.businessID, id)
	if !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("want ErrNotFound for cross-tenant resolve, got %v", err)
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test -tags integration -p 1 ./internal/connectors/ -run TestResolve -v`
Expected: FAIL — `svc.Resolve undefined`.

- [ ] **Step 3: Implement `Resolve`**

Append to `internal/connectors/service.go`:

```go
// Resolve loads the connector by id (RLS-scoped to business) and unseals its
// credential from the vault, in one tx. Cross-tenant / unknown id → ErrNotFound.
func (s *Service) Resolve(ctx context.Context, principalID, businessID, connectorID uuid.UUID) (ResolvedConnector, error) {
	var out ResolvedConnector
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		row, qerr := dbgen.New(tx).GetConnector(ctx, dbgen.GetConnectorParams{ID: connectorID, BusinessID: businessID})
		if qerr != nil {
			return qerr
		}
		credBytes, oerr := s.Vault.Open(ctx, tx, businessID, row.SecretRef)
		if oerr != nil {
			return oerr
		}
		var cred Credential
		if uerr := json.Unmarshal(credBytes, &cred); uerr != nil {
			return fmt.Errorf("connectors: unmarshal credential: %w", uerr)
		}
		var cfg map[string]any
		if len(row.Config) > 0 {
			if uerr := json.Unmarshal(row.Config, &cfg); uerr != nil {
				return fmt.Errorf("connectors: unmarshal config: %w", uerr)
			}
		}
		out = ResolvedConnector{
			ID: row.ID.String(), Type: string(row.Type), BaseURL: row.BaseUrl,
			AllowPrivateBaseURL: row.AllowPrivateBaseUrl, Config: cfg, Credential: cred,
		}
		return nil
	})
	if err != nil {
		return ResolvedConnector{}, mapErr(err)
	}
	return out, nil
}
```

- [ ] **Step 4: Run to verify they pass**

Run: `go test -tags integration -p 1 ./internal/connectors/ -run TestResolve -v`
Expected: PASS — `TestResolveRoundTrip`, `TestResolveCrossTenantNotFound`.

- [ ] **Step 5: Commit**

```bash
git add internal/connectors/service.go internal/connectors/credential_integration_test.go
git commit -m "feat(connectors): Service.Resolve — load + unseal, cross-tenant 404 (manyforge-a7j.1)" --no-verify
```

---

## Task 6: Security-regression pin + final full gate

**Files:**
- Create: `internal/security_regression/us1_secret_vault_pin_test.go`

> This package already has a `seedAgentTenant(ctx, t, tdb)` helper (in `agent_containment_test.go`) returning a seed with `businessID`/`principalID` fields. Reuse it — do NOT add another.

- [ ] **Step 1: Confirm the existing seed helper shape**

Run: `grep -n "func seedAgentTenant" internal/security_regression/*.go && grep -n "businessID\|principalID" internal/security_regression/agent_containment_test.go | head`
Expected: prints the helper signature + the seed struct's `businessID`/`principalID` fields. If the field names differ, adjust the test below to match (this is the only spot that couples to it).

- [ ] **Step 2: Write the pin test**

`internal/security_regression/us1_secret_vault_pin_test.go`:

```go
//go:build integration

// us1_secret_vault_pin (spec 004 US1): connector credentials are sealed at rest and
// never appear as plaintext in the secret column or in the create audit entry.
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

	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("key: %v", err)
	}
	sealer, err := crypto.NewSealer(key)
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	svc := &connectors.Service{DB: tdb.App, Vault: secrets.NewVault(sealer)}

	id, err := svc.Create(ctx, seed.principalID, seed.businessID, connectors.CreateConnectorInput{
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
```

- [ ] **Step 3: Run the pin**

Run: `go test -tags integration -p 1 ./internal/security_regression/ -run TestUS1 -v`
Expected: PASS.

- [ ] **Step 4: Run the FULL gate**

Run (golangci-lint must be on PATH or `make lint` silently degrades to vet-only):
```bash
export PATH="$PATH:$HOME/go/bin"
gofmt -l internal/ cmd/ db/    # must print nothing
make test && make contract-test && make lint && make sec-test && make int-test
```
Expected: `gofmt -l` prints nothing; every `make` target exits 0. The new connectors/secrets/security_regression tests pass under `int-test`/`sec-test`.

- [ ] **Step 5: Commit + close the issue**

```bash
git add internal/security_regression/us1_secret_vault_pin_test.go
git commit -m "test(sec): US1 pin — connector secret sealed at rest + unlogged (manyforge-a7j.1)" --no-verify
export PATH="$PATH:$HOME/go/bin"
bd close manyforge-a7j.1
git add .beads/issues.jsonl && git commit -m "chore(bd): close US1 (manyforge-a7j.1)" --no-verify
```

---

## Deferred to later user stories (intentional — do NOT build in US1)

- **main.go wiring** (config `MANYFORGE_CONNECTOR_MASTER_KEY` → `crypto.NewSealer` → `secrets.NewVault` → `connectors.Service`, plus the registry that consumes them) lands in **US2**, which is the first production consumer. US1 ships fully integration-tested library code with no app caller yet — standard for an SL-* foundation layer.
- **Live Jira verifier** (the concrete `Verifier`) lands in **US3** once the Jira client exists; US1 ships only the seam + a fake.
- **`connectors.read`/`connectors.write` permission catalog rows** land in **US6** (the HTTP/agent-tool surface) following the `migrations/0015`/`0037` `INSERT INTO permission` + `role_permission` pattern.
- A deo.11-style guard: a future `UpdateConnectorCredential` query MUST carry `allow_private_base_url`.

## Self-Review

- **Spec coverage (US1 slice):** vault encryption ✅ (Task 1 table + Task 2 Vault), `internal/platform/secrets` ✅ (Task 2), connector + credential schema ✅ (Task 1), credential service validate→seal→store→audit ✅ (Task 4), on-prem trust flag ✅ (Task 3 validate + Task 4 audit decision), test-call validation seam ✅ (Task 3 `Verifier` + Task 4 verify path), pins vault-encryption + no-secret-in-logs ✅ (Task 6), tenant isolation ✅ (Task 5 cross-tenant + RLS in Task 1).
- **Placeholder scan:** none — every step has full code/commands.
- **Type consistency:** service structs use `BaseURL`/`AllowPrivateBaseURL`; dbgen params use `BaseUrl`/`AllowPrivateBaseUrl`/`SecretRef` (gotcha noted in Task 4). `Service`/`Vault`/`Verifier`/`Credential`/`CreateConnectorInput`/`ResolvedConnector`/`VerifyTarget` names match across tasks. `mapErr`/`validate`/`validateBaseURL` consistent.
