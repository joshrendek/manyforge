# AI Provider Credential — Scoped Config Edit

**Date:** 2026-07-20
**Issue:** manyforge-bxev
**Branch:** creds-edit-config

## Goal

Let a user edit an existing AI provider credential's **`max_concurrent_lanes`** and
**`default_model`** without deleting and recreating it. Today the AI-credentials page
(`web/src/app/pages/credentials/ai/list.ts`) supports only add + delete, so adjusting the
review-lane concurrency cap forces a delete+recreate — which for `openai_codex` means a full
"Sign in with ChatGPT" re-auth, and for key-based providers means re-entering the API key.

## Background — why credentials are immutable today (the load-bearing constraint)

Credentials are **create/list/delete only by design**. `db/query/ai.sql` carries an explicit note:

```
-- NOTE (manyforge-deo.11): there is intentionally NO UpdateAIProviderCredential query
-- yet. When one is added it MUST include allow_private_base_url (and the service must
-- re-validate it via validateBaseURL and re-audit the trust grant) — otherwise an update
-- built from a partial body silently zeros the SSRF trust flag, demoting a trusted
-- self-host credential to the locked-down dialer (or leaving a stale trust the operator
-- believes was revoked). Pinned in
-- internal/security_regression/ai_credential_update_pin_test.go.
```

This is a **deliberate SSRF posture**, not a gap. The sensitive column is `allow_private_base_url`
— a per-credential trust flag that opts the outbound dialer out of the SSRF guard (permits
loopback / RFC1918 `base_url` for self-hosted Ollama/vLLM). A naive partial-body PATCH would
default that flag to `false` and silently revoke a granted trust (or leave a stale one).
`TestPin_UpdateAICredentialCarriesTrustFlag` arms a tripwire: it `t.Skip`s while no
`UpdateAIProviderCredential` query exists and `t.Error`s the moment one lands without
`allow_private_base_url`.

**Precedent for the safe path:** Increment 2's `UpdateCodexOAuthTokens` (`db/query/ai.sql`) is a
distinctly-named, column-scoped UPDATE on this same table that *deliberately* does not touch the
trust flag, with a comment saying so. We follow that shape exactly.

## Scope

**In:** edit `max_concurrent_lanes` + `default_model` only, via a distinctly-named scoped update.
**Out (stay immutable — delete+recreate):** `base_url`, `allow_private_base_url`, `api_key`
(`sealed_key_ref`), `provider`, and all `openai_codex` OAuth columns. Editing these would reopen the
`deo.11` SSRF trust surface and is explicitly not part of this feature.

## Design

### 1. Backend — scoped, deo.11-safe update

**Query** (`db/query/ai.sql`) — new, distinctly named so the `UpdateAIProviderCredential` tripwire
stays green, touching only the two config columns:

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

**Service** (`internal/agents/credential.go`) — `Update` mirroring `Create`:

```go
type UpdateCredentialInput struct {
    DefaultModel       *string // nil = absent (preserve)
    MaxConcurrentLanes *int    // nil = absent (preserve)
}

func (s *CredentialService) Update(ctx context.Context, principalID, businessID, credentialID uuid.UUID, in UpdateCredentialInput) (CredentialView, error)
```

- Runs inside `s.DB.WithPrincipal(ctx, principalID, ...)` (RLS tx) with the explicit
  `business_id` predicate in the query (defense-in-depth, no-oracle).
- `MaxConcurrentLanes`: when set, clamp through the existing `credLanes` helper ([1,16], 0⇒4)
  before passing as narg; when nil, pass narg NULL (preserve).
- `DefaultModel`: when set, reject blank after trim (`default_model` is `NOT NULL` and required)
  → `errs.ErrValidation`; when nil, pass narg NULL (preserve).
- Rows-affected 0 (foreign or unknown id, or invisible under RLS) → `errs.ErrNotFound`. Same
  not-found shape for "not yours" and "doesn't exist" (no existence oracle).
- Errors wrapped via `mapCredErr`; returns the refreshed `CredentialView`.
- Added to the `CredentialCRUD` interface (`credential_handler.go`).

**Handler** (`internal/agents/credential_handler.go`) — `r.Patch("/{credentialID}", h.updateCredential)`
under the `agents.configure` gate, mirroring the existing agent PATCH:

```go
// Pointer fields distinguish "absent" from "set" for PATCH semantics.
type updateCredentialBody struct {
    DefaultModel       *string `json:"default_model"`
    MaxConcurrentLanes *int    `json:"max_concurrent_lanes"`
}
```

`pid` from `httpx.PrincipalFromContext`, `bid`/`cid` from URL params (parse failure → `ErrNotFound`).
Typed-error mapping: `ErrValidation`→400 (safe message), `ErrNotFound`→404, else 500.

### 2. Security regression pin

New source-level pin (alongside `internal/security_regression/ai_credential_update_pin_test.go`):
assert the `UpdateAICredentialConfig` query text does **not** contain `allow_private_base_url`,
`base_url`, or `sealed_key_ref`. This arms a tripwire so a future edit can't quietly widen the
scoped config update into the SSRF trust surface. (Runs under `make test`/`make sec-test`, no build tag.)

### 3. OpenAPI

Add `patch:` on `/businesses/{id}/ai_credentials/{credentialID}` in
`specs/003-agent-runtime/contracts/openapi.yaml` (mirroring the agent PATCH at that file's
`/businesses/{id}/agents/{agentID}`) + an `UpdateAICredentialRequest` schema
(`{ default_model?, max_concurrent_lanes? }`) → 200 `AICredential` / 400 / 404. Covered by the
existing `go test -tags contract ./cmd/...` drift check.

### 4. Frontend

**Service** (`web/src/app/core/ai-credentials.service.ts`): add after `remove()`:

```ts
export interface UpdateAICredentialBody {
  default_model?: string;
  max_concurrent_lanes?: number;
}
update(businessId: string, id: string, body: UpdateAICredentialBody): Observable<AICredential> {
  return this.http.patch<AICredential>(`${this.base(businessId)}/${id}`, body);
}
```

(`AICredential` already carries `max_concurrent_lanes` and `default_model` — no read-shape change.)

**List** (`web/src/app/pages/credentials/ai/list.ts`):
- Render `max_concurrent_lanes` in the row (currently not shown) so the current value is visible.
- Add an **Edit** button in the action cell (alongside Delete / codex Reconnect), gated by an
  `editId` signal like `showAdd`/`confirmDeleteId`. Clicking opens a compact inline form for that
  row: a prefilled `default_model` **text input** + a `max_concurrent_lanes` **number input**
  (min 1, max 16), with Save / Cancel. Save calls `update()`, then updates the row signal (or
  reloads) and toasts. Errors mapped like the create form's `describe()`.
- **Deliberate simplification:** the edit form uses a prefilled text input for `default_model`
  rather than the create form's catalog `<select>`, to avoid pulling provider-catalog loading into
  `list.ts`. A catalog dropdown can be a follow-up if desired.
- For `openai_codex` rows this changes model/lanes **without** re-auth — the scoped query never
  touches the OAuth token/expiry columns.

## Testing plan

Per repo policy: automated at every layer, real-browser verification for the visible UI, then codify.

- **Backend (Go):** `CredentialService.Update` unit tests — lanes clamp ([1,16], 0⇒4), omitted
  field preserved (COALESCE), blank `default_model` rejected (`ErrValidation`), foreign/unknown id →
  `ErrNotFound` (no-oracle); the new security pin (query is config-scoped); `go test -tags contract
  ./cmd/...` for the PATCH path.
- **FE (Vitest):** `ai-credentials.service.spec` (PATCH hits the right URL + body shape);
  `list.spec` (Edit opens the inline form prefilled → Save PATCHes → row reflects the new values;
  non-edited rows unchanged).
- **E2e (Playwright, `web/e2e/ai-credentials.spec.ts`):** in a real browser, open Edit on a
  credential, change `max_concurrent_lanes`, Save, assert the PATCH fired and the row shows the new
  value. Reuse the existing `auth()` helper (the `**/api/**` catch-all is already first).

## Pointers

- Issue: `manyforge-bxev`.
- deo.11 rationale + tripwire: `db/query/ai.sql` note, `internal/security_regression/ai_credential_update_pin_test.go`; design `docs/superpowers/specs/2026-06-15-agent-management-ui-design.md`.
- Precedents: `UpdateCodexOAuthTokens` (scoped update on this table), `UpdateAgent` (PATCH pointer-DTO + COALESCE), `credLanes` clamp (`internal/agents/credential.go`).
- Surfaces: `internal/agents/credential.go`, `internal/agents/credential_handler.go`, `db/query/ai.sql`, `specs/003-agent-runtime/contracts/openapi.yaml`, `web/src/app/core/ai-credentials.service.ts`, `web/src/app/pages/credentials/ai/list.ts`, `web/e2e/ai-credentials.spec.ts`.
