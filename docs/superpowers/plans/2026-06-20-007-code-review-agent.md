# Code Review Agent (Spec 007 — Slice 1) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a user point an agent at a GitHub pull request and get an automatic code review posted back, produced by opencode running read-only inside an ephemeral, credential-free Docker sandbox.

**Architecture:** A new `internal/connectors/github` repo connector (distinct from the issue-shaped `TicketingConnector`) resolves a vault-sealed GitHub token. A `CodeReviewService` in `internal/agents/coding` orchestrates one review: it clones the PR head **on the host**, runs opencode in a Docker sandbox (read-only checkout, only the LLM key inside, egress allowlisted to the LLM endpoint via a forced proxy), parses opencode's structured findings, and posts a single PR review **automatically** (advisory output → no approval gate). The review reuses the spec-003 run lifecycle (`agent_run`/`RunStore`), audit, and budget, but does **not** invoke the LLM loop `Engine.Run` — opencode is the only reviewing LLM.

**Tech Stack:** Go (`internal/` layout), PostgreSQL + sqlc **v1.27.0**, pgx/v5, chi router, testcontainers for integration, Docker (sandbox backend, shelled out via the `docker` CLI), `git` CLI (host-side clone), opencode (baked into a container image).

## Global Constraints

- **Go module layout:** all new code under `internal/`; thin HTTP handlers (validate → service → JSON), business logic in services.
- **sqlc:** generate with the pinned bottle only — `/opt/homebrew/Cellar/sqlc/1.27.0/bin/sqlc generate`. Never hand-edit `internal/platform/db/dbgen/`. After editing `db/query/*.sql` or `db/schema.sql`, regenerate.
- **DB tenancy:** every new table carries `business_id uuid NOT NULL` + `tenant_root_id uuid NOT NULL`, `UNIQUE (id, tenant_root_id)`, `FOREIGN KEY (business_id, tenant_root_id) REFERENCES business (id, tenant_root_id)`, `GRANT SELECT, INSERT, UPDATE, DELETE ... TO manyforge_app`, RLS `ENABLE` + business-scoped policy `USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))`.
- **schema.sql:** `db/schema.sql` carries tables/types ONLY (no policies/functions/DEFINERs) — add the new tables there too so sqlc validates queries.
- **Migrations:** `migrations/NNNN_snake.up.sql` + `.down.sql`, numbered after the current latest (currently **0069** → start at **0070**). The backend refuses to boot on schema drift, so after adding migrations run `make migrate` against the dev DB **before** starting `air`.
- **Errors:** service layer wraps `errs.ErrValidation/ErrNotFound/ErrConflict`; handlers call `httpx.WriteError`. Foreign/unknown ids return the same `ErrNotFound` (no existence oracle). Never echo raw `err.Error()` except typed validation errors.
- **Audit:** `audit.Write(ctx, tx, audit.Entry{...})` in the SAME tx as the change; append-only.
- **RLS access:** services run inside `DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {...})`.
- **Verification gates before every commit:** `make test` (unit + fast pins), `make lint` (vet + staticcheck), and where the task touched routes/openapi `go test -tags contract ./cmd/...`, where it touched security pins `make sec-test`. No Co-Authored-By trailer on commits.
- **Security invariant (this slice):** the sandbox holds NO repo credential and NO ambient host secret — only the run-scoped LLM key; the repo connector exposes NO code-write method; review posting is intentionally ungated.

---

## File Structure

**New files**
- `migrations/0070_repo_connector.up.sql` / `.down.sql` — repo_connector table + RLS.
- `migrations/0071_code_review.up.sql` / `.down.sql` — code_review table + RLS.
- `db/query/repo_connector.sql`, `db/query/code_review.sql` — sqlc queries.
- `internal/connectors/repo_connector.go` — `RepoConnector` interface + DTOs (`PullRequest`, `Finding`, `Review`, `ReviewRef`, `ResolvedRepoConnector`, `CreateRepoConnectorInput`). **No write/push method.**
- `internal/connectors/repo_service.go` — `RepoConnectorService.Create/Resolve` (vault + SSRF guard + dbgen).
- `internal/connectors/github/client.go` — GitHub `RepoConnector` impl.
- `internal/connectors/github/factory.go` — builds the client from a `ResolvedRepoConnector`.
- `internal/agents/coding/sandbox/runner.go` — `SandboxRunner` interface + `SandboxSpec`/`SandboxResult` + a `FakeRunner`.
- `internal/agents/coding/sandbox/docker.go` — Docker backend (CLI).
- `internal/agents/coding/sandbox/proxy.go` — egress-proxy lifecycle helpers (start/ensure the allowlisting proxy + network).
- `cmd/mf-egress-proxy/main.go` — minimal allowlisting HTTP CONNECT proxy (baked into a tiny image).
- `deploy/egress-proxy/Dockerfile`, `deploy/sandbox/Dockerfile`, `deploy/sandbox/opencode.json`, `deploy/sandbox-stub/Dockerfile` — images.
- `internal/agents/coding/findings.go` — findings JSON schema + validation + markdown render.
- `internal/agents/coding/clone.go` — host-side clone at PR head.
- `internal/agents/coding/credresolver.go` — `AICredentialResolver` seam + adapter to the existing AI credential service.
- `internal/agents/coding/service.go` — `CodeReviewService.Trigger` orchestration + persistence.
- `internal/agents/coding/handler.go` — HTTP handlers (repo connectors, code reviews).
- `specs/007-coding-review-agents/contracts/openapi.yaml` — 007 REST contract.
- `cmd/manyforge/drift_007_test.go` — 007 OpenAPI drift test.
- `internal/security_regression/coding_review_pins_test.go` — slice-1 security pins.

**Modified files**
- `internal/agents/agent_run.go` — add `targetTypeCodeReview` + accept it in `validTargetType()`.
- `db/schema.sql` — add `repo_connector` + `code_review` table definitions.
- `cmd/manyforge/main.go` — construct `RepoConnectorService` + `CodeReviewService`, mount routes, ensure egress proxy/network at boot.

---

## Task 1: `repo_connector` table + sqlc

**Files:**
- Create: `migrations/0070_repo_connector.up.sql`, `migrations/0070_repo_connector.down.sql`
- Modify: `db/schema.sql` (append table)
- Create: `db/query/repo_connector.sql`
- Modify (generated): `internal/platform/db/dbgen/*` via sqlc

**Interfaces:**
- Produces: dbgen `InsertRepoConnector`, `GetRepoConnector` query funcs + `RepoConnector` row type.

- [ ] **Step 1: Confirm latest migration number**

Run: `ls migrations | grep -E '^[0-9]{4}' | sort | tail -3`
Expected: highest prefix is `0069`. (If higher, bump 0070/0071 in this plan accordingly.)

- [ ] **Step 2: Write the up migration**

Create `migrations/0070_repo_connector.up.sql`:

```sql
-- 0070: repo_connector — a per-business code-hosting repo (GitHub) with a vault-sealed credential.
CREATE TABLE repo_connector (
    id                     uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    business_id            uuid NOT NULL,
    tenant_root_id         uuid NOT NULL,
    type                   text NOT NULL DEFAULT 'github',
    display_name           text NOT NULL,
    base_url               text NOT NULL,
    repo                   text NOT NULL,            -- "owner/name"
    allow_private_base_url boolean NOT NULL DEFAULT false,
    secret_ref             uuid NOT NULL,
    config                 jsonb NOT NULL DEFAULT '{}'::jsonb,
    status                 text NOT NULL DEFAULT 'enabled',
    created_at             timestamptz NOT NULL DEFAULT now(),
    updated_at             timestamptz NOT NULL DEFAULT now(),
    UNIQUE (id, tenant_root_id),
    FOREIGN KEY (business_id, tenant_root_id) REFERENCES business (id, tenant_root_id),
    CONSTRAINT repo_connector_type_chk CHECK (type IN ('github'))
);

GRANT SELECT, INSERT, UPDATE, DELETE ON repo_connector TO manyforge_app;

ALTER TABLE repo_connector ENABLE ROW LEVEL SECURITY;
CREATE POLICY repo_connector_rls ON repo_connector FOR ALL
    USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (true);
```

- [ ] **Step 3: Write the down migration**

Create `migrations/0070_repo_connector.down.sql`:

```sql
DROP TABLE IF EXISTS repo_connector;
```

- [ ] **Step 4: Mirror the table into `db/schema.sql`**

Append the same `CREATE TABLE repo_connector (...)` block (the columns/constraints only — NOT the GRANT/RLS lines) to `db/schema.sql`, next to the existing `connector` table definition.

- [ ] **Step 5: Write the queries**

Create `db/query/repo_connector.sql`:

```sql
-- name: InsertRepoConnector :one
INSERT INTO repo_connector (id, business_id, tenant_root_id, type, display_name, base_url,
    repo, allow_private_base_url, secret_ref, config, status, created_at, updated_at)
SELECT sqlc.arg('id'), b.id, b.tenant_root_id, sqlc.arg('type'),
    sqlc.arg('display_name'), sqlc.arg('base_url'), sqlc.arg('repo'),
    sqlc.arg('allow_private_base_url'), sqlc.arg('secret_ref'), sqlc.arg('config'), sqlc.arg('status'), now(), now()
FROM business b
WHERE b.id = sqlc.arg('business_id')::uuid
  AND EXISTS (SELECT 1 FROM secret s WHERE s.id = sqlc.arg('secret_ref') AND s.business_id = b.id)
RETURNING *;

-- name: GetRepoConnector :one
SELECT * FROM repo_connector WHERE id = sqlc.arg('id')::uuid;
```

- [ ] **Step 6: Generate**

Run: `/opt/homebrew/Cellar/sqlc/1.27.0/bin/sqlc generate`
Expected: no errors; `git status` shows changes only under `internal/platform/db/dbgen/`.

- [ ] **Step 7: Build + migrate dev DB**

Run: `go build ./... && MANYFORGE_DATABASE_URL="postgres://manyforge:devpassword@localhost:55432/manyforge?sslmode=disable" make migrate`
Expected: build clean; migrate applies 0070.

- [ ] **Step 8: Commit**

```bash
git add migrations/0070_* db/schema.sql db/query/repo_connector.sql internal/platform/db/dbgen
git commit -m "feat(007): repo_connector table + sqlc queries"
```

---

## Task 2: `RepoConnector` interface + DTOs (no write capability)

**Files:**
- Create: `internal/connectors/repo_connector.go`
- Test: `internal/security_regression/coding_review_pins_test.go` (first pin only; expanded in Task 16)

**Interfaces:**
- Produces: `RepoConnector`, `PullRequest`, `Finding`, `Review`, `ReviewRef`, `ResolvedRepoConnector`, `CreateRepoConnectorInput`.

- [ ] **Step 1: Write the interface + DTOs**

Create `internal/connectors/repo_connector.go`:

```go
package connectors

import "context"

// RepoConnector is a code-hosting connector for read-only review.
// SECURITY (spec 007 slice 1): it exposes NO method that can push, commit, or
// open a pull request. The only outbound write is PostReview (advisory comments).
type RepoConnector interface {
	// FetchPR returns metadata for an open pull request (host-side, uses the credential).
	FetchPR(ctx context.Context, prNumber int) (PullRequest, error)
	// CloneURL returns the https clone URL for the repo (host-side clone uses header auth).
	CloneURL() string
	// PostReview posts a single review (summary + findings) to the pull request. Advisory only.
	PostReview(ctx context.Context, prNumber int, r Review) (ReviewRef, error)
}

type PullRequest struct {
	Number  int
	Title   string
	HeadSHA string
	BaseRef string
	HeadRef string
	State   string // "open" | "closed" | "merged"
}

type Finding struct {
	File     string `json:"file"`
	Line     *int   `json:"line"`
	Severity string `json:"severity"` // "info" | "warning" | "error"
	Title    string `json:"title"`
	Detail   string `json:"detail"`
}

type Review struct {
	Summary  string
	Findings []Finding
	Body     string // rendered markdown body actually posted
}

type ReviewRef struct {
	ExternalID string // provider review id
	URL        string
}

type ResolvedRepoConnector struct {
	ID                  string
	Type                string
	BaseURL             string
	Repo                string // "owner/name"
	AllowPrivateBaseURL bool
	Config              map[string]any
	Credential          Credential // reuses connectors.Credential (APIToken used as the GitHub token)
}

type CreateRepoConnectorInput struct {
	Type                string
	DisplayName         string
	BaseURL             string
	Repo                string
	AllowPrivateBaseURL bool
	APIToken            string
}
```

- [ ] **Step 2: Write the no-write-capability pin (failing)**

Create `internal/security_regression/coding_review_pins_test.go`:

```go
package security_regression

import (
	"reflect"
	"strings"
	"testing"

	"github.com/<module>/internal/connectors" // replace <module> with the real module path
)

// MF007-PIN-1: the slice-1 repo connector must expose no code-write capability.
func TestRepoConnectorHasNoWriteCapability(t *testing.T) {
	typ := reflect.TypeOf((*connectors.RepoConnector)(nil)).Elem()
	banned := []string{"Push", "Commit", "CreatePR", "CreatePullRequest", "OpenPR", "Merge", "Write"}
	for i := 0; i < typ.NumMethod(); i++ {
		name := typ.Method(i).Name
		for _, b := range banned {
			if strings.Contains(name, b) {
				t.Fatalf("RepoConnector exposes write-capable method %q (banned substring %q) — slice 1 is read-only", name, b)
			}
		}
	}
}
```

Find the module path: `head -1 go.mod` and substitute it for `<module>`.

- [ ] **Step 3: Run the pin (verify it passes — the interface already has no write methods)**

Run: `go test ./internal/security_regression/ -run TestRepoConnectorHasNoWriteCapability -v`
Expected: PASS. (This pin guards against a future write method being added.)

- [ ] **Step 4: Commit**

```bash
git add internal/connectors/repo_connector.go internal/security_regression/coding_review_pins_test.go
git commit -m "feat(007): RepoConnector interface (read-only) + no-write-capability pin"
```

---

## Task 3: `RepoConnectorService` (Create/Resolve)

**Files:**
- Create: `internal/connectors/repo_service.go`
- Test: `internal/connectors/repo_service_test.go`, `internal/connectors/repo_service_integration_test.go`

**Interfaces:**
- Consumes: `Vault.Put/Open`, `validateBaseURL`, dbgen `InsertRepoConnector`/`GetRepoConnector`, `DB.WithPrincipal`.
- Produces: `RepoConnectorService{DB, Vault}`, `Create(ctx, pid, bid, CreateRepoConnectorInput) (uuid.UUID, error)`, `Resolve(ctx, pid, bid, id) (ResolvedRepoConnector, error)`.

- [ ] **Step 1: Write the service**

Create `internal/connectors/repo_service.go` (mirror `service.go`'s Create/Resolve pattern):

```go
package connectors

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/<module>/internal/platform/db/dbgen"
	"github.com/<module>/internal/platform/errs"
	"github.com/<module>/internal/platform/secrets"
)

type RepoConnectorService struct {
	DB    *db.DB // same DB type connectors.Service uses; match its import
	Vault *secrets.Vault
}

func (s *RepoConnectorService) Create(ctx context.Context, principalID, businessID uuid.UUID, in CreateRepoConnectorInput) (uuid.UUID, error) {
	if in.Type == "" {
		in.Type = "github"
	}
	if in.Type != "github" {
		return uuid.Nil, fmt.Errorf("repo_connector: unsupported type %q: %w", in.Type, errs.ErrValidation)
	}
	if in.DisplayName == "" || in.APIToken == "" {
		return uuid.Nil, fmt.Errorf("repo_connector: display_name and api_token required: %w", errs.ErrValidation)
	}
	if !strings.Contains(in.Repo, "/") {
		return uuid.Nil, fmt.Errorf("repo_connector: repo must be owner/name: %w", errs.ErrValidation)
	}
	if in.BaseURL == "" {
		in.BaseURL = "https://api.github.com"
	}
	if err := validateBaseURL(in.BaseURL, in.AllowPrivateBaseURL); err != nil {
		return uuid.Nil, err
	}

	credBytes, _ := json.Marshal(Credential{APIToken: in.APIToken})
	cfgJSON, _ := json.Marshal(map[string]any{})
	id := uuid.New()

	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		secretID, perr := s.Vault.Put(ctx, tx, businessID, "repo_connector", credBytes)
		if perr != nil {
			return perr
		}
		_, ierr := dbgen.New(tx).InsertRepoConnector(ctx, dbgen.InsertRepoConnectorParams{
			ID:                  id,
			BusinessID:          businessID,
			Type:                in.Type,
			DisplayName:         in.DisplayName,
			BaseUrl:             in.BaseURL,
			Repo:                in.Repo,
			AllowPrivateBaseUrl: in.AllowPrivateBaseURL,
			SecretRef:           secretID,
			Config:              cfgJSON,
			Status:              "enabled",
		})
		return ierr
	})
	if err != nil {
		return uuid.Nil, mapRepoErr(err)
	}
	return id, nil
}

func (s *RepoConnectorService) Resolve(ctx context.Context, principalID, businessID, id uuid.UUID) (ResolvedRepoConnector, error) {
	var out ResolvedRepoConnector
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		row, gerr := dbgen.New(tx).GetRepoConnector(ctx, id)
		if gerr != nil {
			return gerr
		}
		cred, oerr := s.Vault.Open(ctx, tx, businessID, row.SecretRef)
		if oerr != nil {
			return oerr
		}
		var c Credential
		if jerr := json.Unmarshal(cred, &c); jerr != nil {
			return jerr
		}
		out = ResolvedRepoConnector{
			ID: row.ID.String(), Type: row.Type, BaseURL: row.BaseUrl, Repo: row.Repo,
			AllowPrivateBaseURL: row.AllowPrivateBaseUrl, Credential: c,
		}
		return nil
	})
	if err != nil {
		return ResolvedRepoConnector{}, mapRepoErr(err)
	}
	return out, nil
}

func mapRepoErr(err error) error {
	if err == nil {
		return nil
	}
	if err.Error() == pgx.ErrNoRows.Error() {
		return fmt.Errorf("repo_connector: %w", errs.ErrNotFound)
	}
	return err
}
```

Note: match the exact `db.DB` import path used by `connectors.Service` (read the top of `internal/connectors/service.go`). Confirm `dbgen.InsertRepoConnectorParams` field names against the generated file (sqlc camelCases `base_url`→`BaseUrl`, `allow_private_base_url`→`AllowPrivateBaseUrl`).

- [ ] **Step 2: Write the unit test (validation + SSRF) — failing first**

Create `internal/connectors/repo_service_test.go`:

```go
package connectors

import (
	"errors"
	"testing"

	"github.com/<module>/internal/platform/errs"
)

func TestRepoConnectorValidation(t *testing.T) {
	s := &RepoConnectorService{} // validation runs before DB
	cases := []struct{ name string; in CreateRepoConnectorInput }{
		{"missing token", CreateRepoConnectorInput{DisplayName: "x", Repo: "o/r"}},
		{"bad repo", CreateRepoConnectorInput{DisplayName: "x", APIToken: "t", Repo: "noslash"}},
		{"private base url blocked", CreateRepoConnectorInput{DisplayName: "x", APIToken: "t", Repo: "o/r", BaseURL: "http://169.254.169.254"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := s.Create(t.Context(), uuidNil(), uuidNil(), c.in)
			if !errors.Is(err, errs.ErrValidation) {
				t.Fatalf("want ErrValidation, got %v", err)
			}
		})
	}
}
```

Add a small `uuidNil()` helper (or use `uuid.Nil` directly with the import). Use `t.Context()` if the Go version supports it; otherwise `context.Background()`.

- [ ] **Step 3: Run unit test**

Run: `go test ./internal/connectors/ -run TestRepoConnectorValidation -v`
Expected: PASS (validation rejects before any DB call).

- [ ] **Step 4: Write the integration test (RLS round-trip) — `//go:build integration`**

Create `internal/connectors/repo_service_integration_test.go`. Reuse the existing connectors integration harness/seed helper (read `internal/connectors/*_integration_test.go` for the seed function name; mirror it). The test must:
1. Start `testdb`, seed tenant + principal + business.
2. `Create` a github repo connector; assert returned id non-nil.
3. `Resolve` it under the same principal; assert `Repo`, `BaseURL`, and `Credential.APIToken` round-trip.
4. `Resolve` under a DIFFERENT business's principal; assert `errs.ErrNotFound` (RLS isolation, no oracle).

```go
//go:build integration

package connectors

// ... reuse seedConnectorTenant (or the existing seed helper) + testdb.Start ...
// Assertions per the four points above.
```

- [ ] **Step 5: Run integration test**

Run: `go test -tags integration -p 1 ./internal/connectors/ -run TestRepoConnector -v`
Expected: PASS (Docker required).

- [ ] **Step 6: Commit**

```bash
git add internal/connectors/repo_service.go internal/connectors/repo_service_test.go internal/connectors/repo_service_integration_test.go
git commit -m "feat(007): RepoConnectorService create/resolve with vault + RLS"
```

---

## Task 4: GitHub `RepoConnector` client

**Files:**
- Create: `internal/connectors/github/client.go`, `internal/connectors/github/factory.go`
- Test: `internal/connectors/github/client_test.go`

**Interfaces:**
- Consumes: `netsafe.NewClientWithOptions`, `ResolvedRepoConnector`.
- Produces: `github.NewFactory(timeout) func(ResolvedRepoConnector) (connectors.RepoConnector, error)`; a `*client` implementing `FetchPR`, `CloneURL`, `PostReview`.

- [ ] **Step 1: Write the client**

Create `internal/connectors/github/client.go`:

```go
package github

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/<module>/internal/connectors"
	"github.com/<module>/internal/platform/errs"
	"github.com/<module>/internal/platform/netsafe"
)

type client struct {
	http    *http.Client
	apiBase string // e.g. https://api.github.com
	repo    string // owner/name
	token   string
}

func (c *client) authHeader() string {
	return "Bearer " + c.token
}

func (c *client) CloneURL() string {
	// https clone; host-side clone injects auth via http.extraHeader (NOT in the URL).
	host := "github.com"
	if !strings.Contains(c.apiBase, "api.github.com") {
		host = strings.TrimPrefix(strings.TrimPrefix(c.apiBase, "https://"), "http://")
		host = strings.TrimSuffix(host, "/api/v3")
	}
	return fmt.Sprintf("https://%s/%s.git", host, c.repo)
}

func (c *client) FetchPR(ctx context.Context, prNumber int) (connectors.PullRequest, error) {
	url := fmt.Sprintf("%s/repos/%s/pulls/%d", c.apiBase, c.repo, prNumber)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("Authorization", c.authHeader())
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := c.http.Do(req)
	if err != nil {
		return connectors.PullRequest{}, fmt.Errorf("github: fetch pr: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return connectors.PullRequest{}, fmt.Errorf("github: pr %d: %w", prNumber, errs.ErrNotFound)
	}
	if resp.StatusCode != http.StatusOK {
		return connectors.PullRequest{}, fmt.Errorf("github: fetch pr status %d", resp.StatusCode)
	}
	var body struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		State  string `json:"state"`
		Merged bool   `json:"merged"`
		Head   struct {
			SHA string `json:"sha"`
			Ref string `json:"ref"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return connectors.PullRequest{}, fmt.Errorf("github: decode pr: %w", err)
	}
	state := body.State
	if body.Merged {
		state = "merged"
	}
	return connectors.PullRequest{
		Number: body.Number, Title: body.Title, HeadSHA: body.Head.SHA,
		HeadRef: body.Head.Ref, BaseRef: body.Base.Ref, State: state,
	}, nil
}

func (c *client) PostReview(ctx context.Context, prNumber int, r connectors.Review) (connectors.ReviewRef, error) {
	url := fmt.Sprintf("%s/repos/%s/pulls/%d/reviews", c.apiBase, c.repo, prNumber)
	payload, _ := json.Marshal(map[string]any{"event": "COMMENT", "body": r.Body})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	req.Header.Set("Authorization", c.authHeader())
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return connectors.ReviewRef{}, fmt.Errorf("github: post review: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return connectors.ReviewRef{}, fmt.Errorf("github: pr %d: %w", prNumber, errs.ErrNotFound)
	}
	if resp.StatusCode/100 != 2 {
		return connectors.ReviewRef{}, fmt.Errorf("github: post review status %d", resp.StatusCode)
	}
	var body struct {
		ID      int64  `json:"id"`
		HTMLURL string `json:"html_url"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return connectors.ReviewRef{ExternalID: fmt.Sprintf("%d", body.ID), URL: body.HTMLURL}, nil
}

// BasicAuthHeader builds the header value used for host-side `git` clone auth.
func BasicAuthHeader(token string) string {
	return "AUTHORIZATION: basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token))
}

var _ = time.Second
```

- [ ] **Step 2: Write the factory**

Create `internal/connectors/github/factory.go`:

```go
package github

import (
	"fmt"
	"strings"
	"time"

	"github.com/<module>/internal/connectors"
	"github.com/<module>/internal/platform/netsafe"
)

func NewFactory(timeout time.Duration) func(connectors.ResolvedRepoConnector) (connectors.RepoConnector, error) {
	return func(rc connectors.ResolvedRepoConnector) (connectors.RepoConnector, error) {
		if rc.Credential.APIToken == "" {
			return nil, fmt.Errorf("github: factory: api_token required")
		}
		if !strings.Contains(rc.Repo, "/") {
			return nil, fmt.Errorf("github: factory: repo must be owner/name")
		}
		base := rc.BaseURL
		if base == "" {
			base = "https://api.github.com"
		}
		hc := netsafe.NewClientWithOptions(timeout, netsafe.Options{
			AllowLoopback: rc.AllowPrivateBaseURL,
			AllowPrivate:  rc.AllowPrivateBaseURL,
		})
		return &client{http: hc, apiBase: strings.TrimSuffix(base, "/"), repo: rc.Repo, token: rc.Credential.APIToken}, nil
	}
}
```

- [ ] **Step 3: Write client tests against a stub HTTP server — failing first**

Create `internal/connectors/github/client_test.go`:

```go
package github

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/<module>/internal/connectors"
)

func newStubClient(t *testing.T, h http.HandlerFunc) *client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &client{http: srv.Client(), apiBase: srv.URL, repo: "o/r", token: "tkn"}
}

func TestFetchPR(t *testing.T) {
	c := newStubClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tkn" {
			t.Errorf("missing auth header")
		}
		w.Write([]byte(`{"number":42,"title":"x","state":"open","merged":false,"head":{"sha":"abc","ref":"feat"},"base":{"ref":"main"}}`))
	})
	pr, err := c.FetchPR(t.Context(), 42)
	if err != nil || pr.HeadSHA != "abc" || pr.State != "open" {
		t.Fatalf("got %+v err %v", pr, err)
	}
}

func TestPostReview(t *testing.T) {
	c := newStubClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(201)
		w.Write([]byte(`{"id":7,"html_url":"http://x/7"}`))
	})
	ref, err := c.PostReview(t.Context(), 42, connectors.Review{Body: "hi"})
	if err != nil || ref.ExternalID != "7" {
		t.Fatalf("got %+v err %v", ref, err)
	}
}
```

- [ ] **Step 4: Run client tests**

Run: `go test ./internal/connectors/github/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/connectors/github/
git commit -m "feat(007): GitHub repo connector client (FetchPR, CloneURL, PostReview)"
```

---

## Task 5: Host-side clone at PR head

**Files:**
- Create: `internal/agents/coding/clone.go`
- Test: `internal/agents/coding/clone_test.go`

**Interfaces:**
- Consumes: `connectors.RepoConnector` (CloneURL), `github.BasicAuthHeader`.
- Produces: `CloneAtSHA(ctx, cloneURL, authHeader, sha, destDir string) error`.

- [ ] **Step 1: Write the clone helper**

Create `internal/agents/coding/clone.go`:

```go
package coding

import (
	"context"
	"fmt"
	"os/exec"
)

// CloneAtSHA clones cloneURL into destDir and checks out sha. The token is passed
// via an in-memory http.extraHeader (-c), never written to disk or the URL.
// destDir must be empty/non-existent. The caller owns destDir's lifecycle.
func CloneAtSHA(ctx context.Context, cloneURL, authHeader, sha, destDir string) error {
	clone := exec.CommandContext(ctx, "git",
		"-c", "http.extraHeader="+authHeader,
		"clone", "--no-tags", "--depth", "50", cloneURL, destDir)
	if out, err := clone.CombinedOutput(); err != nil {
		return fmt.Errorf("coding: git clone failed: %w (%s)", err, string(out))
	}
	co := exec.CommandContext(ctx, "git", "-C", destDir, "checkout", "--quiet", sha)
	if out, err := co.CombinedOutput(); err != nil {
		// Shallow clone may not contain sha; fetch it then checkout.
		fetch := exec.CommandContext(ctx, "git", "-C", destDir,
			"-c", "http.extraHeader="+authHeader, "fetch", "--depth", "50", "origin", sha)
		if fout, ferr := fetch.CombinedOutput(); ferr != nil {
			return fmt.Errorf("coding: git fetch sha failed: %w (%s)", ferr, string(fout))
		}
		co2 := exec.CommandContext(ctx, "git", "-C", destDir, "checkout", "--quiet", sha)
		if out2, err2 := co2.CombinedOutput(); err2 != nil {
			return fmt.Errorf("coding: git checkout failed: %w (%s / %s)", err2, string(out), string(out2))
		}
	}
	return nil
}
```

- [ ] **Step 2: Write the test against a local git repo — failing first**

Create `internal/agents/coding/clone_test.go`. Build a local source repo in a temp dir, commit a file, capture the SHA, then clone via `file://` URL (no auth header needed for file://; pass an empty header).

```go
package coding

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCloneAtSHA(t *testing.T) {
	src := t.TempDir()
	run := func(args ...string) string {
		c := exec.Command("git", append([]string{"-C", src}, args...)...)
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q")
	os.WriteFile(filepath.Join(src, "a.txt"), []byte("hi"), 0o644)
	run("add", ".")
	run("commit", "-q", "-m", "x")
	sha := run("rev-parse", "HEAD")

	dest := filepath.Join(t.TempDir(), "checkout")
	if err := CloneAtSHA(t.Context(), "file://"+src, "", sha, dest); err != nil {
		t.Fatalf("clone: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "a.txt")); err != nil {
		t.Fatalf("expected a.txt in checkout: %v", err)
	}
}
```

- [ ] **Step 3: Run the test**

Run: `go test ./internal/agents/coding/ -run TestCloneAtSHA -v`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/agents/coding/clone.go internal/agents/coding/clone_test.go
git commit -m "feat(007): host-side clone at PR head via in-memory auth header"
```

---

## Task 6: `SandboxRunner` interface + fake

**Files:**
- Create: `internal/agents/coding/sandbox/runner.go`
- Test: `internal/agents/coding/sandbox/runner_test.go`

**Interfaces:**
- Produces: `SandboxRunner`, `SandboxSpec`, `SandboxResult`, `FakeRunner`.

- [ ] **Step 1: Write the interface + fake**

Create `internal/agents/coding/sandbox/runner.go`:

```go
package sandbox

import (
	"context"
	"time"
)

type SandboxSpec struct {
	Image       string            // container image
	ReadOnlyDir string            // host path mounted read-only at /work
	OutputDir   string            // host path mounted read-write at /out
	Cmd         []string          // command run inside the container
	Env         map[string]string // ONLY allowlisted run-scoped secrets/config
	EgressAllow []string          // allowlisted egress hosts (informational; enforced by proxy/network)
	Timeout     time.Duration     // wall-clock cap
}

type SandboxResult struct {
	ExitCode int
	Stdout   []byte
	Stderr   []byte
	TimedOut bool
}

type SandboxRunner interface {
	Run(ctx context.Context, spec SandboxSpec) (SandboxResult, error)
}

// FakeRunner is for service-layer tests. It records the last spec and returns Result/Err.
type FakeRunner struct {
	Last   SandboxSpec
	Result SandboxResult
	Err    error
}

func (f *FakeRunner) Run(ctx context.Context, spec SandboxSpec) (SandboxResult, error) {
	f.Last = spec
	return f.Result, f.Err
}
```

- [ ] **Step 2: Write a trivial fake test — failing first**

Create `internal/agents/coding/sandbox/runner_test.go`:

```go
package sandbox

import "testing"

func TestFakeRunnerRecordsSpec(t *testing.T) {
	f := &FakeRunner{Result: SandboxResult{ExitCode: 0}}
	_, _ = f.Run(t.Context(), SandboxSpec{Image: "img", Cmd: []string{"echo"}})
	if f.Last.Image != "img" {
		t.Fatalf("spec not recorded")
	}
}
```

- [ ] **Step 3: Run + commit**

Run: `go test ./internal/agents/coding/sandbox/ -v` → PASS.

```bash
git add internal/agents/coding/sandbox/runner.go internal/agents/coding/sandbox/runner_test.go
git commit -m "feat(007): SandboxRunner interface + fake"
```

---

## Task 7: Allowlisting egress proxy

**Files:**
- Create: `cmd/mf-egress-proxy/main.go`, `deploy/egress-proxy/Dockerfile`
- Test: `cmd/mf-egress-proxy/main_test.go`

**Interfaces:**
- Produces: a CONNECT proxy binary that allows TLS tunnels only to hosts in `EGRESS_ALLOW` (comma-separated host or host:port), refusing everything else with 403.

- [ ] **Step 1: Write the proxy**

Create `cmd/mf-egress-proxy/main.go`:

```go
package main

import (
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

func allowed(set map[string]bool, hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	return set[host] || set[hostport]
}

func main() {
	allow := map[string]bool{}
	for _, h := range strings.Split(os.Getenv("EGRESS_ALLOW"), ",") {
		if h = strings.TrimSpace(h); h != "" {
			allow[h] = true
		}
	}
	addr := os.Getenv("LISTEN")
	if addr == "" {
		addr = ":8080"
	}
	h := func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "only CONNECT supported", http.StatusMethodNotAllowed)
			return
		}
		if !allowed(allow, r.Host) {
			http.Error(w, "egress not allowed", http.StatusForbidden)
			return
		}
		dst, err := net.DialTimeout("tcp", r.Host, 10*time.Second)
		if err != nil {
			http.Error(w, "dial failed", http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
		hj, ok := w.(http.Hijacker)
		if !ok {
			dst.Close()
			return
		}
		src, _, err := hj.Hijack()
		if err != nil {
			dst.Close()
			return
		}
		go func() { io.Copy(dst, src); dst.Close() }()
		go func() { io.Copy(src, dst); src.Close() }()
	}
	srv := &http.Server{Addr: addr, Handler: http.HandlerFunc(h)}
	log.Printf("mf-egress-proxy listening on %s allow=%v", addr, allow)
	log.Fatal(srv.ListenAndServe())
}
```

- [ ] **Step 2: Write the allow/deny unit test (no full tunnel needed — test `allowed`) — failing first**

Create `cmd/mf-egress-proxy/main_test.go`:

```go
package main

import "testing"

func TestAllowed(t *testing.T) {
	set := map[string]bool{"api.anthropic.com": true}
	if !allowed(set, "api.anthropic.com:443") {
		t.Fatal("should allow allowlisted host with port")
	}
	if allowed(set, "evil.example.com:443") {
		t.Fatal("should deny non-allowlisted host")
	}
}
```

- [ ] **Step 3: Run + Dockerfile**

Run: `go test ./cmd/mf-egress-proxy/ -v` → PASS.

Create `deploy/egress-proxy/Dockerfile`:

```dockerfile
FROM golang:1.23-alpine AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -o /mf-egress-proxy ./cmd/mf-egress-proxy

FROM alpine:3.20
COPY --from=build /mf-egress-proxy /mf-egress-proxy
ENTRYPOINT ["/mf-egress-proxy"]
```

- [ ] **Step 4: Build the image**

Run: `docker build -f deploy/egress-proxy/Dockerfile -t manyforge/egress-proxy:dev .`
Expected: image builds.

- [ ] **Step 5: Commit**

```bash
git add cmd/mf-egress-proxy/ deploy/egress-proxy/
git commit -m "feat(007): allowlisting CONNECT egress proxy + image"
```

---

## Task 8: Docker sandbox backend + isolation test

**Files:**
- Create: `internal/agents/coding/sandbox/docker.go`, `internal/agents/coding/sandbox/proxy.go`
- Create: `deploy/sandbox-stub/Dockerfile`
- Test: `internal/agents/coding/sandbox/docker_integration_test.go`

**Interfaces:**
- Consumes: `SandboxSpec`/`SandboxResult`; the `docker` CLI; the egress-proxy image (Task 7).
- Produces: `NewDockerRunner(networkName, proxyAddr string) *DockerRunner` implementing `SandboxRunner`; `EnsureEgressInfra(ctx, proxyImage string, allow []string) (network, proxyAddr string, err error)`.

- [ ] **Step 1: Write the egress infra helper**

Create `internal/agents/coding/sandbox/proxy.go`:

```go
package sandbox

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

const (
	NetworkName    = "mf-sandbox-net"
	ProxyName      = "mf-egress-proxy"
	ProxyDNSAddr   = "http://mf-egress-proxy:8080"
)

// EnsureEgressInfra creates an internal docker network (no external route) and a
// long-lived egress-proxy container attached to BOTH that network and the default
// bridge, allowlisting `allow`. Idempotent.
func EnsureEgressInfra(ctx context.Context, proxyImage string, allow []string) error {
	// internal network: containers on it have no external connectivity except via the proxy.
	_ = exec.CommandContext(ctx, "docker", "network", "create", "--internal", NetworkName).Run()

	// already running?
	out, _ := exec.CommandContext(ctx, "docker", "ps", "-q", "-f", "name=^/"+ProxyName+"$").Output()
	if strings.TrimSpace(string(out)) != "" {
		return nil
	}
	_ = exec.CommandContext(ctx, "docker", "rm", "-f", ProxyName).Run()
	// start proxy on the default bridge (external), then attach the internal network.
	run := exec.CommandContext(ctx, "docker", "run", "-d", "--name", ProxyName,
		"-e", "EGRESS_ALLOW="+strings.Join(allow, ","), proxyImage)
	if b, err := run.CombinedOutput(); err != nil {
		return fmt.Errorf("sandbox: start egress proxy: %w (%s)", err, string(b))
	}
	if b, err := exec.CommandContext(ctx, "docker", "network", "connect", NetworkName, ProxyName).CombinedOutput(); err != nil {
		return fmt.Errorf("sandbox: attach proxy to internal net: %w (%s)", err, string(b))
	}
	return nil
}
```

- [ ] **Step 2: Write the Docker runner**

Create `internal/agents/coding/sandbox/docker.go`:

```go
package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"time"
)

type DockerRunner struct {
	Network   string // internal network the sandbox joins
	ProxyAddr string // e.g. http://mf-egress-proxy:8080
}

func NewDockerRunner(network, proxyAddr string) *DockerRunner {
	return &DockerRunner{Network: network, ProxyAddr: proxyAddr}
}

func (d *DockerRunner) Run(ctx context.Context, spec SandboxSpec) (SandboxResult, error) {
	if spec.Timeout <= 0 {
		spec.Timeout = 5 * time.Minute
	}
	runCtx, cancel := context.WithTimeout(ctx, spec.Timeout)
	defer cancel()

	args := []string{
		"run", "--rm",
		"--network", d.Network,
		"--read-only",                       // read-only root fs
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--pids-limit", "256",
		"--memory", "2g",
		"-v", spec.ReadOnlyDir + ":/work:ro", // checkout read-only
		"-v", spec.OutputDir + ":/out:rw",    // findings output
		"--tmpfs", "/tmp:rw,size=256m",
		"-w", "/work",
		// force ALL egress through the allowlisting proxy:
		"-e", "HTTPS_PROXY=" + d.ProxyAddr,
		"-e", "HTTP_PROXY=" + d.ProxyAddr,
		"-e", "https_proxy=" + d.ProxyAddr,
		"-e", "http_proxy=" + d.ProxyAddr,
	}
	for k, v := range spec.Env { // ONLY allowlisted run-scoped secrets/config
		args = append(args, "-e", k+"="+v)
	}
	args = append(args, spec.Image)
	args = append(args, spec.Cmd...)

	cmd := exec.CommandContext(runCtx, "docker", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	res := SandboxResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	if runCtx.Err() == context.DeadlineExceeded {
		res.TimedOut = true
		return res, fmt.Errorf("sandbox: timed out after %s", spec.Timeout)
	}
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			res.ExitCode = ee.ExitCode()
			return res, nil // non-zero exit is a result, not a Go error
		}
		return res, fmt.Errorf("sandbox: docker run: %w", err)
	}
	return res, nil
}
```

- [ ] **Step 3: Write the stub sandbox image used by tests**

Create `deploy/sandbox-stub/Dockerfile` (busybox-based; lets tests probe isolation):

```dockerfile
FROM busybox:1.36
# no opencode; tests drive it with explicit Cmd (sh -c "...").
ENTRYPOINT []
```

- [ ] **Step 4: Write the isolation integration test — `//go:build integration`**

Create `internal/agents/coding/sandbox/docker_integration_test.go`. It builds the proxy + stub images (or assumes `make` built them — include `docker build` via `exec` in test setup), then asserts the four isolation properties.

```go
//go:build integration

package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func dockerBuild(t *testing.T, dockerfile, tag string) {
	t.Helper()
	c := exec.Command("docker", "build", "-f", dockerfile, "-t", tag, ".")
	c.Dir = repoRoot(t)
	if b, err := c.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v (%s)", tag, err, b)
	}
}

func repoRoot(t *testing.T) string {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func TestSandboxIsolation(t *testing.T) {
	dockerBuild(t, "deploy/egress-proxy/Dockerfile", "manyforge/egress-proxy:test")
	dockerBuild(t, "deploy/sandbox-stub/Dockerfile", "manyforge/sandbox-stub:test")
	ctx := t.Context()
	// allowlist a host that does NOT resolve to anything reachable; we only assert deny behavior here.
	if err := EnsureEgressInfra(ctx, "manyforge/egress-proxy:test", []string{"allowed.invalid"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", ProxyName).Run() })

	ro := t.TempDir()
	os.WriteFile(filepath.Join(ro, "code.txt"), []byte("secret-code"), 0o644)
	out := t.TempDir()
	r := NewDockerRunner(NetworkName, ProxyDNSAddr)

	// 1. no host env leaks: HOME/PATH from host must not equal host's; check a sentinel host var is absent.
	os.Setenv("MF_SENTINEL_SECRET", "leak-me")
	res, err := r.Run(ctx, SandboxSpec{
		Image: "manyforge/sandbox-stub:test",
		ReadOnlyDir: ro, OutputDir: out,
		Cmd: []string{"sh", "-c", "echo START; printenv MF_SENTINEL_SECRET || echo NO_SENTINEL"},
		Env: map[string]string{"LLM_API_KEY": "only-this"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(res.Stdout), "NO_SENTINEL") {
		t.Fatalf("host env leaked into sandbox: %s", res.Stdout)
	}

	// 2. checkout is read-only.
	res, _ = r.Run(ctx, SandboxSpec{
		Image: "manyforge/sandbox-stub:test", ReadOnlyDir: ro, OutputDir: out,
		Cmd: []string{"sh", "-c", "echo x > /work/code.txt && echo WROTE || echo READONLY"},
	})
	if !strings.Contains(string(res.Stdout), "READONLY") {
		t.Fatalf("checkout was writable: %s", res.Stdout)
	}

	// 3. direct egress to a non-allowlisted host is refused (no route on internal net / proxy denies).
	res, _ = r.Run(ctx, SandboxSpec{
		Image: "manyforge/sandbox-stub:test", ReadOnlyDir: ro, OutputDir: out,
		Cmd: []string{"sh", "-c", "wget -T 5 -q -O- http://example.com >/dev/null 2>&1 && echo REACHED || echo BLOCKED"},
	})
	if !strings.Contains(string(res.Stdout), "BLOCKED") {
		t.Fatalf("egress was not blocked: %s", res.Stdout)
	}

	// 4. /out is writable (findings channel works).
	res, _ = r.Run(ctx, SandboxSpec{
		Image: "manyforge/sandbox-stub:test", ReadOnlyDir: ro, OutputDir: out,
		Cmd: []string{"sh", "-c", "echo ok > /out/review.json && echo WROTE_OUT"},
	})
	if !strings.Contains(string(res.Stdout), "WROTE_OUT") {
		t.Fatalf("output dir not writable: %s", res.Stdout)
	}
	if _, err := os.Stat(filepath.Join(out, "review.json")); err != nil {
		t.Fatalf("expected review.json on host: %v", err)
	}
}
```

- [ ] **Step 5: Run the isolation test**

Run: `go test -tags integration -p 1 ./internal/agents/coding/sandbox/ -run TestSandboxIsolation -v`
Expected: PASS. (This is the highest-integration-risk task; if `wget` isn't present in busybox use `nc`/`wget` is in busybox by default. If egress still reaches out, verify the network is `--internal` and the sandbox is NOT also on the default bridge.)

- [ ] **Step 6: Commit**

```bash
git add internal/agents/coding/sandbox/docker.go internal/agents/coding/sandbox/proxy.go deploy/sandbox-stub/ internal/agents/coding/sandbox/docker_integration_test.go
git commit -m "feat(007): Docker sandbox backend (ro mount, env allowlist, forced egress proxy) + isolation test"
```

---

## Task 9: `code_review` table + sqlc

**Files:**
- Create: `migrations/0071_code_review.up.sql`, `.down.sql`; `db/query/code_review.sql`
- Modify: `db/schema.sql`

**Interfaces:**
- Produces: dbgen `InsertCodeReview`, `UpdateCodeReviewResult`, `GetCodeReview`.

- [ ] **Step 1: Up migration**

Create `migrations/0071_code_review.up.sql`:

```sql
-- 0071: code_review — one review of one PR, linked to an agent_run.
CREATE TABLE code_review (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    business_id        uuid NOT NULL,
    tenant_root_id     uuid NOT NULL,
    agent_run_id       uuid,
    repo_connector_id  uuid NOT NULL,
    pr_number          integer NOT NULL,
    head_sha           text NOT NULL DEFAULT '',
    status             text NOT NULL DEFAULT 'pending',
    summary            text NOT NULL DEFAULT '',
    findings           jsonb NOT NULL DEFAULT '[]'::jsonb,
    external_review_ref text NOT NULL DEFAULT '',
    posted_at          timestamptz,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    UNIQUE (id, tenant_root_id),
    FOREIGN KEY (business_id, tenant_root_id) REFERENCES business (id, tenant_root_id),
    CONSTRAINT code_review_status_chk CHECK (status IN ('pending','running','succeeded','failed'))
);

GRANT SELECT, INSERT, UPDATE, DELETE ON code_review TO manyforge_app;

ALTER TABLE code_review ENABLE ROW LEVEL SECURITY;
CREATE POLICY code_review_rls ON code_review FOR ALL
    USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (true);
```

- [ ] **Step 2: Down migration**

Create `migrations/0071_code_review.down.sql`:

```sql
DROP TABLE IF EXISTS code_review;
```

- [ ] **Step 3: Mirror into `db/schema.sql`** (table block only, no GRANT/RLS).

- [ ] **Step 4: Queries**

Create `db/query/code_review.sql`:

```sql
-- name: InsertCodeReview :one
INSERT INTO code_review (id, business_id, tenant_root_id, agent_run_id, repo_connector_id, pr_number, status, created_at, updated_at)
SELECT sqlc.arg('id'), b.id, b.tenant_root_id, sqlc.narg('agent_run_id'), sqlc.arg('repo_connector_id'),
    sqlc.arg('pr_number'), 'pending', now(), now()
FROM business b
WHERE b.id = sqlc.arg('business_id')::uuid
RETURNING *;

-- name: UpdateCodeReviewResult :one
UPDATE code_review SET
    status = sqlc.arg('status'),
    head_sha = sqlc.arg('head_sha'),
    summary = sqlc.arg('summary'),
    findings = sqlc.arg('findings'),
    external_review_ref = sqlc.arg('external_review_ref'),
    posted_at = sqlc.narg('posted_at'),
    updated_at = now()
WHERE id = sqlc.arg('id')::uuid
RETURNING *;

-- name: GetCodeReview :one
SELECT * FROM code_review WHERE id = sqlc.arg('id')::uuid;
```

- [ ] **Step 5: Generate + migrate + build**

Run: `/opt/homebrew/Cellar/sqlc/1.27.0/bin/sqlc generate && go build ./... && MANYFORGE_DATABASE_URL="postgres://manyforge:devpassword@localhost:55432/manyforge?sslmode=disable" make migrate`
Expected: clean; 0071 applied.

- [ ] **Step 6: Commit**

```bash
git add migrations/0071_* db/schema.sql db/query/code_review.sql internal/platform/db/dbgen
git commit -m "feat(007): code_review table + sqlc queries"
```

---

## Task 10: Findings schema + validation + render

**Files:**
- Create: `internal/agents/coding/findings.go`
- Test: `internal/agents/coding/findings_test.go`

**Interfaces:**
- Consumes: `connectors.Finding`.
- Produces: `ParseFindings([]byte) (FindingsDoc, error)`, `FindingsDoc{Summary string; Findings []connectors.Finding}`, `RenderMarkdown(FindingsDoc) string`.

- [ ] **Step 1: Write the parser/validator/renderer**

Create `internal/agents/coding/findings.go`:

```go
package coding

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/<module>/internal/connectors"
)

type FindingsDoc struct {
	Summary  string               `json:"summary"`
	Findings []connectors.Finding `json:"findings"`
}

var validSeverity = map[string]bool{"info": true, "warning": true, "error": true}

// ParseFindings validates opencode's structured output. Empty/malformed → error
// (no partial review is ever posted).
func ParseFindings(raw []byte) (FindingsDoc, error) {
	raw = []byte(strings.TrimSpace(string(raw)))
	if len(raw) == 0 {
		return FindingsDoc{}, fmt.Errorf("coding: empty findings output")
	}
	var doc FindingsDoc
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return FindingsDoc{}, fmt.Errorf("coding: malformed findings json: %w", err)
	}
	if strings.TrimSpace(doc.Summary) == "" {
		return FindingsDoc{}, fmt.Errorf("coding: findings missing summary")
	}
	for i, f := range doc.Findings {
		if f.File == "" || f.Title == "" {
			return FindingsDoc{}, fmt.Errorf("coding: finding %d missing file/title", i)
		}
		if !validSeverity[f.Severity] {
			return FindingsDoc{}, fmt.Errorf("coding: finding %d bad severity %q", i, f.Severity)
		}
	}
	return doc, nil
}

func RenderMarkdown(doc FindingsDoc) string {
	var b strings.Builder
	b.WriteString("## 🤖 Automated code review\n\n")
	b.WriteString(doc.Summary)
	b.WriteString("\n\n")
	if len(doc.Findings) == 0 {
		b.WriteString("_No specific findings._\n")
		return b.String()
	}
	b.WriteString(fmt.Sprintf("### Findings (%d)\n\n", len(doc.Findings)))
	for _, f := range doc.Findings {
		loc := f.File
		if f.Line != nil {
			loc = fmt.Sprintf("%s:%d", f.File, *f.Line)
		}
		b.WriteString(fmt.Sprintf("- **[%s]** `%s` — %s\n", strings.ToUpper(f.Severity), loc, f.Title))
		if strings.TrimSpace(f.Detail) != "" {
			b.WriteString("  " + f.Detail + "\n")
		}
	}
	return b.String()
}
```

- [ ] **Step 2: Write tests — failing first**

Create `internal/agents/coding/findings_test.go`:

```go
package coding

import "testing"

func TestParseFindings(t *testing.T) {
	good := `{"summary":"looks ok","findings":[{"file":"a.go","line":3,"severity":"warning","title":"naming","detail":"rename x"}]}`
	doc, err := ParseFindings([]byte(good))
	if err != nil || doc.Summary != "looks ok" || len(doc.Findings) != 1 {
		t.Fatalf("good parse failed: %+v %v", doc, err)
	}
	bad := []string{
		``,                                  // empty
		`not json`,                          // malformed
		`{"findings":[]}`,                   // missing summary
		`{"summary":"s","findings":[{"file":"a","severity":"bad","title":"t"}]}`, // bad severity
		`{"summary":"s","findings":[{"severity":"info","title":"t"}]}`,           // missing file
	}
	for i, b := range bad {
		if _, err := ParseFindings([]byte(b)); err == nil {
			t.Fatalf("case %d: expected error, got nil", i)
		}
	}
}

func TestRenderMarkdown(t *testing.T) {
	md := RenderMarkdown(FindingsDoc{Summary: "S"})
	if md == "" || !contains(md, "Automated code review") {
		t.Fatalf("render missing header: %s", md)
	}
}

func contains(s, sub string) bool { return len(s) >= len(sub) && (indexOf(s, sub) >= 0) }
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
```

- [ ] **Step 3: Run + commit**

Run: `go test ./internal/agents/coding/ -run 'TestParseFindings|TestRenderMarkdown' -v` → PASS.

```bash
git add internal/agents/coding/findings.go internal/agents/coding/findings_test.go
git commit -m "feat(007): findings schema validation + markdown render"
```

---

## Task 11: `agent_run` "code_review" target type

**Files:**
- Modify: `internal/agents/agent_run.go`
- Test: `internal/agents/agent_run_test.go` (add a case)

**Interfaces:**
- Produces: `targetTypeCodeReview = "code_review"` accepted by `validTargetType`.

- [ ] **Step 1: Inspect current target-type handling**

Run: `grep -n "targetType\|validTargetType\|\"ticket\"" internal/agents/agent_run.go`
Expected: find the `validTargetType` func and the `"ticket"` constant.

- [ ] **Step 2: Check for a DB CHECK constraint on target_type**

Run: `grep -rn "target_type" migrations/`
Expected: note whether a CHECK/enum constrains `agent_run.target_type`. If it does, add a migration `0072_agent_run_target_code_review.up.sql` doing `ALTER TABLE agent_run DROP CONSTRAINT <name>; ALTER TABLE agent_run ADD CONSTRAINT <name> CHECK (target_type IS NULL OR target_type IN ('ticket','code_review'));` (and the inverse in `.down.sql`), plus mirror in `db/schema.sql`. If target_type is free text, skip the migration.

- [ ] **Step 3: Add the constant + extend the validator**

In `internal/agents/agent_run.go`, near the existing `targetTypeTicket` constant add:

```go
const targetTypeCodeReview = "code_review"
```

Extend `validTargetType` to accept it (match the existing function's exact shape; e.g. if it is a switch, add a `case targetTypeCodeReview: return true`).

- [ ] **Step 4: Add a unit test**

In `internal/agents/agent_run_test.go` add:

```go
func TestValidTargetTypeCodeReview(t *testing.T) {
	if !validTargetType(targetTypeCodeReview) {
		t.Fatal("code_review must be a valid target type")
	}
}
```

- [ ] **Step 5: Run + migrate (if a migration was added) + commit**

Run: `go test ./internal/agents/ -run TestValidTargetTypeCodeReview -v` → PASS. If a migration was added: `MANYFORGE_DATABASE_URL=... make migrate`.

```bash
git add internal/agents/agent_run.go internal/agents/agent_run_test.go migrations/0072_* db/schema.sql 2>/dev/null
git commit -m "feat(007): accept code_review agent_run target type"
```

---

## Task 12: AI credential resolver seam

**Files:**
- Create: `internal/agents/coding/credresolver.go`
- Test: `internal/agents/coding/credresolver_test.go`

**Interfaces:**
- Produces: `AICredential{APIKey, BaseURL, Model, ProviderHost string}`, `AICredentialResolver` interface, `FakeCredResolver`.

- [ ] **Step 1: Locate the existing AI credential service**

Run: `grep -rn "AI_MASTER_KEY\|aicred\|AICredential\|NewProvider" internal/ cmd/ | head -30`
Expected: find the AI credential service that `NewProvider` uses to resolve an agent's BYO key. Note its package + method signature for the adapter.

- [ ] **Step 2: Define the seam + fake**

Create `internal/agents/coding/credresolver.go`:

```go
package coding

import (
	"context"
	"net/url"

	"github.com/google/uuid"
)

// AICredential is the minimal set opencode needs, extracted from the agent's BYO provider credential.
type AICredential struct {
	APIKey   string
	BaseURL  string // provider API base (e.g. https://api.anthropic.com)
	Model    string // e.g. anthropic/claude-...
	Provider string // e.g. "anthropic"
}

// Host returns the bare host of BaseURL for the egress allowlist.
func (c AICredential) Host() string {
	u, err := url.Parse(c.BaseURL)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Hostname()
}

// AICredentialResolver yields the LLM credential for an agent under the caller's RLS.
type AICredentialResolver interface {
	Resolve(ctx context.Context, principalID, businessID, agentID uuid.UUID) (AICredential, error)
}

type FakeCredResolver struct {
	Cred AICredential
	Err  error
}

func (f *FakeCredResolver) Resolve(ctx context.Context, _, _, _ uuid.UUID) (AICredential, error) {
	return f.Cred, f.Err
}
```

- [ ] **Step 3: Write the adapter to the real AI credential service**

Add to `credresolver.go` a concrete type (e.g. `AgentCredResolver`) wrapping the service found in Step 1. Implement `Resolve` by calling that service and mapping its result into `AICredential`. (Exact field mapping depends on Step 1's findings; map provider→BaseURL using the same host table `NewProvider` uses.)

- [ ] **Step 4: Test the Host() helper + fake**

Create `internal/agents/coding/credresolver_test.go`:

```go
package coding

import "testing"

func TestAICredentialHost(t *testing.T) {
	c := AICredential{BaseURL: "https://api.anthropic.com"}
	if c.Host() != "api.anthropic.com" {
		t.Fatalf("got %q", c.Host())
	}
}
```

- [ ] **Step 5: Run + commit**

Run: `go test ./internal/agents/coding/ -run TestAICredentialHost -v` → PASS.

```bash
git add internal/agents/coding/credresolver.go internal/agents/coding/credresolver_test.go
git commit -m "feat(007): AI credential resolver seam for opencode"
```

---

## Task 13: `CodeReviewService.Trigger` orchestration

**Files:**
- Create: `internal/agents/coding/service.go`
- Test: `internal/agents/coding/service_integration_test.go`

**Interfaces:**
- Consumes: `RepoConnectorService.Resolve`, `github.NewFactory`, `CloneAtSHA`, `sandbox.SandboxRunner`, `AICredentialResolver`, `ParseFindings`/`RenderMarkdown`, dbgen `InsertCodeReview`/`UpdateCodeReviewResult`, `audit.Write`, `DB.WithPrincipal`, the run-lifecycle `RunStore` (CreateRun + finalize).
- Produces: `CodeReviewService.Trigger(ctx, principalID, businessID, agentID, repoConnectorID uuid.UUID, prNumber int) (CodeReview, error)`.

- [ ] **Step 1: Write the service**

Create `internal/agents/coding/service.go`. The orchestration sequence (each external step audited in-tx where it persists state):

```go
package coding

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/<module>/internal/connectors"
	"github.com/<module>/internal/connectors/github"
	"github.com/<module>/internal/agents/coding/sandbox"
	"github.com/<module>/internal/platform/audit"
	"github.com/<module>/internal/platform/db/dbgen"
	"github.com/<module>/internal/platform/errs"
)

type CodeReview struct {
	ID         uuid.UUID
	Status     string
	Summary    string
	ReviewURL  string
	PRNumber   int
}

type repoResolver interface {
	Resolve(ctx context.Context, principalID, businessID, id uuid.UUID) (connectors.ResolvedRepoConnector, error)
}

type CodeReviewService struct {
	DB        *db.DB                 // match the project's db.DB import
	Repos     repoResolver           // *connectors.RepoConnectorService
	Sandbox   sandbox.SandboxRunner
	Creds     AICredentialResolver
	Image     string                 // opencode sandbox image
	WorkRoot  string                 // host temp root for checkouts
	Timeout   time.Duration
}

func (s *CodeReviewService) Trigger(ctx context.Context, principalID, businessID, agentID, repoConnectorID uuid.UUID, prNumber int) (CodeReview, error) {
	// 1. Resolve connector (RLS) + build client.
	rc, err := s.Repos.Resolve(ctx, principalID, businessID, repoConnectorID)
	if err != nil {
		return CodeReview{}, err
	}
	conn, err := github.NewFactory(60 * time.Second)(rc)
	if err != nil {
		return CodeReview{}, fmt.Errorf("coding: build connector: %w", err)
	}

	// 2. Resolve LLM credential (RLS).
	cred, err := s.Creds.Resolve(ctx, principalID, businessID, agentID)
	if err != nil {
		return CodeReview{}, err
	}
	if cred.Host() == "" {
		return CodeReview{}, fmt.Errorf("coding: agent has no usable AI credential: %w", errs.ErrValidation)
	}

	// 3. Persist a pending code_review row + audit "review.requested".
	crID := uuid.New()
	if err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		if _, ierr := dbgen.New(tx).InsertCodeReview(ctx, dbgen.InsertCodeReviewParams{
			ID: crID, BusinessID: businessID, RepoConnectorID: repoConnectorID, PrNumber: int32(prNumber),
		}); ierr != nil {
			return ierr
		}
		return audit.Write(ctx, tx, codingAudit(businessID, principalID, crID, "agent.coding.review.requested",
			map[string]any{"pr": prNumber, "repo_connector": repoConnectorID}, nil, "requested"))
	}); err != nil {
		return CodeReview{}, err
	}

	// 4. Fetch PR (host) — must be open.
	pr, err := conn.FetchPR(ctx, prNumber)
	if err != nil {
		return s.fail(ctx, principalID, businessID, crID, prNumber, err)
	}

	// 5. Clone PR head (host), into an isolated temp dir.
	checkout := filepath.Join(s.WorkRoot, crID.String(), "checkout")
	outDir := filepath.Join(s.WorkRoot, crID.String(), "out")
	if err := os.MkdirAll(checkout, 0o700); err != nil {
		return s.fail(ctx, principalID, businessID, crID, prNumber, err)
	}
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		return s.fail(ctx, principalID, businessID, crID, prNumber, err)
	}
	defer os.RemoveAll(filepath.Join(s.WorkRoot, crID.String())) // host-side teardown
	if err := CloneAtSHA(ctx, conn.CloneURL(), github.BasicAuthHeader(rc.Credential.APIToken), pr.HeadSHA, checkout); err != nil {
		return s.fail(ctx, principalID, businessID, crID, prNumber, err)
	}

	// 6. Run opencode in the sandbox (audit invocation). The ONLY secret inside is the LLM key.
	spec := sandbox.SandboxSpec{
		Image: s.Image, ReadOnlyDir: checkout, OutputDir: outDir,
		Cmd: opencodeCmd(cred.Model),
		Env: map[string]string{
			"LLM_API_KEY":  cred.APIKey,
			"LLM_BASE_URL": cred.BaseURL,
			"LLM_MODEL":    cred.Model,
		},
		EgressAllow: []string{cred.Host()},
		Timeout:     s.timeout(),
	}
	_ = s.auditStep(ctx, principalID, businessID, crID, "agent.coding.opencode.invoked",
		map[string]any{"image": s.Image, "head_sha": pr.HeadSHA, "model": cred.Model}, nil, "executed")
	res, err := s.Sandbox.Run(ctx, spec)
	if err != nil {
		return s.fail(ctx, principalID, businessID, crID, prNumber, err)
	}

	// 7. Read + validate findings from /out/review.json.
	rawFindings, err := os.ReadFile(filepath.Join(outDir, "review.json"))
	if err != nil {
		return s.fail(ctx, principalID, businessID, crID, prNumber, fmt.Errorf("coding: no findings produced (exit %d): %w", res.ExitCode, err))
	}
	doc, err := ParseFindings(rawFindings)
	if err != nil {
		return s.fail(ctx, principalID, businessID, crID, prNumber, err)
	}

	// 8. Post the review (host, intentionally UNGATED — advisory).
	body := RenderMarkdown(doc)
	ref, err := conn.PostReview(ctx, prNumber, connectors.Review{Summary: doc.Summary, Findings: doc.Findings, Body: body})
	if err != nil {
		return s.fail(ctx, principalID, businessID, crID, prNumber, err)
	}

	// 9. Finalize: persist result + audit "review.posted".
	findingsJSON, _ := json.Marshal(doc.Findings)
	now := time.Now()
	var out CodeReview
	if err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		row, uerr := dbgen.New(tx).UpdateCodeReviewResult(ctx, dbgen.UpdateCodeReviewResultParams{
			ID: crID, Status: "succeeded", HeadSha: pr.HeadSHA, Summary: doc.Summary,
			Findings: findingsJSON, ExternalReviewRef: ref.ExternalID, PostedAt: pgTimestamptz(now),
		})
		if uerr != nil {
			return uerr
		}
		out = CodeReview{ID: row.ID, Status: row.Status, Summary: row.Summary, ReviewURL: ref.URL, PRNumber: prNumber}
		return audit.Write(ctx, tx, codingAudit(businessID, principalID, crID, "agent.coding.review.posted",
			nil, map[string]any{"review_url": ref.URL, "findings": len(doc.Findings)}, "posted"))
	}); err != nil {
		return CodeReview{}, err
	}
	return out, nil
}

// fail marks the code_review failed (status only — no provider/schema detail leaked) + audits, returns a typed error.
func (s *CodeReviewService) fail(ctx context.Context, pid, bid, crID uuid.UUID, prNumber int, cause error) (CodeReview, error) {
	_ = s.DB.WithPrincipal(ctx, pid, func(tx pgx.Tx) error {
		_, _ = dbgen.New(tx).UpdateCodeReviewResult(ctx, dbgen.UpdateCodeReviewResultParams{
			ID: crID, Status: "failed", HeadSha: "", Summary: "", Findings: []byte("[]"), ExternalReviewRef: "", PostedAt: pgNull(),
		})
		return audit.Write(ctx, tx, codingAudit(bid, pid, crID, "agent.coding.review.failed",
			map[string]any{"pr": prNumber}, map[string]any{"error": cause.Error()}, "failed"))
	})
	return CodeReview{}, cause
}
```

Helper notes to implement in the same file: `codingAudit(...)` builds an `audit.Entry` with `BusinessID`, `ActorPrincipalID`, `Action`, `TargetType="code_review"`, `TargetID=crID`, `Inputs`, `Outputs`, `Decision`; `auditStep(...)` opens a short `WithPrincipal` tx to write a standalone audit entry; `opencodeCmd(model)` returns the exec argv (Task 14 pins this); `pgTimestamptz`/`pgNull` adapt `time.Time`→pgtype (reuse the project's `db.PG*` helpers seen in `crm` — `grep -rn "func PG" internal/platform/db`). Match the real `db.DB` import.

- [ ] **Step 2: Write the end-to-end integration test — `//go:build integration`**

Create `internal/agents/coding/service_integration_test.go`. Use `testdb` + seed (tenant/principal/business), a stub GitHub server (httptest), a `FakeCredResolver`, and a **`FakeRunner`** whose `Run` writes a valid `review.json` into `spec.OutputDir`. Assert:
1. Trigger returns `Status=="succeeded"` and a non-empty `ReviewURL`.
2. The stub GitHub server received exactly one `POST .../reviews` with a body containing the summary.
3. `GetCodeReview` shows `status='succeeded'`, `posted_at` set, findings persisted.
4. A second case: `FakeRunner` writes malformed json → Trigger returns error AND `GetCodeReview` shows `status='failed'` AND no POST to GitHub occurred.
5. RLS: `GetCodeReview` via a different business's principal → not found.

```go
//go:build integration

package coding

// Build the connector to point at the httptest GitHub stub by inserting a repo_connector
// whose base_url = stub.URL and allow_private_base_url=true (stub is 127.0.0.1).
// FakeRunner writes spec.OutputDir/review.json. Assert the 5 points above.
```

- [ ] **Step 3: Run the integration test**

Run: `go test -tags integration -p 1 ./internal/agents/coding/ -run TestCodeReviewTrigger -v`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/agents/coding/service.go internal/agents/coding/service_integration_test.go
git commit -m "feat(007): CodeReviewService orchestration (clone -> sandbox -> findings -> post) + e2e test"
```

---

## Task 14: Real sandbox image (opencode + constrained config) + invocation pin

**Files:**
- Create: `deploy/sandbox/Dockerfile`, `deploy/sandbox/opencode.json`, `deploy/sandbox/entrypoint.sh`
- Modify: `internal/agents/coding/service.go` (finalize `opencodeCmd`)
- Test: `internal/security_regression/coding_review_pins_test.go` (add invocation pin)

**Interfaces:**
- Produces: `manyforge/opencode-sandbox` image; `opencodeCmd(model)` argv.

- [ ] **Step 1: Confirm opencode's headless invocation + config**

Run: `gh search repos opencode --limit 3` then read opencode's docs for: the non-interactive run subcommand, how to set the model + an OpenAI-compatible base URL + API key via env, and how to restrict tools (disable shell/edit). Capture the exact flags. (opencode is configured via `opencode.json` + env; the run is non-interactive `opencode run "<prompt>"`.)

- [ ] **Step 2: Write the constrained opencode config**

Create `deploy/sandbox/opencode.json` (read+reason only — no shell/edit/write tools; the sandbox is the backstop):

```json
{
  "$schema": "https://opencode.ai/config.json",
  "permission": { "edit": "deny", "bash": "deny", "webfetch": "deny" }
}
```

(Adjust keys to opencode's current schema confirmed in Step 1; intent: deny all mutate/exec/network-tool capabilities so opencode only reads files and reasons.)

- [ ] **Step 3: Write the entrypoint that emits structured findings to /out/review.json**

Create `deploy/sandbox/entrypoint.sh`:

```sh
#!/bin/sh
set -eu
PROMPT='Review the code in /work for bugs, security issues, and quality problems.
Output ONLY a JSON object to stdout, no prose, matching exactly:
{"summary": string, "findings": [{"file": string, "line": number|null, "severity": "info"|"warning"|"error", "title": string, "detail": string}]}'
# opencode reads model/key/base-url from env (set by the sandbox runner).
opencode run "$PROMPT" > /out/review.json 2> /out/stderr.log
```

- [ ] **Step 4: Write the Dockerfile**

Create `deploy/sandbox/Dockerfile`:

```dockerfile
FROM node:22-alpine
RUN apk add --no-cache git
# Install opencode (confirm the real install method in Step 1; npm shown as the common path):
RUN npm install -g opencode-ai || npm install -g opencode
COPY deploy/sandbox/opencode.json /etc/opencode/opencode.json
COPY deploy/sandbox/entrypoint.sh /usr/local/bin/review
RUN chmod +x /usr/local/bin/review
ENV OPENCODE_CONFIG=/etc/opencode/opencode.json
ENTRYPOINT ["/usr/local/bin/review"]
```

- [ ] **Step 5: Finalize `opencodeCmd` + map env to opencode's expected vars**

In `service.go`, set `opencodeCmd` to `[]string{}` (entrypoint runs the review) and ensure the `Env` keys match what opencode reads (e.g. `OPENCODE_MODEL`, the provider key var). Update the `Env` map in `Trigger` Step 6 to the confirmed variable names from Step 1.

- [ ] **Step 6: Build the image**

Run: `docker build -f deploy/sandbox/Dockerfile -t manyforge/opencode-sandbox:dev .`
Expected: image builds (opencode present).

- [ ] **Step 7: Add the opencode invocation source pin**

Append to `internal/security_regression/coding_review_pins_test.go`:

```go
// MF007-PIN-2: the sandbox must be read-only + drop caps + force the egress proxy.
func TestSandboxRunArgsPinned(t *testing.T) {
	src := mustReadFile(t, "../agents/coding/sandbox/docker.go")
	for _, frag := range []string{`"--read-only"`, `"--cap-drop", "ALL"`, `":/work:ro"`, `"HTTPS_PROXY="`, `"--network"`} {
		if !strings.Contains(src, frag) {
			t.Fatalf("sandbox hardening fragment %q missing from docker.go", frag)
		}
	}
}
```

Add a `mustReadFile` helper (or reuse the existing `mustRead` in the package).

- [ ] **Step 8: Run pins + commit**

Run: `go test ./internal/security_regression/ -run 'TestSandboxRunArgsPinned|TestRepoConnectorHasNoWriteCapability' -v` → PASS.

```bash
git add deploy/sandbox/ internal/agents/coding/service.go internal/security_regression/coding_review_pins_test.go
git commit -m "feat(007): opencode sandbox image (constrained) + invocation/hardening pins"
```

---

## Task 15: HTTP handlers + routes + main wiring

**Files:**
- Create: `internal/agents/coding/handler.go`
- Test: `internal/agents/coding/handler_test.go`
- Modify: `cmd/manyforge/main.go`

**Interfaces:**
- Consumes: `httpx.PrincipalFromContext`, `httpx.WriteJSON`/`WriteError`, chi, `RepoConnectorService`, `CodeReviewService`.
- Produces: `Handler{RepoSvc, ReviewSvc}` with `ProtectedRoutes(chi.Router)`; routes `POST /businesses/{id}/repo-connectors`, `POST /businesses/{id}/code-reviews`, `GET /businesses/{id}/code-reviews/{reviewID}`.

- [ ] **Step 1: Write the handler** (mirror `agent_handler.go`'s thin pattern)

Create `internal/agents/coding/handler.go`:

```go
package coding

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/<module>/internal/connectors"
	"github.com/<module>/internal/platform/errs"
	"github.com/<module>/internal/platform/httpx"
)

type Handler struct {
	RepoSvc   *connectors.RepoConnectorService
	ReviewSvc *CodeReviewService
}

func (h *Handler) ProtectedRoutes(r chi.Router) {
	r.Route("/businesses/{id}/repo-connectors", func(r chi.Router) {
		r.Post("/", h.createRepoConnector)
	})
	r.Route("/businesses/{id}/code-reviews", func(r chi.Router) {
		r.Post("/", h.triggerReview)
		r.Get("/{reviewID}", h.getReview)
	})
}

func businessID(r *http.Request) (uuid.UUID, error) {
	return uuid.Parse(chi.URLParam(r, "id"))
}

func (h *Handler) createRepoConnector(w http.ResponseWriter, r *http.Request) {
	pid, ok := httpx.PrincipalFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	bid, err := businessID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	var in connectors.CreateRepoConnectorInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteError(w, r, errs.ErrValidation)
		return
	}
	id, err := h.RepoSvc.Create(r.Context(), pid, bid, in)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"id": id})
}

func (h *Handler) triggerReview(w http.ResponseWriter, r *http.Request) {
	pid, ok := httpx.PrincipalFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	bid, err := businessID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	var in struct {
		AgentID         string `json:"agent_id"`
		RepoConnectorID string `json:"repo_connector_id"`
		PRNumber        int    `json:"pr_number"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteError(w, r, errs.ErrValidation)
		return
	}
	agentID, e1 := uuid.Parse(in.AgentID)
	rcID, e2 := uuid.Parse(in.RepoConnectorID)
	if e1 != nil || e2 != nil || in.PRNumber <= 0 {
		httpx.WriteError(w, r, errs.ErrValidation)
		return
	}
	cr, err := h.ReviewSvc.Trigger(r.Context(), pid, bid, agentID, rcID, in.PRNumber)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusAccepted, map[string]any{
		"id": cr.ID, "status": cr.Status, "review_url": cr.ReviewURL,
	})
}

func (h *Handler) getReview(w http.ResponseWriter, r *http.Request) {
	pid, ok := httpx.PrincipalFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	bid, err := businessID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	reviewID, err := uuid.Parse(chi.URLParam(r, "reviewID"))
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	cr, err := h.ReviewSvc.Get(r.Context(), pid, bid, reviewID)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, cr)
}
```

Add a `CodeReviewService.Get(ctx, pid, bid, id) (CodeReview, error)` method in `service.go` using `GetCodeReview` under `WithPrincipal` (map `pgx.ErrNoRows`→`errs.ErrNotFound`).

- [ ] **Step 2: Write handler unit tests** (validation + auth-missing → 404/400) using `httptest` + a chi router with a fake principal injected via the same context key the middleware uses (read `httpx.PrincipalFromContext` for the key).

Create `internal/agents/coding/handler_test.go` covering: missing principal → 404; bad JSON → 400; bad uuids → 400.

- [ ] **Step 3: Wire into `main.go`**

In `cmd/manyforge/main.go`, after the connector service block (~line 326):

```go
repoSvc := &connectors.RepoConnectorService{DB: database, Vault: secrets.NewVault(connSealer)}
if err := sandbox.EnsureEgressInfra(ctx, cfg.EgressProxyImage, cfg.SandboxEgressAllow); err != nil {
	logger.Error("init sandbox egress infra", "err", err)
	// non-fatal in dev if Docker absent; coding reviews will fail at run time.
}
codingSvc := &coding.CodeReviewService{
	DB: database, Repos: repoSvc, Sandbox: sandbox.NewDockerRunner(sandbox.NetworkName, sandbox.ProxyDNSAddr),
	Creds: coding.NewAgentCredResolver(/* wire AI cred service */), Image: cfg.SandboxImage,
	WorkRoot: cfg.SandboxWorkRoot, Timeout: 5 * time.Minute,
}
codingHandler := &coding.Handler{RepoSvc: repoSvc, ReviewSvc: codingSvc}
```

Then register `codingHandler.ProtectedRoutes` on the same protected `/api/v1` subrouter the other handlers use (find the `mountAPIRoutes`/protected group and add it). Add config fields (`SandboxImage`, `EgressProxyImage`, `SandboxEgressAllow`, `SandboxWorkRoot`) with env defaults (`manyforge/opencode-sandbox:dev`, `manyforge/egress-proxy:dev`, the provider host list, `os.TempDir()+"/mf-sandbox"`).

- [ ] **Step 4: Build + run handler tests**

Run: `go build ./... && go test ./internal/agents/coding/ -run TestHandler -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/agents/coding/handler.go internal/agents/coding/handler_test.go internal/agents/coding/service.go cmd/manyforge/main.go
git commit -m "feat(007): code-review HTTP handlers + main wiring"
```

---

## Task 16: OpenAPI 007 contract + drift test

**Files:**
- Create: `specs/007-coding-review-agents/contracts/openapi.yaml`
- Create: `cmd/manyforge/drift_007_test.go`

**Interfaces:**
- Consumes: the `apiRoutes`/`mountAPIRoutes` seam used by `drift_003_test.go`.

- [ ] **Step 1: Read the 003 drift test to copy the mechanism**

Run: `sed -n '1,90p' cmd/manyforge/drift_003_test.go` and note `apiRoutes`, `spec003Routes`, and the in-scope list shape.

- [ ] **Step 2: Write the 007 OpenAPI**

Create `specs/007-coding-review-agents/contracts/openapi.yaml` documenting the three endpoints (mirror the 003 file's component refs for NotFound/ValidationError):

```yaml
openapi: 3.1.0
info: { title: manyforge Coding & Review API, version: 0.1.0 }
paths:
  /businesses/{id}/repo-connectors:
    parameters:
      - { name: id, in: path, required: true, schema: { type: string, format: uuid } }
    post:
      operationId: createRepoConnector
      summary: Register a GitHub repo connector
      responses:
        "201": { description: Created }
        "400": { description: Validation error }
        "404": { description: Not found }
  /businesses/{id}/code-reviews:
    parameters:
      - { name: id, in: path, required: true, schema: { type: string, format: uuid } }
    post:
      operationId: triggerCodeReview
      summary: Trigger a code review of a pull request
      responses:
        "202": { description: Accepted }
        "400": { description: Validation error }
        "404": { description: Not found }
  /businesses/{id}/code-reviews/{reviewID}:
    parameters:
      - { name: id, in: path, required: true, schema: { type: string, format: uuid } }
      - { name: reviewID, in: path, required: true, schema: { type: string, format: uuid } }
    get:
      operationId: getCodeReview
      summary: Get a code review record
      responses:
        "200": { description: Code review }
        "404": { description: Not found }
```

- [ ] **Step 3: Write the drift test** (copy `drift_003_test.go`'s structure, point at the 007 yaml, list the 3 in-scope ops)

Create `cmd/manyforge/drift_007_test.go`:

```go
//go:build contract

package main

// Mirror drift_003_test.go: parse specs/007-coding-review-agents/contracts/openapi.yaml,
// walk the production router via the same seam, assert presence + no drift for:
var inScope007Ops = []string{
	"POST /businesses/{}/repo-connectors",
	"POST /businesses/{}/code-reviews",
	"GET /businesses/{}/code-reviews/{}",
}
```

- [ ] **Step 4: Run the contract test**

Run: `go test -tags contract ./cmd/manyforge/ -run Drift007 -v`
Expected: PASS (routes registered ↔ documented).

- [ ] **Step 5: Commit**

```bash
git add specs/007-coding-review-agents/contracts/openapi.yaml cmd/manyforge/drift_007_test.go
git commit -m "feat(007): OpenAPI contract + drift test for coding endpoints"
```

---

## Task 17: Security pins, quickstart, full verification

**Files:**
- Modify: `internal/security_regression/coding_review_pins_test.go` (add ungated-posting + no-ambient-creds pins)
- Create: `specs/007-coding-review-agents/quickstart.md`

- [ ] **Step 1: Add the ungated-posting + env-allowlist source pins**

Append to `internal/security_regression/coding_review_pins_test.go`:

```go
// MF007-PIN-3: review posting is intentionally ungated — the service must NOT route
// the post through the approval queue (no CreatePending / approval in service.go).
func TestReviewPostingIsUngated(t *testing.T) {
	src := mustReadFile(t, "../agents/coding/service.go")
	for _, banned := range []string{"CreatePending", "ApprovalPending", "awaiting_approval"} {
		if strings.Contains(src, banned) {
			t.Fatalf("service.go references %q — review posting must stay ungated/advisory", banned)
		}
	}
	if !strings.Contains(src, "PostReview") {
		t.Fatal("service.go must post the review directly")
	}
}

// MF007-PIN-4: only allowlisted run-scoped secrets enter the sandbox Env — the service
// must build Env from the resolved LLM credential, never from os.Environ()/host.
func TestSandboxEnvNoHostInheritance(t *testing.T) {
	src := mustReadFile(t, "../agents/coding/service.go")
	if strings.Contains(src, "os.Environ()") {
		t.Fatal("service.go must not pass host environment into the sandbox spec")
	}
}
```

- [ ] **Step 2: Run the full pin suite**

Run: `go test ./internal/security_regression/ -run 'TestRepoConnectorHasNoWriteCapability|TestSandboxRunArgsPinned|TestReviewPostingIsUngated|TestSandboxEnvNoHostInheritance' -v`
Expected: all PASS.

- [ ] **Step 3: Write the quickstart/demo**

Create `specs/007-coding-review-agents/quickstart.md` documenting the demo: build images (`docker build` proxy + sandbox), create AI credential + review agent + repo connector (curl with a real fine-grained PAT against a throwaway repo), `POST /code-reviews`, observe the review on the PR, and `GET /code-reviews/{id}` + the audit trail. Include the exact curl commands and the four env/config values.

- [ ] **Step 4: Full verification gate**

Run each and confirm:
- `make test` → PASS (unit + fast pins)
- `make lint` → clean (vet + staticcheck)
- `go test -tags contract ./cmd/...` → PASS (003 + 007 drift)
- `go test -tags integration -p 1 ./internal/connectors/ ./internal/agents/coding/...` → PASS (Docker required)
- `make sec-test` → PASS

- [ ] **Step 5: Commit**

```bash
git add internal/security_regression/coding_review_pins_test.go specs/007-coding-review-agents/quickstart.md
git commit -m "feat(007): ungated-posting + no-ambient-creds pins; quickstart"
```

---

## Self-Review

**Spec coverage** (each spec requirement → task):
- FR-001 repo connector register + vault + RLS → T1, T3. FR-002 SSRF guard → T3 (validateBaseURL). FR-003 trigger by PR number → T15. FR-004 reuse run lifecycle → T9/T11/T13 (code_review + agent_run target; see deviation note below). FR-005 host clone + RO provision → T5, T8, T13. FR-006 no ambient creds → T8, T13, T17 (PIN-4). FR-007 egress allowlist → T7, T8. FR-008 opencode constrained → T14. FR-009 validate findings → T10. FR-010 render + post one review → T4, T10, T13. FR-011 auto-post ungated → T13, T17 (PIN-3). FR-012 no write capability → T2 (PIN-1). FR-013 ephemeral teardown → T8 (`--rm`), T13 (`os.RemoveAll`). FR-014 wall-clock cap → T8 (timeout). FR-015 persist code_review → T9, T13. FR-016 audit each step → T13. FR-017 no oracle → T3, T4, T15 (ErrNotFound). FR-018 SandboxRunner interface → T6.
- User stories: US1 → T13/T15; US2 → T8 (isolation test) + pins; US3 → T13 (audit) + integration assertions.
- Success criteria SC-001..007 → demo (T17), isolation test (T8), pins (T2/T14/T17), audit (T13), timeout (T8), findings validation (T10), contract+sec-test (T16/T17).

**Deviation from spec wording (flag at handoff):** FR-004 says "execute as a spec 003 agent run (reusing the existing Engine, run lifecycle, and gating)." The plan reuses the run *lifecycle* (`code_review` row + the `code_review` agent_run target type), audit, and the gating *concept* (posting is deliberately ungated), but does **not** call the LLM-loop `Engine.Run` — opencode is the sole LLM. If you want literal `Engine.Run` reuse (a manyforge LLM orchestrating tool calls on top of opencode), that's a different, heavier shape — raise it before execution.

**Placeholder scan:** no TBD/TODO/"similar to". Two tasks contain explicit *verification* steps where an external contract must be confirmed against upstream (opencode flags — T14 Step 1; the AI credential service signature — T12 Step 1); both define concrete interfaces/configs so dependents compile, with the upstream detail confirmed in-task. The `<module>` token in import paths must be replaced with `head -1 go.mod` everywhere it appears.

**Type consistency:** `connectors.Finding`/`Review`/`PullRequest`/`ReviewRef`/`ResolvedRepoConnector` defined in T2 used consistently in T4/T10/T13. `sandbox.SandboxSpec`/`SandboxResult`/`SandboxRunner` defined T6, used T8/T13. `AICredential`/`AICredentialResolver` defined T12, used T13. `CodeReviewService.Trigger`/`Get` signatures stable across T13/T15. dbgen funcs (`InsertRepoConnector`, `GetRepoConnector`, `InsertCodeReview`, `UpdateCodeReviewResult`, `GetCodeReview`) defined in T1/T9, used in T3/T13.
