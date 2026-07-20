# AI Credential Scoped Config Edit — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a user edit an AI credential's `max_concurrent_lanes` + `default_model` (only) via a scoped, deo.11-safe PATCH, with an Edit affordance in the credentials list.

**Architecture:** A distinctly-named column-scoped SQL update (`UpdateAICredentialConfig`, modeled on `UpdateCodexOAuthTokens`) touching only the two config columns — never `allow_private_base_url`/`base_url`/`sealed_key_ref` — plus a `CredentialService.Update` mirroring `Create`, a `PATCH` handler mirroring the agent PATCH, a new security pin, read-side exposure of `max_concurrent_lanes`, and a compact inline edit form in `list.ts`.

**Tech Stack:** Go + pgx + sqlc v1.27.0 + chi (backend); Angular 21 (standalone, signals, `@if`/`@for`, `inject()`, `[(ngModel)]`); Vitest + Playwright (FE); PostgreSQL.

## Global Constraints

- **deo.11 SSRF invariant (load-bearing):** the new update MUST NOT touch `allow_private_base_url`, `base_url`, or `sealed_key_ref`. The query is named `UpdateAICredentialConfig` (NOT `UpdateAIProviderCredential`, which is a reserved tripwire name). `base_url`/`allow_private_base_url`/`api_key`/`provider`/OAuth columns stay immutable.
- **sqlc:** global sqlc is `v1.27.0` (== repo pin). Regenerate with the repo's normal generate step; the diff must be ONLY the new query's generated code in `internal/platform/db/dbgen/ai.sql.go` (+ possibly `models.go`). If any other generated file churns, STOP — you have the wrong sqlc.
- **No-oracle:** foreign/unknown/invisible credential id → `errs.ErrNotFound` (never distinguish 403/404).
- **Lanes clamp:** any lanes value written goes through the existing `credLanes` helper ([1,16], 0⇒4).
- **Angular 21 idioms** matching the existing `list.ts` (signals, `@if`/`@for`, `inject()`, decorator I/O, `[(ngModel)]`). **FE tests:** `cd web && npx ng test --include="<path>" --watch=false` (NOT `npx vitest run`). **e2e:** `cd web && npx playwright test e2e/ai-credentials.spec.ts` (a dev server on :4300 must be running — the controller manages it).
- **Backend gates (backend tasks):** `go build ./...`, `go test ./internal/agents/...`, `go test -tags integration ./internal/agents/...` (DB round-trip tests), `go test -tags contract ./cmd/...`, `make lint`, `make sec-test`.
- **Commits:** no `Co-Authored-By` trailer. Work on branch `creds-edit-config`.

---

### Task 1: Backend read-side — expose `max_concurrent_lanes`

**Files:**
- Modify: `internal/agents/credential.go` (`CredentialView` + `credViewFromRow`)
- Modify: `internal/agents/credential_handler.go` (`credentialResp` + `toCredentialResp`)
- Modify: `specs/003-agent-runtime/contracts/openapi.yaml` (`AICredential` schema)
- Test: `internal/agents/credential_handler_test.go` (or the existing handler test file)

**Interfaces:**
- Produces: `CredentialView.MaxConcurrentLanes int32`; `credentialResp` JSON field `max_concurrent_lanes`; OpenAPI `AICredential.max_concurrent_lanes`.

- [ ] **Step 1: Write the failing test** — assert a listed/created credential response includes `max_concurrent_lanes`. In the handler test file, add a test that creates a credential (default lanes 4) and asserts the response JSON contains `"max_concurrent_lanes":4`. (Follow the existing handler-test setup in that file; if the tests are integration-tagged, add it there.)

- [ ] **Step 2: Run it to confirm it fails**
Run: `go test ./internal/agents/ -run MaxConcurrentLanes` (or the integration variant)
Expected: FAIL (field absent).

- [ ] **Step 3: Add the field through the read path.**
In `internal/agents/credential.go`, add to the `CredentialView` struct (after `DefaultModel`):
```go
	MaxConcurrentLanes  int32
```
In `credViewFromRow`, set it in the struct literal (after `DefaultModel: row.DefaultModel,`):
```go
		MaxConcurrentLanes:  row.MaxConcurrentLanes,
```
In `internal/agents/credential_handler.go`, add to the `credentialResp` struct a field with tag `json:"max_concurrent_lanes"` (int), and in `toCredentialResp` set it from `v.MaxConcurrentLanes` (cast to int). Follow the exact style of the sibling fields in those two blocks (read them first).
In `specs/003-agent-runtime/contracts/openapi.yaml`, add `max_concurrent_lanes: { type: integer }` to the `AICredential` schema's properties.

- [ ] **Step 4: Run the test + contract**
Run: `go test ./internal/agents/... && go test -tags contract ./cmd/...`
Expected: PASS.

- [ ] **Step 5: Commit**
```bash
git add internal/agents/credential.go internal/agents/credential_handler.go specs/003-agent-runtime/contracts/openapi.yaml internal/agents/*_test.go
git commit -m "feat(creds): expose max_concurrent_lanes on the credential read model"
```

---

### Task 2: SQL scoped update query + sqlc regen + security pin

**Files:**
- Modify: `db/query/ai.sql` (add `UpdateAICredentialConfig`)
- Generated: `internal/platform/db/dbgen/ai.sql.go` (via sqlc regen)
- Create: `internal/security_regression/ai_credential_config_update_pin_test.go`

**Interfaces:**
- Produces: `dbgen.UpdateAICredentialConfig(ctx, dbgen.UpdateAICredentialConfigParams{ID, BusinessID, DefaultModel *string, MaxConcurrentLanes *int32})` returning `dbgen.AiProviderCredential`.

- [ ] **Step 1: Write the failing security pin.**
Create `internal/security_regression/ai_credential_config_update_pin_test.go`:
```go
// Finding: manyforge-deo.11 (sibling) — the scoped config-edit query UpdateAICredentialConfig
// must touch ONLY config columns (default_model, max_concurrent_lanes) and must NEVER reference
// the SSRF trust flag or the sealed key, so a config edit can't reopen the trust surface the
// deo.11 note protects. Source-level pin (no build tag → runs under make test / make sec-test).
package security_regression

import (
	"strings"
	"testing"
)

// aiQuerySlice returns the text of the named query (from its `-- name: X` marker to the next
// `-- name:` marker or EOF), so per-query assertions don't false-match strings elsewhere in the file.
func aiQuerySlice(t *testing.T, sql, name string) string {
	t.Helper()
	marker := "-- name: " + name
	i := strings.Index(sql, marker)
	if i < 0 {
		return ""
	}
	rest := sql[i+len(marker):]
	if j := strings.Index(rest, "-- name:"); j >= 0 {
		return rest[:j]
	}
	return rest
}

func TestPin_UpdateAICredentialConfigIsConfigScoped(t *testing.T) {
	sql := mustReadAIQueries(t) // shared helper in ai_credential_update_pin_test.go
	q := aiQuerySlice(t, sql, "UpdateAICredentialConfig")
	if q == "" {
		t.Skip("no UpdateAICredentialConfig query yet — tripwire armed")
	}
	for _, forbidden := range []string{"allow_private_base_url", "base_url", "sealed_key_ref", "oauth_refresh_token"} {
		if strings.Contains(q, forbidden) {
			t.Errorf("UpdateAICredentialConfig must NOT touch %q — config-only, no trust/secret surface (deo.11)", forbidden)
		}
	}
	// It must be scoped to (id, business_id) — the ownership predicate.
	if !strings.Contains(q, "business_id") {
		t.Error("UpdateAICredentialConfig must be scoped by business_id (ownership predicate)")
	}
}
```

- [ ] **Step 2: Run it — confirm it SKIPS (query absent, tripwire armed).**
Run: `go test ./internal/security_regression/ -run UpdateAICredentialConfigIsConfigScoped -v`
Expected: SKIP ("no UpdateAICredentialConfig query yet").

- [ ] **Step 3: Add the query.**
In `db/query/ai.sql`, add (after the existing `UpdateCodexOAuthTokens` block):
```sql
-- UpdateAICredentialConfig partially updates the two SAFE config columns of a credential
-- (PATCH): COALESCE(narg, col) preserves any field the caller omitted. Scoped to (id,
-- business_id). Deliberately does NOT touch allow_private_base_url / base_url / sealed_key_ref
-- (config-only, no SSRF trust surface — see manyforge-deo.11).
-- name: UpdateAICredentialConfig :one
UPDATE ai_provider_credential
SET default_model        = COALESCE(sqlc.narg('default_model'), default_model),
    max_concurrent_lanes = COALESCE(sqlc.narg('max_concurrent_lanes')::integer, max_concurrent_lanes),
    updated_at           = now()
WHERE id = sqlc.arg('id')::uuid AND business_id = sqlc.arg('business_id')::uuid
RETURNING *;
```

- [ ] **Step 4: Regenerate sqlc.**
Run the repo's generate step (global sqlc is v1.27.0). Verify the diff is ONLY `internal/platform/db/dbgen/ai.sql.go` (new `UpdateAICredentialConfig` + params). If other files churn, STOP.
Run: `go build ./...`
Expected: builds; `dbgen.UpdateAICredentialConfig` exists.

- [ ] **Step 5: Run the pin — now it must PASS.**
Run: `go test ./internal/security_regression/ -run UpdateAICredentialConfig`
Expected: PASS (config-scoped, no forbidden columns).

- [ ] **Step 6: Full sec gate**
Run: `make sec-test`
Expected: PASS (existing deo.11 pins still green — the `UpdateAIProviderCredential` tripwire still SKIPs since we used a different name).

- [ ] **Step 7: Commit**
```bash
git add db/query/ai.sql internal/platform/db/dbgen/ai.sql.go internal/security_regression/ai_credential_config_update_pin_test.go
git commit -m "feat(creds): scoped UpdateAICredentialConfig query (deo.11-safe) + pin"
```

---

### Task 3: Service `Update` + PATCH handler + OpenAPI

**Files:**
- Modify: `internal/agents/credential.go` (`UpdateCredentialInput`, `validateUpdateCredential`, `Update`)
- Modify: `internal/agents/credential_handler.go` (`CredentialCRUD` interface, route, `updateCredential`)
- Modify: `specs/003-agent-runtime/contracts/openapi.yaml` (PATCH path + `UpdateAICredentialRequest`)
- Test: `internal/agents/credential_test.go` (unit) + the integration test file (DB round-trip)

**Interfaces:**
- Consumes: `dbgen.UpdateAICredentialConfig` (Task 2), `credViewFromRow`/`mapCredErr`/`credLanes` (existing), `CredentialView.MaxConcurrentLanes` (Task 1).
- Produces: `CredentialService.Update(ctx, principalID, businessID, credentialID uuid.UUID, in UpdateCredentialInput) (CredentialView, error)`; `PATCH /businesses/{id}/ai_credentials/{credentialID}`.

- [ ] **Step 1: Write the failing tests.**
Unit test in `credential_test.go` (no DB) for `validateUpdateCredential`:
```go
func TestValidateUpdateCredential(t *testing.T) {
	empty := ""
	if err := validateUpdateCredential(UpdateCredentialInput{DefaultModel: &empty}); !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("blank default_model should be ErrValidation, got %v", err)
	}
	if err := validateUpdateCredential(UpdateCredentialInput{}); err != nil {
		t.Fatalf("empty patch is valid (no-op preserve), got %v", err)
	}
}
```
Integration test (DB, `//go:build integration`) mirroring the existing credential integration tests: create a credential (lanes 4), `Update` with `MaxConcurrentLanes: ptr(9)` + `DefaultModel: ptr("gpt-5")`, assert the returned view has lanes 9 + model gpt-5 and `allow_private_base_url` UNCHANGED; `Update` with `MaxConcurrentLanes: ptr(99)` clamps to 16; `Update` on a random uuid → `ErrNotFound`.

- [ ] **Step 2: Run — confirm failure** (`validateUpdateCredential`/`Update` undefined).
Run: `go test ./internal/agents/ -run ValidateUpdateCredential`
Expected: FAIL (undefined).

- [ ] **Step 3: Implement `UpdateCredentialInput` + `validateUpdateCredential` + `Update`.**
In `internal/agents/credential.go`:
```go
// UpdateCredentialInput is a partial (PATCH) config update — nil fields are preserved.
// Only the two SAFE config columns are editable; base_url / allow_private_base_url /
// api_key / provider are immutable (delete+recreate) — see manyforge-deo.11.
type UpdateCredentialInput struct {
	DefaultModel       *string // nil = absent (preserve); "" = invalid (NOT NULL)
	MaxConcurrentLanes *int    // nil = absent (preserve); clamped via credLanes when set
}

func validateUpdateCredential(in UpdateCredentialInput) error {
	if in.DefaultModel != nil && strings.TrimSpace(*in.DefaultModel) == "" {
		return fmt.Errorf("agents: default_model cannot be empty: %w", errs.ErrValidation)
	}
	return nil
}

// Update applies a partial config change (default_model / max_concurrent_lanes). Omitted
// (nil) fields are preserved via COALESCE. No matching (id, business_id) → ErrNotFound (no
// oracle). Deliberately cannot touch the SSRF trust flag or the sealed key (deo.11).
func (s *CredentialService) Update(ctx context.Context, principalID, businessID, credentialID uuid.UUID, in UpdateCredentialInput) (CredentialView, error) {
	if err := validateUpdateCredential(in); err != nil {
		return CredentialView{}, err
	}
	params := dbgen.UpdateAICredentialConfigParams{ID: credentialID, BusinessID: businessID}
	if in.DefaultModel != nil {
		m := strings.TrimSpace(*in.DefaultModel)
		params.DefaultModel = &m
	}
	if in.MaxConcurrentLanes != nil {
		n := credLanes(*in.MaxConcurrentLanes)
		params.MaxConcurrentLanes = &n
	}
	var row dbgen.AiProviderCredential
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		r, qerr := dbgen.New(tx).UpdateAICredentialConfig(ctx, params)
		row = r
		return qerr
	})
	if err != nil {
		return CredentialView{}, mapCredErr(err)
	}
	return credViewFromRow(row), nil
}
```
(Verify the exact generated param field names/types from `ai.sql.go` — `DefaultModel *string`, `MaxConcurrentLanes *int32`. If the generated narg type differs, adapt the pointer construction. `strings` is already imported; add if not.)

- [ ] **Step 4: Add `Update` to the `CredentialCRUD` interface** (`credential_handler.go`), after `Delete`:
```go
	Update(ctx context.Context, principalID, businessID, credentialID uuid.UUID, in UpdateCredentialInput) (CredentialView, error)
```

- [ ] **Step 5: Add the route + handler.**
In `ProtectedRoutes`, after the Delete line:
```go
		r.Patch("/{credentialID}", h.updateCredential)
```
Add the handler (mirror `updateAgent`):
```go
func (h *CredentialHandler) updateCredential(w http.ResponseWriter, r *http.Request) {
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
	// Pointer fields distinguish "absent" from "set" for PATCH semantics.
	var in struct {
		DefaultModel       *string `json:"default_model"`
		MaxConcurrentLanes *int    `json:"max_concurrent_lanes"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	view, err := h.svc.Update(r.Context(), pid, bid, cid, UpdateCredentialInput{
		DefaultModel:       in.DefaultModel,
		MaxConcurrentLanes: in.MaxConcurrentLanes,
	})
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toCredentialResp(view))
}
```
(Confirm the handler receiver/field: `createCredential` uses `h.svc.Create` — use the same `h.svc`.)

- [ ] **Step 6: OpenAPI PATCH.**
In `specs/003-agent-runtime/contracts/openapi.yaml`, under `/businesses/{id}/ai_credentials/{credentialID}` add a `patch:` (mirror the agent PATCH): request `UpdateAICredentialRequest` (`{ default_model?: string, max_concurrent_lanes?: integer }`), responses 200 `AICredential`, 400, 404. Add the `UpdateAICredentialRequest` component schema.

- [ ] **Step 7: Run the gates.**
Run: `go build ./... && go test ./internal/agents/... && go test -tags integration ./internal/agents/... && go test -tags contract ./cmd/... && make lint && make sec-test`
Expected: PASS.

- [ ] **Step 8: Commit**
```bash
git add internal/agents/credential.go internal/agents/credential_handler.go internal/agents/*_test.go specs/003-agent-runtime/contracts/openapi.yaml
git commit -m "feat(creds): scoped PATCH endpoint to edit credential config (lanes + model)"
```

---

### Task 4: FE service `update()`

**Files:**
- Modify: `web/src/app/core/ai-credentials.service.ts`
- Modify: `web/src/app/core/ai-credentials.service.spec.ts`

**Interfaces:**
- Produces: `UpdateAICredentialBody { default_model?: string; max_concurrent_lanes?: number }`; `update(businessId, id, body): Observable<AICredential>`.

- [ ] **Step 1: Write the failing spec.**
Add to `ai-credentials.service.spec.ts`:
```ts
  it('update PATCHes config fields to the credential path', () => {
    let ok = false;
    svc.update('b1', 'cred1', { max_concurrent_lanes: 9, default_model: 'gpt-5' }).subscribe(() => (ok = true));
    const req = http.expectOne('/api/v1/businesses/b1/ai_credentials/cred1');
    expect(req.request.method).toBe('PATCH');
    expect(req.request.body).toEqual({ max_concurrent_lanes: 9, default_model: 'gpt-5' });
    req.flush({ id: 'cred1', business_id: 'b1', provider: 'openai', base_url: '', default_model: 'gpt-5', allow_private_base_url: false, max_concurrent_lanes: 9, created_at: '', updated_at: '' });
    expect(ok).toBe(true);
  });
```

- [ ] **Step 2: Run — confirm failure.**
Run: `cd web && npx ng test --include="src/app/core/ai-credentials.service.spec.ts" --watch=false`
Expected: FAIL (`update` undefined).

- [ ] **Step 3: Implement.** In `ai-credentials.service.ts`, after `remove()`:
```ts
  update(businessId: string, id: string, body: UpdateAICredentialBody): Observable<AICredential> {
    return this.http.patch<AICredential>(`${this.base(businessId)}/${id}`, body);
  }
```
And add the DTO after `CreateAICredentialBody`:
```ts
export interface UpdateAICredentialBody {
  default_model?: string;
  max_concurrent_lanes?: number;
}
```

- [ ] **Step 4: Run — PASS.**
Run: `cd web && npx ng test --include="src/app/core/ai-credentials.service.spec.ts" --watch=false`
Expected: PASS.

- [ ] **Step 5: Commit**
```bash
git add web/src/app/core/ai-credentials.service.ts web/src/app/core/ai-credentials.service.spec.ts
git commit -m "feat(creds): FE service update() for scoped credential config edit"
```

---

### Task 5: FE list — render lanes + inline Edit form

**Files:**
- Modify: `web/src/app/pages/credentials/ai/list.ts`
- Modify: `web/src/app/pages/credentials/ai/list.spec.ts`

**Interfaces:**
- Consumes: `AICredentialsService.update` (Task 4); `AICredential.max_concurrent_lanes` (now emitted, Task 1).

- [ ] **Step 1: Write the failing spec.**
Add to `list.spec.ts` a test: mount with a credential, click `[data-testid="credential-edit"]`, assert an inline form appears prefilled, change the lanes input, click `[data-testid="credential-edit-save"]`, expect a PATCH to `/api/v1/businesses/b1/ai_credentials/cred1`, flush the updated row, and assert the row reflects the new lanes. (Mirror the existing delete-confirm test's interaction style in that file.)

- [ ] **Step 2: Run — confirm failure.**
Run: `cd web && npx ng test --include="src/app/pages/credentials/ai/list.spec.ts" --watch=false`
Expected: FAIL (no edit controls).

- [ ] **Step 3: Implement.** In `list.ts`:
1. Import `UpdateAICredentialBody` from the service (alongside the existing imports).
2. Add signals near `confirmDeleteId`:
```ts
  editId = signal<string>('');
  editModel = '';
  editLanes = 4;
```
3. Add a `max_concurrent_lanes` column. In the header row (`mf-th`) add `<span style="width:90px">Lanes</span>` before the actions span, and in the data row add a cell after the Private-URL span:
```html
            <span style="width:90px;color:var(--mf-text-muted);font-size:var(--mf-fs-sm)" data-testid="credential-lanes">{{ c.max_concurrent_lanes }}</span>
```
4. In the action cell, add an Edit button + inline form. Before the delete controls block:
```html
              @if (editId() === c.id) {
                <input type="text" class="mf-input mf-input-sm" data-testid="credential-edit-model"
                       [(ngModel)]="editModel" name="editModel" style="width:130px" aria-label="Default model" />
                <input type="number" min="1" max="16" class="mf-input mf-input-sm" data-testid="credential-edit-lanes"
                       [(ngModel)]="editLanes" name="editLanes" style="width:64px" aria-label="Max concurrent lanes" />
                <button class="mf-btn mf-btn-primary mf-btn-sm" data-testid="credential-edit-save" (click)="saveEdit(c)">Save</button>
                <button class="mf-btn mf-btn-ghost mf-btn-sm" data-testid="credential-edit-cancel" (click)="editId.set('')">Cancel</button>
              } @else {
                <button class="mf-btn mf-btn-ghost mf-btn-sm" data-testid="credential-edit"
                        [attr.aria-label]="'Edit ' + c.provider" (click)="startEdit(c)">Edit</button>
              }
```
5. Add the methods (near `remove`):
```ts
  startEdit(c: AICredential): void {
    this.confirmDeleteId.set('');
    this.editModel = c.default_model;
    this.editLanes = c.max_concurrent_lanes ?? 4;
    this.editId.set(c.id);
  }

  saveEdit(c: AICredential): void {
    const body: UpdateAICredentialBody = {
      default_model: this.editModel.trim(),
      max_concurrent_lanes: Math.min(16, Math.max(1, Math.round(Number(this.editLanes) || 4))),
    };
    this.api.update(this.businessId(), c.id, body).subscribe({
      next: (updated) => {
        this.items.update((xs) => xs.map((x) => (x.id === c.id ? updated : x)));
        this.editId.set('');
        this.toast.success('Credential updated');
      },
      error: (e: HttpErrorResponse) => {
        this.toast.error(e.status === 404 ? 'Not found' : e.status === 400 ? 'Invalid values' : 'Update failed');
      },
    });
  }
```
6. Widen the action span if needed (the edit form + Edit button share the 220px cell; bump to `width:300px` on both the header spacer and the action span, and the header `Lanes` addition keeps columns aligned).

- [ ] **Step 4: Run — PASS.**
Run: `cd web && npx ng test --include="src/app/pages/credentials/ai/list.spec.ts" --watch=false`
Expected: PASS (new + existing tests).

- [ ] **Step 5: Commit**
```bash
git add web/src/app/pages/credentials/ai/list.ts web/src/app/pages/credentials/ai/list.spec.ts
git commit -m "feat(creds): inline Edit for credential lanes + model in the list"
```

---

### Task 6: End-to-end coverage

**Files:**
- Modify: `web/e2e/ai-credentials.spec.ts`

- [ ] **Step 1: Add the e2e test** (reuse the existing `auth(page)` helper; the `**/api/**` catch-all is already first). Mock the credential list to return one credential (lanes 4), and a PATCH route that flips a closed-over `edited` flag and returns lanes 9. Drive it:
```ts
test('ai-credentials: edit a credential concurrency limit', async ({ page }) => {
  await auth(page);
  const base = { id: 'cred1', business_id: 'b1', provider: 'openai', base_url: 'https://api.openai.com/v1', default_model: 'gpt-4o', allow_private_base_url: false, max_concurrent_lanes: 4, created_at: '2026-07-01T00:00:00Z', updated_at: '2026-07-01T00:00:00Z' };
  let edited = false;
  await page.route('**/api/v1/businesses/b1/ai_credentials', (r) =>
    r.fulfill({ json: { items: [edited ? { ...base, max_concurrent_lanes: 9 } : base] } }),
  );
  await page.route('**/api/v1/businesses/b1/ai_credentials/cred1', (r) => {
    if (r.request().method() === 'PATCH') { edited = true; return r.fulfill({ json: { ...base, max_concurrent_lanes: 9 } }); }
    return r.fallback();
  });
  await page.goto('/credentials/ai');
  await expect(page.getByTestId('credential-lanes')).toHaveText('4');
  await page.getByTestId('credential-edit').click();
  await page.getByTestId('credential-edit-lanes').fill('9');
  await page.getByTestId('credential-edit-save').click();
  await expect(page.getByTestId('credential-lanes')).toHaveText('9');
});
```

- [ ] **Step 2: Run in a real browser** (dev server on :4300 running):
Run: `cd web && npx playwright test e2e/ai-credentials.spec.ts`
Expected: all pass incl. the new edit test. If it reveals a real bug, STOP and report.

- [ ] **Step 3: Commit**
```bash
git add web/e2e/ai-credentials.spec.ts
git commit -m "test(creds): e2e for editing a credential concurrency limit"
```

---

## Self-Review

**Spec coverage:** read-side exposure (T1) → so the UI can show/prefill lanes; scoped query + pin (T2) → deo.11-safe; service+handler+openapi (T3); FE service (T4); FE list edit (T5); e2e (T6). All spec sections covered. ✓

**Placeholder scan:** the T1 `credentialResp`/`toCredentialResp` and the T5 column-width tweak reference reading the exact current file — bounded, with the exact field/attr to add given. No TBDs. The generated param types in T3 are flagged to verify against `ai.sql.go`.

**Type consistency:** `UpdateAICredentialConfig` query name matches the pin (T2) and the service call (T3). `MaxConcurrentLanes int32` on `CredentialView` (T1) matches `row.MaxConcurrentLanes` and is consumed by `credentialResp`. `UpdateAICredentialBody`/`update()` (T4) URL + PATCH match the handler route (T3). `credential-edit*` testids consistent across T5 + T6.

**deo.11 safety:** the query name is `UpdateAICredentialConfig` (not the reserved tripwire name); the pin asserts omission of the trust/secret columns on the sliced query text; `base_url`/`allow_private_base_url`/`api_key` never appear in the update path.

## Pointers
- Spec: `docs/superpowers/specs/2026-07-20-ai-credential-edit-config-design.md`. Issue: `manyforge-bxev`.
- Precedents: `UpdateCodexOAuthTokens` / `InsertAIProviderCredential` (`db/query/ai.sql`), `UpdateAgent` (`db/query/agent.sql`, `agent.go`, `agent_handler.go`), the deo.11 pin (`internal/security_regression/ai_credential_update_pin_test.go`), `credLanes`/`credViewFromRow`/`mapCredErr` (`internal/agents/credential.go`).
