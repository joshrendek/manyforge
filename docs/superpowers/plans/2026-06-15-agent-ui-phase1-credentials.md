# Provider Credentials UI — Implementation Plan (Phase 1 of 2)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a web surface to create / list / delete per-business AI-provider BYO credentials (`ai_provider_credential`), backed by a new HTTP API, and establish the `/credentials` section (moving the existing connectors page under `/credentials/connector`).

**Architecture:** Backend mirrors the existing MCP/connector handler pattern exactly — a thin `credential_handler.go` that delegates to `CredentialService` (extended with `List`/`Delete`, and `Create` upgraded to return a view), all error→status mapping via the shared `httpx.WriteError`, write-only secret DTO, gated server-side by the existing `agents.configure` permission. A nil AI sealer leaves the route group unregistered (same nil-guard as connectors). Frontend mirrors the Angular 21 standalone + signals + template-driven connectors page, with the service in `core/` and the page under `pages/credentials/ai/`.

**Tech Stack:** Go (chi, pgx, sqlc v1.27.0, `internal/agents`), Postgres (RLS via `WithPrincipal`), Angular 21 (standalone components, signals, `FormsModule` `[(ngModel)]`, `@if/@for`), Playwright e2e.

**Spec:** `docs/superpowers/specs/2026-06-15-agent-management-ui-design.md` (Phase 1).
**bd issue:** `manyforge-1kv` (claimed).

---

## Conventions for every task

- **Go env:** `export PATH="$HOME/go/bin:$PATH"`.
- **Build gate:** `go build ./...` → exit 0. **Test gate:** `make test`. **Security regression:** `make sec-test`. **Lint:** `make lint`.
- **Integration tests** (need testdb): `go test -tags integration ./internal/agents/...`.
- **sqlc:** if SQL changes, run `/opt/homebrew/bin/sqlc generate` (pinned v1.27.0). **NEVER `make generate`.** sqlc reads `db/schema.sql`, not migrations. **This plan adds NO new SQL** (all queries already exist), so sqlc is not run.
- **gopls squiggles are stale** right after any regen — trust `go build`.
- **Frontend (`cd web`):** unit `npm test` (runs once), build `npm run build`, e2e `npm run e2e -- e2e/<file>.spec.ts` (needs the dev server on :4300, but specs are `page.route`-mocked so no backend).
- **Commit cadence:** one commit per task (or per the explicit commit step). The bd journal auto-stages; after any `bd close`, make a separate `chore(bd): …` commit.

---

## File Structure

**Backend (create/modify):**
- Modify `internal/agents/credential.go` — add `CredentialView` type, change `Create` to return the view, add `List` + `Delete`.
- Create `internal/agents/credential_handler.go` — HTTP handler (`CredentialHandler`, `NewCredentialHandler`, `ProtectedRoutes`, DTOs).
- Create `internal/agents/credential_handler_test.go` — handler unit tests (fake service).
- Modify `internal/agents/credential_test.go` (or wherever Create is tested) — update Create assertions; add `List`/`Delete` integration tests (may live in a new `credential_integration_test.go`).
- Modify `internal/platform/config/<config>.go` — add `AIMasterKey` (env `MANYFORGE_AI_MASTER_KEY`).
- Modify `cmd/manyforge/main.go` — build the AI sealer, wire it into `credSvc`, construct + mount the credential handler.
- Create `internal/security_regression/ai_credential_response_pin_test.go` — pin: response never serializes the API key; routes stay `agents.configure`-gated.
- Modify `specs/003-agent-runtime/contracts/openapi.yaml` — add credential schemas + paths.

**Frontend (create/modify):**
- Create `web/src/app/core/ai-credentials.service.ts` — service + interfaces.
- Create `web/src/app/pages/credentials/ai/list.ts` — list component.
- Create `web/src/app/pages/credentials/ai/credential-form.ts` — create form.
- Create `web/src/app/pages/credentials/ai/list.spec.ts`, `web/src/app/pages/credentials/ai/credential-form.spec.ts` — unit specs.
- Modify `web/src/app/app.routes.ts` — add `/credentials/ai`, `/credentials` redirect; move connectors → `/credentials/connector`.
- Modify `web/src/app/ui/nav.ts` — add "AI Credentials"; repoint "Connectors".
- Modify `web/src/app/app.ts` — repoint the connectors badge route string to `/credentials/connector`.
- Modify `web/e2e/connectors.spec.ts` — change `goto('/connectors')` → `goto('/credentials/connector')`.
- Create `web/e2e/ai-credentials.spec.ts` — e2e for the new page.

---

## Task 1: Extend `CredentialService` — `CredentialView`, view-returning `Create`, `List`, `Delete`

**Files:**
- Modify: `internal/agents/credential.go`
- Test: `internal/agents/credential_integration_test.go` (create), and the existing Create test file

**Context (verified):** `ai_provider_credential` stores the sealed key inline in `sealed_key_ref text` (no vault table), so `DeleteAIProviderCredential(id, business_id)` removes the secret in one statement. `InsertAIProviderCredential` is `:one … RETURNING *`, so `Create` already receives the full row but currently discards it. `ListAIProviderCredentials(ctx, businessID)` → `[]AiProviderCredential`. `DeleteAIProviderCredential(ctx, {ID, BusinessID})` → `int64` rows. `mapCredErr` already maps `pgx.ErrNoRows`→`ErrNotFound` and `23505`→`ErrConflict`.

- [ ] **Step 1: Write the failing integration tests for `List` and `Delete`**

Create `internal/agents/credential_integration_test.go`. (Match the build-tag and testdb-harness convention used by the other `*_integration_test.go` files in this package — open one to copy the `//go:build integration` header and the DB/principal setup helper, e.g. `newTestDB(t)` / the helper that mints a business + principal. Replace the harness calls below with that package's actual helpers.)

```go
//go:build integration

package agents_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/manyforge/manyforge/internal/agents"
	"github.com/manyforge/manyforge/internal/platform/errs"
)

func TestCredentialService_ListDelete(t *testing.T) {
	ctx := context.Background()
	db, sealer := newCredentialTestDeps(t)      // testdb + crypto.Sealer (copy from existing Create integration test)
	svc := &agents.CredentialService{DB: db, Sealer: sealer}
	pid, bizA := seedPrincipalAndBusiness(t, db) // existing helper: returns a principal with agents.configure on bizA
	_, bizB := seedPrincipalAndBusiness(t, db)   // a second tenant the principal cannot see

	// Create two credentials in bizA.
	vAnthropic, err := svc.Create(ctx, pid, bizA, agents.CreateCredentialInput{
		Provider: "anthropic", APIKey: "sk-ant-xxx", DefaultModel: "claude-opus-4-8",
	})
	if err != nil {
		t.Fatalf("create anthropic: %v", err)
	}
	if _, err := svc.Create(ctx, pid, bizA, agents.CreateCredentialInput{
		Provider: "openai", APIKey: "sk-oai-xxx", DefaultModel: "gpt-5",
	}); err != nil {
		t.Fatalf("create openai: %v", err)
	}

	// List returns both, ordered by provider, and NEVER carries a key field (CredentialView has none).
	views, err := svc.List(ctx, pid, bizA)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(views) != 2 {
		t.Fatalf("want 2 credentials, got %d", len(views))
	}
	if views[0].Provider != "anthropic" || views[1].Provider != "openai" {
		t.Fatalf("want providers ordered [anthropic openai], got [%s %s]", views[0].Provider, views[1].Provider)
	}

	// Cross-tenant: listing bizB as the same principal sees nothing (RLS).
	if other, err := svc.List(ctx, pid, bizB); err != nil || len(other) != 0 {
		t.Fatalf("cross-tenant list: want 0 rows no error, got %d / %v", len(other), err)
	}

	// Delete a real credential → no error; row is gone.
	if err := svc.Delete(ctx, pid, bizA, vAnthropic.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if views, _ := svc.List(ctx, pid, bizA); len(views) != 1 {
		t.Fatalf("want 1 credential after delete, got %d", len(views))
	}

	// Delete an unknown / already-deleted id → ErrNotFound (no oracle).
	if err := svc.Delete(ctx, pid, bizA, uuid.New()); err == nil || !errorsIs(err, errs.ErrNotFound) {
		t.Fatalf("delete unknown: want ErrNotFound, got %v", err)
	}
}
```

(If the package already has an `errorsIs` shim, reuse it; otherwise use `errors.Is` directly and import `errors`. Reuse the existing testdb helpers from the package's current Create integration test rather than the placeholder helper names above.)

- [ ] **Step 2: Run the integration tests to verify they fail to compile/run**

Run: `export PATH="$HOME/go/bin:$PATH" && go test -tags integration ./internal/agents/ -run TestCredentialService_ListDelete -v`
Expected: FAIL — `svc.List` / `svc.Delete` undefined, and `views[0].Provider` references a field on a type that doesn't exist yet.

- [ ] **Step 3: Add `CredentialView`, the row→view helper, and change `Create` to return it**

In `internal/agents/credential.go`, add the type near `CreateCredentialInput` (after line ~56):

```go
// CredentialView is the non-secret projection of a stored credential. It NEVER
// carries the API key (sealed_key_ref) — reads are write-only for the secret.
type CredentialView struct {
	ID                  uuid.UUID
	BusinessID          uuid.UUID
	Provider            string
	BaseURL             string
	DefaultModel        string
	AllowPrivateBaseURL bool
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// credViewFromRow projects a stored row to its non-secret view.
func credViewFromRow(row dbgen.AiProviderCredential) CredentialView {
	base := ""
	if row.BaseUrl != nil {
		base = *row.BaseUrl
	}
	return CredentialView{
		ID:                  row.ID,
		BusinessID:          row.BusinessID,
		Provider:            string(row.Provider),
		BaseURL:             base,
		DefaultModel:        row.DefaultModel,
		AllowPrivateBaseURL: row.AllowPrivateBaseUrl,
		CreatedAt:           row.CreatedAt,
		UpdatedAt:           row.UpdatedAt,
	}
}
```

(Add `"time"` to the import block if not already present.)

Then change `Create` to capture the `RETURNING *` row and return the view. Replace the signature and the insert call:

```go
// Create seals the API key and inserts the credential, returning its non-secret view.
func (s *CredentialService) Create(ctx context.Context, principalID, businessID uuid.UUID, in CreateCredentialInput) (CredentialView, error) {
	if err := s.validate(in); err != nil {
		return CredentialView{}, err
	}
	ref, err := s.sealAPIKey(in.APIKey)
	if err != nil {
		return CredentialView{}, err
	}
	id := uuid.New()
	var refArg *string
	if ref != "" {
		refArg = &ref
	}
	var baseArg *string
	if in.BaseURL != "" {
		baseArg = &in.BaseURL
	}
	var row dbgen.AiProviderCredential
	err = s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		r, qerr := dbgen.New(tx).InsertAIProviderCredential(ctx, dbgen.InsertAIProviderCredentialParams{
			ID:                  id,
			BusinessID:          businessID,
			Provider:            dbgen.AiProvider(in.Provider),
			SealedKeyRef:        refArg,
			BaseUrl:             baseArg,
			DefaultModel:        in.DefaultModel,
			AllowPrivateBaseUrl: in.AllowPrivateBaseURL,
		})
		if qerr != nil {
			return qerr
		}
		row = r
		// Trusting a private/loopback endpoint is a security-sensitive grant — audit it
		// in the SAME tx as the insert (atomicity invariant).
		if in.AllowPrivateBaseURL {
			tt := "ai_credential"
			dec := "trust_private_base_url"
			return audit.Write(ctx, tx, audit.Entry{
				BusinessID:       &businessID,
				ActorPrincipalID: &principalID,
				Action:           "ai_credential.created",
				TargetType:       &tt,
				TargetID:         &id,
				Decision:         &dec,
				Inputs:           map[string]any{"provider": in.Provider, "base_url": in.BaseURL},
			})
		}
		return nil
	})
	if err != nil {
		return CredentialView{}, mapCredErr(err)
	}
	return credViewFromRow(row), nil
}
```

- [ ] **Step 4: Add `List` and `Delete`**

Append to `internal/agents/credential.go`:

```go
// List returns the non-secret views of all credentials for a business, ordered
// by provider. RLS-scoped via WithPrincipal; the SQL also pins business_id.
func (s *CredentialService) List(ctx context.Context, principalID, businessID uuid.UUID) ([]CredentialView, error) {
	var rows []dbgen.AiProviderCredential
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		r, qerr := dbgen.New(tx).ListAIProviderCredentials(ctx, businessID)
		rows = r
		return qerr
	})
	if err != nil {
		return nil, mapCredErr(err)
	}
	out := make([]CredentialView, 0, len(rows))
	for _, row := range rows {
		out = append(out, credViewFromRow(row))
	}
	return out, nil
}

// Delete removes a credential by id within a business. 0 rows affected ⇒
// ErrNotFound (no existence oracle for unknown/foreign ids). Deleting the row
// removes the inline sealed_key_ref, so no separate secret cleanup is needed.
func (s *CredentialService) Delete(ctx context.Context, principalID, businessID, credentialID uuid.UUID) error {
	var affected int64
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		n, qerr := dbgen.New(tx).DeleteAIProviderCredential(ctx, dbgen.DeleteAIProviderCredentialParams{
			ID: credentialID, BusinessID: businessID,
		})
		affected = n
		return qerr
	})
	if err != nil {
		return mapCredErr(err)
	}
	if affected == 0 {
		return fmt.Errorf("agents: credential not found: %w", errs.ErrNotFound)
	}
	return nil
}
```

- [ ] **Step 5: Update the existing `Create` tests for the new return type**

Open the current Create test file (`internal/agents/credential_test.go` and/or the existing Create integration test). Every `id, err := svc.Create(...)` that asserted on a `uuid.UUID` must change to `view, err := svc.Create(...)` asserting on `view.ID` / `view.Provider`. Update assertions accordingly. Do not change behavior — only the variable + field access.

- [ ] **Step 6: Run the build and tests to verify they pass**

Run: `export PATH="$HOME/go/bin:$PATH" && go build ./... && go test ./internal/agents/ && go test -tags integration ./internal/agents/ -run 'TestCredentialService' -v`
Expected: build exit 0; unit tests PASS; the new `TestCredentialService_ListDelete` PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/agents/credential.go internal/agents/credential_test.go internal/agents/credential_integration_test.go
git commit -m "feat(agents): CredentialService List/Delete + view-returning Create"
```

---

## Task 2: Wire the AI credential Sealer (config + main.go)

**Files:**
- Modify: `internal/platform/config/<config>.go` (the file that defines `MCPMasterKey`)
- Modify: `cmd/manyforge/main.go`

**Context (verified):** `credSvc := &agents.CredentialService{DB: database}` at `main.go:157` has a **nil `Sealer`** and it is never assigned — `Create`/`Resolve` nil-panic on any non-empty key. No `MANYFORGE_AI_MASTER_KEY` exists. The fix copies the MCP sealer pattern (`main.go:243-254`): a config master key → `mfcrypto.NewSealer(...)` → assigned into the service. Imports in main.go: `agents` and `authz` unaliased; crypto aliased `mfcrypto` (`main.go:35`).

- [ ] **Step 1: Add the `AIMasterKey` config field**

Find every occurrence of `MCPMasterKey` in `internal/platform/config/` (it appears as a struct field plus an env-binding line for `MANYFORGE_MCP_MASTER_KEY`):

Run: `export PATH="$HOME/go/bin:$PATH" && grep -rn "MCPMasterKey\|MANYFORGE_MCP_MASTER_KEY" internal/platform/config/`

For each occurrence, add a sibling `AIMasterKey` bound to `MANYFORGE_AI_MASTER_KEY`, with the same type/default/optionality as `MCPMasterKey`. (Read the file with the Read tool to see the exact struct + binding style before editing — match it verbatim.)

- [ ] **Step 2: Build the AI sealer and wire it into `credSvc` in main.go**

In `cmd/manyforge/main.go`, immediately before `credSvc := &agents.CredentialService{DB: database}` (line ~157), insert a sealer build that mirrors the MCP sealer (`main.go:243-254`). Then set the field. Replace line 157:

```go
	// Agent BYO-credential sealing. Optional: with no AI master key the
	// credential HTTP surface is disabled (handler left nil, like connectors),
	// and the run engine cannot resolve BYO keys. Set MANYFORGE_AI_MASTER_KEY in
	// any environment that uses provider credentials.
	var aiSealer *mfcrypto.Sealer
	if len(cfg.AIMasterKey) > 0 {
		s, serr := mfcrypto.NewSealer(cfg.AIMasterKey)
		if serr != nil {
			logger.Error("ai credential sealer", "err", serr)
			os.Exit(1)
		}
		aiSealer = s
	} else {
		logger.Warn("MANYFORGE_AI_MASTER_KEY unset — AI provider credentials disabled")
	}
	credSvc := &agents.CredentialService{DB: database, Sealer: aiSealer}
```

(Confirm the exact warn/fatal idiom against the MCP block at `main.go:243-254` and match it — e.g. whether it uses `logger.Warn` vs `slog`, and `os.Exit(1)` vs returning an error.)

- [ ] **Step 3: Verify the build**

Run: `export PATH="$HOME/go/bin:$PATH" && go build ./...`
Expected: exit 0.

- [ ] **Step 4: Commit**

```bash
git add internal/platform/config cmd/manyforge/main.go
git commit -m "feat(agents): wire AI credential sealer (MANYFORGE_AI_MASTER_KEY)"
```

---

## Task 3: Credential HTTP handler

**Files:**
- Create: `internal/agents/credential_handler.go`
- Test: `internal/agents/credential_handler_test.go`

**Context (verified):** Mirror `mcp_server_handler.go` / `agent_handler.go`. Handlers are thin: pull principal via `httpx.PrincipalFromContext`, parse business id from `{id}`, delegate to the service, map errors via `httpx.WriteError`, write JSON via `httpx.WriteJSON`, decode via `httpx.DecodeJSON`. List shape is `{"items": [...]}` with a non-nil slice. The permission gate is **not** applied here — it's applied in main.go via `pr.Group(... .Use(gate))`.

- [ ] **Step 1: Write the failing handler unit tests (fake service)**

Create `internal/agents/credential_handler_test.go`:

```go
package agents_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/manyforge/manyforge/internal/agents"
	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/httpx"
)

// fakeCredSvc implements the handler's credentialCRUD seam.
type fakeCredSvc struct {
	createView agents.CredentialView
	createErr  error
	listViews  []agents.CredentialView
	listErr    error
	deleteErr  error
	gotDeleteID uuid.UUID
}

func (f *fakeCredSvc) Create(_ context.Context, _, _ uuid.UUID, _ agents.CreateCredentialInput) (agents.CredentialView, error) {
	return f.createView, f.createErr
}
func (f *fakeCredSvc) List(_ context.Context, _, _ uuid.UUID) ([]agents.CredentialView, error) {
	return f.listViews, f.listErr
}
func (f *fakeCredSvc) Delete(_ context.Context, _, _, id uuid.UUID) error {
	f.gotDeleteID = id
	return f.deleteErr
}

// withPrincipal mounts the handler behind a context that carries a principal +
// the chi {id} business param, so the thin handler's lookups succeed.
func mountCred(svc agents.CredentialCRUD) http.Handler {
	h := agents.NewCredentialHandler(svc)
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := httpx.WithPrincipal(req.Context(), uuid.New()) // see note below
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	})
	h.ProtectedRoutes(r)
	return r
}

func TestCredentialHandler_CreateReturnsViewWithoutKey(t *testing.T) {
	id := uuid.New()
	svc := &fakeCredSvc{createView: agents.CredentialView{ID: id, Provider: "anthropic", DefaultModel: "claude-opus-4-8"}}
	srv := mountCred(svc)

	body := `{"provider":"anthropic","api_key":"sk-ant-secret","default_model":"claude-opus-4-8"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/businesses/"+uuid.New().String()+"/ai_credentials", strings.NewReader(body))
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d (%s)", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "secret") || strings.Contains(strings.ToLower(rec.Body.String()), "api_key") {
		t.Fatalf("response leaked the api key: %s", rec.Body.String())
	}
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["provider"] != "anthropic" {
		t.Fatalf("want provider anthropic, got %v", got["provider"])
	}
}

func TestCredentialHandler_ListShape(t *testing.T) {
	svc := &fakeCredSvc{listViews: []agents.CredentialView{{ID: uuid.New(), Provider: "openai"}}}
	srv := mountCred(svc)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/businesses/"+uuid.New().String()+"/ai_credentials", nil)
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	var got struct{ Items []map[string]any `json:"items"` }
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Items) != 1 || got.Items[0]["provider"] != "openai" {
		t.Fatalf("want one openai item, got %v", got.Items)
	}
}

func TestCredentialHandler_DeleteNoContent(t *testing.T) {
	id := uuid.New()
	svc := &fakeCredSvc{}
	srv := mountCred(svc)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/businesses/"+uuid.New().String()+"/ai_credentials/"+id.String(), nil)
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("want 204, got %d", rec.Code)
	}
	if svc.gotDeleteID != id {
		t.Fatalf("want delete id %s, got %s", id, svc.gotDeleteID)
	}
}

func TestCredentialHandler_DeleteUnknownIs404(t *testing.T) {
	svc := &fakeCredSvc{deleteErr: errs.ErrNotFound}
	srv := mountCred(svc)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/businesses/"+uuid.New().String()+"/ai_credentials/"+uuid.New().String(), nil)
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", rec.Code)
	}
}

func TestCredentialHandler_DuplicateIs409(t *testing.T) {
	svc := &fakeCredSvc{createErr: errs.ErrConflict}
	srv := mountCred(svc)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/businesses/"+uuid.New().String()+"/ai_credentials",
		strings.NewReader(`{"provider":"anthropic","api_key":"k","default_model":"m"}`))
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d", rec.Code)
	}
}
```

> **Note on the principal helper:** the agent/mcp handler tests already mount a handler behind a context carrying a principal. Open `internal/agents/agent_handler_test.go` (or `mcp_server_handler_test.go`) and copy its exact principal-injection helper — use that instead of the illustrative `httpx.WithPrincipal` above (the real setter name may differ). The interface type is exported as `agents.CredentialCRUD` (defined in Step 2) so the fake can satisfy it.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `export PATH="$HOME/go/bin:$PATH" && go test ./internal/agents/ -run TestCredentialHandler -v`
Expected: FAIL — `agents.NewCredentialHandler` / `agents.CredentialCRUD` undefined.

- [ ] **Step 3: Implement the handler**

Create `internal/agents/credential_handler.go`:

```go
package agents

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/httpx"
)

// CredentialCRUD is the service seam the handler depends on (so unit tests can
// supply a fake). Satisfied by *CredentialService.
type CredentialCRUD interface {
	Create(ctx context.Context, principalID, businessID uuid.UUID, in CreateCredentialInput) (CredentialView, error)
	List(ctx context.Context, principalID, businessID uuid.UUID) ([]CredentialView, error)
	Delete(ctx context.Context, principalID, businessID, credentialID uuid.UUID) error
}

var _ CredentialCRUD = (*CredentialService)(nil)

// CredentialHandler exposes AI-provider credential management over HTTP. Mounted
// behind the agents.configure RequirePermission gate (so a lacking perm /
// invisible business is a no-oracle 404). The API key is write-only: it is
// accepted on create and never returned.
type CredentialHandler struct{ svc CredentialCRUD }

// NewCredentialHandler builds the credential HTTP handler.
func NewCredentialHandler(svc CredentialCRUD) *CredentialHandler { return &CredentialHandler{svc: svc} }

// ProtectedRoutes mounts authenticated credential endpoints under a business.
func (h *CredentialHandler) ProtectedRoutes(r chi.Router) {
	r.Route("/businesses/{id}/ai_credentials", func(r chi.Router) {
		r.Get("/", h.listCredentials)
		r.Post("/", h.createCredential)
		r.Delete("/{credentialID}", h.deleteCredential)
	})
}

func credBusinessID(r *http.Request) (uuid.UUID, error) { return uuid.Parse(chi.URLParam(r, "id")) }
func credPathID(r *http.Request) (uuid.UUID, error)     { return uuid.Parse(chi.URLParam(r, "credentialID")) }

// credentialResp is the non-secret response DTO. CRITICAL: there is no api_key /
// sealed_key_ref field — the secret is write-only.
type credentialResp struct {
	ID                  string `json:"id"`
	BusinessID          string `json:"business_id"`
	Provider            string `json:"provider"`
	BaseURL             string `json:"base_url"`
	DefaultModel        string `json:"default_model"`
	AllowPrivateBaseURL bool   `json:"allow_private_base_url"`
	CreatedAt           string `json:"created_at"`
	UpdatedAt           string `json:"updated_at"`
}

func toCredentialResp(v CredentialView) credentialResp {
	return credentialResp{
		ID:                  v.ID.String(),
		BusinessID:          v.BusinessID.String(),
		Provider:            v.Provider,
		BaseURL:             v.BaseURL,
		DefaultModel:        v.DefaultModel,
		AllowPrivateBaseURL: v.AllowPrivateBaseURL,
		CreatedAt:           v.CreatedAt.Format(http.TimeFormat),
		UpdatedAt:           v.UpdatedAt.Format(http.TimeFormat),
	}
}

func (h *CredentialHandler) listCredentials(w http.ResponseWriter, r *http.Request) {
	pid, ok := httpx.PrincipalFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	bid, err := credBusinessID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	views, err := h.svc.List(r.Context(), pid, bid)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	out := make([]credentialResp, 0, len(views))
	for _, v := range views {
		out = append(out, toCredentialResp(v))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": out})
}

func (h *CredentialHandler) createCredential(w http.ResponseWriter, r *http.Request) {
	pid, ok := httpx.PrincipalFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	bid, err := credBusinessID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	var in struct {
		Provider            string `json:"provider"`
		APIKey              string `json:"api_key"`
		BaseURL             string `json:"base_url"`
		DefaultModel        string `json:"default_model"`
		AllowPrivateBaseURL bool   `json:"allow_private_base_url"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	view, err := h.svc.Create(r.Context(), pid, bid, CreateCredentialInput{
		Provider:            in.Provider,
		APIKey:              in.APIKey,
		BaseURL:             in.BaseURL,
		DefaultModel:        in.DefaultModel,
		AllowPrivateBaseURL: in.AllowPrivateBaseURL,
	})
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, toCredentialResp(view))
}

func (h *CredentialHandler) deleteCredential(w http.ResponseWriter, r *http.Request) {
	pid, ok := httpx.PrincipalFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	bid, err := credBusinessID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	cid, err := credPathID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	if err := h.svc.Delete(r.Context(), pid, bid, cid); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `export PATH="$HOME/go/bin:$PATH" && go build ./... && go test ./internal/agents/ -run TestCredentialHandler -v`
Expected: build exit 0; all `TestCredentialHandler_*` PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/agents/credential_handler.go internal/agents/credential_handler_test.go
git commit -m "feat(agents): credential HTTP handler (create/list/delete, write-only key)"
```

---

## Task 4: Mount the credential handler in main.go

**Files:**
- Modify: `cmd/manyforge/main.go`

**Context (verified):** The handler-aggregate struct (fields ~665-675) holds each handler + its gate; the struct literal (~487-505) assigns them; the route-mount block (~785-822) applies the gate via `pr.Group(func(g){ g.Use(gate); h.X.ProtectedRoutes(g) })`. Reuse the existing `agentsConfigure` gate (same `PermAgentsConfigure`). Mirror the connectors **nil-guard** so the group is skipped when the AI sealer is absent.

- [ ] **Step 1: Construct the credential handler (nil when no sealer)**

In main.go, right after the `credSvc := &agents.CredentialService{DB: database, Sealer: aiSealer}` line (from Task 2), add:

```go
	// Credential HTTP surface is only mounted when the AI sealer is configured —
	// without it, Create would fail to seal. Mirrors the connectors nil-guard.
	var credH *agents.CredentialHandler
	if aiSealer != nil {
		credH = agents.NewCredentialHandler(credSvc)
	}
```

- [ ] **Step 2: Add the aggregate-struct field**

In the handler-aggregate struct definition (near line 666, alongside `agents *agents.Handler`), add:

```go
	// credentials is the AI-provider credential CRUD handler (Phase 1 of the
	// agent-management UI). Nil when MANYFORGE_AI_MASTER_KEY is unset; the mount
	// block guards on nil so the route group is not registered in that case.
	credentials *agents.CredentialHandler
```

(No new gate field — reuse `agentsConfigure`.)

- [ ] **Step 3: Assign the field in the struct literal**

In the aggregate struct literal (near line 493, alongside `agents: agentH,`), add:

```go
		credentials: credH,
```

- [ ] **Step 4: Mount the route group with the agents.configure gate**

In the route-mount section (near the agents group, ~785), add a nil-guarded group that reuses `h.agentsConfigure`:

```go
			// AI-provider credential slice: create/list/delete BYO provider keys,
			// gated on agents.configure (same permission as agent-definition CRUD).
			if h.credentials != nil {
				pr.Group(func(cg chi.Router) {
					cg.Use(h.agentsConfigure)
					h.credentials.ProtectedRoutes(cg)
				})
			}
```

- [ ] **Step 5: Build and run the full test suite**

Run: `export PATH="$HOME/go/bin:$PATH" && go build ./... && make test`
Expected: build exit 0; all tests PASS.

- [ ] **Step 6: Commit**

```bash
git add cmd/manyforge/main.go
git commit -m "feat(agents): mount credential handler under agents.configure"
```

---

## Task 5: Security-regression pin

**Files:**
- Create: `internal/security_regression/ai_credential_response_pin_test.go`

**Context:** Per the security-test discipline, pin the two invariants this feature must never regress: (1) the credential response DTO never serializes the API key / sealed ref; (2) the credential routes stay gated on `agents.configure`. Use a source-level pin (`strings.Contains` over the handler source) so a future refactor that adds a key field or drops the gate fails CI loudly — no DB/network needed. **Recall the memory note:** source-level pins grep literals — keep them in sync if the source changes.

- [ ] **Step 1: Write the pin test**

Create `internal/security_regression/ai_credential_response_pin_test.go`:

```go
package security_regression

import (
	"os"
	"strings"
	"testing"
)

// FINDING: manyforge-1kv — the AI credential HTTP response must never carry the
// API key (sealed_key_ref). Source-level pin: credentialResp must declare no
// json key/secret field, and the route group must stay agents.configure-gated.

func TestAICredentialResponseHasNoKeyField(t *testing.T) {
	src, err := os.ReadFile("../agents/credential_handler.go")
	if err != nil {
		t.Fatalf("read handler: %v", err)
	}
	s := string(src)
	for _, banned := range []string{`json:"api_key"`, `json:"sealed_key_ref"`, `json:"key"`} {
		if strings.Contains(s, banned) {
			t.Fatalf("credential handler must not serialize a secret field, found %q", banned)
		}
	}
}

func TestAICredentialRouteStaysAgentsConfigureGated(t *testing.T) {
	src, err := os.ReadFile("../../cmd/manyforge/main.go")
	if err != nil {
		t.Fatalf("read main: %v", err)
	}
	s := string(src)
	// The credentials group must apply the agentsConfigure gate.
	if !strings.Contains(s, "h.credentials.ProtectedRoutes") {
		t.Fatal("credentials handler is no longer mounted")
	}
	if !strings.Contains(s, "cg.Use(h.agentsConfigure)") {
		t.Fatal("credentials route group is no longer gated on agents.configure")
	}
}
```

(If the mount block used a different group-variable name than `cg`, update the literal here to match — keep the pin and the source in sync.)

- [ ] **Step 2: Run the pin (and the sec-test target)**

Run: `export PATH="$HOME/go/bin:$PATH" && go test ./internal/security_regression/ -run TestAICredential -v && make sec-test`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/security_regression/ai_credential_response_pin_test.go
git commit -m "test(sec): pin AI credential response key-omission + agents.configure gate"
```

---

## Task 6: OpenAPI — credential schemas + paths

**Files:**
- Modify: `specs/003-agent-runtime/contracts/openapi.yaml`

**Context:** Documentation-only (no CI contract test referenced by the spec). Add the credential schemas + the two paths, matching the existing file's indentation and the connector/MCP path style. Read the file first; place the new `paths` entry near the other `/businesses/{id}/...` entries and the schemas under `components.schemas`.

- [ ] **Step 1: Add the schemas**

Under `components.schemas`, add (adjust indentation to match the file):

```yaml
    AICredential:
      type: object
      description: Non-secret view of a stored AI-provider credential. The API key is write-only and never returned.
      properties:
        id: { type: string, format: uuid }
        business_id: { type: string, format: uuid }
        provider: { type: string, enum: [anthropic, openai, ollama, vllm] }
        base_url: { type: string }
        default_model: { type: string }
        allow_private_base_url: { type: boolean }
        created_at: { type: string }
        updated_at: { type: string }
      required: [id, business_id, provider, default_model, allow_private_base_url]
    CreateAICredentialRequest:
      type: object
      properties:
        provider: { type: string, enum: [anthropic, openai, ollama, vllm] }
        api_key: { type: string, description: Write-only. Sealed at rest, never returned. }
        base_url: { type: string, description: Optional. For openai-compatible / self-host endpoints. }
        default_model: { type: string }
        allow_private_base_url: { type: boolean, description: Opt-in to a loopback/RFC1918 base_url (SSRF trust grant). }
      required: [provider, api_key, default_model]
```

- [ ] **Step 2: Add the paths**

Under `paths`, add (gated `agents.configure`; 404 on lacking perm, mirroring the existing security note):

```yaml
  /businesses/{id}/ai_credentials:
    get:
      summary: List AI-provider credentials for a business
      responses:
        '200':
          description: OK
          content:
            application/json:
              schema:
                type: object
                properties:
                  items: { type: array, items: { $ref: '#/components/schemas/AICredential' } }
        '404': { description: Not found / not permitted }
    post:
      summary: Create an AI-provider credential
      requestBody:
        required: true
        content:
          application/json:
            schema: { $ref: '#/components/schemas/CreateAICredentialRequest' }
      responses:
        '201':
          description: Created
          content:
            application/json:
              schema: { $ref: '#/components/schemas/AICredential' }
        '400': { description: Validation / SSRF rejection }
        '409': { description: A credential for that provider already exists }
        '404': { description: Not found / not permitted }
  /businesses/{id}/ai_credentials/{credentialID}:
    delete:
      summary: Delete an AI-provider credential
      responses:
        '204': { description: Deleted }
        '404': { description: Not found / not permitted }
```

- [ ] **Step 3: Validate YAML + commit**

Run: `export PATH="$HOME/go/bin:$PATH" && python3 -c "import yaml,sys; yaml.safe_load(open('specs/003-agent-runtime/contracts/openapi.yaml')); print('ok')"`
Expected: `ok`.

```bash
git add specs/003-agent-runtime/contracts/openapi.yaml
git commit -m "docs(openapi): AI credential schemas + paths"
```

---

## Task 7: Frontend service — `core/ai-credentials.service.ts`

**Files:**
- Create: `web/src/app/core/ai-credentials.service.ts`

**Context (verified):** Services live in `core/` (NOT `pages/`). `businessId` is passed as an argument. Base URL `/api/v1/businesses/${businessId}/ai_credentials`. Use `@Injectable({ providedIn: 'root' })`, `inject(HttpClient)`, RxJS `Observable` at the HTTP boundary. No key on read.

- [ ] **Step 1: Write the service**

Create `web/src/app/core/ai-credentials.service.ts`:

```ts
import { HttpClient } from '@angular/common/http';
import { Injectable, inject } from '@angular/core';
import { Observable } from 'rxjs';

export type AIProvider = 'anthropic' | 'openai' | 'ollama' | 'vllm';

// Read shape: no api_key — the secret is write-only.
export interface AICredential {
  id: string;
  business_id: string;
  provider: AIProvider;
  base_url: string;
  default_model: string;
  allow_private_base_url: boolean;
  created_at: string;
  updated_at: string;
}

export interface CreateAICredentialBody {
  provider: AIProvider;
  api_key: string;
  default_model: string;
  base_url?: string;
  allow_private_base_url?: boolean;
}

// AICredentialsService talks to the agents.configure-gated credential API.
@Injectable({ providedIn: 'root' })
export class AICredentialsService {
  private http = inject(HttpClient);

  private base(businessId: string): string {
    return `/api/v1/businesses/${businessId}/ai_credentials`;
  }

  list(businessId: string): Observable<{ items: AICredential[] }> {
    return this.http.get<{ items: AICredential[] }>(this.base(businessId));
  }
  create(businessId: string, body: CreateAICredentialBody): Observable<AICredential> {
    return this.http.post<AICredential>(this.base(businessId), body);
  }
  remove(businessId: string, id: string): Observable<void> {
    return this.http.delete<void>(`${this.base(businessId)}/${id}`);
  }
}
```

- [ ] **Step 2: Verify it compiles (typecheck via build)**

Run: `cd web && npm run build`
Expected: build succeeds (the new service is tree-shaken out if unused, but must typecheck).

- [ ] **Step 3: Commit**

```bash
git add web/src/app/core/ai-credentials.service.ts
git commit -m "feat(web): AICredentialsService (list/create/remove)"
```

---

## Task 8: Frontend — credential form component

**Files:**
- Create: `web/src/app/pages/credentials/ai/credential-form.ts`
- Test: `web/src/app/pages/credentials/ai/credential-form.spec.ts`

**Context (verified):** Mirror `connector-form.ts`: standalone, `FormsModule`, `@Input`/`@Output`, plain class fields with `[(ngModel)]`, `submitting`/`error` signals, the `describe()` error-mapping block verbatim, write-only password field. `base_url` + `allow_private_base_url` are **always visible** (design decision #17707) with helper text.

- [ ] **Step 1: Write the failing unit spec**

Create `web/src/app/pages/credentials/ai/credential-form.spec.ts`. Mirror `connector-form.spec.ts` (open it for the exact `TestBed`/`provideHttpClientTesting` setup). The spec asserts the emitted create payload:

```ts
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { provideHttpClient } from '@angular/common/http';
import { provideHttpClientTesting, HttpTestingController } from '@angular/common/http/testing';
import { CredentialFormComponent } from './credential-form';

describe('CredentialFormComponent', () => {
  let fixture: ComponentFixture<CredentialFormComponent>;
  let http: HttpTestingController;

  beforeEach(() => {
    TestBed.configureTestingModule({
      imports: [CredentialFormComponent],
      providers: [provideHttpClient(), provideHttpClientTesting()],
    });
    fixture = TestBed.createComponent(CredentialFormComponent);
    fixture.componentInstance.businessId = 'b1';
    fixture.detectChanges();
    http = TestBed.inject(HttpTestingController);
  });

  it('emits a create payload with provider, api_key, default_model', () => {
    const c = fixture.componentInstance;
    c.provider.set('anthropic');
    c.apiKey = 'sk-ant-xyz';
    c.defaultModel = 'claude-opus-4-8';
    let saved = false;
    c.saved.subscribe(() => (saved = true));

    c.submit();

    const req = http.expectOne('/api/v1/businesses/b1/ai_credentials');
    expect(req.request.method).toBe('POST');
    expect(req.request.body).toEqual(
      jasmine.objectContaining({ provider: 'anthropic', api_key: 'sk-ant-xyz', default_model: 'claude-opus-4-8' }),
    );
    req.flush({
      id: 'cred1', business_id: 'b1', provider: 'anthropic', base_url: '', default_model: 'claude-opus-4-8',
      allow_private_base_url: false, created_at: '', updated_at: '',
    });
    expect(saved).toBeTrue();
  });

  it('maps a 400 to a "Rejected:" message', () => {
    const c = fixture.componentInstance;
    c.provider.set('openai');
    c.apiKey = 'k';
    c.defaultModel = 'gpt-5';
    c.submit();
    const req = http.expectOne('/api/v1/businesses/b1/ai_credentials');
    req.flush({ message: 'base_url not allowed' }, { status: 400, statusText: 'Bad Request' });
    expect(c.error()).toContain('Rejected: base_url not allowed');
  });
});
```

- [ ] **Step 2: Run the spec to verify it fails**

Run: `cd web && npm test`
Expected: FAIL — `./credential-form` cannot be resolved (component not created yet).

- [ ] **Step 3: Implement the form component**

Create `web/src/app/pages/credentials/ai/credential-form.ts`:

```ts
import { HttpErrorResponse } from '@angular/common/http';
import { Component, EventEmitter, Input, Output, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { AICredential, AICredentialsService, AIProvider } from '../../../core/ai-credentials.service';

@Component({
  selector: 'app-credential-form',
  imports: [FormsModule],
  template: `
    <form class="mf-card mf-form" (ngSubmit)="submit()" data-testid="credential-form">
      <div class="mf-field">
        <label for="cred-provider">Provider</label>
        <select id="cred-provider" class="mf-select" data-testid="cred-provider"
                [ngModel]="provider()" (ngModelChange)="provider.set($event)" name="provider">
          <option value="anthropic">Anthropic</option>
          <option value="openai">OpenAI</option>
          <option value="ollama">Ollama (self-host)</option>
          <option value="vllm">vLLM (self-host)</option>
        </select>
      </div>

      <div class="mf-field">
        <label for="cred-key">API key</label>
        <input id="cred-key" class="mf-input" type="password" autocomplete="off"
               data-testid="cred-api-key" name="api_key"
               [(ngModel)]="apiKey" placeholder="••••••••" />
      </div>

      <div class="mf-field">
        <label for="cred-model">Default model</label>
        <input id="cred-model" class="mf-input" type="text" data-testid="cred-default-model"
               name="default_model" [(ngModel)]="defaultModel" placeholder="e.g. claude-opus-4-8" />
      </div>

      <div class="mf-field">
        <label for="cred-base-url">Base URL <span class="mf-hint">(optional)</span></label>
        <input id="cred-base-url" class="mf-input" type="text" data-testid="cred-base-url"
               name="base_url" [(ngModel)]="baseUrl" placeholder="https://… (openai-compatible / self-host only)" />
        <small class="mf-hint">Only needed for OpenAI-compatible or self-hosted (Ollama/vLLM) endpoints. Leave blank for the provider default.</small>
      </div>

      <label class="mf-check" data-testid="cred-allow-private-wrap">
        <input type="checkbox" data-testid="cred-allow-private" name="allow_private_base_url"
               [(ngModel)]="allowPrivateBaseUrl" />
        Allow a private / loopback base URL (self-host only)
      </label>

      @if (error()) {
        <p class="mf-err" data-testid="credential-form-error">{{ error() }}</p>
      }

      <div class="mf-form-actions">
        <button type="button" class="mf-btn mf-btn-ghost mf-btn-sm" (click)="cancelled.emit()">Cancel</button>
        <button type="submit" class="mf-btn mf-btn-primary mf-btn-sm" data-testid="credential-form-submit"
                [disabled]="submitting() || !valid()">
          {{ submitting() ? 'Saving…' : 'Add credential' }}
        </button>
      </div>
    </form>
  `,
})
export class CredentialFormComponent {
  @Input() businessId = '';
  @Output() saved = new EventEmitter<AICredential>();
  @Output() cancelled = new EventEmitter<void>();

  private api = inject(AICredentialsService);

  provider = signal<AIProvider>('anthropic');
  apiKey = '';
  defaultModel = '';
  baseUrl = '';
  allowPrivateBaseUrl = false;

  submitting = signal(false);
  error = signal('');

  valid(): boolean {
    return this.apiKey.trim().length > 0 && this.defaultModel.trim().length > 0;
  }

  submit(): void {
    if (this.submitting() || !this.valid()) return;
    this.submitting.set(true);
    this.error.set('');
    this.api
      .create(this.businessId, {
        provider: this.provider(),
        api_key: this.apiKey,
        default_model: this.defaultModel.trim(),
        base_url: this.baseUrl.trim() || undefined,
        allow_private_base_url: this.allowPrivateBaseUrl,
      })
      .subscribe({
        next: (c) => {
          this.reset();
          this.submitting.set(false);
          this.saved.emit(c);
        },
        error: (e: HttpErrorResponse) => {
          this.submitting.set(false);
          this.error.set(this.describe(e));
        },
      });
  }

  private reset(): void {
    this.apiKey = '';
    this.defaultModel = '';
    this.baseUrl = '';
    this.allowPrivateBaseUrl = false;
    this.provider.set('anthropic');
  }

  private describe(e: HttpErrorResponse): string {
    if (e.status === 400) {
      const msg = (e.error as { message?: string } | null)?.message;
      return msg ? `Rejected: ${msg}` : 'Rejected. Check the values and try again.';
    }
    if (e.status === 409) return 'A credential for that provider already exists.';
    if (e.status === 403 || e.status === 404) return "You don't have access to do that.";
    return 'Could not save. Please try again.';
  }
}
```

- [ ] **Step 4: Run the spec to verify it passes**

Run: `cd web && npm test`
Expected: `CredentialFormComponent` specs PASS.

- [ ] **Step 5: Commit**

```bash
git add web/src/app/pages/credentials/ai/credential-form.ts web/src/app/pages/credentials/ai/credential-form.spec.ts
git commit -m "feat(web): AI credential create form"
```

---

## Task 9: Frontend — credential list component

**Files:**
- Create: `web/src/app/pages/credentials/ai/list.ts`
- Test: `web/src/app/pages/credentials/ai/list.spec.ts`

**Context (verified):** Mirror `connectors/list.ts`: standalone; injects `BusinessService`, `AICredentialsService`, `CurrentBusinessService`, `ToastService`; seeds the business from `CurrentBusinessService` (`current.businessId() ?? items[0]?.id`, then `current.set(id)`); `reload()` with the in-flight guard (`if (this.businessId() !== biz) return;`); delete-with-confirm via a `confirmDeleteId` signal; an "Add credential" toggle revealing the form. Reuse the shared UI components (`PageHeader`, `EmptyState`, `Spinner`). No 20s poll needed (credentials don't have health that changes) — omit the timer.

- [ ] **Step 1: Write the failing unit spec**

Create `web/src/app/pages/credentials/ai/list.spec.ts` (mirror `connectors/list.spec.ts` setup — open it for the exact `BusinessService` / HTTP mocking style):

```ts
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { provideHttpClient } from '@angular/common/http';
import { provideHttpClientTesting, HttpTestingController } from '@angular/common/http/testing';
import { AICredentialsListComponent } from './list';

describe('AICredentialsListComponent', () => {
  let fixture: ComponentFixture<AICredentialsListComponent>;
  let http: HttpTestingController;

  beforeEach(() => {
    TestBed.configureTestingModule({
      imports: [AICredentialsListComponent],
      providers: [provideHttpClient(), provideHttpClientTesting()],
    });
    fixture = TestBed.createComponent(AICredentialsListComponent);
    fixture.detectChanges();
    http = TestBed.inject(HttpTestingController);
  });

  it('loads businesses then lists credentials for the selected business', () => {
    http.expectOne('/api/v1/businesses').flush({
      items: [{ id: 'b1', parent_id: null, tenant_root_id: 'b1', name: 'Acme', status: 'active', is_tenant_root: true }],
      next_cursor: null,
    });
    const req = http.expectOne('/api/v1/businesses/b1/ai_credentials');
    req.flush({
      items: [{
        id: 'cred1', business_id: 'b1', provider: 'anthropic', base_url: '', default_model: 'claude-opus-4-8',
        allow_private_base_url: false, created_at: '', updated_at: '',
      }],
    });
    expect(fixture.componentInstance.items().length).toBe(1);
    expect(fixture.componentInstance.items()[0].provider).toBe('anthropic');
  });
});
```

- [ ] **Step 2: Run the spec to verify it fails**

Run: `cd web && npm test`
Expected: FAIL — `./list` cannot resolve (component not created).

- [ ] **Step 3: Implement the list component**

Create `web/src/app/pages/credentials/ai/list.ts`:

```ts
import { HttpErrorResponse } from '@angular/common/http';
import { Component, OnInit, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { AICredential, AICredentialsService } from '../../../core/ai-credentials.service';
import { BusinessService } from '../../../core/business.service';
import { CurrentBusinessService } from '../../../core/current-business.service';
import { Business } from '../../../core/tree';
import { EmptyState } from '../../../ui/empty-state/empty-state';
import { PageHeader } from '../../../ui/page-header/page-header';
import { Spinner } from '../../../ui/spinner/spinner';
import { ToastService } from '../../../ui/toast/toast.service';
import { CredentialFormComponent } from './credential-form';

@Component({
  selector: 'app-ai-credentials-list',
  imports: [FormsModule, PageHeader, EmptyState, Spinner, CredentialFormComponent],
  template: `
    <mf-page-header title="AI Credentials" subtitle="Per-business provider keys for your agents">
      @if (loading()) { <mf-spinner /> }
    </mf-page-header>

    <div class="mf-filters">
      <div class="mf-field" style="flex:1 1 220px">
        <label for="cred-biz-select">Business</label>
        <select id="cred-biz-select" class="mf-select" data-testid="business-select"
                [ngModel]="businessId()" (ngModelChange)="selectBusiness($event)" name="biz">
          <option value="" disabled>Choose a business…</option>
          @for (b of businesses(); track b.id) {
            <option [value]="b.id">{{ b.is_tenant_root ? b.name + ' (master)' : b.name }}</option>
          }
        </select>
      </div>
      <div style="display:flex;align-items:flex-end">
        <button class="mf-btn mf-btn-primary mf-btn-sm" data-testid="credential-add-toggle"
                (click)="showAdd.set(!showAdd())" [disabled]="!businessId()">
          {{ showAdd() ? 'Close' : 'Add credential' }}
        </button>
      </div>
    </div>

    @if (showAdd() && businessId()) {
      <app-credential-form [businessId]="businessId()" (saved)="onCreated()" (cancelled)="showAdd.set(false)" />
    }

    @if (error()) { <p class="mf-err" data-testid="credentials-error">{{ error() }}</p> }

    @if (!loading() && items().length === 0 && businessId()) {
      <mf-empty-state message="No credentials yet. Add one to let an agent call a provider." />
    }

    @if (items().length > 0) {
      <div class="mf-card">
        <table class="mf-table">
          <thead>
            <tr class="mf-tr">
              <th class="mf-th">Provider</th>
              <th class="mf-th">Default model</th>
              <th class="mf-th">Base URL</th>
              <th class="mf-th">Private URL</th>
              <th class="mf-th"></th>
            </tr>
          </thead>
          <tbody>
            @for (c of items(); track c.id) {
              <tr class="mf-tr" data-testid="credential-row">
                <td data-testid="credential-provider">{{ c.provider }}</td>
                <td>{{ c.default_model }}</td>
                <td>{{ c.base_url || '—' }}</td>
                <td>{{ c.allow_private_base_url ? 'allowed' : '—' }}</td>
                <td style="text-align:right">
                  @if (confirmDeleteId() === c.id) {
                    <span class="mf-err" data-testid="credential-delete-confirm" style="font-size:var(--mf-fs-xs)">
                      Delete {{ c.provider }} credential?
                    </span>
                    <button class="mf-btn mf-btn-ghost mf-btn-sm" data-testid="credential-delete-no"
                            (click)="confirmDeleteId.set('')">Cancel</button>
                    <button class="mf-btn mf-btn-danger mf-btn-sm" data-testid="credential-delete-yes"
                            (click)="remove(c)">Delete</button>
                  } @else {
                    <button class="mf-btn mf-btn-danger mf-btn-sm" data-testid="credential-delete"
                            (click)="confirmDeleteId.set(c.id)">Delete</button>
                  }
                </td>
              </tr>
            }
          </tbody>
        </table>
      </div>
    }
  `,
})
export class AICredentialsListComponent implements OnInit {
  private bizApi = inject(BusinessService);
  private api = inject(AICredentialsService);
  private current = inject(CurrentBusinessService);
  private toast = inject(ToastService);

  businesses = signal<Business[]>([]);
  businessId = signal<string>('');
  items = signal<AICredential[]>([]);
  loading = signal(false);
  error = signal('');
  showAdd = signal(false);
  confirmDeleteId = signal<string>('');

  ngOnInit(): void {
    this.bizApi.list().subscribe({
      next: (r) => {
        const items = r.items ?? [];
        this.businesses.set(items);
        const id = this.current.businessId() ?? items[0]?.id;
        if (id) {
          this.businessId.set(id);
          this.current.set(id);
          this.reload();
        }
      },
      error: () => this.error.set('Could not load businesses'),
    });
  }

  selectBusiness(id: string): void {
    this.businessId.set(id);
    this.current.set(id);
    this.confirmDeleteId.set('');
    this.showAdd.set(false);
    this.reload();
  }

  reload(): void {
    if (!this.businessId()) return;
    const biz = this.businessId();
    this.loading.set(true);
    this.api.list(biz).subscribe({
      next: (r) => {
        if (this.businessId() !== biz) return;
        this.items.set(r.items ?? []);
        this.error.set('');
        this.loading.set(false);
      },
      error: () => {
        if (this.businessId() !== biz) return;
        this.items.set([]);
        this.error.set('Could not load credentials');
        this.loading.set(false);
      },
    });
  }

  onCreated(): void {
    this.showAdd.set(false);
    this.toast.success('Credential added');
    this.reload();
  }

  remove(c: AICredential): void {
    this.api.remove(this.businessId(), c.id).subscribe({
      next: () => {
        this.items.update((xs) => xs.filter((x) => x.id !== c.id));
        this.confirmDeleteId.set('');
        this.toast.success('Credential deleted');
      },
      error: (e: HttpErrorResponse) => {
        this.confirmDeleteId.set('');
        this.toast.error(e.status === 404 ? 'Not found' : 'Delete failed');
      },
    });
  }
}
```

> **Before implementing**, open `web/src/app/ui/page-header/page-header.ts`, `empty-state/empty-state.ts`, `spinner/spinner.ts`, and `toast/toast.service.ts` to confirm the exact selectors / `@Input` names / `ToastService` method names (`success`/`error`). The connectors list uses `<mf-page-header>`, `<mf-empty-state>`, `<mf-spinner>`, `toast.success(...)`, `toast.error(...)` — match whatever those files actually export.

- [ ] **Step 4: Run the spec to verify it passes**

Run: `cd web && npm test`
Expected: `AICredentialsListComponent` spec PASS.

- [ ] **Step 5: Commit**

```bash
git add web/src/app/pages/credentials/ai/list.ts web/src/app/pages/credentials/ai/list.spec.ts
git commit -m "feat(web): AI credentials list page"
```

---

## Task 10: Routes, nav, and the connectors → `/credentials/connector` move

**Files:**
- Modify: `web/src/app/app.routes.ts`
- Modify: `web/src/app/ui/nav.ts`
- Modify: `web/src/app/app.ts`
- Modify: `web/e2e/connectors.spec.ts`

**Context (verified):** Routes are a flat array of lazy `loadComponent` entries with `canActivate: [authGuard]`. The literal `/connectors` is hard-coded in `nav.ts` (NAV_ITEMS), `app.ts` (badge `computed`), and the connectors e2e. Moving it means editing all of them in lockstep. There is no nav-group mechanism — add flat entries.

- [ ] **Step 1: Update routes**

In `web/src/app/app.routes.ts`: change the connectors entry's `path` from `'connectors'` to `'credentials/connector'`, and add the AI credentials route + a `/credentials` redirect. Replace the connectors entry and add:

```ts
  {
    path: 'credentials',
    pathMatch: 'full',
    redirectTo: 'credentials/ai',
  },
  {
    path: 'credentials/ai',
    canActivate: [authGuard],
    loadComponent: () => import('./pages/credentials/ai/list').then((m) => m.AICredentialsListComponent),
  },
  {
    path: 'credentials/connector',
    canActivate: [authGuard],
    loadComponent: () => import('./pages/connectors/list').then((m) => m.ConnectorsListComponent),
  },
```

(Remove the old `{ path: 'connectors', … }` entry.)

- [ ] **Step 2: Update nav**

In `web/src/app/ui/nav.ts`, change the Connectors route and add AI Credentials:

```ts
  { label: 'Connectors', route: '/credentials/connector', testid: 'nav-connectors' },
  { label: 'AI Credentials', route: '/credentials/ai', testid: 'nav-ai-credentials' },
```

- [ ] **Step 3: Update the connectors badge route string in app.ts**

In `web/src/app/app.ts`, the badge `computed` keys on `item.route === '/connectors'`. Change it to `'/credentials/connector'`:

```ts
    if (item.route === '/credentials/connector' && hasBiz && degraded > 0) return { ...item, badge: degraded };
```

- [ ] **Step 4: Update the connectors e2e navigations**

In `web/e2e/connectors.spec.ts`, change every `page.goto('/connectors')` to `page.goto('/credentials/connector')`. (Run `grep -n "goto('/connectors')" web/e2e/connectors.spec.ts` to find them all.)

- [ ] **Step 5: Build, unit-test, and run the connectors e2e to confirm the move didn't break it**

Run: `cd web && npm run build && npm test`
Then (dev server on :4300 must be up — it is, per the running stack): `cd web && npm run e2e -- e2e/connectors.spec.ts`
Expected: build + unit PASS; connectors e2e still PASS at the new route.

- [ ] **Step 6: Commit**

```bash
git add web/src/app/app.routes.ts web/src/app/ui/nav.ts web/src/app/app.ts web/e2e/connectors.spec.ts
git commit -m "feat(web): /credentials section — move connectors, add AI credentials route + nav"
```

---

## Task 11: Frontend e2e for the AI credentials page

**Files:**
- Create: `web/e2e/ai-credentials.spec.ts`

**Context (verified):** Mirror `connectors.spec.ts`: an `auth(page)` helper sets `mf_access` in localStorage and mocks `/me` + `/businesses`; each test mocks the per-business endpoint and drives via `getByTestId`. POST flips a `created` flag so the follow-up GET returns the new row.

- [ ] **Step 1: Write the e2e spec**

Create `web/e2e/ai-credentials.spec.ts`:

```ts
import { expect, test } from '@playwright/test';

const profile = { id: '1', email: 'a@b.c', display_name: 'A', email_verified: true, status: 'active' };
const biz = {
  items: [{ id: 'b1', parent_id: null, tenant_root_id: 'b1', name: 'Acme', status: 'active', is_tenant_root: true }],
  next_cursor: null,
};
const cred = {
  id: 'cred1', business_id: 'b1', provider: 'anthropic', base_url: '', default_model: 'claude-opus-4-8',
  allow_private_base_url: false, created_at: '2026-06-15T00:00:00Z', updated_at: '2026-06-15T00:00:00Z',
};

async function auth(page: import('@playwright/test').Page) {
  await page.addInitScript(() => localStorage.setItem('mf_access', 'tok'));
  await page.route('**/api/v1/me', (r) => r.fulfill({ json: profile }));
  await page.route('**/api/v1/businesses', (r) => r.fulfill({ json: biz }));
}

test('ai-credentials: lists configured providers', async ({ page }) => {
  await auth(page);
  await page.route('**/api/v1/businesses/b1/ai_credentials', (r) => r.fulfill({ json: { items: [cred] } }));
  await page.goto('/credentials/ai');
  await expect(page.getByTestId('credential-provider')).toContainText('anthropic');
});

test('ai-credentials: create a credential', async ({ page }) => {
  await auth(page);
  let created = false;
  let body: Record<string, unknown> | null = null;
  await page.route('**/api/v1/businesses/b1/ai_credentials', (r) => {
    if (r.request().method() === 'POST') {
      created = true;
      body = r.request().postDataJSON() as Record<string, unknown>;
      return r.fulfill({ status: 201, json: cred });
    }
    return r.fulfill({ json: { items: created ? [cred] : [] } });
  });
  await page.goto('/credentials/ai');
  await page.getByTestId('credential-add-toggle').click();
  await page.getByTestId('cred-api-key').fill('sk-ant-secret');
  await page.getByTestId('cred-default-model').fill('claude-opus-4-8');
  await page.getByTestId('credential-form-submit').click();
  await expect(page.getByTestId('credential-provider')).toContainText('anthropic');
  expect(body).not.toBeNull();
  expect(body!['api_key']).toBe('sk-ant-secret');
});

test('ai-credentials: delete asks to confirm then removes the row', async ({ page }) => {
  await auth(page);
  await page.route('**/api/v1/businesses/b1/ai_credentials', (r) => r.fulfill({ json: { items: [cred] } }));
  await page.route('**/api/v1/businesses/b1/ai_credentials/cred1', (r) =>
    r.request().method() === 'DELETE' ? r.fulfill({ status: 204, body: '' }) : r.fulfill({ json: cred }),
  );
  await page.goto('/credentials/ai');
  await page.getByTestId('credential-delete').click();
  await expect(page.getByTestId('credential-delete-confirm')).toContainText('Delete anthropic');
  await page.getByTestId('credential-delete-yes').click();
  await expect(page.getByTestId('credential-row')).toHaveCount(0);
});
```

- [ ] **Step 2: Run the e2e**

Run (dev server on :4300 up): `cd web && npm run e2e -- e2e/ai-credentials.spec.ts`
Expected: all three tests PASS.

- [ ] **Step 3: Commit**

```bash
git add web/e2e/ai-credentials.spec.ts
git commit -m "test(web): e2e for AI credentials page"
```

---

## Task 12: Phase 1 verification & PR

- [ ] **Step 1: Full backend gate**

Run: `export PATH="$HOME/go/bin:$PATH" && go build ./... && make test && make sec-test && make lint`
Expected: all exit 0.

- [ ] **Step 2: Full frontend gate**

Run: `cd web && npm run build && npm test && npm run e2e -- e2e/ai-credentials.spec.ts && npm run e2e -- e2e/connectors.spec.ts`
Expected: all PASS.

- [ ] **Step 3: Manual smoke (real stack)**

Ensure `MANYFORGE_AI_MASTER_KEY` is set for the dev backend (air on :8081). With the dev server on :4300 and `mf-dev` DB up, log in (`live-demo@manyforge.test` / `DevPassw0rd!`), open `/credentials/ai`, add an Anthropic credential, confirm it appears, delete it. Confirm `/credentials/connector` still works and the connectors nav badge still updates.

- [ ] **Step 4: Open the PR into master**

```bash
git push -u origin <branch>
gh pr create --base master --title "Provider Credentials UI (agent-management Phase 1)" --body "Implements Phase 1 of docs/superpowers/specs/2026-06-15-agent-management-ui-design.md (bd manyforge-1kv). Adds the AI-provider credential HTTP API + /credentials/ai page, wires the AI sealer, and moves connectors under /credentials/connector."
```

- [ ] **Step 5: Update bd**

Run: `export PATH="$HOME/go/bin:$PATH" && bd update manyforge-1kv --notes "Phase 1 (credentials) PR opened; Phase 2 (agents) plan in docs/superpowers/plans/2026-06-15-agent-ui-phase2-agents.md"` then commit the bd journal (`chore(bd): …`).

---

## Self-Review (completed by plan author)

- **Spec coverage (Phase 1):** credentials create/list/delete ✓ (Tasks 1,3); no-edit-by-design ✓ (no Update added); write-only key ✓ (Tasks 3,5); `agents.configure` server-side gate ✓ (Task 4); nav + routes ✓ (Task 10); business-select + `CurrentBusinessService` seeding ✓ (Task 9); test plan (handler unit, service integration, sec pin, FE unit, e2e) ✓ (Tasks 1,3,5,8,9,11). The **route decision** (unified `/credentials` with `/credentials/{connector,ai}`) is reflected in Task 10.
- **Beyond-spec but required:** the AI sealer was found unwired (Task 2) — without it Create nil-panics; this is a hard prerequisite, not scope creep.
- **Type consistency:** `CredentialView` fields (Task 1) match `credViewFromRow`, `toCredentialResp` (Task 3), and the `AICredential` TS interface (Task 7). `CredentialCRUD` (Task 3) matches the service methods (Task 1) and the fake (Task 3 test). `provider()` is a `signal` in the form (Task 8) consistent with the spec's connector-form mirror.
- **Placeholder scan:** the only deferred specifics are by design — copy the existing testdb harness helpers (Task 1) and confirm shared-UI selectors/`ToastService` names (Task 9) and the principal-injection test helper (Task 3) from named existing files; each names the exact file to read. No "TBD"/"add validation"/"similar to" placeholders.
