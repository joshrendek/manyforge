# GitHub App Auto-Review — Slice 1: App identity, setup & verified linking — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let an instance operator create a public GitHub App via the manifest flow, receive signature-verified `installation` webhooks, and let a business member link an installation to their business (with a review agent) only after proving GitHub-side control via OAuth — no PR reviews yet.

**Architecture:** A new `internal/githubapp` package holds an instance-level sealed config store (App id/slug/client-id + sealed private key/client-secret/webhook-secret under a dedicated master key), a fakeable GitHub API client (manifest conversion, OAuth code exchange, list-user-installations), and HTTP handlers. Setup and linking both use the "authenticated start mints a signed single-use `state` → GitHub redirects to a public callback that validates `state`" pattern, because GitHub's redirect is a browser navigation with no `Authorization` header. Installation lifecycle and linking mutate rows through `SECURITY DEFINER` functions (the webhook is principal-less; an unlinked row is invisible to a business principal under RLS).

**Tech Stack:** Go, chi router, pgx/v5, sqlc (`make generate`), golang-migrate, `github.com/golang-jwt/jwt/v5` (RS256), `internal/platform/netsafe` (SSRF-safe HTTP), `internal/platform/crypto` (AES-256-GCM sealer), stdlib `testing` (table-driven; no testify), `testcontainers` via `internal/platform/db/testdb` for `//go:build integration` tests.

## Global Constraints

- **Spec:** `docs/superpowers/specs/2026-07-05-github-app-auto-review-design.md`. This slice covers spec §4.1, §4.2, §7.1, §7.2, and the `installation` half of §6; the `pull_request` trigger, `github_installation_context`, app-backed connector, and per-repo token minting are Slice 2.
- **Zero secrets in git.** The App private key / client secret / webhook secret exist only sealed in the DB (or in k8s secrets); never in fixtures, logs, or the repo. Test fixtures use obviously-fake values and are allowlisted in `.gitleaks.toml`.
- **Fail closed.** The feature is disabled (routes not mounted) when `MANYFORGE_GITHUB_APP_MASTER_KEY` is unset. A set-but-wrong-length key is a hard boot error (via `envKey32`).
- **Next migration number is `0080`.** Zero-padded `NNNN_snake_desc.up.sql` / `.down.sql`. Highest existing is `0079`.
- **Two-role DB model.** Runtime connects as `manyforge_app` (`NOLOGIN NOSUPERUSER NOBYPASSRLS`). Every new table needs explicit `GRANT`s to `manyforge_app`; every `SECURITY DEFINER` function needs `REVOKE ALL … FROM PUBLIC` + `GRANT EXECUTE … TO manyforge_app`. The migration owner role owns tables/functions and is RLS-exempt. There is **no** `manyforge_owner` role.
- **RLS.** Tenant tables `ENABLE ROW LEVEL SECURITY` with a policy `USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))`. Reads inside `WithPrincipal` rely on RLS; writes/deletes additionally push an explicit `business_id` predicate (defense in depth).
- **Error hygiene.** Service methods wrap typed sentinels from `internal/platform/errs` (`ErrNotFound`, `ErrValidation`, `ErrConflict`, `ErrForbidden`). Handlers use `httpx.WriteError` (never echo `err.Error()`). Foreign/unknown ids return the same 404 shape (no existence oracle).
- **Webhook oracle policy.** Unknown/unconfigured → 202; bad signature on a configured secret → 401; everything accepted (new or replay) → 202. Uniform `202` otherwise.
- **sqlc:** edit SQL in `db/query/*.sql`, mirror new tables into `db/schema.sql`, run `make generate` (never hand-edit `internal/platform/db/dbgen/`). Pin sqlc to v1.27.0 locally to avoid re-churning the generated diff.
- **OpenAPI drift.** `cmd/manyforge/main.go` `mountAPIRoutes` is checked against the OpenAPI contract by `go test -tags contract ./cmd/...`. New routes must be added to the contract in the same task.
- **Commits:** no `Co-Authored-By` trailer. Run `make test` (and `make sec-test` when security pins change) before committing.
- **bd:** this work is tracked under `manyforge-q4h`.

---

### Task 1: Instance App-config storage (sealed, non-overwritable) + dedicated master key

**Files:**
- Create: `migrations/0080_github_app_config.up.sql`, `migrations/0080_github_app_config.down.sql`
- Modify: `db/schema.sql` (append the new table), `internal/platform/config/config.go` (add master-key field + load)
- Create: `db/query/github_app_config.sql`
- Create: `internal/githubapp/config_store.go`
- Test: `internal/githubapp/config_store_test.go` (unit, seal round-trip against a real Sealer), `internal/githubapp/config_store_integration_test.go` (`//go:build integration`, real DB)

**Interfaces:**
- Produces:
  - `type AppCreds struct { AppID int64; Slug, ClientID, ClientSecret, PrivateKeyPEM, WebhookSecret string }`
  - `type AppConfig struct { AppID int64; Slug, ClientID, ClientSecret, PrivateKeyPEM, WebhookSecret string }` (decrypted; returned by `Get`)
  - `type ConfigStore struct { DB txRunner; Sealer *crypto.Sealer }` where `txRunner` is `interface{ WithTx(ctx context.Context, fn func(pgx.Tx) error) error }`
  - `func (s *ConfigStore) Get(ctx context.Context) (AppConfig, error)` — `errs.ErrNotFound` when unset
  - `func (s *ConfigStore) Save(ctx context.Context, c AppCreds) error` — `errs.ErrConflict` when already set
  - `config.Config.GitHubAppMasterKey []byte`

- [ ] **Step 1: Write the migration**

`migrations/0080_github_app_config.up.sql`:
```sql
-- Instance-level (tenantless) GitHub App configuration. Single row (id = 1).
-- Secrets are AES-256-GCM sealed under MANYFORGE_GITHUB_APP_MASTER_KEY; the
-- table itself has NO RLS (like `principal`) — it is never exposed via a
-- tenant API and manyforge_app only ever reads it to verify webhooks / mint
-- tokens. The migration owner owns it; manyforge_app gets explicit grants.
CREATE TABLE github_app_config (
    id                    integer PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    app_id                bigint  NOT NULL,
    slug                  text    NOT NULL,
    client_id             text    NOT NULL,
    sealed_client_secret  text    NOT NULL,
    sealed_private_key    text    NOT NULL,
    sealed_webhook_secret text    NOT NULL,
    created_at            timestamptz NOT NULL DEFAULT now()
);
GRANT SELECT, INSERT, UPDATE, DELETE ON github_app_config TO manyforge_app;
```

`migrations/0080_github_app_config.down.sql`:
```sql
DROP TABLE IF EXISTS github_app_config;
```

Append the same `CREATE TABLE github_app_config (...)` (without the GRANT) to `db/schema.sql` so sqlc sees it.

- [ ] **Step 2: Add the master key to config**

In `internal/platform/config/config.go`, add a struct field next to `ConnectorMasterKey`:
```go
// GitHubAppMasterKey seals the instance GitHub App private key + client/webhook
// secrets at rest. Supplied via MANYFORGE_GITHUB_APP_MASTER_KEY as base64 or hex;
// decoded value MUST be 32 bytes (AES-256). Nil/empty when unset — the GitHub App
// integration (manifest setup, webhook, linking) is disabled and the server still
// boots. A set-but-wrong-length key is a hard config error.
GitHubAppMasterKey []byte
```
And in `Load()`, next to the `ConnectorMasterKey` load:
```go
if cfg.GitHubAppMasterKey, err = envKey32("MANYFORGE_GITHUB_APP_MASTER_KEY"); err != nil {
    return Config{}, fmt.Errorf("MANYFORGE_GITHUB_APP_MASTER_KEY: %w", err)
}
```

- [ ] **Step 3: Write the sqlc queries**

`db/query/github_app_config.sql`:
```sql
-- name: GetGithubAppConfig :one
SELECT app_id, slug, client_id, sealed_client_secret, sealed_private_key, sealed_webhook_secret
FROM github_app_config WHERE id = 1;

-- name: InsertGithubAppConfig :execrows
INSERT INTO github_app_config (id, app_id, slug, client_id, sealed_client_secret, sealed_private_key, sealed_webhook_secret)
VALUES (1, sqlc.arg('app_id'), sqlc.arg('slug'), sqlc.arg('client_id'),
        sqlc.arg('sealed_client_secret'), sqlc.arg('sealed_private_key'), sqlc.arg('sealed_webhook_secret'))
ON CONFLICT (id) DO NOTHING;
```
Run `make generate`. Expected: `internal/platform/db/dbgen/github_app_config.sql.go` now has `GetGithubAppConfig(ctx) (GetGithubAppConfigRow, error)` and `InsertGithubAppConfig(ctx, InsertGithubAppConfigParams) (int64, error)`.

- [ ] **Step 4: Write the failing unit test for the store**

`internal/githubapp/config_store_test.go`:
```go
package githubapp

import (
    "testing"
    "manyforge/internal/platform/crypto"
)

func TestSealRoundTripFields(t *testing.T) {
    // 32-byte all-zero key is fine for a unit test of seal/open symmetry.
    s, err := crypto.NewSealer(make([]byte, 32))
    if err != nil { t.Fatalf("NewSealer: %v", err) }
    for _, pt := range []string{"-----BEGIN KEY-----fake-----END KEY-----", "whsec_fake", ""} {
        ref, err := s.Seal([]byte(pt))
        if err != nil { t.Fatalf("Seal(%q): %v", pt, err) }
        got, err := s.Open(ref)
        if err != nil { t.Fatalf("Open: %v", err) }
        if string(got) != pt { t.Errorf("round trip = %q, want %q", got, pt) }
    }
}
```
(Adjust the module import path prefix — check `go.mod`'s `module` line; use it in place of `manyforge/` everywhere in this plan.)

- [ ] **Step 5: Run it, expect FAIL (package doesn't compile yet)**

Run: `go test ./internal/githubapp/ -run TestSealRoundTripFields`
Expected: FAIL — no non-test `.go` file in the package yet.

- [ ] **Step 6: Implement `ConfigStore`**

`internal/githubapp/config_store.go`:
```go
package githubapp

import (
    "context"
    "errors"
    "fmt"

    "github.com/jackc/pgx/v5"
    "manyforge/internal/platform/crypto"
    "manyforge/internal/platform/db/dbgen"
    "manyforge/internal/platform/errs"
)

type txRunner interface {
    WithTx(ctx context.Context, fn func(pgx.Tx) error) error
}

type AppCreds struct {
    AppID         int64
    Slug          string
    ClientID      string
    ClientSecret  string
    PrivateKeyPEM string
    WebhookSecret string
}

type AppConfig struct {
    AppID         int64
    Slug          string
    ClientID      string
    ClientSecret  string
    PrivateKeyPEM string
    WebhookSecret string
}

type ConfigStore struct {
    DB     txRunner
    Sealer *crypto.Sealer
}

func (s *ConfigStore) Save(ctx context.Context, c AppCreds) error {
    sealedSecret, err := s.Sealer.Seal([]byte(c.ClientSecret))
    if err != nil { return fmt.Errorf("seal client secret: %w", err) }
    sealedKey, err := s.Sealer.Seal([]byte(c.PrivateKeyPEM))
    if err != nil { return fmt.Errorf("seal private key: %w", err) }
    sealedHook, err := s.Sealer.Seal([]byte(c.WebhookSecret))
    if err != nil { return fmt.Errorf("seal webhook secret: %w", err) }
    return s.DB.WithTx(ctx, func(tx pgx.Tx) error {
        n, err := dbgen.New(tx).InsertGithubAppConfig(ctx, dbgen.InsertGithubAppConfigParams{
            AppID: c.AppID, Slug: c.Slug, ClientID: c.ClientID,
            SealedClientSecret: sealedSecret, SealedPrivateKey: sealedKey, SealedWebhookSecret: sealedHook,
        })
        if err != nil { return fmt.Errorf("insert github app config: %w", err) }
        if n == 0 { return fmt.Errorf("github app already configured: %w", errs.ErrConflict) }
        return nil
    })
}

func (s *ConfigStore) Get(ctx context.Context) (AppConfig, error) {
    var out AppConfig
    err := s.DB.WithTx(ctx, func(tx pgx.Tx) error {
        row, err := dbgen.New(tx).GetGithubAppConfig(ctx)
        if errors.Is(err, pgx.ErrNoRows) { return errs.ErrNotFound }
        if err != nil { return fmt.Errorf("get github app config: %w", err) }
        sec, err := s.Sealer.Open(row.SealedClientSecret)
        if err != nil { return fmt.Errorf("open client secret: %w", err) }
        key, err := s.Sealer.Open(row.SealedPrivateKey)
        if err != nil { return fmt.Errorf("open private key: %w", err) }
        hook, err := s.Sealer.Open(row.SealedWebhookSecret)
        if err != nil { return fmt.Errorf("open webhook secret: %w", err) }
        out = AppConfig{AppID: row.AppID, Slug: row.Slug, ClientID: row.ClientID,
            ClientSecret: string(sec), PrivateKeyPEM: string(key), WebhookSecret: string(hook)}
        return nil
    })
    return out, err
}
```
(Confirm the concrete `*db.DB` exposes `WithTx(ctx, func(pgx.Tx) error) error` — per `internal/platform/db/db.go:60-70` it does.)

- [ ] **Step 7: Run the unit test, expect PASS**

Run: `go test ./internal/githubapp/ -run TestSealRoundTripFields`
Expected: PASS.

- [ ] **Step 8: Write the integration test (real DB, non-overwrite)**

`internal/githubapp/config_store_integration_test.go`:
```go
//go:build integration

package githubapp_test

import (
    "context"
    "errors"
    "testing"

    "manyforge/internal/githubapp"
    "manyforge/internal/platform/crypto"
    "manyforge/internal/platform/db/testdb"
    "manyforge/internal/platform/errs"
)

func TestConfigStoreSaveIsNonOverwritable(t *testing.T) {
    ctx := context.Background()
    tdb := testdb.Start(ctx, t)
    sealer, _ := crypto.NewSealer(make([]byte, 32))
    store := &githubapp.ConfigStore{DB: tdb.App, Sealer: sealer}

    creds := githubapp.AppCreds{AppID: 42, Slug: "mf-review", ClientID: "Iv1.fake",
        ClientSecret: "cs_fake", PrivateKeyPEM: "-----BEGIN RSA PRIVATE KEY-----fake", WebhookSecret: "whsec_fake"}
    if err := store.Save(ctx, creds); err != nil { t.Fatalf("first Save: %v", err) }

    err := store.Save(ctx, creds)
    if !errors.Is(err, errs.ErrConflict) { t.Fatalf("second Save = %v, want ErrConflict", err) }

    got, err := store.Get(ctx)
    if err != nil { t.Fatalf("Get: %v", err) }
    if got.AppID != 42 || got.WebhookSecret != "whsec_fake" || got.PrivateKeyPEM != creds.PrivateKeyPEM {
        t.Fatalf("Get returned %+v, want decrypted creds", got)
    }
}
```
(Check `testdb.Start`'s exact signature — the fact-find shows `testdb.Start(ctx)`; if it takes `*testing.T` too, match it. `tdb.App` is the RLS-subject `*db.DB`.)

- [ ] **Step 9: Run the integration test, expect PASS**

Run: `go test -tags integration ./internal/githubapp/ -run TestConfigStoreSaveIsNonOverwritable`
Expected: PASS (migration applies, non-overwrite enforced, secrets decrypt).

- [ ] **Step 10: Commit**

```bash
git add migrations/0080_github_app_config.up.sql migrations/0080_github_app_config.down.sql db/schema.sql db/query/github_app_config.sql internal/platform/config/config.go internal/platform/db/dbgen internal/githubapp/config_store.go internal/githubapp/config_store_test.go internal/githubapp/config_store_integration_test.go
git commit -m "feat(009): sealed instance GitHub App config store (manyforge-q4h)"
```

---

### Task 2: Fakeable GitHub API client — manifest conversion, OAuth exchange, list installations

**Files:**
- Create: `internal/githubapp/client.go`
- Test: `internal/githubapp/client_test.go` (unit, `httptest.Server` + a permissive `*http.Client`)

**Interfaces:**
- Produces:
  - `type GitHubAPI interface { ConvertManifest(ctx, code string) (AppCreds, error); ExchangeOAuthCode(ctx, clientID, clientSecret, code string) (string, error); ListUserInstallations(ctx, userToken string) ([]int64, error) }`
  - `type Client struct { HTTP *http.Client; APIBase, WebBase string }` implementing `GitHubAPI`
  - `func NewClient(timeout time.Duration) *Client` — builds a `netsafe.NewClient(timeout)` HTTP client, `APIBase="https://api.github.com"`, `WebBase="https://github.com"`
- Consumes: `AppCreds` (Task 1)

- [ ] **Step 1: Write the failing test**

`internal/githubapp/client_test.go`:
```go
package githubapp

import (
    "context"
    "encoding/json"
    "net/http"
    "net/http/httptest"
    "testing"
)

func TestConvertManifestParsesCreds(t *testing.T) {
    srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        if r.Method != http.MethodPost || r.URL.Path != "/app-manifests/thecode/conversions" {
            t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
        }
        _ = json.NewEncoder(w).Encode(map[string]any{
            "id": 99, "slug": "mf-review", "client_id": "Iv1.x", "client_secret": "cs",
            "pem": "-----BEGIN RSA PRIVATE KEY-----k", "webhook_secret": "whsec",
        })
    }))
    defer srv.Close()
    c := &Client{HTTP: srv.Client(), APIBase: srv.URL, WebBase: srv.URL}

    creds, err := c.ConvertManifest(context.Background(), "thecode")
    if err != nil { t.Fatalf("ConvertManifest: %v", err) }
    if creds.AppID != 99 || creds.Slug != "mf-review" || creds.PrivateKeyPEM == "" || creds.WebhookSecret != "whsec" {
        t.Fatalf("got %+v", creds)
    }
}

func TestListUserInstallationsExtractsIDs(t *testing.T) {
    srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        if got := r.Header.Get("Authorization"); got != "Bearer utoken" {
            t.Errorf("auth header = %q", got)
        }
        _ = json.NewEncoder(w).Encode(map[string]any{
            "total_count": 2,
            "installations": []map[string]any{{"id": 11}, {"id": 22}},
        })
    }))
    defer srv.Close()
    c := &Client{HTTP: srv.Client(), APIBase: srv.URL, WebBase: srv.URL}

    ids, err := c.ListUserInstallations(context.Background(), "utoken")
    if err != nil { t.Fatalf("ListUserInstallations: %v", err) }
    if len(ids) != 2 || ids[0] != 11 || ids[1] != 22 { t.Fatalf("ids = %v", ids) }
}
```

- [ ] **Step 2: Run it, expect FAIL**

Run: `go test ./internal/githubapp/ -run 'TestConvertManifest|TestListUserInstallations'`
Expected: FAIL — `Client` methods undefined.

- [ ] **Step 3: Implement the client**

`internal/githubapp/client.go`:
```go
package githubapp

import (
    "context"
    "encoding/json"
    "fmt"
    "io"
    "net/http"
    "net/url"
    "strings"
    "time"

    "manyforge/internal/platform/netsafe"
)

type GitHubAPI interface {
    ConvertManifest(ctx context.Context, code string) (AppCreds, error)
    ExchangeOAuthCode(ctx context.Context, clientID, clientSecret, code string) (string, error)
    ListUserInstallations(ctx context.Context, userToken string) ([]int64, error)
}

type Client struct {
    HTTP    *http.Client
    APIBase string
    WebBase string
}

func NewClient(timeout time.Duration) *Client {
    return &Client{HTTP: netsafe.NewClient(timeout), APIBase: "https://api.github.com", WebBase: "https://github.com"}
}

func (c *Client) do(ctx context.Context, req *http.Request, out any) error {
    req.Header.Set("Accept", "application/vnd.github+json")
    resp, err := c.HTTP.Do(req)
    if err != nil { return fmt.Errorf("github request: %w", err) }
    defer resp.Body.Close()
    body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
    if resp.StatusCode < 200 || resp.StatusCode >= 300 {
        // Never surface upstream bodies to callers; log-worthy but generic here.
        return fmt.Errorf("github status %d", resp.StatusCode)
    }
    if out != nil {
        if err := json.Unmarshal(body, out); err != nil { return fmt.Errorf("github decode: %w", err) }
    }
    return nil
}

func (c *Client) ConvertManifest(ctx context.Context, code string) (AppCreds, error) {
    u := fmt.Sprintf("%s/app-manifests/%s/conversions", c.APIBase, url.PathEscape(code))
    req, _ := http.NewRequestWithContext(ctx, http.MethodPost, u, nil)
    var r struct {
        ID            int64  `json:"id"`
        Slug          string `json:"slug"`
        ClientID      string `json:"client_id"`
        ClientSecret  string `json:"client_secret"`
        PEM           string `json:"pem"`
        WebhookSecret string `json:"webhook_secret"`
    }
    if err := c.do(ctx, req, &r); err != nil { return AppCreds{}, err }
    return AppCreds{AppID: r.ID, Slug: r.Slug, ClientID: r.ClientID, ClientSecret: r.ClientSecret,
        PrivateKeyPEM: r.PEM, WebhookSecret: r.WebhookSecret}, nil
}

func (c *Client) ExchangeOAuthCode(ctx context.Context, clientID, clientSecret, code string) (string, error) {
    form := url.Values{"client_id": {clientID}, "client_secret": {clientSecret}, "code": {code}}
    req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.WebBase+"/login/oauth/access_token", strings.NewReader(form.Encode()))
    req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
    var r struct {
        AccessToken string `json:"access_token"`
        Error       string `json:"error"`
    }
    if err := c.do(ctx, req, &r); err != nil { return "", err }
    if r.AccessToken == "" { return "", fmt.Errorf("github oauth: no access token") }
    return r.AccessToken, nil
}

func (c *Client) ListUserInstallations(ctx context.Context, userToken string) ([]int64, error) {
    req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.APIBase+"/user/installations?per_page=100", nil)
    req.Header.Set("Authorization", "Bearer "+userToken)
    var r struct {
        Installations []struct{ ID int64 `json:"id"` } `json:"installations"`
    }
    if err := c.do(ctx, req, &r); err != nil { return nil, err }
    ids := make([]int64, 0, len(r.Installations))
    for _, in := range r.Installations { ids = append(ids, in.ID) }
    return ids, nil
}
```
Note: `ExchangeOAuthCode` sends `Accept: application/vnd.github+json` so GitHub returns JSON (not the default form-encoded body).

- [ ] **Step 4: Run the tests, expect PASS**

Run: `go test ./internal/githubapp/ -run 'TestConvertManifest|TestListUserInstallations'`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/githubapp/client.go internal/githubapp/client_test.go
git commit -m "feat(009): fakeable GitHub App API client (manifest/oauth/installs) (manyforge-q4h)"
```

---

### Task 3: Signed state helper + operator-gated manifest setup flow

**Files:**
- Create: `internal/githubapp/state.go`, `internal/githubapp/handler.go`, `internal/githubapp/manifest.go`
- Modify: `internal/platform/config/config.go` (operator principal + public base URL), `cmd/manyforge/main.go` (build + mount), the OpenAPI contract
- Test: `internal/githubapp/state_test.go`, `internal/githubapp/handler_manifest_test.go`

**Interfaces:**
- Produces:
  - `func signState(key []byte, payload StatePayload, now time.Time) string` / `func verifyState(key []byte, raw string, now time.Time) (StatePayload, error)` where `StatePayload struct { Purpose string; BusinessID, PrincipalID, AgentID uuid.UUID; Nonce string; Exp int64 }` (`Purpose` = `"manifest"` or `"link"`)
  - Two dependency interfaces (defined here, satisfied by `*ConfigStore` / `*InstallationService`), so `Handler` depends on behavior not concrete types and every test can stub them:
    - `type appConfigStore interface { Get(ctx context.Context) (AppConfig, error); Save(ctx context.Context, c AppCreds) error }`
    - `type installOps interface { UpsertFromEvent(ctx context.Context, id int64, login, accountType string) error; MarkDeleted(ctx context.Context, id int64) error; SetSuspended(ctx context.Context, id int64, suspended bool) error; Link(ctx context.Context, id int64, businessID, agentID uuid.UUID) error }`
  - `type Handler struct { Store appConfigStore; Installs installOps; API GitHubAPI; OperatorPrincipal uuid.UUID; PublicBaseURL string; StateKey []byte; Now func() time.Time; Logger *slog.Logger }` — `Store`/`API` are exercised this task; `Installs` stays nil until Task 4 (the manifest flow never calls it).
  - `func (h *Handler) OperatorRoutes(r chi.Router)` — `GET /github/app/manifest`
  - `func (h *Handler) SetupCallbackRoutes(r chi.Router)` — `GET /github/app/setup` (public; the manifest + link callback)
  - `config.Config.InstanceOperatorPrincipal uuid.UUID`, `config.Config.PublicBaseURL string`
- Consumes: `AppCreds`, `GitHubAPI`, `ConfigStore` (Tasks 1–2)

- [ ] **Step 1: Write the failing state test**

`internal/githubapp/state_test.go`:
```go
package githubapp

import (
    "testing"
    "time"

    "github.com/google/uuid"
)

func TestStateRoundTripAndTamper(t *testing.T) {
    key := []byte("0123456789abcdef0123456789abcdef")
    now := time.Unix(1_700_000_000, 0)
    p := StatePayload{Purpose: "manifest", PrincipalID: uuid.New(), Nonce: "n1", Exp: now.Add(5 * time.Minute).Unix()}
    tok := signState(key, p, now)

    got, err := verifyState(key, tok, now)
    if err != nil { t.Fatalf("verifyState: %v", err) }
    if got.Purpose != "manifest" || got.PrincipalID != p.PrincipalID { t.Fatalf("payload mismatch: %+v", got) }

    if _, err := verifyState(key, tok[:len(tok)-2]+"xx", now); err == nil { t.Error("tampered token verified") }
    if _, err := verifyState(key, tok, now.Add(10*time.Minute)); err == nil { t.Error("expired token verified") }
    if _, err := verifyState([]byte("different-key-different-key-1234"), tok, now); err == nil { t.Error("wrong key verified") }
}
```

- [ ] **Step 2: Run it, expect FAIL**

Run: `go test ./internal/githubapp/ -run TestStateRoundTripAndTamper`
Expected: FAIL — `signState`/`verifyState` undefined.

- [ ] **Step 3: Implement the signed state helper**

`internal/githubapp/state.go`:
```go
package githubapp

import (
    "crypto/hmac"
    "crypto/sha256"
    "encoding/base64"
    "encoding/json"
    "fmt"
    "time"

    "github.com/google/uuid"
    "manyforge/internal/platform/errs"
)

type StatePayload struct {
    Purpose     string    `json:"p"`
    BusinessID  uuid.UUID `json:"b"`
    PrincipalID uuid.UUID `json:"pr"`
    AgentID     uuid.UUID `json:"a"`
    Nonce       string    `json:"n"`
    Exp         int64     `json:"e"`
}

// DeriveStateKey domain-separates a 32-byte HMAC state key from the App master key
// so no additional secret/config is required.
func DeriveStateKey(masterKey []byte) []byte {
    m := hmac.New(sha256.New, masterKey)
    m.Write([]byte("github-app-oauth-state/v1"))
    return m.Sum(nil)
}

func signState(key []byte, payload StatePayload, now time.Time) string {
    body, _ := json.Marshal(payload)
    b := base64.RawURLEncoding.EncodeToString(body)
    m := hmac.New(sha256.New, key)
    m.Write([]byte(b))
    sig := base64.RawURLEncoding.EncodeToString(m.Sum(nil))
    return b + "." + sig
}

func verifyState(key []byte, raw string, now time.Time) (StatePayload, error) {
    var p StatePayload
    dot := -1
    for i := 0; i < len(raw); i++ { if raw[i] == '.' { dot = i; break } }
    if dot < 0 { return p, fmt.Errorf("malformed state: %w", errs.ErrValidation) }
    b, sigStr := raw[:dot], raw[dot+1:]
    m := hmac.New(sha256.New, key)
    m.Write([]byte(b))
    want := m.Sum(nil)
    got, err := base64.RawURLEncoding.DecodeString(sigStr)
    if err != nil || !hmac.Equal(got, want) { return p, fmt.Errorf("bad state signature: %w", errs.ErrValidation) }
    body, err := base64.RawURLEncoding.DecodeString(b)
    if err != nil { return p, fmt.Errorf("bad state body: %w", errs.ErrValidation) }
    if err := json.Unmarshal(body, &p); err != nil { return p, fmt.Errorf("bad state json: %w", errs.ErrValidation) }
    if now.Unix() > p.Exp { return p, fmt.Errorf("state expired: %w", errs.ErrValidation) }
    return p, nil
}
```

- [ ] **Step 4: Run the state test, expect PASS**

Run: `go test ./internal/githubapp/ -run TestStateRoundTripAndTamper`
Expected: PASS.

- [ ] **Step 5: Add operator-principal + public-base-URL config**

In `config.go` add:
```go
// InstanceOperatorPrincipal gates instance-level setup routes (GitHub App manifest
// creation). Supplied via MANYFORGE_INSTANCE_OPERATOR_PRINCIPAL as a UUID; uuid.Nil
// when unset — operator-gated setup routes then reject everyone (404).
InstanceOperatorPrincipal uuid.UUID
// PublicBaseURL is the externally-reachable base (e.g. https://hub.example.com) used
// to build GitHub webhook + OAuth redirect URLs. MANYFORGE_PUBLIC_BASE_URL.
PublicBaseURL string
```
In `Load()` (reuse an existing UUID-parse helper if one exists; otherwise):
```go
if v := os.Getenv("MANYFORGE_INSTANCE_OPERATOR_PRINCIPAL"); v != "" {
    if cfg.InstanceOperatorPrincipal, err = uuid.Parse(v); err != nil {
        return Config{}, fmt.Errorf("MANYFORGE_INSTANCE_OPERATOR_PRINCIPAL: %w", err)
    }
}
cfg.PublicBaseURL = strings.TrimSuffix(os.Getenv("MANYFORGE_PUBLIC_BASE_URL"), "/")
```
(If a public-base-URL config already exists in this repo, reuse it instead of adding a duplicate.)

- [ ] **Step 6: Write the failing manifest-flow handler test**

`internal/githubapp/handler_manifest_test.go`:
```go
package githubapp

import (
    "context"
    "net/http"
    "net/http/httptest"
    "strings"
    "testing"
    "time"

    "github.com/go-chi/chi/v5"
    "github.com/google/uuid"
    "manyforge/internal/platform/httpx"
)

type fakeAPI struct {
    convertCreds AppCreds
    userInstalls []int64
}
func (f *fakeAPI) ConvertManifest(ctx context.Context, code string) (AppCreds, error) { return f.convertCreds, nil }
func (f *fakeAPI) ExchangeOAuthCode(ctx context.Context, a, b, c string) (string, error) { return "utoken", nil }
func (f *fakeAPI) ListUserInstallations(ctx context.Context, t string) ([]int64, error) { return f.userInstalls, nil }

func newTestHandler(api GitHubAPI, store appConfigStore, installs installOps, op uuid.UUID) *Handler {
    return &Handler{Store: store, Installs: installs, API: api, OperatorPrincipal: op,
        PublicBaseURL: "https://hub.example.com", StateKey: []byte("0123456789abcdef0123456789abcdef"),
        Now: func() time.Time { return time.Unix(1_700_000_000, 0) }}
}

func TestManifestRouteRejectsNonOperator(t *testing.T) {
    op := uuid.New()
    h := newTestHandler(&fakeAPI{}, nil, nil, op)
    r := chi.NewRouter()
    r.Group(func(g chi.Router) {
        g.Use(func(next http.Handler) http.Handler {
            return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
                next.ServeHTTP(w, req.WithContext(httpx.WithPrincipal(req.Context(), uuid.New()))) // not the operator
            })
        })
        h.OperatorRoutes(g)
    })
    req := httptest.NewRequest(http.MethodGet, "/github/app/manifest", nil)
    w := httptest.NewRecorder()
    r.ServeHTTP(w, req)
    if w.Code != http.StatusNotFound { t.Fatalf("status = %d, want 404", w.Code) }
}

func TestManifestRouteRendersFormForOperator(t *testing.T) {
    op := uuid.New()
    h := newTestHandler(&fakeAPI{}, nil, nil, op)
    r := chi.NewRouter()
    r.Group(func(g chi.Router) {
        g.Use(func(next http.Handler) http.Handler {
            return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
                next.ServeHTTP(w, req.WithContext(httpx.WithPrincipal(req.Context(), op)))
            })
        })
        h.OperatorRoutes(g)
    })
    req := httptest.NewRequest(http.MethodGet, "/github/app/manifest", nil)
    w := httptest.NewRecorder()
    r.ServeHTTP(w, req)
    if w.Code != http.StatusOK { t.Fatalf("status = %d, want 200", w.Code) }
    if !strings.Contains(w.Body.String(), "settings/apps/new") { t.Errorf("form missing GitHub create-app action") }
    if !strings.Contains(w.Body.String(), "hub.example.com/api/v1/github/webhook") { t.Errorf("manifest missing webhook url") }
}
```
Notes for the implementer: confirm the exact helper names — the fact-find shows `httpx.PrincipalFromContext(ctx)`; there must be a matching setter used by `AuthToPrincipal`. If the setter is unexported, add an exported `httpx.WithPrincipal(ctx, uuid.UUID) context.Context` test helper (or use the existing one). Match whatever `AuthToPrincipal` uses so the middleware and handler agree on the context key.

- [ ] **Step 7: Run it, expect FAIL**

Run: `go test ./internal/githubapp/ -run TestManifestRoute`
Expected: FAIL — `Handler.OperatorRoutes` undefined.

- [ ] **Step 8: Implement the operator gate + manifest form + callback**

`internal/githubapp/handler.go` (operator gate + shared helpers):
```go
package githubapp

import (
    "context"
    "log/slog"
    "net/http"
    "time"

    "github.com/go-chi/chi/v5"
    "github.com/google/uuid"
    "manyforge/internal/platform/errs"
    "manyforge/internal/platform/httpx"
)

// Handler depends on these two behaviors, not concrete services — *ConfigStore
// (Task 1) satisfies appConfigStore and *InstallationService (Task 4) satisfies
// installOps. Both are also what the unit-test stubs implement.
type appConfigStore interface {
    Get(ctx context.Context) (AppConfig, error)
    Save(ctx context.Context, c AppCreds) error
}
type installOps interface {
    UpsertFromEvent(ctx context.Context, id int64, login, accountType string) error
    MarkDeleted(ctx context.Context, id int64) error
    SetSuspended(ctx context.Context, id int64, suspended bool) error
    Link(ctx context.Context, id int64, businessID, agentID uuid.UUID) error
}

type Handler struct {
    Store             appConfigStore
    Installs          installOps
    API               GitHubAPI
    OperatorPrincipal uuid.UUID
    PublicBaseURL     string
    StateKey          []byte
    Now               func() time.Time
    Logger            *slog.Logger
}

// operatorOnly gates a route on the config-pinned instance operator principal.
// Returns the uniform 404 (no oracle) for anyone else, and when no operator is set.
func (h *Handler) operatorOnly(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        pid, ok := httpx.PrincipalFromContext(r.Context())
        if !ok || h.OperatorPrincipal == uuid.Nil || pid != h.OperatorPrincipal {
            httpx.WriteError(w, r, errs.ErrNotFound)
            return
        }
        next.ServeHTTP(w, r)
    })
}

func (h *Handler) OperatorRoutes(r chi.Router) {
    r.Group(func(g chi.Router) {
        g.Use(h.operatorOnly)
        g.Get("/github/app/manifest", h.renderManifest)
    })
}
```

`internal/githubapp/manifest.go` (the form + the callback):
```go
package githubapp

import (
    "context"
    "encoding/json"
    "fmt"
    "html/template"
    "net/http"
    "time"

    "github.com/google/uuid"
    "manyforge/internal/platform/errs"
    "manyforge/internal/platform/httpx"
)

// manifestJSON is GitHub's App-manifest document. Installation lifecycle events
// are delivered to Apps automatically, so only "pull_request" is subscribed here.
func (h *Handler) manifestJSON() (string, error) {
    m := map[string]any{
        "name":        "manyforge-review",
        "url":         h.PublicBaseURL,
        "public":      true,
        "redirect_url": h.PublicBaseURL + "/api/v1/github/app/setup",
        "hook_attributes": map[string]any{"url": h.PublicBaseURL + "/api/v1/github/webhook", "active": true},
        "default_permissions": map[string]any{"contents": "read", "pull_requests": "write", "metadata": "read"},
        "default_events":      []string{"pull_request"},
        "request_oauth_on_install": true,
    }
    b, err := json.Marshal(m)
    return string(b), err
}

var manifestForm = template.Must(template.New("mf").Parse(`<!doctype html><html><body onload="document.forms[0].submit()">
<form action="https://github.com/settings/apps/new?state={{.State}}" method="post">
<input type="hidden" name="manifest" value='{{.Manifest}}'>
<noscript><button type="submit">Create GitHub App</button></noscript>
</form></body></html>`))

func (h *Handler) renderManifest(w http.ResponseWriter, r *http.Request) {
    pid, _ := httpx.PrincipalFromContext(r.Context())
    now := h.Now()
    state := signState(h.StateKey, StatePayload{
        Purpose: "manifest", PrincipalID: pid, Nonce: uuid.NewString(), Exp: now.Add(5 * time.Minute).Unix(),
    }, now)
    manifest, err := h.manifestJSON()
    if err != nil { httpx.WriteError(w, r, fmt.Errorf("manifest: %w", errs.ErrValidation)); return }
    w.Header().Set("Content-Type", "text/html; charset=utf-8")
    _ = manifestForm.Execute(w, struct{ State, Manifest string }{State: state, Manifest: manifest})
}

// setup is the PUBLIC GitHub redirect target for BOTH the manifest conversion and
// the install/link OAuth callback. Trust comes from the signed single-use state.
func (h *Handler) setup(w http.ResponseWriter, r *http.Request) {
    q := r.URL.Query()
    p, err := verifyState(h.StateKey, q.Get("state"), h.Now())
    if err != nil { httpx.WriteError(w, r, err); return } // 400 on bad/expired state
    switch p.Purpose {
    case "manifest":
        h.completeManifest(w, r, q.Get("code"))
    case "link":
        h.completeLink(w, r, p, q.Get("code"), q.Get("installation_id"))
    default:
        httpx.WriteError(w, r, errs.ErrValidation)
    }
}

func (h *Handler) completeManifest(w http.ResponseWriter, r *http.Request, code string) {
    if code == "" { httpx.WriteError(w, r, errs.ErrValidation); return }
    creds, err := h.API.ConvertManifest(r.Context(), code)
    if err != nil { h.log(r.Context(), "manifest convert failed", err); httpx.WriteError(w, r, errs.ErrValidation); return }
    if err := h.Store.Save(r.Context(), creds); err != nil { httpx.WriteError(w, r, err); return } // ErrConflict→409
    w.Header().Set("Content-Type", "text/html; charset=utf-8")
    _, _ = w.Write([]byte(`<!doctype html><p>GitHub App created. You can now install it on your organizations.</p>`))
}

func (h *Handler) log(ctx context.Context, msg string, err error) {
    if h.Logger != nil { h.Logger.ErrorContext(ctx, msg, "err", err) }
}
```
Add the setup route registration to `handler.go`:
```go
func (h *Handler) SetupCallbackRoutes(r chi.Router) {
    r.Get("/github/app/setup", h.setup) // public; validated by signed state
}
```
(`completeLink` is implemented in Task 6; add a temporary stub method `func (h *Handler) completeLink(w http.ResponseWriter, r *http.Request, p StatePayload, code, installID string) { httpx.WriteError(w, r, errs.ErrValidation) }` in `manifest.go` this task so the package compiles, and replace it in Task 6.)

- [ ] **Step 9: Run the handler tests, expect PASS**

Run: `go test ./internal/githubapp/ -run 'TestManifestRoute|TestStateRoundTrip'`
Expected: PASS.

- [ ] **Step 10: Wire into `main.go` + OpenAPI, mount conditionally**

In `cmd/manyforge/main.go`, after building the connector sealer/vault, add (guarded by the master key):
```go
var githubAppH *githubapp.Handler
if len(cfg.GitHubAppMasterKey) > 0 {
    gaSealer, err := mfcrypto.NewSealer(cfg.GitHubAppMasterKey)
    if err != nil { logger.Error("init github app sealer", "err", err); os.Exit(1) }
    githubAppH = &githubapp.Handler{
        Store:             &githubapp.ConfigStore{DB: database, Sealer: gaSealer},
        Installs:          nil, // wired in Task 4 once InstallationService exists; manifest flow never calls it
        API:               githubapp.NewClient(15 * time.Second),
        OperatorPrincipal: cfg.InstanceOperatorPrincipal,
        PublicBaseURL:     cfg.PublicBaseURL,
        StateKey:          githubapp.DeriveStateKey(cfg.GitHubAppMasterKey),
        Now:               time.Now,
        Logger:            logger,
    }
} else {
    logger.Warn("MANYFORGE_GITHUB_APP_MASTER_KEY unset; GitHub App integration disabled")
}
```
Add `githubApp *githubapp.Handler` to `apiHandlers` (near `main.go:783`), pass it through, and in `mountAPIRoutes` (`main.go:878`):
```go
// public (browser redirect from GitHub — trust via signed state)
if h.githubApp != nil { h.githubApp.SetupCallbackRoutes(ingress) }
// authenticated operator-gated
if h.githubApp != nil {
    pr.Group(func(g chi.Router) { h.githubApp.OperatorRoutes(g) })
}
```
Add `GET /api/v1/github/app/manifest` and `GET /api/v1/github/app/setup` to the OpenAPI contract that `mountAPIRoutes` is checked against.

- [ ] **Step 11: Build, run contract + package tests**

Run: `go build ./... && go test ./internal/githubapp/... && go test -tags contract ./cmd/...`
Expected: PASS (routes present in the contract, package green).

- [ ] **Step 12: Commit**

```bash
git add internal/githubapp/state.go internal/githubapp/state_test.go internal/githubapp/handler.go internal/githubapp/manifest.go internal/githubapp/handler_manifest_test.go internal/platform/config/config.go cmd/manyforge/main.go
# plus the OpenAPI contract file you edited
git commit -m "feat(009): operator-gated GitHub App manifest setup flow with signed state (manyforge-q4h)"
```

---

### Task 4: `github_app_installation` table + DEFINER lifecycle/link functions + service

**Files:**
- Create: `migrations/0081_github_app_installation.up.sql`, `.down.sql`, `db/query/github_app_installation.sql`, `internal/githubapp/installations.go`
- Modify: `db/schema.sql`
- Test: `internal/githubapp/installations_integration_test.go` (`//go:build integration`)

**Interfaces:**
- Produces:
  - `type InstallationService struct { DB installDB }` where `installDB interface{ WithTx(ctx, fn) error; WithPrincipal(ctx, uuid.UUID, fn) error }`
  - `func (s *InstallationService) UpsertFromEvent(ctx, installationID int64, login, accountType string) error`
  - `func (s *InstallationService) MarkDeleted(ctx, installationID int64) error`
  - `func (s *InstallationService) SetSuspended(ctx, installationID int64, suspended bool) error`
  - `func (s *InstallationService) Link(ctx, installationID int64, businessID, agentID uuid.UUID) error` — sets business/agent on an **unlinked** row via DEFINER; `errs.ErrNotFound` when no unlinked row matches
- Consumes: nothing new

- [ ] **Step 1: Write the migration**

`migrations/0081_github_app_installation.up.sql`:
```sql
CREATE TABLE github_app_installation (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    installation_id bigint NOT NULL UNIQUE,
    account_login   text   NOT NULL,
    account_type    text   NOT NULL DEFAULT 'Organization',
    business_id     uuid,                        -- NULL until linked
    tenant_root_id  uuid,
    agent_id        uuid,
    enabled         boolean NOT NULL DEFAULT true,
    config          jsonb   NOT NULL DEFAULT '{}'::jsonb,
    suspended_at    timestamptz,
    deleted_at      timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);
GRANT SELECT, INSERT, UPDATE, DELETE ON github_app_installation TO manyforge_app;
ALTER TABLE github_app_installation ENABLE ROW LEVEL SECURITY;
-- Linked rows are visible to their business; unlinked rows (business_id IS NULL)
-- are invisible to every principal and only reachable via the DEFINER functions.
CREATE POLICY github_app_installation_rls ON github_app_installation FOR ALL
    USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())));

-- Principal-less lifecycle (called from the webhook edge). SECURITY DEFINER to
-- bypass RLS; owned by the migration role, executable only by manyforge_app.
CREATE FUNCTION github_upsert_installation(p_installation_id bigint, p_login text, p_account_type text)
RETURNS void LANGUAGE sql SECURITY DEFINER SET search_path = public AS $$
    INSERT INTO github_app_installation (installation_id, account_login, account_type)
    VALUES (p_installation_id, p_login, p_account_type)
    ON CONFLICT (installation_id) DO UPDATE
        SET account_login = EXCLUDED.account_login, account_type = EXCLUDED.account_type,
            deleted_at = NULL, updated_at = now();
$$;
REVOKE ALL ON FUNCTION github_upsert_installation(bigint, text, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION github_upsert_installation(bigint, text, text) TO manyforge_app;

CREATE FUNCTION github_mark_installation_deleted(p_installation_id bigint)
RETURNS void LANGUAGE sql SECURITY DEFINER SET search_path = public AS $$
    UPDATE github_app_installation SET deleted_at = now(), enabled = false, updated_at = now()
    WHERE installation_id = p_installation_id;
$$;
REVOKE ALL ON FUNCTION github_mark_installation_deleted(bigint) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION github_mark_installation_deleted(bigint) TO manyforge_app;

CREATE FUNCTION github_set_installation_suspended(p_installation_id bigint, p_suspended boolean)
RETURNS void LANGUAGE sql SECURITY DEFINER SET search_path = public AS $$
    UPDATE github_app_installation
    SET suspended_at = CASE WHEN p_suspended THEN now() ELSE NULL END, updated_at = now()
    WHERE installation_id = p_installation_id;
$$;
REVOKE ALL ON FUNCTION github_set_installation_suspended(bigint, boolean) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION github_set_installation_suspended(bigint, boolean) TO manyforge_app;

-- Linking: set business/agent on an UNLINKED row only. Returns rows affected so
-- the caller can distinguish "linked" (1) from "no unlinked row / already linked" (0).
CREATE FUNCTION github_link_installation(p_installation_id bigint, p_business_id uuid, p_agent_id uuid)
RETURNS integer LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE n integer;
BEGIN
    UPDATE github_app_installation gi
    SET business_id = p_business_id, agent_id = p_agent_id,
        tenant_root_id = (SELECT b.tenant_root_id FROM business b WHERE b.id = p_business_id),
        enabled = true, updated_at = now()
    WHERE gi.installation_id = p_installation_id AND gi.business_id IS NULL AND gi.deleted_at IS NULL;
    GET DIAGNOSTICS n = ROW_COUNT;
    RETURN n;
END;
$$;
REVOKE ALL ON FUNCTION github_link_installation(bigint, uuid, uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION github_link_installation(bigint, uuid, uuid) TO manyforge_app;
```

`migrations/0081_github_app_installation.down.sql`:
```sql
DROP FUNCTION IF EXISTS github_link_installation(bigint, uuid, uuid);
DROP FUNCTION IF EXISTS github_set_installation_suspended(bigint, boolean);
DROP FUNCTION IF EXISTS github_mark_installation_deleted(bigint);
DROP FUNCTION IF EXISTS github_upsert_installation(bigint, text, text);
DROP TABLE IF EXISTS github_app_installation;
```
Append the `CREATE TABLE github_app_installation (...)` to `db/schema.sql`.

- [ ] **Step 2: Write the sqlc queries (DEFINER wrappers)**

`db/query/github_app_installation.sql`:
```sql
-- name: UpsertInstallation :exec
SELECT github_upsert_installation(sqlc.arg('installation_id'), sqlc.arg('login'), sqlc.arg('account_type'));

-- name: MarkInstallationDeleted :exec
SELECT github_mark_installation_deleted(sqlc.arg('installation_id'));

-- name: SetInstallationSuspended :exec
SELECT github_set_installation_suspended(sqlc.arg('installation_id'), sqlc.arg('suspended'));

-- name: LinkInstallation :one
SELECT github_link_installation(sqlc.arg('installation_id'), sqlc.arg('business_id'), sqlc.arg('agent_id'));
```
Run `make generate`.

- [ ] **Step 3: Write the failing integration test**

`internal/githubapp/installations_integration_test.go`:
```go
//go:build integration

package githubapp_test

import (
    "context"
    "errors"
    "testing"

    "manyforge/internal/githubapp"
    "manyforge/internal/platform/db/testdb"
    "manyforge/internal/platform/errs"
)

func TestInstallationLifecycleAndLink(t *testing.T) {
    ctx := context.Background()
    tdb := testdb.Start(ctx, t)
    // Seed a business + agent via tdb.Super (RLS-exempt). Reuse the shared seed
    // helpers used by other coding/connectors integration tests; capture businessID,
    // agentID, and the business's authorized principal (memberPrincipal).
    businessID, agentID, memberPrincipal := seedBusinessAgentPrincipal(t, ctx, tdb) // helper — mirror existing seeds

    svc := &githubapp.InstallationService{DB: tdb.App}
    if err := svc.UpsertFromEvent(ctx, 7788, "bluescripts-net", "Organization"); err != nil { t.Fatalf("upsert: %v", err) }

    // Unlinked row must be invisible to the business principal under RLS.
    // (list-by-business query returns nothing until linked — assert via a raw
    // App-pool query under WithPrincipal, or skip if no list query exists yet.)

    if err := svc.Link(ctx, 7788, businessID, agentID); err != nil { t.Fatalf("link: %v", err) }
    // Linking again must be a no-op / ErrNotFound (already linked, no unlinked row).
    if err := svc.Link(ctx, 7788, businessID, agentID); !errors.Is(err, errs.ErrNotFound) {
        t.Fatalf("second link = %v, want ErrNotFound", err)
    }
    _ = memberPrincipal
}
```
If no shared `seedBusinessAgentPrincipal` helper exists, write a minimal one in this test file inserting into `business`, `agent`, `principal`, `membership` via `tdb.Super` — mirror the INSERTs an existing `//go:build integration` test in `internal/agents/coding/` already uses.

- [ ] **Step 4: Run it, expect FAIL**

Run: `go test -tags integration ./internal/githubapp/ -run TestInstallationLifecycleAndLink`
Expected: FAIL — `InstallationService` undefined.

- [ ] **Step 5: Implement the service**

`internal/githubapp/installations.go`:
```go
package githubapp

import (
    "context"
    "fmt"

    "github.com/jackc/pgx/v5"
    "github.com/google/uuid"
    "manyforge/internal/platform/db/dbgen"
    "manyforge/internal/platform/errs"
)

type installDB interface {
    WithTx(ctx context.Context, fn func(pgx.Tx) error) error
}

type InstallationService struct{ DB installDB }

func (s *InstallationService) UpsertFromEvent(ctx context.Context, installationID int64, login, accountType string) error {
    return s.DB.WithTx(ctx, func(tx pgx.Tx) error {
        return dbgen.New(tx).UpsertInstallation(ctx, dbgen.UpsertInstallationParams{
            InstallationID: installationID, Login: login, AccountType: accountType,
        })
    })
}

func (s *InstallationService) MarkDeleted(ctx context.Context, installationID int64) error {
    return s.DB.WithTx(ctx, func(tx pgx.Tx) error {
        return dbgen.New(tx).MarkInstallationDeleted(ctx, installationID)
    })
}

func (s *InstallationService) SetSuspended(ctx context.Context, installationID int64, suspended bool) error {
    return s.DB.WithTx(ctx, func(tx pgx.Tx) error {
        return dbgen.New(tx).SetInstallationSuspended(ctx, dbgen.SetInstallationSuspendedParams{
            InstallationID: installationID, Suspended: suspended,
        })
    })
}

func (s *InstallationService) Link(ctx context.Context, installationID int64, businessID, agentID uuid.UUID) error {
    return s.DB.WithTx(ctx, func(tx pgx.Tx) error {
        n, err := dbgen.New(tx).LinkInstallation(ctx, dbgen.LinkInstallationParams{
            InstallationID: installationID, BusinessID: businessID, AgentID: agentID,
        })
        if err != nil { return fmt.Errorf("link installation: %w", err) }
        if n == 0 { return errs.ErrNotFound } // no unlinked row for this installation
        return nil
    })
}
```
(These lifecycle/link ops run under `WithTx` because the DEFINER functions supply their own RLS bypass; no principal GUC is needed. Confirm the generated `LinkInstallation` returns `(int32, error)` or `(int64, error)` and adjust the `n == 0` type accordingly.)

- [ ] **Step 6: Run the integration test, expect PASS**

Run: `go test -tags integration ./internal/githubapp/ -run TestInstallationLifecycleAndLink`
Expected: PASS.

- [ ] **Step 7: Wire `Installs` into the Handler in `main.go`**

Now that `InstallationService` exists, replace the Task 3 placeholder in the GitHub App handler construction:
```go
// was: Installs: nil, // wired in Task 4 ...
Installs: &githubapp.InstallationService{DB: database},
```
Run: `go build ./...` — Expected: compiles.

- [ ] **Step 8: Commit**

```bash
git add migrations/0081_github_app_installation.up.sql migrations/0081_github_app_installation.down.sql db/schema.sql db/query/github_app_installation.sql internal/platform/db/dbgen internal/githubapp/installations.go internal/githubapp/installations_integration_test.go cmd/manyforge/main.go
git commit -m "feat(009): github_app_installation table + DEFINER lifecycle/link + service (manyforge-q4h)"
```

---

### Task 5: GitHub webhook route — signature verification + `installation` lifecycle

**Files:**
- Create: `internal/githubapp/webhook.go`, `internal/githubapp/webhook_test.go`
- Modify: `cmd/manyforge/main.go` (mount on the ingress group), the OpenAPI contract
- Test (security pin): `internal/security_regression/github_webhook_sig_pin_test.go`

**Interfaces:**
- Produces: `func (h *Handler) WebhookRoutes(r chi.Router)` → `POST /github/webhook`
- Consumes: `ConfigStore.Get` (webhook secret + app id), `InstallationService` lifecycle methods

- [ ] **Step 1: Write the failing test**

`internal/githubapp/webhook_test.go`:
```go
package githubapp

import (
    "bytes"
    "context"
    "crypto/hmac"
    "crypto/sha256"
    "encoding/hex"
    "net/http"
    "net/http/httptest"
    "testing"
    "time"

    "github.com/go-chi/chi/v5"
    "github.com/google/uuid"
)

func sign256(secret, body []byte) string {
    m := hmac.New(sha256.New, secret)
    m.Write(body)
    return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

// stubStore + recordInstalls satisfy the unified appConfigStore / installOps
// interfaces (Task 3) so the webhook test runs without a DB. stubStore is reused
// by the Task 6 link test; recordInstalls implements Link as a no-op.
type stubStore struct{ cfg AppConfig; err error }
func (s *stubStore) Get(ctx context.Context) (AppConfig, error) { return s.cfg, s.err }
func (s *stubStore) Save(ctx context.Context, c AppCreds) error { return nil }

type recordInstalls struct{ upserted []int64; deleted []int64 }
func (r *recordInstalls) UpsertFromEvent(ctx context.Context, id int64, login, at string) error { r.upserted = append(r.upserted, id); return nil }
func (r *recordInstalls) MarkDeleted(ctx context.Context, id int64) error { r.deleted = append(r.deleted, id); return nil }
func (r *recordInstalls) SetSuspended(ctx context.Context, id int64, s bool) error { return nil }
func (r *recordInstalls) Link(ctx context.Context, id int64, biz, agent uuid.UUID) error { return nil }

func newWebhookHandler(store appConfigStore, installs installOps) *Handler {
    return &Handler{Store: store, Installs: installs, Now: func() time.Time { return time.Unix(1_700_000_000, 0) }}
}

func TestWebhookRejectsBadSignature(t *testing.T) {
    store := &stubStore{cfg: AppConfig{AppID: 5, WebhookSecret: "whsec"}}
    h := newWebhookHandler(store, &recordInstalls{})
    r := chi.NewRouter(); h.WebhookRoutes(r)

    body := []byte(`{"action":"created","installation":{"id":9}}`)
    req := httptest.NewRequest(http.MethodPost, "/github/webhook", bytes.NewReader(body))
    req.Header.Set("X-GitHub-Event", "installation")
    req.Header.Set("X-GitHub-Hook-Installation-Target-ID", "5")
    req.Header.Set("X-Hub-Signature-256", "sha256=deadbeef")
    w := httptest.NewRecorder(); r.ServeHTTP(w, req)
    if w.Code != http.StatusUnauthorized { t.Fatalf("status = %d, want 401", w.Code) }
}

func TestWebhookInstallationCreatedUpserts(t *testing.T) {
    store := &stubStore{cfg: AppConfig{AppID: 5, WebhookSecret: "whsec"}}
    rec := &recordInstalls{}
    h := newWebhookHandler(store, rec)
    r := chi.NewRouter(); h.WebhookRoutes(r)

    body := []byte(`{"action":"created","installation":{"id":9,"account":{"login":"bluescripts-net","type":"Organization"}}}`)
    req := httptest.NewRequest(http.MethodPost, "/github/webhook", bytes.NewReader(body))
    req.Header.Set("X-GitHub-Event", "installation")
    req.Header.Set("X-GitHub-Hook-Installation-Target-ID", "5")
    req.Header.Set("X-Hub-Signature-256", sign256([]byte("whsec"), body))
    w := httptest.NewRecorder(); r.ServeHTTP(w, req)
    if w.Code != http.StatusAccepted { t.Fatalf("status = %d, want 202", w.Code) }
    if len(rec.upserted) != 1 || rec.upserted[0] != 9 { t.Fatalf("upserted = %v", rec.upserted) }
}
```
No new interfaces are needed — the webhook uses the `Store appConfigStore` (`Get`) and `Installs installOps` (`UpsertFromEvent`/`MarkDeleted`/`SetSuspended`) fields already on `Handler` from Task 3. The test stubs above satisfy those interfaces.

- [ ] **Step 2: Run it, expect FAIL**

Run: `go test ./internal/githubapp/ -run TestWebhook`
Expected: FAIL — `WebhookRoutes` undefined.

- [ ] **Step 3: Implement the webhook**

`internal/githubapp/webhook.go`:
```go
package githubapp

import (
    "crypto/hmac"
    "crypto/sha256"
    "encoding/hex"
    "encoding/json"
    "io"
    "net/http"
    "strconv"
    "strings"

    "github.com/go-chi/chi/v5"
)

const maxWebhookBody = 1 << 20 // 1 MiB

type installationEvent struct {
    Action       string `json:"action"`
    Installation struct {
        ID      int64 `json:"id"`
        Account struct {
            Login string `json:"login"`
            Type  string `json:"type"`
        } `json:"account"`
    } `json:"installation"`
}

func (h *Handler) WebhookRoutes(r chi.Router) {
    r.Post("/github/webhook", h.handleWebhook)
}

// handleWebhook: uniform 202 except a bad signature on a configured secret (401).
func (h *Handler) handleWebhook(w http.ResponseWriter, r *http.Request) {
    body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBody+1))
    if err != nil || len(body) > maxWebhookBody { w.WriteHeader(http.StatusRequestEntityTooLarge); return }

    cfg, err := h.Store.Get(r.Context())
    if err != nil { w.WriteHeader(http.StatusAccepted); return } // unconfigured → no oracle

    if !validSignature(cfg.WebhookSecret, r.Header.Get("X-Hub-Signature-256"), body) {
        w.WriteHeader(http.StatusUnauthorized)
        return
    }
    // Defense in depth: ignore deliveries not meant for this App.
    if tid := r.Header.Get("X-GitHub-Hook-Installation-Target-ID"); tid != "" && tid != strconv.FormatInt(cfg.AppID, 10) {
        w.WriteHeader(http.StatusAccepted)
        return
    }

    switch r.Header.Get("X-GitHub-Event") {
    case "installation":
        h.handleInstallationEvent(r, body)
    // "pull_request" is added in Slice 2.
    }
    w.WriteHeader(http.StatusAccepted)
}

func (h *Handler) handleInstallationEvent(r *http.Request, body []byte) {
    var ev installationEvent
    if err := json.Unmarshal(body, &ev); err != nil || ev.Installation.ID == 0 { return }
    switch ev.Action {
    case "created", "new_permissions_accepted", "unsuspend":
        if ev.Action == "unsuspend" { _ = h.Installs.SetSuspended(r.Context(), ev.Installation.ID, false); return }
        login := ev.Installation.Account.Login
        at := ev.Installation.Account.Type
        if at == "" { at = "Organization" }
        _ = h.Installs.UpsertFromEvent(r.Context(), ev.Installation.ID, login, at)
    case "deleted":
        _ = h.Installs.MarkDeleted(r.Context(), ev.Installation.ID)
    case "suspend":
        _ = h.Installs.SetSuspended(r.Context(), ev.Installation.ID, true)
    }
}

// validSignature verifies GitHub's X-Hub-Signature-256 (HMAC-SHA256, hex) in
// constant time. Empty secret or missing/malformed header → false (fail closed).
func validSignature(secret, header string, body []byte) bool {
    if secret == "" { return false }
    after, ok := strings.CutPrefix(header, "sha256=")
    if !ok { return false }
    got, err := hex.DecodeString(after)
    if err != nil { return false }
    mac := hmac.New(sha256.New, []byte(secret))
    mac.Write(body)
    return hmac.Equal(got, mac.Sum(nil))
}
```

- [ ] **Step 4: Run the tests, expect PASS**

Run: `go test ./internal/githubapp/ -run TestWebhook`
Expected: PASS.

- [ ] **Step 5: Add the security-regression source pin**

`internal/security_regression/github_webhook_sig_pin_test.go`:
```go
//go:build integration

package security_regression

import (
    "os"
    "strings"
    "testing"
)

const FindingGithubWebhookSigPin = "MF-009-GH-WEBHOOK-SIG"

// The GitHub webhook must verify HMAC-SHA256 in constant time and fail closed on
// an empty secret. Pin the source so a refactor can't silently drop it.
func TestGithubWebhookSignaturePinned(t *testing.T) {
    b, err := os.ReadFile("../githubapp/webhook.go")
    if err != nil { t.Fatalf("read webhook.go: %v", err) }
    src := string(b)
    for _, want := range []string{"hmac.Equal", "sha256.New", `secret == ""`, "X-Hub-Signature-256"} {
        if !strings.Contains(src, want) { t.Fatalf("%s: webhook.go missing %q", FindingGithubWebhookSigPin, want) }
    }
}
```
(Match the exact build tag + package name used by the existing files in `internal/security_regression/`.)

- [ ] **Step 6: Mount + OpenAPI + run pins**

In `mountAPIRoutes`, on the `ingress` group: `if h.githubApp != nil { h.githubApp.WebhookRoutes(ingress) }`. Add `POST /api/v1/github/webhook` to the OpenAPI contract.
Run: `go test ./internal/githubapp/... && go test -tags integration ./internal/security_regression/ -run TestGithubWebhookSignaturePinned && go test -tags contract ./cmd/...`
Expected: all PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/githubapp/webhook.go internal/githubapp/webhook_test.go internal/security_regression/github_webhook_sig_pin_test.go cmd/manyforge/main.go
# plus the OpenAPI contract file
git commit -m "feat(009): GitHub webhook route — HMAC verify + installation lifecycle (manyforge-q4h)"
```

---

### Task 6: OAuth-verified linking — install-url start + setup-callback link

**Files:**
- Modify: `internal/githubapp/handler.go` (business-scoped start route), `internal/githubapp/manifest.go` (replace the `completeLink` stub), `cmd/manyforge/main.go` (mount business routes), the OpenAPI contract
- Test: `internal/githubapp/handler_link_test.go`

**Interfaces:**
- Produces:
  - `func (h *Handler) BusinessRoutes(r chi.Router)` → `GET /businesses/{businessID}/github/app/install-url?agent_id=<uuid>` (returns the GitHub install URL carrying a signed `link` state)
  - `func (h *Handler) completeLink(w, r, p StatePayload, code, installID string)` — verifies OAuth control, then `InstallationService.Link`
- Consumes: `GitHubAPI.ExchangeOAuthCode`, `GitHubAPI.ListUserInstallations`, `InstallationService.Link`, `ConfigStore.Get` (client id/secret + slug)

- [ ] **Step 1: Write the failing test**

`internal/githubapp/handler_link_test.go`:
```go
package githubapp

import (
    "context"
    "net/http"
    "net/http/httptest"
    "strings"
    "testing"
    "time"

    "github.com/go-chi/chi/v5"
    "github.com/google/uuid"
)

type linkRec struct{ linkedInstall int64; linkedBiz, linkedAgent uuid.UUID; err error }
func (r *linkRec) UpsertFromEvent(ctx context.Context, id int64, l, a string) error { return nil }
func (r *linkRec) MarkDeleted(ctx context.Context, id int64) error                  { return nil }
func (r *linkRec) SetSuspended(ctx context.Context, id int64, s bool) error         { return nil }
func (r *linkRec) Link(ctx context.Context, id int64, biz, agent uuid.UUID) error {
    r.linkedInstall, r.linkedBiz, r.linkedAgent = id, biz, agent; return r.err
}

func TestCompleteLinkRequiresOAuthProof(t *testing.T) {
    biz, agent := uuid.New(), uuid.New()
    api := &fakeAPI{userInstalls: []int64{}} // user controls NO installations
    rec := &linkRec{}
    h := &Handler{API: api, Installs: rec, Store: &stubStore{cfg: AppConfig{ClientID: "cid", ClientSecret: "sec"}},
        StateKey: []byte("0123456789abcdef0123456789abcdef"), Now: func() time.Time { return time.Unix(1_700_000_000, 0) }}
    now := h.Now()
    state := signState(h.StateKey, StatePayload{Purpose: "link", BusinessID: biz, PrincipalID: uuid.New(), AgentID: agent, Nonce: "n", Exp: now.Add(5 * time.Minute).Unix()}, now)

    r := chi.NewRouter(); h.SetupCallbackRoutes(r)
    req := httptest.NewRequest(http.MethodGet, "/github/app/setup?state="+state+"&code=oc&installation_id=555", nil)
    w := httptest.NewRecorder(); r.ServeHTTP(w, req)
    if w.Code == http.StatusOK { t.Fatalf("link succeeded without proof (status %d)", w.Code) }
    if rec.linkedInstall != 0 { t.Fatalf("Link called despite missing proof") }
}

func TestCompleteLinkSucceedsWithProof(t *testing.T) {
    biz, agent := uuid.New(), uuid.New()
    api := &fakeAPI{userInstalls: []int64{555}} // user DOES control installation 555
    rec := &linkRec{}
    h := &Handler{API: api, Installs: rec, Store: &stubStore{cfg: AppConfig{ClientID: "cid", ClientSecret: "sec"}},
        StateKey: []byte("0123456789abcdef0123456789abcdef"), Now: func() time.Time { return time.Unix(1_700_000_000, 0) }}
    now := h.Now()
    state := signState(h.StateKey, StatePayload{Purpose: "link", BusinessID: biz, PrincipalID: uuid.New(), AgentID: agent, Nonce: "n", Exp: now.Add(5 * time.Minute).Unix()}, now)

    r := chi.NewRouter(); h.SetupCallbackRoutes(r)
    req := httptest.NewRequest(http.MethodGet, "/github/app/setup?state="+state+"&code=oc&installation_id=555", nil)
    w := httptest.NewRecorder(); r.ServeHTTP(w, req)
    if w.Code != http.StatusOK { t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String()) }
    if rec.linkedInstall != 555 || rec.linkedBiz != biz || rec.linkedAgent != agent {
        t.Fatalf("Link got (%d,%v,%v)", rec.linkedInstall, rec.linkedBiz, rec.linkedAgent)
    }
    _ = strings.TrimSpace
}
```
No new `Handler` field is needed — linking calls `h.Installs.Link` (the `installOps` interface from Task 3, which `*InstallationService` satisfies). `linkRec` above implements all four `installOps` methods so it can back the `Installs` field.

- [ ] **Step 2: Run it, expect FAIL**

Run: `go test ./internal/githubapp/ -run TestCompleteLink`
Expected: FAIL — `completeLink` is still the stub (link never happens) and `linker` field missing.

- [ ] **Step 3: Implement `completeLink` (replace the Task 3 stub)**

In `internal/githubapp/manifest.go`:
```go
func (h *Handler) completeLink(w http.ResponseWriter, r *http.Request, p StatePayload, code, installIDStr string) {
    if code == "" || installIDStr == "" { httpx.WriteError(w, r, errs.ErrValidation); return }
    installID, err := strconv.ParseInt(installIDStr, 10, 64)
    if err != nil { httpx.WriteError(w, r, errs.ErrValidation); return }

    cfg, err := h.Store.Get(r.Context())
    if err != nil { httpx.WriteError(w, r, err); return }
    userToken, err := h.API.ExchangeOAuthCode(r.Context(), cfg.ClientID, cfg.ClientSecret, code)
    if err != nil { h.log(r.Context(), "oauth exchange failed", err); httpx.WriteError(w, r, errs.ErrValidation); return }
    ids, err := h.API.ListUserInstallations(r.Context(), userToken)
    if err != nil { h.log(r.Context(), "list user installations failed", err); httpx.WriteError(w, r, errs.ErrValidation); return }

    // C1: prove the linking user actually controls this installation.
    controls := false
    for _, id := range ids { if id == installID { controls = true; break } }
    if !controls { httpx.WriteError(w, r, errs.ErrForbidden); return } // → 404, no oracle

    if err := h.Installs.Link(r.Context(), installID, p.BusinessID, p.AgentID); err != nil {
        httpx.WriteError(w, r, err); return
    }
    w.Header().Set("Content-Type", "text/html; charset=utf-8")
    _, _ = w.Write([]byte(`<!doctype html><p>Installation linked. manyforge will now review pull requests for this organization.</p>`))
}
```
Add the `strconv` import to `manifest.go`. No new `main.go` wiring is required — `completeLink` uses the `Store`/`API`/`Installs` fields already set on `Handler` in Tasks 3–4.

- [ ] **Step 4: Implement the business-scoped start route**

In `internal/githubapp/handler.go`:
```go
func (h *Handler) BusinessRoutes(r chi.Router) {
    r.Get("/businesses/{businessID}/github/app/install-url", h.installURL)
}

func (h *Handler) installURL(w http.ResponseWriter, r *http.Request) {
    pid, ok := httpx.PrincipalFromContext(r.Context())
    if !ok { httpx.WriteError(w, r, errs.ErrNotFound); return }
    bid, err := uuid.Parse(chi.URLParam(r, "businessID"))
    if err != nil { httpx.WriteError(w, r, errs.ErrNotFound); return }
    agentID, err := uuid.Parse(r.URL.Query().Get("agent_id"))
    if err != nil { httpx.WriteError(w, r, errs.ErrValidation); return }
    cfg, err := h.Store.Get(r.Context())
    if err != nil { httpx.WriteError(w, r, err); return } // ErrNotFound when App not yet created

    now := h.Now()
    state := signState(h.StateKey, StatePayload{
        Purpose: "link", BusinessID: bid, PrincipalID: pid, AgentID: agentID,
        Nonce: uuid.NewString(), Exp: now.Add(5 * time.Minute).Unix(),
    }, now)
    url := fmt.Sprintf("https://github.com/apps/%s/installations/new?state=%s", cfg.Slug, state)
    httpx.WriteJSON(w, http.StatusOK, map[string]string{"install_url": url})
}
```
This route is mounted behind `RequireAuth` + the `connectorsManage` (or agents-configure) permission gate for `{businessID}` — the same middleware used for repo-connector routes — so only an authorized member of that business can mint a `link` state for it. GitHub-side control is proven separately by the OAuth check in `completeLink`; the two together close C1.

- [ ] **Step 5: Run the tests, expect PASS**

Run: `go test ./internal/githubapp/ -run 'TestCompleteLink|TestManifestRoute|TestWebhook'`
Expected: PASS.

- [ ] **Step 6: Mount business routes + OpenAPI**

In `mountAPIRoutes`, add a permission-gated group mirroring the repo-connector mount (`main.go:1018`):
```go
if h.githubApp != nil {
    pr.Group(func(g chi.Router) {
        g.Use(h.connectorsManage) // reuse the existing per-business connectors-manage gate
        h.githubApp.BusinessRoutes(g)
    })
}
```
Add `GET /api/v1/businesses/{businessID}/github/app/install-url` to the OpenAPI contract.

- [ ] **Step 7: Full build + suites**

Run: `go build ./... && make test && go test -tags integration ./internal/githubapp/... && go test -tags contract ./cmd/...`
Expected: all PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/githubapp/handler.go internal/githubapp/manifest.go internal/githubapp/handler_link_test.go cmd/manyforge/main.go
# plus the OpenAPI contract file
git commit -m "feat(009): OAuth-verified installation linking (GET /user/installations proof) (manyforge-q4h)"
```

---

## Slice 1 completion checks

- [ ] `make test` green; `make sec-test` green (new pin passes).
- [ ] `go test -tags integration ./internal/githubapp/...` green.
- [ ] `go test -tags contract ./cmd/...` green (all new routes in the OpenAPI contract).
- [ ] `go build -tags ui_embed ./...` green (embedded-SPA build still compiles).
- [ ] Feature is inert when `MANYFORGE_GITHUB_APP_MASTER_KEY` is unset (routes not mounted; server boots).
- [ ] No secret material in the diff (grep the diff for `BEGIN RSA`, `whsec_`, real tokens; only fake fixtures, allowlisted in `.gitleaks.toml`).

## What Slice 1 deliberately does NOT do (→ Slice 2/3)

- No `pull_request` handling, `github_installation_context`, app-backed `repo_connector`, per-repo installation-token minting, or App JWT (Slice 2).
- No filters, budget/rate caps, supersede, delivery-id/head-SHA dedupe, or observability counters (Slice 3).
- No consumed-nonce table for `state` single-use — freshness is a 5-minute `exp` backstopped by GitHub's single-use `code`; a durable nonce store is a later hardening item.
