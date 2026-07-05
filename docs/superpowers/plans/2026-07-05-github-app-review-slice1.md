# GitHub App Auto-Review — Slice 1: App identity, setup & verified linking — Implementation Plan (v2)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let an instance operator create a public GitHub App via the manifest flow, receive signature-verified `installation` webhooks, and let a business member link an installation to their business (with a review agent) only after (a) the caller is an authenticated member with `connectors-manage` on that business and (b) they prove GitHub-side control of the installation via OAuth — no PR reviews yet.

**Architecture:** A new `internal/githubapp` package holds an instance-level sealed config store, a fakeable GitHub API client, a signed single-use `state` helper backed by a consumed-nonce table, and HTTP handlers. GitHub's post-action browser redirects land on **SPA routes** (Bearer auth can't survive a cross-site redirect); the SPA reads the redirect params and calls **authenticated** completion endpoints — `POST /github/app/manifest/convert` (operator-gated) and `POST /github/app/installations/link` (which extracts `business_id` from the signed state and enforces the caller's `connectors-manage` permission **in-handler**). Installation lifecycle and linking mutate rows through `SECURITY DEFINER` functions called via raw pgx (an unlinked row is invisible to a business principal under RLS). This design closes the fable-review findings — a leaked `state` cannot be completed by a non-member, and the `agent` bound into a link is verified to belong to the business.

**Tech Stack:** Go (chi, pgx/v5, sqlc for tables / raw pgx for function calls, golang-migrate, `golang-jwt/v5` — Slice 2 only, `netsafe`, `crypto.Sealer`), stdlib `testing` (table-driven; no testify), `testcontainers` via `internal/platform/db/testdb`, Angular 21 (standalone components, signals) for the SPA, Playwright for e2e.

## Global Constraints

- **Spec:** `docs/superpowers/specs/2026-07-05-github-app-auto-review-design.md`. This slice covers spec §4.1, §4.2, §7.1, §7.2, and the `installation` half of §6. The `pull_request` trigger, `github_installation_context`, app-backed connector, App JWT, and per-repo token minting are Slice 2. (Deferring App JWT/token minting is an intentional re-cut: nothing in Slice 1 consumes an installation token.)
- **Zero secrets in git.** The App private key / client secret / webhook secret exist only sealed in the DB (or k8s secrets); never in fixtures, logs, or the repo. Test fixtures use obviously-fake values.
- **Fail closed.** The feature is disabled (routes not mounted, handler nil) when `MANYFORGE_GITHUB_APP_MASTER_KEY` is unset. A set-but-wrong-length key is a hard boot error (via `envKey32`).
- **Migration numbers:** 0080 (config), 0081 (setup nonce), 0082 (installation). Highest existing is `0079`. Zero-padded `NNNN_snake_desc.up.sql`/`.down.sql`.
- **Two-role DB model.** Runtime connects as `manyforge_app` (`NOLOGIN NOSUPERUSER NOBYPASSRLS`). Every new table needs explicit `GRANT`s to `manyforge_app`; every `SECURITY DEFINER` function needs `REVOKE ALL … FROM PUBLIC` + `GRANT EXECUTE … TO manyforge_app`. The migration owner owns tables/functions and is RLS-exempt. There is **no** `manyforge_owner` role.
- **DB function calls use raw pgx, never sqlc.** House style: `tx.Exec/tx.QueryRow(ctx, "SELECT fn($1,...)", ...)` (see `worker.go:244`, `webhook.go:112`). sqlc is only for table CRUD via `db/query/*.sql` + `db/schema.sql` + `make generate`; never hand-edit `internal/platform/db/dbgen/`.
- **RLS.** Tenant tables `ENABLE ROW LEVEL SECURITY` with `USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))`. Reads inside `WithPrincipal` rely on RLS; writes push an explicit predicate too.
- **Error hygiene.** Wrap typed sentinels from `internal/platform/errs`. Handlers use `httpx.WriteError` (never echo `err.Error()`). Foreign/unknown ids → the same 404 (`ErrNotFound`/`ErrForbidden` both map to 404 in `httpx.WriteError`); no existence oracle.
- **Webhook oracle policy.** Unconfigured → 202; over-cap → 413; bad signature on a configured secret → 401; everything accepted (new or replay) → 202.
- **OpenAPI drift.** There is no global contract — each spec ships `specs/00X-*/contracts/openapi.yaml` plus a `cmd/manyforge/drift_00X_test.go` scoped by an `is00XOp` predicate, and `drift_test.go`'s `apiRoutes` builds `apiHandlers`. New routes need their own `specs/009-*/contracts/openapi.yaml`, a `drift_009_test.go`, AND a non-nil `githubApp` in the `apiRoutes` literal, or the walk never sees them.
- **Commits:** no `Co-Authored-By` trailer. `make test` (and `make sec-test` when pins change) before committing.
- **bd:** tracked under `manyforge-q4h`.
- Replace the `manyforge/` import prefix everywhere below with the actual `module` path from `go.mod`.

---

### Task 1: Instance App-config storage (sealed, non-overwritable) + dedicated master key

**Files:** Create `migrations/0080_github_app_config.{up,down}.sql`, `db/query/github_app_config.sql`, `internal/githubapp/config_store.go`; Modify `db/schema.sql`, `internal/platform/config/config.go`; Test `internal/githubapp/config_store_test.go`, `internal/githubapp/config_store_integration_test.go` (`//go:build integration`).

**Interfaces produced:**
- `type AppCreds struct { AppID int64; Slug, ClientID, ClientSecret, PrivateKeyPEM, WebhookSecret string }`
- `type AppConfig struct { AppID int64; Slug, ClientID, ClientSecret, PrivateKeyPEM, WebhookSecret string }`
- `type ConfigStore struct { DB txRunner; Sealer *crypto.Sealer }`, `txRunner = interface{ WithTx(ctx, func(pgx.Tx) error) error }`
- `func (s *ConfigStore) Get(ctx) (AppConfig, error)` (`ErrNotFound` when unset); `func (s *ConfigStore) Save(ctx, AppCreds) error` (`ErrConflict` when already set)
- `config.Config.GitHubAppMasterKey []byte`

- [ ] **Step 1: Migration.** `migrations/0080_github_app_config.up.sql`:
```sql
-- Instance-level (tenantless) GitHub App config, single row (id = 1). Secrets are
-- AES-256-GCM sealed under MANYFORGE_GITHUB_APP_MASTER_KEY. No RLS (like `principal`);
-- never exposed via any tenant API. SELECT,INSERT only — non-overwrite is DB-enforced.
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
GRANT SELECT, INSERT ON github_app_config TO manyforge_app;
```
`…down.sql`: `DROP TABLE IF EXISTS github_app_config;`. Append the `CREATE TABLE` (no GRANT) to `db/schema.sql`.

- [ ] **Step 2: Config.** In `config.go`, add field + load mirroring `ConnectorMasterKey`:
```go
// GitHubAppMasterKey seals the instance GitHub App private key + client/webhook
// secrets. MANYFORGE_GITHUB_APP_MASTER_KEY (base64/hex, 32 bytes). Nil when unset —
// GitHub App integration disabled, server still boots. Set-but-wrong-length is fatal.
GitHubAppMasterKey []byte
```
```go
if cfg.GitHubAppMasterKey, err = envKey32("MANYFORGE_GITHUB_APP_MASTER_KEY"); err != nil {
    return Config{}, fmt.Errorf("MANYFORGE_GITHUB_APP_MASTER_KEY: %w", err)
}
```

- [ ] **Step 3: Queries.** `db/query/github_app_config.sql`:
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
Run `make generate`. Expect `GetGithubAppConfig(ctx) (GetGithubAppConfigRow, error)` and `InsertGithubAppConfig(ctx, InsertGithubAppConfigParams) (int64, error)`.

- [ ] **Step 4: Failing unit test.** `internal/githubapp/config_store_test.go`:
```go
package githubapp

import (
    "testing"
    "manyforge/internal/platform/crypto"
)

func TestSealRoundTripFields(t *testing.T) {
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

- [ ] **Step 5: Run → FAIL** (`go test ./internal/githubapp/ -run TestSealRoundTripFields`) — no non-test file yet.

- [ ] **Step 6: Implement.** `internal/githubapp/config_store.go`:
```go
package githubapp

import (
    "context"
    "fmt"

    "github.com/jackc/pgx/v5"
    "manyforge/internal/platform/crypto"
    "manyforge/internal/platform/db/dbgen"
    "manyforge/internal/platform/errs"
)

type txRunner interface {
    WithTx(ctx context.Context, fn func(pgx.Tx) error) error
}

type AppCreds struct{ AppID int64; Slug, ClientID, ClientSecret, PrivateKeyPEM, WebhookSecret string }
type AppConfig struct{ AppID int64; Slug, ClientID, ClientSecret, PrivateKeyPEM, WebhookSecret string }

type ConfigStore struct {
    DB     txRunner
    Sealer *crypto.Sealer
}

func (s *ConfigStore) Save(ctx context.Context, c AppCreds) error {
    sec, err := s.Sealer.Seal([]byte(c.ClientSecret))
    if err != nil { return fmt.Errorf("seal client secret: %w", err) }
    key, err := s.Sealer.Seal([]byte(c.PrivateKeyPEM))
    if err != nil { return fmt.Errorf("seal private key: %w", err) }
    hook, err := s.Sealer.Seal([]byte(c.WebhookSecret))
    if err != nil { return fmt.Errorf("seal webhook secret: %w", err) }
    return s.DB.WithTx(ctx, func(tx pgx.Tx) error {
        n, err := dbgen.New(tx).InsertGithubAppConfig(ctx, dbgen.InsertGithubAppConfigParams{
            AppID: c.AppID, Slug: c.Slug, ClientID: c.ClientID,
            SealedClientSecret: sec, SealedPrivateKey: key, SealedWebhookSecret: hook,
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
        if err != nil {
            if pgx.ErrNoRows.Error() == err.Error() { return errs.ErrNotFound } // see note
            return fmt.Errorf("get github app config: %w", err)
        }
        sec, err := s.Sealer.Open(row.SealedClientSecret); if err != nil { return fmt.Errorf("open client secret: %w", err) }
        key, err := s.Sealer.Open(row.SealedPrivateKey);   if err != nil { return fmt.Errorf("open private key: %w", err) }
        hook, err := s.Sealer.Open(row.SealedWebhookSecret); if err != nil { return fmt.Errorf("open webhook secret: %w", err) }
        out = AppConfig{AppID: row.AppID, Slug: row.Slug, ClientID: row.ClientID,
            ClientSecret: string(sec), PrivateKeyPEM: string(key), WebhookSecret: string(hook)}
        return nil
    })
    return out, err
}
```
Note: use `errors.Is(err, pgx.ErrNoRows)` (add `"errors"` import) — the `.Error()` compare above is a placeholder to avoid an unused import; write `if errors.Is(err, pgx.ErrNoRows) { return errs.ErrNotFound }`.

- [ ] **Step 7: Run → PASS** (`go test ./internal/githubapp/ -run TestSealRoundTripFields`).

- [ ] **Step 8: Integration test (non-overwrite + decrypt).** `internal/githubapp/config_store_integration_test.go`:
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
    tdb, err := testdb.Start(ctx)          // Start returns (*TestDB, error)
    if err != nil { t.Fatalf("testdb.Start: %v", err) }
    defer tdb.Close(ctx)
    sealer, _ := crypto.NewSealer(make([]byte, 32))
    store := &githubapp.ConfigStore{DB: tdb.App, Sealer: sealer}

    creds := githubapp.AppCreds{AppID: 42, Slug: "mf-review", ClientID: "Iv1.fake",
        ClientSecret: "cs_fake", PrivateKeyPEM: "-----BEGIN RSA PRIVATE KEY-----fake", WebhookSecret: "whsec_fake"}
    if err := store.Save(ctx, creds); err != nil { t.Fatalf("first Save: %v", err) }
    if err := store.Save(ctx, creds); !errors.Is(err, errs.ErrConflict) { t.Fatalf("second Save = %v, want ErrConflict", err) }

    got, err := store.Get(ctx)
    if err != nil { t.Fatalf("Get: %v", err) }
    if got.AppID != 42 || got.WebhookSecret != "whsec_fake" || got.PrivateKeyPEM != creds.PrivateKeyPEM {
        t.Fatalf("Get returned %+v", got)
    }
}
```
Confirm `tdb.Close`'s real name from `testdb.go` (may be `Terminate`/`Stop`); mirror `startCoding` in `internal/agents/coding/service_integration_test.go:118`.

- [ ] **Step 9: Run → PASS** (`go test -tags integration ./internal/githubapp/ -run TestConfigStoreSaveIsNonOverwritable`).

- [ ] **Step 10: Commit.**
```bash
git add migrations/0080_github_app_config.up.sql migrations/0080_github_app_config.down.sql db/schema.sql db/query/github_app_config.sql internal/platform/config/config.go internal/platform/db/dbgen internal/githubapp/config_store.go internal/githubapp/config_store_test.go internal/githubapp/config_store_integration_test.go
git commit -m "feat(009): sealed instance GitHub App config store (manyforge-q4h)"
```

---

### Task 2: Fakeable GitHub API client

**Files:** Create `internal/githubapp/client.go`; Test `internal/githubapp/client_test.go`.

**Interfaces produced:**
- `type Installation struct { ID int64; Login, Type string }`
- `type GitHubAPI interface { ConvertManifest(ctx, code string) (AppCreds, error); ExchangeOAuthCode(ctx, clientID, clientSecret, code string) (string, error); ListUserInstallations(ctx, userToken string) ([]Installation, error) }`
- `type Client struct { HTTP *http.Client; APIBase, WebBase string }` implementing `GitHubAPI`; `func NewClient(timeout time.Duration) *Client`

- [ ] **Step 1: Failing test.** `internal/githubapp/client_test.go`:
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
            t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
        }
        _ = json.NewEncoder(w).Encode(map[string]any{"id": 99, "slug": "mf-review", "client_id": "Iv1.x",
            "client_secret": "cs", "pem": "-----BEGIN RSA PRIVATE KEY-----k", "webhook_secret": "whsec"})
    }))
    defer srv.Close()
    c := &Client{HTTP: srv.Client(), APIBase: srv.URL, WebBase: srv.URL}
    creds, err := c.ConvertManifest(context.Background(), "thecode")
    if err != nil { t.Fatalf("ConvertManifest: %v", err) }
    if creds.AppID != 99 || creds.Slug != "mf-review" || creds.PrivateKeyPEM == "" || creds.WebhookSecret != "whsec" {
        t.Fatalf("got %+v", creds)
    }
}

func TestListUserInstallationsExtractsIDsAndAccounts(t *testing.T) {
    srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        if got := r.Header.Get("Authorization"); got != "Bearer utoken" { t.Errorf("auth = %q", got) }
        _ = json.NewEncoder(w).Encode(map[string]any{"total_count": 1,
            "installations": []map[string]any{{"id": 22, "account": map[string]any{"login": "bluescripts-net", "type": "Organization"}}}})
    }))
    defer srv.Close()
    c := &Client{HTTP: srv.Client(), APIBase: srv.URL, WebBase: srv.URL}
    got, err := c.ListUserInstallations(context.Background(), "utoken")
    if err != nil { t.Fatalf("ListUserInstallations: %v", err) }
    if len(got) != 1 || got[0].ID != 22 || got[0].Login != "bluescripts-net" || got[0].Type != "Organization" {
        t.Fatalf("got %+v", got)
    }
}
```

- [ ] **Step 2: Run → FAIL.**

- [ ] **Step 3: Implement.** `internal/githubapp/client.go`:
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

type Installation struct{ ID int64; Login, Type string }

type GitHubAPI interface {
    ConvertManifest(ctx context.Context, code string) (AppCreds, error)
    ExchangeOAuthCode(ctx context.Context, clientID, clientSecret, code string) (string, error)
    ListUserInstallations(ctx context.Context, userToken string) ([]Installation, error)
}

type Client struct{ HTTP *http.Client; APIBase, WebBase string }

func NewClient(timeout time.Duration) *Client {
    return &Client{HTTP: netsafe.NewClient(timeout), APIBase: "https://api.github.com", WebBase: "https://github.com"}
}

func (c *Client) do(req *http.Request, out any) error {
    resp, err := c.HTTP.Do(req)
    if err != nil { return fmt.Errorf("github request: %w", err) }
    defer resp.Body.Close()
    body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
    if resp.StatusCode < 200 || resp.StatusCode >= 300 { return fmt.Errorf("github status %d", resp.StatusCode) } // never surface upstream body
    if out != nil {
        if err := json.Unmarshal(body, out); err != nil { return fmt.Errorf("github decode: %w", err) }
    }
    return nil
}

func (c *Client) ConvertManifest(ctx context.Context, code string) (AppCreds, error) {
    u := fmt.Sprintf("%s/app-manifests/%s/conversions", c.APIBase, url.PathEscape(code))
    req, _ := http.NewRequestWithContext(ctx, http.MethodPost, u, nil)
    req.Header.Set("Accept", "application/vnd.github+json")
    var r struct {
        ID           int64  `json:"id"`
        Slug         string `json:"slug"`
        ClientID     string `json:"client_id"`
        ClientSecret string `json:"client_secret"`
        PEM          string `json:"pem"`
        WebhookSecret string `json:"webhook_secret"`
    }
    if err := c.do(req, &r); err != nil { return AppCreds{}, err }
    return AppCreds{AppID: r.ID, Slug: r.Slug, ClientID: r.ClientID, ClientSecret: r.ClientSecret,
        PrivateKeyPEM: r.PEM, WebhookSecret: r.WebhookSecret}, nil
}

func (c *Client) ExchangeOAuthCode(ctx context.Context, clientID, clientSecret, code string) (string, error) {
    form := url.Values{"client_id": {clientID}, "client_secret": {clientSecret}, "code": {code}}
    req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.WebBase+"/login/oauth/access_token", strings.NewReader(form.Encode()))
    req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
    req.Header.Set("Accept", "application/json") // force JSON, not form-encoded
    var r struct{ AccessToken string `json:"access_token"` }
    if err := c.do(req, &r); err != nil { return "", err }
    if r.AccessToken == "" { return "", fmt.Errorf("github oauth: no access token") }
    return r.AccessToken, nil
}

func (c *Client) ListUserInstallations(ctx context.Context, userToken string) ([]Installation, error) {
    req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.APIBase+"/user/installations?per_page=100", nil)
    req.Header.Set("Authorization", "Bearer "+userToken)
    req.Header.Set("Accept", "application/vnd.github+json")
    var r struct {
        Installations []struct {
            ID int64 `json:"id"`
            Account struct{ Login, Type string } `json:"account"`
        } `json:"installations"`
    }
    if err := c.do(req, &r); err != nil { return nil, err }
    out := make([]Installation, 0, len(r.Installations))
    for _, in := range r.Installations { out = append(out, Installation{ID: in.ID, Login: in.Account.Login, Type: in.Account.Type}) }
    return out, nil
}
```
Known limitation: `per_page=100` without pagination — a user with >100 installations could false-negative the proof; acceptable for Slice 1, note it.

- [ ] **Step 4: Run → PASS.** **Step 5: Commit** (`client.go`, `client_test.go`): `feat(009): fakeable GitHub App API client (manyforge-q4h)`.

---

### Task 3: Signed single-use state + consumed-nonce store + operator gate + manifest START

**Files:** Create `migrations/0081_github_setup_nonce.{up,down}.sql`, `internal/githubapp/state.go`, `internal/githubapp/nonce.go`, `internal/githubapp/handler.go`, `internal/githubapp/manifest.go`; Modify `db/schema.sql`, `internal/platform/config/config.go`; Test `internal/githubapp/state_test.go`, `internal/githubapp/handler_manifest_test.go`.

**Interfaces produced:**
- `StatePayload struct { Purpose string; BusinessID, PrincipalID, AgentID uuid.UUID; Nonce string; Exp int64 }` (`Purpose` ∈ `"manifest"`,`"link"`); `signState`/`verifyState`; `func DeriveStateKey(masterKey []byte) []byte`
- `type NonceService struct { DB txRunner }`; `func (s *NonceService) Consume(ctx, nonce string) (bool, error)` (true = first use, false = replay)
- Dependency interfaces (satisfied by the concrete services + test stubs):
  - `appConfigStore { Get(ctx)(AppConfig,error); Save(ctx,AppCreds)error }`
  - `installOps { UpsertFromEvent(ctx,id int64,login,accountType string)error; MarkDeleted(ctx,id int64)error; SetSuspended(ctx,id int64,suspended bool)error; Link(ctx,id int64,businessID,agentID uuid.UUID)error }`
  - `nonceConsumer { Consume(ctx,nonce string)(bool,error) }`
  - `permChecker { Has(ctx, principalID, businessID uuid.UUID, perm string)(bool,error) }`
- `type Handler struct { Store appConfigStore; Installs installOps; API GitHubAPI; Nonces nonceConsumer; Perms permChecker; OperatorPrincipal uuid.UUID; PublicBaseURL string; StateKey []byte; Now func() time.Time; Logger *slog.Logger }`
- `func (h *Handler) OperatorRoutes(r chi.Router)` → `GET /github/app/manifest`, `POST /github/app/manifest/convert` (convert is Task 6)
- `config.Config.InstanceOperatorPrincipal uuid.UUID`, `config.Config.PublicBaseURL string`

- [ ] **Step 1: Failing state test.** `internal/githubapp/state_test.go`:
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
    p := StatePayload{Purpose: "link", BusinessID: uuid.New(), PrincipalID: uuid.New(), AgentID: uuid.New(),
        Nonce: "n1", Exp: now.Add(5 * time.Minute).Unix()}
    tok := signState(key, p)
    got, err := verifyState(key, tok, now)
    if err != nil { t.Fatalf("verifyState: %v", err) }
    if got.Purpose != "link" || got.BusinessID != p.BusinessID || got.AgentID != p.AgentID { t.Fatalf("mismatch: %+v", got) }
    if _, err := verifyState(key, tok[:len(tok)-2]+"xx", now); err == nil { t.Error("tampered verified") }
    if _, err := verifyState(key, tok, now.Add(10*time.Minute)); err == nil { t.Error("expired verified") }
    if _, err := verifyState([]byte("different-key-different-key-1234"), tok, now); err == nil { t.Error("wrong key verified") }
}
```

- [ ] **Step 2: Run → FAIL.**

- [ ] **Step 3: Implement state.** `internal/githubapp/state.go`:
```go
package githubapp

import (
    "crypto/hmac"
    "crypto/sha256"
    "encoding/base64"
    "encoding/json"
    "fmt"
    "strings"
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

func DeriveStateKey(masterKey []byte) []byte {
    m := hmac.New(sha256.New, masterKey)
    m.Write([]byte("github-app-oauth-state/v1"))
    return m.Sum(nil)
}

func signState(key []byte, p StatePayload) string {
    body, _ := json.Marshal(p)
    b := base64.RawURLEncoding.EncodeToString(body)
    m := hmac.New(sha256.New, key); m.Write([]byte(b))
    return b + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func verifyState(key []byte, raw string, now time.Time) (StatePayload, error) {
    var p StatePayload
    b, sigStr, ok := strings.Cut(raw, ".")
    if !ok { return p, fmt.Errorf("malformed state: %w", errs.ErrValidation) }
    m := hmac.New(sha256.New, key); m.Write([]byte(b))
    got, err := base64.RawURLEncoding.DecodeString(sigStr)
    if err != nil || !hmac.Equal(got, m.Sum(nil)) { return p, fmt.Errorf("bad state signature: %w", errs.ErrValidation) }
    body, err := base64.RawURLEncoding.DecodeString(b)
    if err != nil { return p, fmt.Errorf("bad state body: %w", errs.ErrValidation) }
    if err := json.Unmarshal(body, &p); err != nil { return p, fmt.Errorf("bad state json: %w", errs.ErrValidation) }
    if now.Unix() > p.Exp { return p, fmt.Errorf("state expired: %w", errs.ErrValidation) }
    return p, nil
}
```

- [ ] **Step 4: Run → PASS** (`go test ./internal/githubapp/ -run TestStateRoundTripAndTamper`).

- [ ] **Step 5: Nonce migration + service.** `migrations/0081_github_setup_nonce.up.sql`:
```sql
-- Single-use nonces for setup/link state. Tenantless, no RLS; manyforge_app inserts
-- directly (INSERT ... ON CONFLICT DO NOTHING; rows-affected = first use vs replay).
CREATE TABLE github_setup_nonce (
    nonce       text PRIMARY KEY,
    consumed_at timestamptz NOT NULL DEFAULT now()
);
GRANT SELECT, INSERT, DELETE ON github_setup_nonce TO manyforge_app;
```
`…down.sql`: `DROP TABLE IF EXISTS github_setup_nonce;`. Append the table to `db/schema.sql`. `internal/githubapp/nonce.go`:
```go
package githubapp

import (
    "context"
    "github.com/jackc/pgx/v5"
)

type NonceService struct{ DB txRunner }

// Consume returns true the FIRST time a nonce is seen, false on replay.
func (s *NonceService) Consume(ctx context.Context, nonce string) (bool, error) {
    var first bool
    err := s.DB.WithTx(ctx, func(tx pgx.Tx) error {
        ct, err := tx.Exec(ctx, "INSERT INTO github_setup_nonce (nonce) VALUES ($1) ON CONFLICT (nonce) DO NOTHING", nonce)
        if err != nil { return err }
        first = ct.RowsAffected() > 0
        return nil
    })
    return first, err
}
```

- [ ] **Step 6: Add operator-principal + public-base-URL config.** In `config.go`:
```go
// InstanceOperatorPrincipal gates instance setup routes. MANYFORGE_INSTANCE_OPERATOR_PRINCIPAL
// (UUID). uuid.Nil when unset — setup routes reject everyone (404). The operator finds their
// principal id from GET /api/v1/me.
InstanceOperatorPrincipal uuid.UUID
// PublicBaseURL is the externally-reachable base (https://hub.example.com) for GitHub
// redirect + webhook URLs. MANYFORGE_PUBLIC_BASE_URL. (Reuse an existing base-URL config if present.)
PublicBaseURL string
```
```go
if v := os.Getenv("MANYFORGE_INSTANCE_OPERATOR_PRINCIPAL"); v != "" {
    if cfg.InstanceOperatorPrincipal, err = uuid.Parse(v); err != nil {
        return Config{}, fmt.Errorf("MANYFORGE_INSTANCE_OPERATOR_PRINCIPAL: %w", err)
    }
}
cfg.PublicBaseURL = strings.TrimSuffix(os.Getenv("MANYFORGE_PUBLIC_BASE_URL"), "/")
```

- [ ] **Step 7: Add exported principal test-setter to httpx.** The context key is unexported (`middleware.go`, `ctxKeyPrincipal`), so tests can't inject a principal. Add to `internal/platform/httpx/middleware.go`, using the SAME key `AuthToPrincipal` writes:
```go
// WithPrincipal returns a context carrying principalID, as AuthToPrincipal would.
// Exported for handler tests that bypass the auth middleware.
func WithPrincipal(ctx context.Context, principalID uuid.UUID) context.Context {
    return context.WithValue(ctx, ctxKeyPrincipal, principalID)
}
```
(Match the exact key + value type `AuthToPrincipal`/`PrincipalFromContext` use.)

- [ ] **Step 8: Failing manifest handler test.** `internal/githubapp/handler_manifest_test.go`:
```go
package githubapp

import (
    "context"
    "encoding/json"
    "net/http"
    "net/http/httptest"
    "strings"
    "testing"
    "time"

    "github.com/go-chi/chi/v5"
    "github.com/google/uuid"
    "manyforge/internal/platform/httpx"
)

// Shared test doubles (used across Tasks 3/5/6, all package githubapp).
type fakeAPI struct {
    convertCreds AppCreds
    userInstalls []Installation
}
func (f *fakeAPI) ConvertManifest(ctx context.Context, code string) (AppCreds, error) { return f.convertCreds, nil }
func (f *fakeAPI) ExchangeOAuthCode(ctx context.Context, a, b, c string) (string, error) { return "utoken", nil }
func (f *fakeAPI) ListUserInstallations(ctx context.Context, t string) ([]Installation, error) { return f.userInstalls, nil }

type stubStore struct{ cfg AppConfig; getErr, saveErr error; saved *AppCreds }
func (s *stubStore) Get(ctx context.Context) (AppConfig, error) { return s.cfg, s.getErr }
func (s *stubStore) Save(ctx context.Context, c AppCreds) error { s.saved = &c; return s.saveErr }

type stubNonce struct{ first bool }
func (n *stubNonce) Consume(ctx context.Context, nonce string) (bool, error) { return n.first, nil }

func TestManifestRouteRejectsNonOperator(t *testing.T) {
    op := uuid.New()
    h := &Handler{OperatorPrincipal: op, StateKey: []byte("0123456789abcdef0123456789abcdef"),
        PublicBaseURL: "https://hub.example.com", Now: func() time.Time { return time.Unix(1_700_000_000, 0) }}
    r := chi.NewRouter()
    r.Group(func(g chi.Router) {
        g.Use(withPrincipalMW(uuid.New())) // not the operator
        h.OperatorRoutes(g)
    })
    req := httptest.NewRequest(http.MethodGet, "/github/app/manifest", nil)
    w := httptest.NewRecorder(); r.ServeHTTP(w, req)
    if w.Code != http.StatusNotFound { t.Fatalf("status = %d, want 404", w.Code) }
}

func TestManifestRouteReturnsJSONForOperator(t *testing.T) {
    op := uuid.New()
    h := &Handler{OperatorPrincipal: op, StateKey: []byte("0123456789abcdef0123456789abcdef"),
        PublicBaseURL: "https://hub.example.com", Now: func() time.Time { return time.Unix(1_700_000_000, 0) }}
    r := chi.NewRouter()
    r.Group(func(g chi.Router) { g.Use(withPrincipalMW(op)); h.OperatorRoutes(g) })
    req := httptest.NewRequest(http.MethodGet, "/github/app/manifest", nil)
    w := httptest.NewRecorder(); r.ServeHTTP(w, req)
    if w.Code != http.StatusOK { t.Fatalf("status = %d, want 200", w.Code) }
    var body struct{ ActionURL, Manifest, State string }
    if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil { t.Fatalf("decode: %v", err) }
    if !strings.Contains(body.ActionURL, "settings/apps/new") { t.Errorf("action_url = %q", body.ActionURL) }
    if !strings.Contains(body.Manifest, "hub.example.com/api/v1/github/webhook") { t.Error("manifest missing webhook url") }
    if !strings.Contains(body.Manifest, "hub.example.com/settings/github/installed") { t.Error("manifest missing callback_urls") }
    if body.State == "" { t.Error("missing state") }
}

// test helper: inject a principal into the request context via the exported setter.
func withPrincipalMW(pid uuid.UUID) func(http.Handler) http.Handler {
    return func(next http.Handler) http.Handler {
        return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
            next.ServeHTTP(w, req.WithContext(httpx.WithPrincipal(req.Context(), pid)))
        })
    }
}
```

- [ ] **Step 9: Run → FAIL.**

- [ ] **Step 10: Implement handler + manifest.** `internal/githubapp/handler.go`:
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
type nonceConsumer interface {
    Consume(ctx context.Context, nonce string) (bool, error)
}
type permChecker interface {
    Has(ctx context.Context, principalID, businessID uuid.UUID, perm string) (bool, error)
}

type Handler struct {
    Store             appConfigStore
    Installs          installOps
    API               GitHubAPI
    Nonces            nonceConsumer
    Perms             permChecker
    OperatorPrincipal uuid.UUID
    PublicBaseURL     string
    StateKey          []byte
    Now               func() time.Time
    Logger            *slog.Logger
}

func (h *Handler) log(ctx context.Context, msg string, err error) {
    if h.Logger != nil { h.Logger.ErrorContext(ctx, msg, "err", err) }
}

// operatorOnly gates a route on the config-pinned instance operator principal.
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
        g.Post("/github/app/manifest/convert", h.convertManifest) // implemented in Task 6
    })
}
```
(`time` is imported for the `Now func() time.Time` struct field.)
`internal/githubapp/manifest.go`:
```go
package githubapp

import (
    "encoding/json"
    "net/http"
    "time"

    "github.com/google/uuid"
    "manyforge/internal/platform/errs"
    "manyforge/internal/platform/httpx"
)

func (h *Handler) manifestJSON() (string, error) {
    m := map[string]any{
        "name":                     "manyforge-review",
        "url":                      h.PublicBaseURL,
        "public":                   true,
        "redirect_url":             h.PublicBaseURL + "/settings/github/app-created", // manifest-conversion redirect (SPA route)
        "callback_urls":            []string{h.PublicBaseURL + "/settings/github/installed"}, // OAuth-on-install redirect (SPA route)
        "request_oauth_on_install": true,
        "hook_attributes":          map[string]any{"url": h.PublicBaseURL + "/api/v1/github/webhook", "active": true},
        "default_permissions":      map[string]any{"contents": "read", "pull_requests": "write", "metadata": "read"},
        "default_events":           []string{"pull_request"}, // installation events are auto-delivered
    }
    b, err := json.Marshal(m)
    return string(b), err
}

// renderManifest returns the data the SPA needs to POST the App-creation form to GitHub.
func (h *Handler) renderManifest(w http.ResponseWriter, r *http.Request) {
    pid, _ := httpx.PrincipalFromContext(r.Context())
    now := h.Now()
    state := signState(h.StateKey, StatePayload{Purpose: "manifest", PrincipalID: pid,
        Nonce: uuid.NewString(), Exp: now.Add(15 * time.Minute).Unix()})
    manifest, err := h.manifestJSON()
    if err != nil { httpx.WriteError(w, r, errs.ErrValidation); return }
    httpx.WriteJSON(w, http.StatusOK, map[string]string{
        "action_url": "https://github.com/settings/apps/new",
        "manifest":   manifest,
        "state":      state,
    })
}
```
(`convertManifest` is implemented in Task 6; add a stub `func (h *Handler) convertManifest(w http.ResponseWriter, r *http.Request) { httpx.WriteError(w, r, errs.ErrValidation) }` in `manifest.go` now so the package compiles.)

- [ ] **Step 11: Run → PASS** (`go test ./internal/githubapp/ -run 'TestManifestRoute|TestStateRoundTrip'`). **Step 12: Commit** (migrations/0081*, schema.sql, state.go, nonce.go, handler.go, manifest.go, config.go, middleware.go, tests): `feat(009): signed single-use state + nonce store + operator-gated manifest start (manyforge-q4h)`.

---

### Task 4: `github_app_installation` table + DEFINER lifecycle/link (raw pgx) + service

**Files:** Create `migrations/0082_github_app_installation.{up,down}.sql`, `internal/githubapp/installations.go`; Modify `db/schema.sql`; Test `internal/githubapp/installations_integration_test.go` (`//go:build integration`).

**Interfaces produced:** `type InstallationService struct { DB txRunner }` satisfying `installOps` (raw-pgx DEFINER calls). No `db/query` file (functions are called via raw pgx per house style).

- [ ] **Step 1: Migration.** `migrations/0082_github_app_installation.up.sql`:
```sql
CREATE TABLE github_app_installation (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    installation_id bigint NOT NULL UNIQUE,
    account_login   text   NOT NULL,
    account_type    text   NOT NULL DEFAULT 'Organization',
    business_id     uuid,               -- NULL until linked
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
-- Linked rows visible to their business; unlinked rows (business_id IS NULL) invisible
-- to every principal — reachable only via the DEFINER functions below.
CREATE POLICY github_app_installation_rls ON github_app_installation FOR ALL
    USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())));

CREATE FUNCTION github_upsert_installation(p_installation_id bigint, p_login text, p_account_type text)
RETURNS void LANGUAGE sql SECURITY DEFINER SET search_path = public AS $$
    INSERT INTO github_app_installation (installation_id, account_login, account_type)
    VALUES (p_installation_id, p_login, p_account_type)
    ON CONFLICT (installation_id) DO UPDATE
        SET account_login = EXCLUDED.account_login, account_type = EXCLUDED.account_type, updated_at = now();
        -- NOTE: does NOT clear deleted_at (reinstalls always mint a new installation_id).
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

-- Link an UNLINKED row only, and ONLY to an agent that belongs to the business.
-- Returns rows affected (1 = linked, 0 = no eligible unlinked row / agent not in business).
CREATE FUNCTION github_link_installation(p_installation_id bigint, p_business_id uuid, p_agent_id uuid)
RETURNS integer LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE n integer;
BEGIN
    UPDATE github_app_installation gi
    SET business_id = p_business_id, agent_id = p_agent_id,
        tenant_root_id = (SELECT b.tenant_root_id FROM business b WHERE b.id = p_business_id),
        enabled = true, updated_at = now()
    WHERE gi.installation_id = p_installation_id AND gi.business_id IS NULL AND gi.deleted_at IS NULL
      AND EXISTS (SELECT 1 FROM agent a WHERE a.id = p_agent_id AND a.business_id = p_business_id);
    GET DIAGNOSTICS n = ROW_COUNT;
    RETURN n;
END;
$$;
REVOKE ALL ON FUNCTION github_link_installation(bigint, uuid, uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION github_link_installation(bigint, uuid, uuid) TO manyforge_app;
```
`…down.sql` drops the four functions then the table. Append the `CREATE TABLE` to `db/schema.sql`. (Confirm `business` has `tenant_root_id` and `agent` has `business_id` — both verified present.)

- [ ] **Step 2: Failing integration test.** `internal/githubapp/installations_integration_test.go`:
```go
//go:build integration

package githubapp_test

import (
    "context"
    "errors"
    "testing"

    "github.com/google/uuid"
    "github.com/jackc/pgx/v5"
    "manyforge/internal/githubapp"
    "manyforge/internal/platform/db/testdb"
    "manyforge/internal/platform/errs"
)

func TestInstallationLifecycleLinkAndQuarantine(t *testing.T) {
    ctx := context.Background()
    tdb, err := testdb.Start(ctx)
    if err != nil { t.Fatalf("testdb.Start: %v", err) }
    defer tdb.Close(ctx)

    // Seed business A (+member principal +agent), business B (+member principal), via tdb.Super
    // (RLS-exempt). Mirror the seed INSERTs an existing coding/connectors //go:build integration
    // test uses (business, principal, membership, agent). Capture: bizA, agentA, memberA,
    // bizB, memberB.
    bizA, agentA, memberA, bizB, memberB, foreignAgentB := seedTwoBusinesses(t, ctx, tdb)

    svc := &githubapp.InstallationService{DB: tdb.App}
    if err := svc.UpsertFromEvent(ctx, 7788, "bluescripts-net", "Organization"); err != nil { t.Fatalf("upsert: %v", err) }

    // RLS quarantine: unlinked row invisible to any business principal.
    if c := countInstalls(t, ctx, tdb, memberA); c != 0 { t.Fatalf("bizA sees %d unlinked, want 0", c) }

    // C-2: linking with a foreign business's agent must fail (agent not in bizA).
    if err := svc.Link(ctx, 7788, bizA, foreignAgentB); !errors.Is(err, errs.ErrNotFound) {
        t.Fatalf("link with foreign agent = %v, want ErrNotFound", err)
    }
    // Valid link.
    if err := svc.Link(ctx, 7788, bizA, agentA); err != nil { t.Fatalf("link: %v", err) }
    // Re-link is a no-op (already linked).
    if err := svc.Link(ctx, 7788, bizA, agentA); !errors.Is(err, errs.ErrNotFound) { t.Fatalf("relink = %v, want ErrNotFound", err) }
    // Now visible to bizA, invisible to bizB.
    if c := countInstalls(t, ctx, tdb, memberA); c != 1 { t.Fatalf("bizA sees %d after link, want 1", c) }
    if c := countInstalls(t, ctx, tdb, memberB); c != 0 { t.Fatalf("bizB sees %d, want 0", c) }
    _ = bizB
}

// countInstalls runs a raw count under memberPrincipal's RLS context via the App pool.
func countInstalls(t *testing.T, ctx context.Context, tdb *testdb.TestDB, principal uuid.UUID) int {
    var n int
    err := tdb.App.WithPrincipal(ctx, principal, func(tx pgx.Tx) error {
        return tx.QueryRow(ctx, "SELECT count(*) FROM github_app_installation").Scan(&n)
    })
    if err != nil { t.Fatalf("count: %v", err) }
    return n
}
```
Write `seedTwoBusinesses` in this file inserting via `tdb.Super`, mirroring an existing integration test's seed shape (business/principal/membership/agent columns).

- [ ] **Step 3: Run → FAIL.**

- [ ] **Step 4: Implement service (raw pgx).** `internal/githubapp/installations.go`:
```go
package githubapp

import (
    "context"
    "fmt"

    "github.com/google/uuid"
    "github.com/jackc/pgx/v5"
    "manyforge/internal/platform/errs"
)

type InstallationService struct{ DB txRunner }

func (s *InstallationService) UpsertFromEvent(ctx context.Context, id int64, login, accountType string) error {
    return s.DB.WithTx(ctx, func(tx pgx.Tx) error {
        _, err := tx.Exec(ctx, "SELECT github_upsert_installation($1, $2, $3)", id, login, accountType)
        return err
    })
}
func (s *InstallationService) MarkDeleted(ctx context.Context, id int64) error {
    return s.DB.WithTx(ctx, func(tx pgx.Tx) error {
        _, err := tx.Exec(ctx, "SELECT github_mark_installation_deleted($1)", id)
        return err
    })
}
func (s *InstallationService) SetSuspended(ctx context.Context, id int64, suspended bool) error {
    return s.DB.WithTx(ctx, func(tx pgx.Tx) error {
        _, err := tx.Exec(ctx, "SELECT github_set_installation_suspended($1, $2)", id, suspended)
        return err
    })
}
func (s *InstallationService) Link(ctx context.Context, id int64, businessID, agentID uuid.UUID) error {
    return s.DB.WithTx(ctx, func(tx pgx.Tx) error {
        var n int
        if err := tx.QueryRow(ctx, "SELECT github_link_installation($1, $2, $3)", id, businessID, agentID).Scan(&n); err != nil {
            return fmt.Errorf("link installation: %w", err)
        }
        if n == 0 { return errs.ErrNotFound } // no unlinked row, or agent not in business
        return nil
    })
}
```

- [ ] **Step 5: Run → PASS** (`go test -tags integration ./internal/githubapp/ -run TestInstallationLifecycleLinkAndQuarantine`).

- [ ] **Step 6: Wire `Installs` in `main.go`.** Replace the Task 3 placeholder: `Installs: &githubapp.InstallationService{DB: database},`. Run `go build ./...`.

- [ ] **Step 7: Commit** (migrations/0082*, schema.sql, installations.go, main.go, test): `feat(009): github_app_installation + DEFINER lifecycle/link (agent-in-business guarded) + service (manyforge-q4h)`.

---

### Task 5: GitHub webhook route — signature verify + `installation` lifecycle

**Files:** Create `internal/githubapp/webhook.go`, `internal/githubapp/webhook_test.go`, `internal/security_regression/github_webhook_sig_pin_test.go`; Modify `cmd/manyforge/main.go` (mount on `ingress`).

**Interfaces produced:** `func (h *Handler) WebhookRoutes(r chi.Router)` → `POST /github/webhook`.

- [ ] **Step 1: Failing test.** `internal/githubapp/webhook_test.go`:
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
    m := hmac.New(sha256.New, secret); m.Write(body)
    return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

type recordInstalls struct{ upserted, deleted []int64 }
func (r *recordInstalls) UpsertFromEvent(ctx context.Context, id int64, l, a string) error { r.upserted = append(r.upserted, id); return nil }
func (r *recordInstalls) MarkDeleted(ctx context.Context, id int64) error { r.deleted = append(r.deleted, id); return nil }
func (r *recordInstalls) SetSuspended(ctx context.Context, id int64, s bool) error { return nil }
func (r *recordInstalls) Link(ctx context.Context, id int64, biz, agent uuid.UUID) error { return nil }

func newWebhookHandler(store appConfigStore, installs installOps) *Handler {
    return &Handler{Store: store, Installs: installs, Now: func() time.Time { return time.Unix(1_700_000_000, 0) }}
}

func TestWebhookRejectsBadSignature(t *testing.T) {
    h := newWebhookHandler(&stubStore{cfg: AppConfig{AppID: 5, WebhookSecret: "whsec"}}, &recordInstalls{})
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
    rec := &recordInstalls{}
    h := newWebhookHandler(&stubStore{cfg: AppConfig{AppID: 5, WebhookSecret: "whsec"}}, rec)
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

- [ ] **Step 2: Run → FAIL.**

- [ ] **Step 3: Implement.** `internal/githubapp/webhook.go`:
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

const maxWebhookBody = 1 << 20

type installationEvent struct {
    Action       string `json:"action"`
    Installation struct {
        ID      int64 `json:"id"`
        Account struct{ Login, Type string } `json:"account"`
    } `json:"installation"`
}

func (h *Handler) WebhookRoutes(r chi.Router) { r.Post("/github/webhook", h.handleWebhook) }

func (h *Handler) handleWebhook(w http.ResponseWriter, r *http.Request) {
    body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBody+1))
    if err != nil || len(body) > maxWebhookBody { w.WriteHeader(http.StatusRequestEntityTooLarge); return }
    cfg, err := h.Store.Get(r.Context())
    if err != nil { w.WriteHeader(http.StatusAccepted); return } // unconfigured → no oracle
    if !validSignature(cfg.WebhookSecret, r.Header.Get("X-Hub-Signature-256"), body) {
        w.WriteHeader(http.StatusUnauthorized); return
    }
    if tid := r.Header.Get("X-GitHub-Hook-Installation-Target-ID"); tid != "" && tid != strconv.FormatInt(cfg.AppID, 10) {
        w.WriteHeader(http.StatusAccepted); return // not our App
    }
    if r.Header.Get("X-GitHub-Event") == "installation" { h.handleInstallationEvent(r, body) }
    // "pull_request" is Slice 2.
    w.WriteHeader(http.StatusAccepted)
}

func (h *Handler) handleInstallationEvent(r *http.Request, body []byte) {
    var ev installationEvent
    if err := json.Unmarshal(body, &ev); err != nil || ev.Installation.ID == 0 { return }
    switch ev.Action {
    case "created", "new_permissions_accepted":
        at := ev.Installation.Account.Type
        if at == "" { at = "Organization" }
        _ = h.Installs.UpsertFromEvent(r.Context(), ev.Installation.ID, ev.Installation.Account.Login, at)
    case "unsuspend":
        _ = h.Installs.SetSuspended(r.Context(), ev.Installation.ID, false)
    case "suspend":
        _ = h.Installs.SetSuspended(r.Context(), ev.Installation.ID, true)
    case "deleted":
        _ = h.Installs.MarkDeleted(r.Context(), ev.Installation.ID)
    }
}

// validSignature verifies X-Hub-Signature-256 (HMAC-SHA256 hex) constant-time.
// Empty secret or missing/malformed header → false (fail closed).
func validSignature(secret, header string, body []byte) bool {
    if secret == "" { return false }
    after, ok := strings.CutPrefix(header, "sha256=")
    if !ok { return false }
    got, err := hex.DecodeString(after)
    if err != nil { return false }
    mac := hmac.New(sha256.New, []byte(secret)); mac.Write(body)
    return hmac.Equal(got, mac.Sum(nil))
}
```
Follow-up note (Slice 2): reading `Store.Get` (a tx + 3 `Sealer.Open`, incl. the private key) on every delivery is fine at Slice-1 volume; add a `GetWebhookSecret` narrow query or a small cache before PR-event volume.

- [ ] **Step 4: Run → PASS.**

- [ ] **Step 5: Security-regression source pin (NO build tag — runs in `make test`).** `internal/security_regression/github_webhook_sig_pin_test.go`:
```go
package security_regression

import (
    "os"
    "strings"
    "testing"
)

const FindingGithubWebhookSigPin = "MF-009-GH-WEBHOOK-SIG"

func TestGithubWebhookSignaturePinned(t *testing.T) {
    b, err := os.ReadFile("../githubapp/webhook.go")
    if err != nil { t.Fatalf("read webhook.go: %v", err) }
    src := string(b)
    for _, want := range []string{"hmac.Equal", "sha256.New", `secret == ""`, "X-Hub-Signature-256"} {
        if !strings.Contains(src, want) { t.Fatalf("%s: webhook.go missing %q", FindingGithubWebhookSigPin, want) }
    }
}
```
Confirm the package name/header matches the untagged source-pin files (`accounting_us7_pins_test.go`, `agent_containment_pin_test.go`).

- [ ] **Step 6: Mount on `ingress` + run.** In `mountAPIRoutes` ingress group: `if h.githubApp != nil { h.githubApp.WebhookRoutes(ingress) }`. Run `go test ./internal/githubapp/... && go test ./internal/security_regression/ -run TestGithubWebhookSignaturePinned`.

- [ ] **Step 7: Commit** (webhook.go, webhook_test.go, pin test, main.go): `feat(009): GitHub webhook — HMAC verify + installation lifecycle (manyforge-q4h)`.

---

### Task 6: Authenticated completion — manifest convert, install-url start, verified link + wiring + contract

**Files:** Modify `internal/githubapp/manifest.go` (real `convertManifest`), `internal/githubapp/handler.go` (routes), `cmd/manyforge/main.go` (build handler + mount + permChecker adapter); Create `internal/githubapp/link.go`, `specs/009-github-app-review/contracts/openapi.yaml`, `cmd/manyforge/drift_009_test.go`; Test `internal/githubapp/handler_link_test.go`.

**Interfaces produced:**
- `func (h *Handler) BusinessRoutes(r chi.Router)` → `GET /businesses/{id}/github/app/install-url`
- `func (h *Handler) LinkRoutes(r chi.Router)` → `POST /github/app/installations/link`
- real `convertManifest` (operator POST)

- [ ] **Step 1: Failing link test.** `internal/githubapp/handler_link_test.go`:
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

type linkRec struct{ install int64; biz, agent uuid.UUID; called bool; err error }
func (r *linkRec) UpsertFromEvent(ctx context.Context, id int64, l, a string) error { return nil }
func (r *linkRec) MarkDeleted(ctx context.Context, id int64) error                  { return nil }
func (r *linkRec) SetSuspended(ctx context.Context, id int64, s bool) error         { return nil }
func (r *linkRec) Link(ctx context.Context, id int64, biz, agent uuid.UUID) error {
    r.install, r.biz, r.agent, r.called = id, biz, agent, true; return r.err
}

type stubPerms struct{ ok bool }
func (p *stubPerms) Has(ctx context.Context, pr, b uuid.UUID, perm string) (bool, error) { return p.ok, nil }

func linkHandler(api GitHubAPI, installs installOps, perms permChecker, nonceFirst bool) (*Handler, []byte) {
    key := []byte("0123456789abcdef0123456789abcdef")
    return &Handler{API: api, Installs: installs, Perms: perms, Nonces: &stubNonce{first: nonceFirst},
        Store: &stubStore{cfg: AppConfig{ClientID: "cid", ClientSecret: "sec"}}, StateKey: key,
        Now: func() time.Time { return time.Unix(1_700_000_000, 0) }}, key
}

func linkReq(state string) *http.Request {
    body := strings.NewReader(`{"code":"oc","installation_id":"555","state":"` + state + `"}`)
    req := httptest.NewRequest(http.MethodPost, "/github/app/installations/link", body)
    // caller is authenticated:
    return req.WithContext(httpx.WithPrincipal(req.Context(), uuid.New()))
}

func TestLinkRequiresOAuthProof(t *testing.T) {
    biz, agent := uuid.New(), uuid.New()
    rec := &linkRec{}
    h, key := linkHandler(&fakeAPI{userInstalls: []Installation{}}, rec, &stubPerms{ok: true}, true) // controls NO installs
    state := signState(key, StatePayload{Purpose: "link", BusinessID: biz, AgentID: agent, Nonce: "n", Exp: h.Now().Add(time.Minute).Unix()})
    r := chi.NewRouter(); h.LinkRoutes(r)
    w := httptest.NewRecorder(); r.ServeHTTP(w, linkReq(state))
    if w.Code == http.StatusOK || rec.called { t.Fatalf("linked without proof (status %d, called %v)", w.Code, rec.called) }
}

func TestLinkRejectsCallerLackingPermission(t *testing.T) {
    biz, agent := uuid.New(), uuid.New()
    rec := &linkRec{}
    h, key := linkHandler(&fakeAPI{userInstalls: []Installation{{ID: 555}}}, rec, &stubPerms{ok: false}, true) // not a member
    state := signState(key, StatePayload{Purpose: "link", BusinessID: biz, AgentID: agent, Nonce: "n", Exp: h.Now().Add(time.Minute).Unix()})
    r := chi.NewRouter(); h.LinkRoutes(r)
    w := httptest.NewRecorder(); r.ServeHTTP(w, linkReq(state))
    if w.Code != http.StatusNotFound || rec.called { t.Fatalf("non-member linked (status %d, called %v)", w.Code, rec.called) }
}

func TestLinkSucceedsWithProofAndPermission(t *testing.T) {
    biz, agent := uuid.New(), uuid.New()
    rec := &linkRec{}
    h, key := linkHandler(&fakeAPI{userInstalls: []Installation{{ID: 555, Login: "bluescripts-net", Type: "Organization"}}}, rec, &stubPerms{ok: true}, true)
    state := signState(key, StatePayload{Purpose: "link", BusinessID: biz, AgentID: agent, Nonce: "n", Exp: h.Now().Add(time.Minute).Unix()})
    r := chi.NewRouter(); h.LinkRoutes(r)
    w := httptest.NewRecorder(); r.ServeHTTP(w, linkReq(state))
    if w.Code != http.StatusOK { t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String()) }
    if !rec.called || rec.install != 555 || rec.biz != biz || rec.agent != agent { t.Fatalf("Link got (%d,%v,%v)", rec.install, rec.biz, rec.agent) }
}

func TestLinkRejectsReplayedNonce(t *testing.T) {
    biz, agent := uuid.New(), uuid.New()
    rec := &linkRec{}
    h, key := linkHandler(&fakeAPI{userInstalls: []Installation{{ID: 555}}}, rec, &stubPerms{ok: true}, false) // nonce already used
    state := signState(key, StatePayload{Purpose: "link", BusinessID: biz, AgentID: agent, Nonce: "n", Exp: h.Now().Add(time.Minute).Unix()})
    r := chi.NewRouter(); h.LinkRoutes(r)
    w := httptest.NewRecorder(); r.ServeHTTP(w, linkReq(state))
    if w.Code == http.StatusOK || rec.called { t.Fatalf("replayed nonce accepted (status %d)", w.Code) }
}
```

- [ ] **Step 2: Run → FAIL** (LinkRoutes undefined).

- [ ] **Step 3: Implement link + install-url.** `internal/githubapp/link.go`:
```go
package githubapp

import (
    "net/http"
    "strconv"
    "time"

    "github.com/go-chi/chi/v5"
    "github.com/google/uuid"
    "manyforge/internal/platform/authz"
    "manyforge/internal/platform/errs"
    "manyforge/internal/platform/httpx"
)

func (h *Handler) LinkRoutes(r chi.Router)     { r.Post("/github/app/installations/link", h.linkInstallation) }
func (h *Handler) BusinessRoutes(r chi.Router) { r.Get("/businesses/{id}/github/app/install-url", h.installURL) }

// installURL (auth + connectors-manage on {id}): mints the GitHub install URL carrying a signed link state.
func (h *Handler) installURL(w http.ResponseWriter, r *http.Request) {
    pid, ok := httpx.PrincipalFromContext(r.Context())
    if !ok { httpx.WriteError(w, r, errs.ErrNotFound); return }
    bid, err := uuid.Parse(chi.URLParam(r, "id"))
    if err != nil { httpx.WriteError(w, r, errs.ErrNotFound); return }
    agentID, err := uuid.Parse(r.URL.Query().Get("agent_id"))
    if err != nil { httpx.WriteError(w, r, errs.ErrValidation); return }
    cfg, err := h.Store.Get(r.Context())
    if err != nil { httpx.WriteError(w, r, err); return } // ErrNotFound if App not yet created
    now := h.Now()
    state := signState(h.StateKey, StatePayload{Purpose: "link", BusinessID: bid, PrincipalID: pid,
        AgentID: agentID, Nonce: uuid.NewString(), Exp: now.Add(15 * time.Minute).Unix()})
    httpx.WriteJSON(w, http.StatusOK, map[string]string{
        "install_url": "https://github.com/apps/" + cfg.Slug + "/installations/new?state=" + state,
    })
}

// linkInstallation (authenticated; in-handler perm on state.BusinessID): closes M-1.
func (h *Handler) linkInstallation(w http.ResponseWriter, r *http.Request) {
    caller, ok := httpx.PrincipalFromContext(r.Context())
    if !ok { httpx.WriteError(w, r, errs.ErrNotFound); return }
    var in struct {
        Code           string `json:"code"`
        InstallationID string `json:"installation_id"`
        State          string `json:"state"`
    }
    if !httpx.DecodeJSON(w, r, &in) { return }
    p, err := verifyState(h.StateKey, in.State, h.Now())
    if err != nil || p.Purpose != "link" { httpx.WriteError(w, r, errs.ErrValidation); return }
    // Authorization: the caller must be a connectors-manage member of the state's business.
    okPerm, err := h.Perms.Has(r.Context(), caller, p.BusinessID, authz.PermConnectorsManage)
    if err != nil { httpx.WriteError(w, r, err); return }
    if !okPerm { httpx.WriteError(w, r, errs.ErrNotFound); return } // no oracle
    installID, err := strconv.ParseInt(in.InstallationID, 10, 64)
    if err != nil { httpx.WriteError(w, r, errs.ErrValidation); return }
    first, err := h.Nonces.Consume(r.Context(), p.Nonce)
    if err != nil { httpx.WriteError(w, r, err); return }
    if !first { httpx.WriteError(w, r, errs.ErrConflict); return } // replayed state
    // GitHub-side control proof.
    cfg, err := h.Store.Get(r.Context())
    if err != nil { httpx.WriteError(w, r, err); return }
    userToken, err := h.API.ExchangeOAuthCode(r.Context(), cfg.ClientID, cfg.ClientSecret, in.Code)
    if err != nil { h.log(r.Context(), "oauth exchange", err); httpx.WriteError(w, r, errs.ErrValidation); return }
    installs, err := h.API.ListUserInstallations(r.Context(), userToken)
    if err != nil { h.log(r.Context(), "list installations", err); httpx.WriteError(w, r, errs.ErrValidation); return }
    var matched *Installation
    for i := range installs { if installs[i].ID == installID { matched = &installs[i]; break } }
    if matched == nil { httpx.WriteError(w, r, errs.ErrForbidden); return } // caller doesn't control it → 404
    // Link; if the installation.created webhook hasn't landed yet, upsert then retry once (M-2).
    if err := h.Installs.Link(r.Context(), installID, p.BusinessID, p.AgentID); err != nil {
        if uerr := h.Installs.UpsertFromEvent(r.Context(), installID, matched.Login, matched.Type); uerr != nil {
            httpx.WriteError(w, r, uerr); return
        }
        if err := h.Installs.Link(r.Context(), installID, p.BusinessID, p.AgentID); err != nil {
            httpx.WriteError(w, r, err); return // e.g. agent-not-in-business (C-2) → ErrNotFound → 404
        }
    }
    httpx.WriteJSON(w, http.StatusOK, map[string]bool{"linked": true})
}
```

- [ ] **Step 4: Implement `convertManifest` (replace the Task 3 stub) in `manifest.go`:**
```go
func (h *Handler) convertManifest(w http.ResponseWriter, r *http.Request) {
    // route is already operator-gated via OperatorRoutes.
    var in struct{ Code, State string }
    if !httpx.DecodeJSON(w, r, &in) { return }
    p, err := verifyState(h.StateKey, in.State, h.Now())
    if err != nil || p.Purpose != "manifest" { httpx.WriteError(w, r, errs.ErrValidation); return }
    first, err := h.Nonces.Consume(r.Context(), p.Nonce)
    if err != nil { httpx.WriteError(w, r, err); return }
    if !first { httpx.WriteError(w, r, errs.ErrConflict); return }
    creds, err := h.API.ConvertManifest(r.Context(), in.Code)
    if err != nil { h.log(r.Context(), "manifest convert", err); httpx.WriteError(w, r, errs.ErrValidation); return }
    if err := h.Store.Save(r.Context(), creds); err != nil { httpx.WriteError(w, r, err); return } // ErrConflict→409
    httpx.WriteJSON(w, http.StatusOK, map[string]string{"slug": creds.Slug})
}
```
Add imports `strconv`? (manifest.go now needs `httpx`, `errs` — already; no `strconv`.)

- [ ] **Step 5: Run → PASS** (`go test ./internal/githubapp/ -run 'TestLink|TestManifestRoute'`).

- [ ] **Step 6: Wire the handler + permChecker in `main.go`.** After the connector sealer/vault block, guarded by the master key:
```go
var githubAppH *githubapp.Handler
if len(cfg.GitHubAppMasterKey) > 0 {
    gaSealer, err := mfcrypto.NewSealer(cfg.GitHubAppMasterKey)
    if err != nil { logger.Error("init github app sealer", "err", err); os.Exit(1) }
    githubAppH = &githubapp.Handler{
        Store: &githubapp.ConfigStore{DB: database, Sealer: gaSealer},
        Installs: &githubapp.InstallationService{DB: database},
        API: githubapp.NewClient(15 * time.Second),
        Nonces: &githubapp.NonceService{DB: database},
        Perms: ghPerms{db: database, resolve: permResolve}, // adapter below
        OperatorPrincipal: cfg.InstanceOperatorPrincipal,
        PublicBaseURL: cfg.PublicBaseURL,
        StateKey: githubapp.DeriveStateKey(cfg.GitHubAppMasterKey),
        Now: time.Now, Logger: logger,
    }
} else {
    logger.Warn("MANYFORGE_GITHUB_APP_MASTER_KEY unset; GitHub App integration disabled")
}
```
Add a `permChecker` adapter mirroring `httpx.RequirePermission`'s internal resolve+has logic (read `internal/platform/httpx/authz_mw.go:30` for the exact `permResolve` signature + how it checks a permission):
```go
type ghPerms struct { db *db.DB; resolve httpx.PermissionResolver }
func (g ghPerms) Has(ctx context.Context, principalID, businessID uuid.UUID, perm string) (bool, error) {
    var has bool
    err := g.db.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
        perms, err := g.resolve(ctx, tx, businessID) // match PermissionResolver's exact signature (authz_mw.go)
        if err != nil { return err }
        has = perms.Has(perm) // adjust to the real return type, e.g. slices.Contains(perms, perm)
        return nil
    })
    return has, err
}
```
This adapter is the M-1 authorization gate — add a **`//go:build integration` test** (`internal/githubapp` or `cmd`) asserting `Has` returns true for a `connectors-manage` member of a business and false for a non-member/other-business principal, using the `testdb` seed. Add `githubApp *githubapp.Handler` to `apiHandlers` (`main.go:783`), pass `githubAppH`. In `mountAPIRoutes`:
```go
if h.githubApp != nil {
    ingress // already mounted in Task 5: h.githubApp.WebhookRoutes(ingress)
    pr.Group(func(g chi.Router) { h.githubApp.OperatorRoutes(g) })      // RequireAuth + operatorOnly (internal)
    pr.Group(func(g chi.Router) { h.githubApp.LinkRoutes(g) })          // RequireAuth; perm checked in-handler
    pr.Group(func(g chi.Router) { g.Use(h.connectorsManage); h.githubApp.BusinessRoutes(g) }) // {id} gate
}
```

- [ ] **Step 7: OpenAPI contract + drift test (M-3).** Create `specs/009-github-app-review/contracts/openapi.yaml` documenting: `GET /api/v1/github/app/manifest`, `POST /api/v1/github/app/manifest/convert`, `POST /api/v1/github/app/installations/link`, `GET /api/v1/businesses/{id}/github/app/install-url`, `POST /api/v1/github/webhook`. Create `cmd/manyforge/drift_009_test.go` mirroring `drift_007_test.go` with an `is009Op` predicate matching paths containing `/github/`. In `drift_test.go`'s `apiRoutes`, add `githubApp: &githubapp.Handler{}` to the `apiHandlers` literal (mirroring `codingReviews: &coding.Handler{}`) so the route walk sees the new routes.

- [ ] **Step 8: Full suites.** `go build ./... && make test && go test -tags integration ./internal/githubapp/... && go test -tags contract ./cmd/...` — all PASS.

- [ ] **Step 9: Commit** (link.go, manifest.go, handler.go, main.go, specs/009*, drift_009_test.go, drift_test.go, handler_link_test.go): `feat(009): authenticated completion — manifest convert + verified link (closes M-1) + contract (manyforge-q4h)`.

---

### Task 7: SPA redirect-landing routes + Playwright e2e

**Files:** Create `web/src/app/core/github-app.service.ts`, `web/src/app/pages/settings/github-app-created.ts`, `web/src/app/pages/settings/github-installed.ts`, `web/e2e/github-app.spec.ts`; Modify `web/src/app/app.routes.ts`.

**Interfaces produced:** two SPA routes that catch GitHub's redirects and call the Task 6 endpoints; a `GithubAppService`.

- [ ] **Step 1: Service.** `web/src/app/core/github-app.service.ts`:
```ts
import { Injectable, inject } from '@angular/core';
import { HttpClient } from '@angular/common/http';
import { Observable } from 'rxjs';

@Injectable({ providedIn: 'root' })
export class GithubAppService {
  private http = inject(HttpClient);
  convertManifest(body: { code: string; state: string }): Observable<{ slug: string }> {
    return this.http.post<{ slug: string }>('/api/v1/github/app/manifest/convert', body);
  }
  linkInstallation(body: { code: string; installation_id: string; state: string }): Observable<{ linked: boolean }> {
    return this.http.post<{ linked: boolean }>('/api/v1/github/app/installations/link', body);
  }
}
```

- [ ] **Step 2: App-created component.** `web/src/app/pages/settings/github-app-created.ts` — standalone, reads `?code`/`?state` in `ngOnInit`, POSTs, renders success/error with `data-testid`:
```ts
import { Component, OnInit, inject, signal } from '@angular/core';
import { ActivatedRoute } from '@angular/router';
import { HttpErrorResponse } from '@angular/common/http';
import { GithubAppService } from '../../core/github-app.service';

@Component({
  selector: 'app-github-app-created',
  imports: [],
  template: `
    <div class="mf-card" data-testid="github-app-created">
      @if (state() === 'working') { <p>Finishing GitHub App setup…</p> }
      @if (state() === 'done') { <p data-testid="gh-success">GitHub App created. You can now connect your organizations.</p> }
      @if (state() === 'error') { <p class="mf-error" data-testid="gh-error">{{ message() }}</p> }
    </div>`,
})
export class GithubAppCreatedComponent implements OnInit {
  private route = inject(ActivatedRoute);
  private api = inject(GithubAppService);
  state = signal<'working' | 'done' | 'error'>('working');
  message = signal('');
  ngOnInit(): void {
    const code = this.route.snapshot.queryParamMap.get('code') ?? '';
    const st = this.route.snapshot.queryParamMap.get('state') ?? '';
    if (!code || !st) { this.state.set('error'); this.message.set('Missing setup parameters.'); return; }
    this.api.convertManifest({ code, state: st }).subscribe({
      next: () => this.state.set('done'),
      error: (e: HttpErrorResponse) => { this.state.set('error'); this.message.set(this.describe(e)); },
    });
  }
  private describe(e: HttpErrorResponse): string {
    if (e.status === 409) return 'A GitHub App is already configured for this instance.';
    if (e.status === 404) return 'Not authorized to complete this setup.';
    return 'Could not complete GitHub App setup. Please try again.';
  }
}
```

- [ ] **Step 3: Installed component.** `web/src/app/pages/settings/github-installed.ts` — same shape, reads `?code`/`?installation_id`/`?state`, calls `linkInstallation`, handles `?setup_action=request` (admin-approval-pending: no `installation_id`) with a friendly message:
```ts
import { Component, OnInit, inject, signal } from '@angular/core';
import { ActivatedRoute } from '@angular/router';
import { HttpErrorResponse } from '@angular/common/http';
import { GithubAppService } from '../../core/github-app.service';

@Component({
  selector: 'app-github-installed',
  imports: [],
  template: `
    <div class="mf-card" data-testid="github-installed">
      @if (state() === 'working') { <p>Linking your installation…</p> }
      @if (state() === 'pending') { <p data-testid="gh-pending">Installation is awaiting org-admin approval. Re-run this once approved.</p> }
      @if (state() === 'done') { <p data-testid="gh-linked">Installation linked. manyforge will review pull requests for this organization.</p> }
      @if (state() === 'error') { <p class="mf-error" data-testid="gh-error">{{ message() }}</p> }
    </div>`,
})
export class GithubInstalledComponent implements OnInit {
  private route = inject(ActivatedRoute);
  private api = inject(GithubAppService);
  state = signal<'working' | 'pending' | 'done' | 'error'>('working');
  message = signal('');
  ngOnInit(): void {
    const q = this.route.snapshot.queryParamMap;
    const code = q.get('code') ?? '', instId = q.get('installation_id') ?? '', st = q.get('state') ?? '';
    if (q.get('setup_action') === 'request' || !instId) { this.state.set('pending'); return; }
    if (!code || !st) { this.state.set('error'); this.message.set('Missing installation parameters.'); return; }
    this.api.linkInstallation({ code, installation_id: instId, state: st }).subscribe({
      next: () => this.state.set('done'),
      error: (e: HttpErrorResponse) => { this.state.set('error'); this.message.set(this.describe(e)); },
    });
  }
  private describe(e: HttpErrorResponse): string {
    if (e.status === 404) return "You don't have permission to link to this business, or the installation couldn't be verified.";
    if (e.status === 409) return 'This link request was already used. Start again from settings.';
    return 'Could not link the installation. Please try again.';
  }
}
```

- [ ] **Step 4: Routes.** In `web/src/app/app.routes.ts`, add above the `**` catch-all:
```ts
{ path: 'settings/github/app-created', canActivate: [authGuard],
  loadComponent: () => import('./pages/settings/github-app-created').then((m) => m.GithubAppCreatedComponent) },
{ path: 'settings/github/installed', canActivate: [authGuard],
  loadComponent: () => import('./pages/settings/github-installed').then((m) => m.GithubInstalledComponent) },
```

- [ ] **Step 5: Playwright e2e.** `web/e2e/github-app.spec.ts` — mirror `connectors.spec.ts`'s `auth()` + `**/api/**` mock; assert the installed page POSTs and shows success, and that a proof failure shows the error copy:
```ts
import { test, expect } from '@playwright/test';

async function auth(page: import('@playwright/test').Page) {
  await page.addInitScript(() => localStorage.setItem('mf_access', 'tok'));
  await page.route('**/api/v1/me', (r) => r.fulfill({ json: { id: 'p1', email: 'op@example.com' } }));
  await page.route('**/api/v1/businesses', (r) => r.fulfill({ json: { items: [] } }));
}

test('github installed: links successfully', async ({ page }) => {
  await auth(page);
  let posted: any = null;
  await page.route('**/api/v1/github/app/installations/link', async (r) => {
    posted = r.request().postDataJSON();
    await r.fulfill({ json: { linked: true } });
  });
  await page.goto('/settings/github/installed?code=oc&installation_id=555&state=sig.tok');
  await expect(page.getByTestId('gh-linked')).toBeVisible();
  expect(posted).toEqual({ code: 'oc', installation_id: '555', state: 'sig.tok' });
});

test('github installed: shows error on rejected proof', async ({ page }) => {
  await auth(page);
  await page.route('**/api/v1/github/app/installations/link', (r) => r.fulfill({ status: 404, json: { code: 'NOT_FOUND' } }));
  await page.goto('/settings/github/installed?code=oc&installation_id=555&state=sig.tok');
  await expect(page.getByTestId('gh-error')).toBeVisible();
});

test('github installed: pending when awaiting admin approval', async ({ page }) => {
  await auth(page);
  await page.goto('/settings/github/installed?setup_action=request&state=sig.tok');
  await expect(page.getByTestId('gh-pending')).toBeVisible();
});
```

- [ ] **Step 6: Run unit + e2e.** `cd web && npm test` (Vitest, unit) then, with `air` on :8081 and `ng serve --port 4300 --proxy-config proxy.conf.json` running, `npm run e2e -- github-app`. Expected: PASS (the redirect-landing pages POST and render success/error/pending). If `ng serve`/`air` aren't up, start them first (see repo dev docs / `Skill: gstack`).

- [ ] **Step 7: Commit** (service, two components, routes, e2e spec): `feat(009): SPA redirect-landing routes for GitHub App setup + linking + e2e (manyforge-q4h)`.

---

## Slice 1 completion checks

- [ ] `make test` green; `make sec-test` green (or `go test ./internal/security_regression/...` — the new pin is untagged and runs in `make test`).
- [ ] `go test -tags integration ./internal/githubapp/...` green (config non-overwrite, install lifecycle + RLS quarantine + agent-in-business guard).
- [ ] `go test -tags contract ./cmd/...` green — all five new routes in `specs/009-*/contracts/openapi.yaml`, `drift_009_test.go` passes, `apiRoutes` includes a non-nil `githubApp`.
- [ ] `go build -tags ui_embed ./...` green; `cd web && npm run build` green; `npm run e2e -- github-app` green.
- [ ] Feature inert when `MANYFORGE_GITHUB_APP_MASTER_KEY` unset (handler nil → routes not mounted; server boots).
- [ ] Diff carries no real secret material (only fake fixtures).

## What Slice 1 deliberately does NOT do (→ Slice 2/3)

- No `pull_request` handling, `github_installation_context`, app-backed `repo_connector`, App JWT, or per-repo installation-token minting (Slice 2).
- No filters, budget/rate caps, supersede, delivery-id/head-SHA dedupe, or observability counters (Slice 3).
- `ListUserInstallations` doesn't paginate past 100; `Store.Get` is uncached per webhook delivery — both fine at Slice-1 volume, revisit in Slice 2.
- Consumed-nonce rows aren't pruned yet (add a periodic `DELETE FROM github_setup_nonce WHERE consumed_at < now() - interval '1 day'` sweep later).
