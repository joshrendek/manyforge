# GitHub App Slice 2 — `pull_request` auto-review trigger — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Turn a GitHub `pull_request` webhook into an auto-triggered manyforge code review, authenticated by per-repo GitHub App installation tokens, reusing the existing worker→sandbox→PostReview pipeline.

**Architecture:** App JWT (RS256) → per-repo installation token minted in `runJob` (outside any DB tx, no cache). A `type='github_app'` `repo_connector` (auto-created per repo, nullable `secret_ref`) carries the `installation_id`; `Resolve` returns metadata-only and `runJob` mints the token before building the client. The webhook parses/filters the PR and calls ONE atomic `SECURITY DEFINER` (`github_pr_review_ingest`) that dedups the delivery, rate-caps, ensures the connector, skips same-head, supersedes pending, and inserts a `code_review` under the review agent's own principal. `runJob` needs zero changes to clone/post (the minted `ghs_` token plugs into `rc.Credential.APIToken`), plus: mint, an egress pre-flight, and a claim-time same-head re-check.

**Tech Stack:** Go (chi, pgx/v5, sqlc, golang-migrate, `golang-jwt/v5` RS256, `netsafe`), stdlib `testing` (no testify), `testcontainers` via `internal/platform/db/testdb`.

## Global Constraints
- **Spec:** `docs/superpowers/specs/2026-07-05-github-app-slice2-pr-trigger.md` (v2, fable-reviewed). Slice 3 = full budget, opt-out label, per-install filter config, fork review.
- **Migrations:** next numbers are `0083` (repo_connector github_app), `0084` (context + delivery + ingest + status/guards). Highest existing is `0082`.
- **Module path:** `github.com/manyforge/manyforge` (replace the plan's `manyforge/` prefix).
- **DB function calls via raw pgx**, never sqlc. Every `SECURITY DEFINER` fn: `SET search_path = public`, `REVOKE ALL … FROM PUBLIC`, `GRANT EXECUTE … TO manyforge_app`. Owner-bypasses-RLS (no FORCE RLS).
- **sqlc:** mirror table changes into `db/schema.sql`; `make generate` with **sqlc v1.27.0** (else churn). Making `repo_connector.secret_ref` nullable turns `SecretRef uuid.UUID` into `pgtype.UUID` in dbgen — mechanical edits follow in `Create`/`Resolve`.
- **Fail closed / inert:** github_app connectors only exist when the App is configured (master key set). `runJob`'s mint path must fail with a clear error (not nil-panic) if `Tokens` is nil.
- **Error hygiene:** wrap `errs` sentinels; uniform 202 on the webhook (no oracle); no `err.Error()` to clients; never log/echo the App private key or minted tokens.
- **Commits:** no `Co-Authored-By` trailer. `make test` + `make sec-test` before committing when pins change.
- **bd:** tracked under `manyforge-qpc`.

---

### Task 1: App JWT + installation-token minting (`internal/githubapp`)

**Files:** Create `internal/githubapp/apptoken.go`, `internal/githubapp/apptoken_test.go`; Modify `internal/githubapp/client.go` (add `MintInstallationToken`).

**Interfaces produced:**
- `func AppJWT(appID int64, privateKeyPEM string, now time.Time) (string, error)`
- `func (c *Client) MintInstallationToken(ctx context.Context, installationID int64, appJWT, repoFullName string) (token string, expiresAt time.Time, err error)`
- `type tokenMinter interface { MintInstallationToken(ctx context.Context, installationID int64, appJWT, repoFullName string) (string, time.Time, error) }`
- `type InstallationTokenSource struct { Store appConfigGetter; API tokenMinter; Now func() time.Time }` where `appConfigGetter interface { Get(ctx context.Context) (AppConfig, error) }` (satisfied by `*ConfigStore`)
- `func (s *InstallationTokenSource) Token(ctx context.Context, installationID int64, repoFullName string) (string, error)`
- Sentinel: `var ErrInstallationAuth = …` (wraps `errs.ErrForbidden`) for a mint 401/403/404 (terminal).

- [ ] **Step 1: Failing unit test.** `internal/githubapp/apptoken_test.go`:
```go
package githubapp

import (
    "context"
    "crypto/rand"
    "crypto/rsa"
    "crypto/x509"
    "encoding/json"
    "encoding/pem"
    "net/http"
    "net/http/httptest"
    "testing"
    "time"

    "github.com/golang-jwt/jwt/v5"
)

func testRSAKeyPEM(t *testing.T) string {
    t.Helper()
    k, err := rsa.GenerateKey(rand.Reader, 2048)
    if err != nil { t.Fatal(err) }
    der := x509.MarshalPKCS1PrivateKey(k)
    return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: der}))
}

func TestAppJWTClaims(t *testing.T) {
    pemStr := testRSAKeyPEM(t)
    now := time.Unix(1_700_000_000, 0)
    tok, err := AppJWT(42, pemStr, now)
    if err != nil { t.Fatalf("AppJWT: %v", err) }
    parsed, _, err := jwt.NewParser().ParseUnverified(tok, jwt.MapClaims{})
    if err != nil { t.Fatalf("parse: %v", err) }
    c := parsed.Claims.(jwt.MapClaims)
    if c["iss"] != "42" && c["iss"] != float64(42) { t.Errorf("iss = %v", c["iss"]) }
    iat := int64(c["iat"].(float64)); exp := int64(c["exp"].(float64))
    if iat != now.Add(-60*time.Second).Unix() { t.Errorf("iat not backdated 60s: %d", iat) }
    if exp-iat > 600 { t.Errorf("exp-iat = %d, want <= 600", exp-iat) }
}

func TestMintInstallationTokenPerRepo(t *testing.T) {
    srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        if r.URL.Path != "/app/installations/77/access_tokens" || r.Method != http.MethodPost { t.Errorf("req %s %s", r.Method, r.URL.Path) }
        if r.Header.Get("Authorization") != "Bearer appjwt" { t.Errorf("auth %q", r.Header.Get("Authorization")) }
        var body struct{ Repositories []string `json:"repositories"` }
        _ = json.NewDecoder(r.Body).Decode(&body)
        if len(body.Repositories) != 1 || body.Repositories[0] != "name" { t.Errorf("repos %v", body.Repositories) }
        _ = json.NewEncoder(w).Encode(map[string]any{"token": "ghs_abc", "expires_at": "2026-07-05T13:00:00Z"})
    }))
    defer srv.Close()
    c := &Client{HTTP: srv.Client(), APIBase: srv.URL, WebBase: srv.URL}
    tok, exp, err := c.MintInstallationToken(context.Background(), 77, "appjwt", "owner/name")
    if err != nil { t.Fatalf("mint: %v", err) }
    if tok != "ghs_abc" || exp.IsZero() { t.Fatalf("tok=%q exp=%v", tok, exp) }
}
```

- [ ] **Step 2: Run → FAIL** (`go test ./internal/githubapp/ -run 'TestAppJWT|TestMintInstallation'`).

- [ ] **Step 3: Implement.** Add to `internal/githubapp/client.go`:
```go
func (c *Client) MintInstallationToken(ctx context.Context, installationID int64, appJWT, repoFullName string) (string, time.Time, error) {
    name := repoFullName
    if i := strings.LastIndex(repoFullName, "/"); i >= 0 { name = repoFullName[i+1:] }
    payload, _ := json.Marshal(map[string]any{"repositories": []string{name}})
    u := fmt.Sprintf("%s/app/installations/%d/access_tokens", c.APIBase, installationID)
    req, _ := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(payload))
    req.Header.Set("Authorization", "Bearer "+appJWT)
    req.Header.Set("Accept", "application/vnd.github+json")
    var r struct {
        Token     string    `json:"token"`
        ExpiresAt time.Time `json:"expires_at"`
    }
    if err := c.do(req, &r); err != nil { return "", time.Time{}, err }
    if r.Token == "" { return "", time.Time{}, fmt.Errorf("github: empty installation token") }
    return r.Token, r.ExpiresAt, nil
}
```
(Add imports `bytes`, `strings`, `time` if missing. Confirm `Client.do` returns a distinguishable error on non-2xx — if `do` can surface the status, map 401/403/404 to `ErrInstallationAuth`; otherwise the caller treats any mint error as terminal.)

`internal/githubapp/apptoken.go`:
```go
package githubapp

import (
    "context"
    "fmt"
    "time"

    "github.com/golang-jwt/jwt/v5"
    "manyforge/internal/platform/errs"
)

var ErrInstallationAuth = fmt.Errorf("github app installation auth failed: %w", errs.ErrForbidden)

func AppJWT(appID int64, privateKeyPEM string, now time.Time) (string, error) {
    key, err := jwt.ParseRSAPrivateKeyFromPEM([]byte(privateKeyPEM))
    if err != nil { return "", fmt.Errorf("parse app private key: %w", err) }
    claims := jwt.RegisteredClaims{
        Issuer:    fmt.Sprintf("%d", appID),
        IssuedAt:  jwt.NewNumericDate(now.Add(-60 * time.Second)), // clock-skew backdate
        ExpiresAt: jwt.NewNumericDate(now.Add(9 * time.Minute)),   // <= 10m
    }
    return jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(key)
}

type tokenMinter interface {
    MintInstallationToken(ctx context.Context, installationID int64, appJWT, repoFullName string) (string, time.Time, error)
}
type appConfigGetter interface {
    Get(ctx context.Context) (AppConfig, error)
}

// InstallationTokenSource mints a fresh per-repo installation token on every call
// (no cache — the only caller is runJob, ~once per review; a cached token could
// expire mid-job and 401 PostReview, re-billing the whole run).
type InstallationTokenSource struct {
    Store appConfigGetter
    API   tokenMinter
    Now   func() time.Time
}

func (s *InstallationTokenSource) Token(ctx context.Context, installationID int64, repoFullName string) (string, error) {
    cfg, err := s.Store.Get(ctx)
    if err != nil { return "", fmt.Errorf("github app config: %w", err) }
    appJWT, err := AppJWT(cfg.AppID, cfg.PrivateKeyPEM, s.Now())
    if err != nil { return "", err }
    tok, _, err := s.API.MintInstallationToken(ctx, installationID, appJWT, repoFullName)
    if err != nil { return "", err }
    return tok, nil
}
```

- [ ] **Step 4: Run → PASS.** Add an `InstallationTokenSource.Token` unit test with a fake `tokenMinter` + a stub `appConfigGetter` (returns an `AppConfig` with the test RSA PEM) asserting it mints and returns the token. **Step 5: Commit** (`apptoken.go`, `apptoken_test.go`, `client.go`): `feat(011): App JWT + per-repo installation-token minting (manyforge-qpc)`.

---

### Task 2: App-backed `repo_connector` (migration + `repo_service.go`)

**Files:** Create `migrations/0083_repo_connector_github_app.{up,down}.sql`; Modify `db/schema.sql`, `db/query/repo_connector.sql` (if the connector insert/get needs a nullable secret_ref shape), `internal/connectors/repo_service.go`, `internal/connectors/repo_connector.go` (summary DTO); regenerate dbgen. Test: `internal/connectors/repo_service_test.go` / a `//go:build integration` test.

**Interfaces produced:** `RepoConnectorSummary.AutoManaged bool`; `Resolve` returns a github_app connector with `Credential.APIToken=""` + `Config["installation_id"]`; `Delete` rejects `type='github_app'`; `knownRepoConnectorTypes["github_app"]=true`.

- [ ] **Step 1: Migration.** `migrations/0083_repo_connector_github_app.up.sql`:
```sql
-- Allow app-backed repo connectors (no stored PAT): type='github_app', secret_ref NULL,
-- config carries the installation_id. One app-backed connector per (business, repo).
ALTER TABLE repo_connector DROP CONSTRAINT repo_connector_type_chk;
ALTER TABLE repo_connector ADD CONSTRAINT repo_connector_type_chk CHECK (type IN ('github', 'github_app'));
ALTER TABLE repo_connector ALTER COLUMN secret_ref DROP NOT NULL;
ALTER TABLE repo_connector ADD CONSTRAINT repo_connector_secret_ref_chk CHECK (
    (type = 'github'     AND secret_ref IS NOT NULL) OR
    (type = 'github_app' AND secret_ref IS NULL AND config ? 'installation_id')
);
CREATE UNIQUE INDEX repo_connector_github_app_repo_uq ON repo_connector (business_id, repo) WHERE type = 'github_app';
```
`…down.sql`: drop the index + the secret_ref CHECK, `ALTER COLUMN secret_ref SET NOT NULL`, restore the type CHECK to `('github')`. Update `db/schema.sql` (the `repo_connector` table: `secret_ref uuid` nullable + the new CHECK + index).

- [ ] **Step 2: Regenerate sqlc (secret_ref → pgtype.UUID).** Run `make generate` (sqlc v1.27.0). `RepoConnector.SecretRef` becomes `pgtype.UUID`; `InsertRepoConnectorParams.SecretRef` too. Expect churn ONLY in `repo_connector.sql.go`.

- [ ] **Step 3: Fix `Create`/`Resolve` for the nullable secret_ref (mechanical).** In `repo_service.go` `Create` (L63-74), wrap `SecretRef: pgtype.UUID{Bytes: secretID, Valid: true}`. In `Resolve` (L105-116), the existing `github` path must read `row.SecretRef.Bytes` (a `uuid.UUID`) when `row.SecretRef.Valid`. Add the github_app branch (below).

- [ ] **Step 4: Failing test for the github_app Resolve branch + Delete block.** `internal/connectors/repo_service_github_app_test.go` (`//go:build integration`):
```go
//go:build integration

package connectors_test

import (
    "context"
    "errors"
    "testing"

    "github.com/google/uuid"
    "github.com/jackc/pgx/v5"
    "manyforge/internal/connectors"
    "manyforge/internal/platform/db/testdb"
    "manyforge/internal/platform/errs"
)

func TestResolveGithubAppReturnsMetadataNoMint(t *testing.T) {
    ctx := context.Background()
    tdb, err := testdb.Start(ctx); if err != nil { t.Fatal(err) }; defer tdb.Close(ctx)
    biz, member := seedBusinessMember(t, ctx, tdb) // mirror existing connectors integration seeds
    // Insert an app-backed connector via the super pool (bypassing Create, which requires a PAT):
    connID := uuid.New()
    mustExec(t, ctx, tdb, `INSERT INTO repo_connector (id, business_id, tenant_root_id, type, display_name, base_url, repo, secret_ref, config, status)
        SELECT $1, b.id, b.tenant_root_id, 'github_app', 'owner/name', 'https://api.github.com', 'owner/name', NULL, '{"installation_id": 77}'::jsonb, 'enabled'
        FROM business b WHERE b.id=$2`, connID, biz)

    svc := &connectors.RepoConnectorService{DB: tdb.App, Vault: nil} // Vault must NOT be called for github_app
    rc, err := svc.Resolve(ctx, member, biz, connID)
    if err != nil { t.Fatalf("Resolve: %v", err) }
    if rc.Type != "github_app" || rc.Credential.APIToken != "" { t.Fatalf("got %+v", rc) }
    if rc.Config["installation_id"] == nil { t.Fatalf("missing installation_id in config") }

    // Delete is blocked for github_app.
    if err := svc.Delete(ctx, member, biz, connID); !errors.Is(err, errs.ErrValidation) {
        t.Fatalf("Delete github_app = %v, want ErrValidation", err)
    }
}
```
(Write `seedBusinessMember`/`mustExec` mirroring the existing connectors `//go:build integration` seed helpers.)

- [ ] **Step 5: Run → FAIL.**

- [ ] **Step 6: Implement.** In `repo_service.go`:
```go
var knownRepoConnectorTypes = map[string]bool{"github": true, "github_app": true}
```
`Resolve` — add before the `Vault.Open` call (after loading `row` + unmarshalling `cfg`; reorder so `cfg` is parsed first):
```go
        var cfg map[string]any
        if len(row.Config) > 0 {
            if uerr := json.Unmarshal(row.Config, &cfg); uerr != nil {
                return fmt.Errorf("repo_connectors: unmarshal config: %w", uerr)
            }
        }
        if row.Type == "github_app" {
            // App-backed: no stored credential; runJob mints a per-repo installation
            // token from cfg["installation_id"]. Return metadata only.
            out = ResolvedRepoConnector{ID: row.ID.String(), Type: row.Type, BaseURL: row.BaseUrl,
                Repo: row.Repo, AllowPrivateBaseURL: row.AllowPrivateBaseUrl, Config: cfg}
            return nil
        }
        credBytes, oerr := s.Vault.Open(ctx, tx, businessID, row.SecretRef.Bytes)
        // … existing github path unchanged …
```
`Delete` — block github_app before the DELETE:
```go
func (s *RepoConnectorService) Delete(ctx context.Context, principalID, businessID, id uuid.UUID) error {
    return mapRepoErr(s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
        row, err := dbgen.New(tx).GetRepoConnector(ctx, id)
        if err != nil { return err } // pgx.ErrNoRows → ErrNotFound via mapRepoErr
        if row.Type == "github_app" {
            return fmt.Errorf("repo_connectors: github_app connectors are managed by the GitHub App install: %w", errs.ErrValidation)
        }
        n, err := dbgen.New(tx).DeleteRepoConnector(ctx, dbgen.DeleteRepoConnectorParams{ID: id, BusinessID: businessID})
        if err != nil { return fmt.Errorf("connectors: delete repo connector: %w", err) }
        if n == 0 { return errs.ErrNotFound }
        return nil
    }))
}
```
`RepoConnectorSummary` — add `AutoManaged bool json:"auto_managed"`; in `List`, set `AutoManaged: r.Type == "github_app"`.

- [ ] **Step 7: Run → PASS** (`go test -tags integration ./internal/connectors/ -run TestResolveGithubApp`). **Step 8: `make generate` diff is only repo_connector; `go build ./...` green. Commit** (migration, schema.sql, dbgen, repo_service.go, repo_connector.go, test): `feat(011): app-backed github_app repo_connector — nullable secret_ref, metadata Resolve, delete-blocked (manyforge-qpc)`.

---

### Task 3: DEFINERs — installation context, delivery dedup, atomic PR-review ingest

**Files:** Create `migrations/0084_github_pr_review.{up,down}.sql`, `internal/githubapp/prreview.go` (the `PRReviewEnqueuer` raw-pgx wrapper); Modify `db/schema.sql`. Test: `internal/githubapp/prreview_integration_test.go` (`//go:build integration`).

**Interfaces produced:**
- DEFINERs `github_installation_context(bigint)`, `github_ingest_delivery`-folded-into `github_pr_review_ingest(...)`.
- `type PRReviewEnqueuer struct { DB txRunner }` with:
  - `ResolveInstallation(ctx, installationID int64) (InstallationContext, bool, error)` (calls `github_installation_context`)
  - `IngestPRReview(ctx, in PRReviewInput) (reviewID uuid.UUID, ok bool, err error)` (calls `github_pr_review_ingest`; ok=false on replay/rate/dup)
- `type InstallationContext struct { BusinessID, TenantRootID, AgentID, AgentPrincipalID uuid.UUID; AgentEnabled, Enabled, Suspended bool }`
- `type PRReviewInput struct { InstallationID int64; DeliveryID, Repo string; PRNumber int; HeadSHA string; BusinessID, TenantRootID, AgentID, AgentPrincipalID uuid.UUID }`

- [ ] **Step 1: Migration.** `migrations/0084_github_pr_review.up.sql`:
```sql
-- Extend code_review status for supersede (Slice 2 dedup) + guard requeue/fail.
ALTER TABLE code_review DROP CONSTRAINT code_review_status_chk;
ALTER TABLE code_review ADD CONSTRAINT code_review_status_chk CHECK (status IN ('pending','running','succeeded','failed','superseded'));

-- Principal-less installation → (business, agent, agent principal) resolution for the webhook.
CREATE FUNCTION github_installation_context(p_installation_id bigint)
RETURNS TABLE(business_id uuid, tenant_root_id uuid, agent_id uuid, agent_principal_id uuid,
              agent_enabled boolean, enabled boolean, suspended boolean)
LANGUAGE sql SECURITY DEFINER SET search_path = public AS $$
    SELECT gi.business_id, gi.tenant_root_id, gi.agent_id, a.principal_id,
           COALESCE(a.enabled, false), gi.enabled, gi.suspended_at IS NOT NULL
    FROM github_app_installation gi
    LEFT JOIN agent a ON a.id = gi.agent_id
    WHERE gi.installation_id = p_installation_id AND gi.deleted_at IS NULL;
$$;
REVOKE ALL ON FUNCTION github_installation_context(bigint) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION github_installation_context(bigint) TO manyforge_app;

-- Delivery dedup table (tenantless — installation is the key pre-link).
CREATE TABLE github_webhook_delivery (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    installation_id bigint NOT NULL,
    external_delivery_id text NOT NULL,
    received_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (installation_id, external_delivery_id)
);
GRANT SELECT, INSERT, DELETE ON github_webhook_delivery TO manyforge_app;

-- One atomic principal-less DEFINER: dedup → rate cap → ensure connector → same-head skip
-- → pending-supersede → insert. Returns the new review id, or NULL on replay/rate/dup.
CREATE FUNCTION github_pr_review_ingest(
    p_installation_id bigint, p_delivery_id text, p_business_id uuid, p_tenant_root uuid,
    p_agent_id uuid, p_agent_principal uuid, p_repo text, p_pr_number int, p_head_sha text
) RETURNS uuid LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE v_conn uuid; v_rows int; v_review uuid; v_cap constant int := 30;
BEGIN
    -- ① delivery dedup (skip when delivery id empty)
    IF p_delivery_id IS NOT NULL AND p_delivery_id <> '' THEN
        INSERT INTO github_webhook_delivery (installation_id, external_delivery_id)
        VALUES (p_installation_id, p_delivery_id) ON CONFLICT DO NOTHING;
        GET DIAGNOSTICS v_rows = ROW_COUNT;
        IF v_rows = 0 THEN RETURN NULL; END IF; -- replay
    END IF;
    -- ② hourly rate cap per installation
    IF (SELECT count(*) FROM code_review cr JOIN repo_connector rc ON rc.id = cr.repo_connector_id
        WHERE rc.type='github_app' AND (rc.config->>'installation_id')::bigint = p_installation_id
          AND cr.created_at > now() - interval '1 hour') >= v_cap THEN
        RETURN NULL;
    END IF;
    -- ③ ensure app-backed connector (race-safe: ON CONFLICT + FOR UPDATE)
    INSERT INTO repo_connector (id, business_id, tenant_root_id, type, display_name, base_url, repo,
        allow_private_base_url, config, secret_ref, status, created_at, updated_at)
    VALUES (gen_random_uuid(), p_business_id, p_tenant_root, 'github_app', p_repo, 'https://api.github.com',
        p_repo, false, jsonb_build_object('installation_id', p_installation_id), NULL, 'enabled', now(), now())
    ON CONFLICT (business_id, repo) WHERE type='github_app' DO NOTHING;
    SELECT id INTO v_conn FROM repo_connector
        WHERE business_id=p_business_id AND repo=p_repo AND type='github_app' FOR UPDATE;
    -- ④ same-head skip
    IF EXISTS (SELECT 1 FROM code_review WHERE repo_connector_id=v_conn AND pr_number=p_pr_number
               AND head_sha=p_head_sha AND status IN ('pending','running','succeeded')) THEN
        RETURN NULL;
    END IF;
    -- ⑤ pending-supersede (a new push cancels an unstarted review for the PR)
    UPDATE code_review SET status='superseded', updated_at=now()
        WHERE repo_connector_id=v_conn AND pr_number=p_pr_number AND status='pending';
    -- ⑥ insert
    INSERT INTO code_review (id, business_id, tenant_root_id, repo_connector_id, pr_number, head_sha,
        status, principal_id, agent_id, model, created_at, updated_at)
    VALUES (gen_random_uuid(), p_business_id, p_tenant_root, v_conn, p_pr_number, p_head_sha,
        'pending', p_agent_principal, p_agent_id, '', now(), now())
    RETURNING id INTO v_review;
    RETURN v_review;
END; $$;
REVOKE ALL ON FUNCTION github_pr_review_ingest(bigint, text, uuid, uuid, uuid, uuid, text, int, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION github_pr_review_ingest(bigint, text, uuid, uuid, uuid, uuid, text, int, text) TO manyforge_app;

-- Guard requeue/fail so a superseded row can't be resurrected.
CREATE OR REPLACE FUNCTION requeue_code_review(p_id uuid, p_delay_seconds int, p_last_error text) RETURNS void
LANGUAGE sql VOLATILE SECURITY DEFINER SET search_path = public AS $$
    UPDATE code_review SET status='pending', run_after=now()+make_interval(secs=>p_delay_seconds),
        lease_expires_at=NULL, last_error=p_last_error, updated_at=now()
    WHERE id=p_id AND status='running';
$$;
CREATE OR REPLACE FUNCTION fail_code_review(p_id uuid, p_last_error text) RETURNS void
LANGUAGE sql VOLATILE SECURITY DEFINER SET search_path = public AS $$
    UPDATE code_review SET status='failed', lease_expires_at=NULL, last_error=p_last_error, updated_at=now()
    WHERE id=p_id AND status='running';
$$;
```
`…down.sql`: drop `github_pr_review_ingest`, `github_installation_context`, `github_webhook_delivery`; restore requeue/fail without the status guard; restore the status CHECK without `'superseded'`. Mirror `github_webhook_delivery` into `db/schema.sql`. (Confirm `CREATE OR REPLACE` matches the original requeue/fail signatures exactly from 0073.)

- [ ] **Step 2: `PRReviewEnqueuer` (raw pgx).** `internal/githubapp/prreview.go`:
```go
package githubapp

import (
    "context"
    "errors"
    "fmt"

    "github.com/google/uuid"
    "github.com/jackc/pgx/v5"
)

type InstallationContext struct {
    BusinessID, TenantRootID, AgentID, AgentPrincipalID uuid.UUID
    AgentEnabled, Enabled, Suspended bool
}
type PRReviewInput struct {
    InstallationID int64
    DeliveryID, Repo string
    PRNumber int
    HeadSHA string
    BusinessID, TenantRootID, AgentID, AgentPrincipalID uuid.UUID
}
type PRReviewEnqueuer struct{ DB txRunner }

func (e *PRReviewEnqueuer) ResolveInstallation(ctx context.Context, installationID int64) (InstallationContext, bool, error) {
    var c InstallationContext
    found := false
    err := e.DB.WithTx(ctx, func(tx pgx.Tx) error {
        row := tx.QueryRow(ctx, `SELECT business_id, tenant_root_id, agent_id, agent_principal_id, agent_enabled, enabled, suspended
            FROM github_installation_context($1)`, installationID)
        var bid, trid, aid, apid pgtype.UUID // nullable business/agent when unlinked
        if err := row.Scan(&bid, &trid, &aid, &apid, &c.AgentEnabled, &c.Enabled, &c.Suspended); err != nil {
            if errors.Is(err, pgx.ErrNoRows) { return nil }
            return err
        }
        found = true
        c.BusinessID = fromPg(bid); c.TenantRootID = fromPg(trid); c.AgentID = fromPg(aid); c.AgentPrincipalID = fromPg(apid)
        return nil
    })
    return c, found, err
}

func (e *PRReviewEnqueuer) IngestPRReview(ctx context.Context, in PRReviewInput) (uuid.UUID, bool, error) {
    var id pgtype.UUID
    err := e.DB.WithTx(ctx, func(tx pgx.Tx) error {
        return tx.QueryRow(ctx, `SELECT github_pr_review_ingest($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
            in.InstallationID, in.DeliveryID, in.BusinessID, in.TenantRootID, in.AgentID,
            in.AgentPrincipalID, in.Repo, in.PRNumber, in.HeadSHA).Scan(&id)
    })
    if err != nil { return uuid.Nil, false, fmt.Errorf("ingest pr review: %w", err) }
    if !id.Valid { return uuid.Nil, false, nil } // replay/rate/dup
    return id.Bytes, true, nil
}

func fromPg(u pgtype.UUID) uuid.UUID { if u.Valid { return u.Bytes }; return uuid.Nil }
```
(Add `github.com/jackc/pgx/v5/pgtype` import. `txRunner` is the `WithTx` interface already in the package from Slice 1.)

- [ ] **Step 3: Failing integration test.** `internal/githubapp/prreview_integration_test.go` (`//go:build integration`): seed a linked installation (business + agent + membership + `github_app_installation` linked), then assert: `ResolveInstallation` returns the business/agent/agent-principal; `IngestPRReview` creates an app-backed `repo_connector` + a pending `code_review` (ok=true); a second call with the **same delivery id** → ok=false (replay); a call with a **new delivery, same (repo,pr,head_sha)** → ok=false (dup); a call with a **new head + a still-pending prior review** supersedes the prior (prior status='superseded'); >30 reviews in an hour → ok=false (rate). Mirror Slice-1 `seedTwoBusinesses`/`installations_integration_test.go`.

- [ ] **Step 4: Run → PASS** (`go test -tags integration ./internal/githubapp/ -run TestPRReviewIngest`). **Step 5: Commit** (migration 0084, schema.sql, prreview.go, test): `feat(011): github_installation_context + atomic github_pr_review_ingest DEFINER (dedup/rate/supersede) + enqueuer (manyforge-qpc)`.

---

### Task 4: `pull_request` webhook handler

**Files:** Create `internal/githubapp/pullrequest.go`, `internal/githubapp/pullrequest_test.go`, `internal/security_regression/github_pr_trigger_pin_test.go`; Modify `internal/githubapp/webhook.go` (route the event), `internal/githubapp/handler.go` (add the `PRReviews` field).

**Interfaces produced:** `Handler.PRReviews prReviewOps` (interface: `ResolveInstallation` + `IngestPRReview`, satisfied by `*PRReviewEnqueuer`); `func (h *Handler) handlePullRequestEvent(r *http.Request, body []byte)`.

- [ ] **Step 1: Failing test.** `internal/githubapp/pullrequest_test.go` — table-driven over the filter matrix + the happy path, using a fake `prReviewOps` recording calls and a `stubStore` with `AppID`/`WebhookSecret`. Assert: draft/bot-author/fork/non-trigger-action → **no** `IngestPRReview` call (202); unlinked/suspended/disabled-agent context → no ingest (202); a valid opened PR → `ResolveInstallation` then `IngestPRReview` with the parsed repo/number/head_sha; a bad signature → 401 (reuse the Slice-1 webhook test harness). Include the exact JSON payloads (opened, draft, fork via `head.repo.id != base.repo.id`, bot via `pull_request.user.type=="Bot"`).

- [ ] **Step 2: Run → FAIL.**

- [ ] **Step 3: Implement.** Add to `handler.go`: `PRReviews prReviewOps` field + the interface:
```go
type prReviewOps interface {
    ResolveInstallation(ctx context.Context, installationID int64) (InstallationContext, bool, error)
    IngestPRReview(ctx context.Context, in PRReviewInput) (uuid.UUID, bool, error)
}
```
`webhook.go` `handleWebhook`, after the installation branch:
```go
    } else if r.Header.Get("X-GitHub-Event") == "pull_request" {
        h.handlePullRequestEvent(r, body)
    }
```
`internal/githubapp/pullrequest.go`:
```go
package githubapp

import (
    "encoding/json"
    "net/http"
)

type pullRequestEvent struct {
    Action       string `json:"action"`
    Number       int    `json:"number"`
    Installation struct{ ID int64 `json:"id"` } `json:"installation"`
    Repository   struct{ FullName string `json:"full_name"` } `json:"repository"`
    PullRequest  struct {
        Draft bool `json:"draft"`
        User  struct{ Type string `json:"type"` } `json:"user"`
        Head  struct {
            SHA  string `json:"sha"`
            Repo *struct{ ID int64 `json:"id"` } `json:"repo"`
        } `json:"head"`
        Base struct{ Repo struct{ ID int64 `json:"id"` } `json:"repo"` } `json:"base"`
    } `json:"pull_request"`
}

func prTriggerAction(a string) bool {
    switch a { case "opened", "synchronize", "reopened", "ready_for_review": return true }
    return false
}

func (h *Handler) handlePullRequestEvent(r *http.Request, body []byte) {
    var ev pullRequestEvent
    if err := json.Unmarshal(body, &ev); err != nil || ev.Installation.ID == 0 { return }
    if !prTriggerAction(ev.Action) { return }
    // Filters (no DB write / no delivery consumption).
    if ev.PullRequest.Draft { h.log(r.Context(), "pr skipped: draft", nil); return }
    if ev.PullRequest.User.Type == "Bot" { h.log(r.Context(), "pr skipped: bot author", nil); return }
    if ev.PullRequest.Head.Repo == nil || ev.PullRequest.Head.Repo.ID != ev.PullRequest.Base.Repo.ID {
        h.log(r.Context(), "pr skipped: fork", nil); return
    }
    ic, ok, err := h.PRReviews.ResolveInstallation(r.Context(), ev.Installation.ID)
    if err != nil { h.log(r.Context(), "installation context", err); return }
    if !ok || ic.BusinessID == (uuid.UUID{}) || ic.AgentID == (uuid.UUID{}) || !ic.Enabled || ic.Suspended || !ic.AgentEnabled {
        h.log(r.Context(), "pr skipped: install not linked/enabled", nil); return
    }
    if _, _, err := h.PRReviews.IngestPRReview(r.Context(), PRReviewInput{
        InstallationID: ev.Installation.ID, DeliveryID: r.Header.Get("X-GitHub-Delivery"),
        Repo: ev.Repository.FullName, PRNumber: ev.Number, HeadSHA: ev.PullRequest.Head.SHA,
        BusinessID: ic.BusinessID, TenantRootID: ic.TenantRootID, AgentID: ic.AgentID, AgentPrincipalID: ic.AgentPrincipalID,
    }); err != nil {
        h.log(r.Context(), "pr enqueue", err)
    }
}
```
(Add the `uuid` import.)

- [ ] **Step 4: Run → PASS.** **Step 5: Security-regression pin** (untagged) `github_pr_trigger_pin_test.go`: assert `pullrequest.go` contains the fork check (`Head.Repo == nil || … != … Base.Repo.ID`), the bot-author check (`User.Type == "Bot"`), the draft skip, and that the ingest carries `X-GitHub-Delivery`. **Step 6: Commit** (pullrequest.go, webhook.go, handler.go, tests, pin): `feat(011): pull_request webhook handler — filter + installation-context + atomic ingest (manyforge-qpc)`.

---

### Task 5: `runJob` mint + egress + claim-time re-check + wiring + end-to-end

**Files:** Modify `internal/agents/coding/service.go` (`runJob`, `CodeReviewService` struct), `cmd/manyforge/main.go` (wire `Tokens` + `PRReviews`), the OpenAPI contract (`POST /api/v1/github/webhook` already exists from Slice 1 — no new route; add nothing unless a new endpoint appears). Test: `internal/agents/coding/service_github_app_test.go` (`//go:build integration`) + unit tests for the runJob branches.

**Interfaces produced:** `CodeReviewService.Tokens installationTokenSource` (interface `Token(ctx, installationID int64, repo string) (string, error)`, satisfied by `*githubapp.InstallationTokenSource`).

- [ ] **Step 1: Failing unit test** for the three `runJob` additions (mint for github_app, egress pre-flight, claim-time re-check) using a `FakeRunner` + a fake `Tokens` + a fake repo resolver returning a `type='github_app'` connector, asserting: the minted token reaches `BasicAuthHeader`/the client; a provider host outside `EgressAllow` fails fast (terminal, no sandbox launch); when a succeeded review already exists for `(connector, pr, head)`, `PostReview` is NOT called. (Adapt the existing `service_test.go` harness.)

- [ ] **Step 2: Run → FAIL.**

- [ ] **Step 3: Implement `runJob` changes.** In `internal/agents/coding/service.go`, between `Repos.Resolve` (L253) and `github.NewFactory` (L256):
```go
    // App-backed connector: mint a fresh per-repo installation token (outside any DB tx)
    // and set it as the connector credential — runJob's clone/post paths are otherwise
    // unchanged (the ghs_ token is a drop-in for a PAT). NewFactory requires a non-empty
    // token, so this must precede it.
    if rc.Type == "github_app" {
        if s.Tokens == nil {
            return s.failJob(ctx, job.PrincipalID, job.BusinessID, job.ID, job.PRNumber,
                fmt.Errorf("coding: github_app connector but no installation-token source configured: %w", errs.ErrValidation))
        }
        instID, ok := installationIDFromConfig(rc.Config)
        if !ok {
            return s.failJob(ctx, job.PrincipalID, job.BusinessID, job.ID, job.PRNumber,
                fmt.Errorf("coding: github_app connector missing installation_id: %w", errs.ErrValidation))
        }
        tok, terr := s.Tokens.Token(ctx, instID, rc.Repo)
        if terr != nil { // mint failure (incl. suspended/deleted install) is terminal
            return s.failJob(ctx, job.PrincipalID, job.BusinessID, job.ID, job.PRNumber,
                fmt.Errorf("coding: mint installation token: %w", terr))
        }
        rc.Credential.APIToken = tok
    }
```
After `Creds.Resolve` + the `cred.Host()` check (L268), add the egress pre-flight (M5):
```go
    if !isLocalProvider(cred.Provider) && !s.EgressAllow.Allows(cred.Host()) {
        return s.failJob(ctx, job.PrincipalID, job.BusinessID, job.ID, job.PRNumber,
            fmt.Errorf("coding: provider host %q not in sandbox egress allowlist: %w", cred.Host(), errs.ErrValidation))
    }
```
After `FetchPR` returns `pr` (find the `pr, err := conn.FetchPR(...)` line), add the claim-time re-check before the review runs:
```go
    // Claim-time same-head re-check: a rapid-push sibling may have already reviewed this
    // exact head. Skip (finalize succeeded, no post) to avoid a duplicate review.
    if already, cerr := s.reviewedHead(ctx, job.PrincipalID, job.BusinessID, job.RepoConnectorID, job.PRNumber, pr.HeadSHA, job.ID); cerr == nil && already {
        return s.finalizeSkipped(ctx, job, pr.HeadSHA) // sets status='succeeded' w/ a "superseded by same head" note, no PostReview
    }
```
Add helpers: `installationIDFromConfig(cfg map[string]any) (int64, bool)` (typed decode — handle `float64`/`json.Number`); `reviewedHead(...)` (a small query: EXISTS a succeeded `code_review` for `(repo_connector_id, pr_number, head_sha)` with `id <> job.ID`); `finalizeSkipped(...)` (calls `UpdateCodeReviewResult` with `status='succeeded'`, empty summary, a note). Add `Tokens installationTokenSource` to the `CodeReviewService` struct + the interface.

- [ ] **Step 4: Run → PASS.**

- [ ] **Step 5: Wire `main.go`.** Add `installationTokenSource` + `PRReviews` wiring. In the `if len(cfg.GitHubAppMasterKey) > 0` block (L430-447), after building `githubAppH`'s `Store`, also build + late-wire:
```go
        gaStore := githubAppH.Store.(*githubapp.ConfigStore) // or hoist the store var
        codingSvc.Tokens = &githubapp.InstallationTokenSource{Store: gaStore, API: githubapp.NewClient(60 * time.Second), Now: time.Now}
        githubAppH.PRReviews = &githubapp.PRReviewEnqueuer{DB: database}
```
(Cleaner: hoist `gaStore := &githubapp.ConfigStore{DB: database, Sealer: gaSealer}` into its own var, use it for both `githubAppH.Store` and the token source. `codingSvc` exists from L396, so this late-wire assignment is valid — mirrors the `agentH.SetMetadata` late-wire at L456.)

- [ ] **Step 6: End-to-end integration test.** `internal/agents/coding/service_github_app_test.go` (`//go:build integration`): seed a linked installation; POST a signed `opened` `pull_request` delivery to the webhook handler (or call `handlePullRequestEvent` + the enqueuer directly against real Postgres) → assert an app-backed `repo_connector` + pending `code_review` under the agent principal exist; run the worker/`runJob` with a `FakeRunner` + a fake `Tokens` returning `"ghs_test"` → assert the fake token reached the clone auth and `PostReview` was called (or the fake connector recorded it). Reuse Slice-1 seeds + the coding `FakeRunner`.

- [ ] **Step 7: Full suites.** `go build ./... && make test && go test -tags integration ./internal/githubapp/... ./internal/connectors/... ./internal/agents/coding/... && go test -tags contract ./cmd/... && make sec-test`. **Step 8: Commit** (service.go, main.go, tests): `feat(011): runJob mints installation token + egress pre-flight + claim-time re-check; wire token source + enqueuer (manyforge-qpc)`.

---

## Slice 2 completion checks
- [ ] `make test` + `make sec-test` green; `go build ./...` + `golangci-lint run ./...` clean.
- [ ] `go test -tags integration ./...` green (mint/cache, connector Resolve+Delete, the ingest DEFINER dedup/rate/supersede, the webhook filter matrix, the end-to-end webhook→pending→FakeRunner review).
- [ ] `go test -tags contract ./cmd/...` green (no new route; `/github/webhook` unchanged).
- [ ] Feature inert when `MANYFORGE_GITHUB_APP_MASTER_KEY` unset (no token source; no github_app connectors created; server boots).
- [ ] No secret material in the diff.
