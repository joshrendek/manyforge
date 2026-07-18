# Codex (ChatGPT-subscription) Credentials — Increment 1 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make an `openai_codex` AI-provider credential (ChatGPT subscription) drive an opencode cloud review end-to-end, using a *manually supplied* access token + account-id — proving the sandbox → ChatGPT backend path before any OAuth automation.

**Architecture:** A ChatGPT-subscription credential reuses the existing sealed-key + sandbox-env machinery, with two additions: (1) a new `openai_codex` provider whose default base URL is the ChatGPT Codex backend, and (2) a non-secret `chatgpt_account_id` carried alongside the sealed access token and injected into the sandbox. `deploy/sandbox/entrypoint.sh` gets a new arm that configures opencode's built-in `openai` (Responses-wire) provider to point at that backend with the impersonation headers. No OAuth, no refresh, no FE yet — those are later increments.

**Tech Stack:** Go 1.x, PostgreSQL + sqlc (pgx/v5), golang-migrate, sst/opencode (in a hardened Docker/Kube sandbox), Angular (FE — not touched in this increment).

**Design doc:** `docs/superpowers/specs/2026-07-11-codex-chatgpt-subscription-credentials-design.md`
**Epic:** manyforge-6fx · **Branch:** `manyforge-6fx-codex-credentials` (already created off `master`)

## Global Constraints

- **Scope of THIS plan:** Phase 0 spike + Increment 1 only. OAuth device-code flow + token refresh (Increment 2), FE connect UX + model catalog (Increment 3), and full hardening (Increment 4) are separate plans. Do not build them here.
- **sqlc is pinned to v1.27.0.** Regenerate with `go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.27.0 generate` from the repo root. Do NOT use a globally-installed `sqlc` (v1.31.x re-churns every generated file — see memory `sqlc-version-pin-v127`).
- **Commits: NO `Co-Authored-By` trailer** (per global CLAUDE.md). Conventional-commit style, reference `manyforge-6fx`.
- **One branch off master.** All tasks commit to `manyforge-6fx-codex-credentials`. Do not stack branches.
- **Verification gates that per-package `go test` misses** (memory `backend-verification-gates-easy-to-miss`): `go test -tags contract ./cmd/...` (OpenAPI drift) and `make lint` (staticcheck). A new provider must appear in `openapi.yaml` in the same change. Also run `make sec-test` (the `internal/security_regression` suite).
- **Never commit a real provider token.** The access token is a secret: seal it via the existing `sealAPIKey` path; use obviously-fake tokens (`sk-test`, `codex-test-token`) in tests (the secret-scan pins reject real shapes).
- **entrypoint JSON-metacharacter guard** must cover every new value interpolated into opencode JSON (extend the existing `"`/`\` check).
- **Provider string is `openai_codex`; opencode's provider id is `openai`.** manyforge's enum value / `LLM_PROVIDER` is `openai_codex`, but inside the sandbox the opencode provider id, `auth.json` key, and model prefix are the built-in `openai`. Do not conflate them.
- **ChatGPT backend facts** (from the design + spike): base URL `https://chatgpt.com/backend-api/codex`; required headers `ChatGPT-Account-Id: <id>`, `originator: codex_cli_rs`, and a versioned `User-Agent: codex_cli_rs/<ver> (...)`; wire is the Responses API (`POST /responses`). The exact working User-Agent string is captured by Task 0 and transcribed verbatim in Task 5.

---

### Task 0: Phase 0 spike — prove opencode-in-sandbox reaches the ChatGPT backend (GATE, manual, no commit)

This validates **Approach A** (reuse opencode's built-in `openai` provider via config) before any code. Nothing downstream is worth building if opencode can't reach the backend. This task produces no repo commit — its deliverable is a go/no-go recorded in the design doc.

**Files:**
- Scratch only: `/private/tmp/.../scratchpad/` for a throwaway config; do NOT add to git.
- Update at end: `docs/superpowers/specs/2026-07-11-codex-chatgpt-subscription-credentials-design.md` §15 (record outcome).

- [ ] **Step 1: Obtain a real ChatGPT-subscription token.** On your workstation, install/run the OpenAI `codex` CLI and complete "Sign in with ChatGPT". Then read `~/.codex/auth.json` and extract `tokens.access_token` and `tokens.account_id` (the account id is also the `chatgpt_account_id` claim inside the `id_token` JWT). Keep these in shell vars, not on disk in the repo:

```bash
CODEX_ACCESS_TOKEN=$(jq -r '.tokens.access_token' ~/.codex/auth.json)
CODEX_ACCOUNT_ID=$(jq -r '.tokens.account_id'  ~/.codex/auth.json)
# Note the codex CLI version for the User-Agent you will replicate:
codex --version   # record e.g. "codex_cli_rs/0.20.0"
```

- [ ] **Step 2: Build (or pull) the current sandbox image locally.**

```bash
cd /Users/jigglypuff/dev/manyforge
docker build -t manyforge/opencode-sandbox:spike deploy/sandbox
```

- [ ] **Step 3: Hand-write the Approach-A opencode config + auth.json in a scratch dir** matching what Task 5 will generate. Point the built-in `openai` provider at the ChatGPT backend with the impersonation headers:

```bash
SP=/private/tmp/claude-501/-Users-jigglypuff-dev-manyforge/*/scratchpad/codex-spike
mkdir -p "$SP/data/opencode" "$SP/work"
printf '{"openai":{"type":"api","key":"%s"}}\n' "$CODEX_ACCESS_TOKEN" > "$SP/data/opencode/auth.json"
cat > "$SP/opencode.json" <<JSON
{ "\$schema": "https://opencode.ai/config.json",
  "model": "openai/gpt-5",
  "small_model": "openai/gpt-5",
  "provider": { "openai": {
    "options": {
      "baseURL": "https://chatgpt.com/backend-api/codex",
      "headers": { "ChatGPT-Account-Id": "$CODEX_ACCOUNT_ID", "originator": "codex_cli_rs", "User-Agent": "codex_cli_rs/0.20.0 (spike)" }
    },
    "models": { "gpt-5": { "options": { "max_tokens": 32000 } } } } },
  "permission": { "read":"allow","glob":"allow","grep":"allow","edit":"deny","bash":"deny","webfetch":"deny","websearch":"deny","task":"deny","external_directory":"deny" } }
JSON
echo "def add(a,b): return a - b  # bug: should be +" > "$SP/work/calc.py"
```

- [ ] **Step 4: Run opencode headlessly against the backend** (mirror `entrypoint.sh`'s invocation; egress unrestricted for the spike — the sandbox restricts it later):

```bash
docker run --rm \
  -e HOME=/tmp -e XDG_DATA_HOME=/tmp/.local/share -e XDG_CONFIG_HOME=/tmp/.config \
  -e OPENCODE_DISABLE_MODELS_FETCH=1 -e OPENCODE_DISABLE_AUTOUPDATE=1 -e OPENCODE_CONFIG=/cfg/opencode.json \
  -v "$SP/opencode.json:/cfg/opencode.json:ro" \
  -v "$SP/data/opencode/auth.json:/tmp/.local/share/opencode/auth.json:ro" \
  -v "$SP/work:/tmp/src:ro" \
  --entrypoint sh manyforge/opencode-sandbox:spike -c \
  'cp -r /tmp/src /tmp/w && cd /tmp/w && NO_COLOR=1 opencode run -m openai/gpt-5 "Review calc.py and output ONLY JSON {\"summary\":string,\"findings\":[{\"file\":string,\"line\":number,\"severity\":string,\"title\":string,\"detail\":string}]}"'
```

- [ ] **Step 5: Evaluate the GATE.**
  - **PASS (Approach A confirmed):** opencode returns a well-formed findings JSON that flags the `-`-vs-`+` bug, with requests going to `chatgpt.com`. Record in design §15: "Approach A confirmed on <date>, codex CLI version `codex_cli_rs/<ver>`." Proceed to Task 1.
  - **FAIL:** capture the error. If it's a 403 → check the `User-Agent`/`originator` (try the exact `codex --version` string). If opencode ignored `provider.openai.options.headers`/`baseURL` (requests still hit `api.openai.com`, or headers absent) → **Approach A is not viable**; STOP and record "Approach A failed: <reason>; switching to Approach B (bake a codex-auth plugin into the image)". Do not proceed to Task 5 as written — Increment 1 Tasks 1–4, 6, 7 are still valid, but Task 5 must be re-planned for Approach B before continuing.

- [ ] **Step 6: Record the outcome** in `docs/superpowers/specs/2026-07-11-codex-chatgpt-subscription-credentials-design.md` §15 and commit ONLY that doc edit:

```bash
git add docs/superpowers/specs/2026-07-11-codex-chatgpt-subscription-credentials-design.md
git commit -m "docs(codex): record Phase 0 spike outcome (manyforge-6fx)"
```

---

### Task 1: Add the `openai_codex` provider enum value + `chatgpt_account_id` column (DB + sqlc regen)

**Files:**
- Create: `migrations/0093_ai_provider_openai_codex.up.sql`, `migrations/0093_ai_provider_openai_codex.down.sql`
- Create: `migrations/0094_ai_credential_chatgpt_account_id.up.sql`, `migrations/0094_ai_credential_chatgpt_account_id.down.sql`
- Modify: `db/schema.sql` (enum list + table column)
- Modify: `db/query/ai.sql` (Insert sets the new column)
- Regenerate: `internal/platform/db/dbgen/models.go`, `internal/platform/db/dbgen/ai.sql.go` (via sqlc — do not hand-edit)

**Interfaces:**
- Produces: `dbgen.AiProviderOpenaiCodex AiProvider = "openai_codex"`; `dbgen.AiProviderCredential.ChatgptAccountID *string`; `dbgen.InsertAIProviderCredentialParams.ChatgptAccountID *string`.

- [ ] **Step 1: Write the enum migration** (alone in its file, `IF NOT EXISTS`, matching the 0092 pattern).

`migrations/0093_ai_provider_openai_codex.up.sql`:
```sql
-- Add 'openai_codex' to the ai_provider enum. This is a ChatGPT-subscription credential
-- ("Sign in with ChatGPT"): the sealed key is a short-lived OAuth access token and completions
-- go to the ChatGPT backend (https://chatgpt.com/backend-api/codex, Responses wire) via opencode's
-- built-in openai provider, NOT api.openai.com. See specs .../2026-07-11-codex-...-design.md.
-- (PG: a newly added enum value cannot be USED in the same tx that adds it; nothing below uses
-- it — credentials reference it post-commit — so this is safe.) Idempotent.
ALTER TYPE ai_provider ADD VALUE IF NOT EXISTS 'openai_codex';
```

`migrations/0093_ai_provider_openai_codex.down.sql`:
```sql
-- Reverse 0093. PostgreSQL cannot remove a value from an enum type, so 'openai_codex'
-- PERSISTS after this down-migration. That matches every other enum addition in this schema
-- and is acceptable — nothing references it once its credentials are removed.
```

- [ ] **Step 2: Write the column migration.**

`migrations/0094_ai_credential_chatgpt_account_id.up.sql`:
```sql
-- ChatGPT-Account-Id header value for openai_codex credentials. Non-secret (an account
-- identifier, not a token); NULL for every other provider. The sealed access token continues
-- to live in sealed_key_ref. Sent as a request header by the sandbox entrypoint's openai_codex arm.
ALTER TABLE ai_provider_credential ADD COLUMN chatgpt_account_id text;
```

`migrations/0094_ai_credential_chatgpt_account_id.down.sql`:
```sql
ALTER TABLE ai_provider_credential DROP COLUMN IF EXISTS chatgpt_account_id;
```

- [ ] **Step 3: Update `db/schema.sql`** (the sqlc input). Add `'openai_codex'` to the enum and the column to the table:

```sql
CREATE TYPE ai_provider AS ENUM ('anthropic', 'openai', 'ollama', 'vllm', 'openrouter', 'huggingface', 'openai_codex');
```
and inside `CREATE TABLE ai_provider_credential (...)`, add after `max_concurrent_lanes integer NOT NULL,`:
```sql
    chatgpt_account_id text,
```

- [ ] **Step 4: Update the Insert query** `db/query/ai.sql` to set the column. In `-- name: InsertAIProviderCredential :one`, add `chatgpt_account_id` to the column list and `sqlc.arg('chatgpt_account_id')` to the SELECT (keep `now(), now()` last):

```sql
-- name: InsertAIProviderCredential :one
INSERT INTO ai_provider_credential (
    id, business_id, tenant_root_id, provider, sealed_key_ref, base_url, default_model,
    allow_private_base_url, max_concurrent_lanes, chatgpt_account_id, created_at, updated_at)
SELECT
    $1,
    b.id,
    b.tenant_root_id,
    sqlc.arg('provider')::ai_provider,
    sqlc.arg('sealed_key_ref'),
    sqlc.arg('base_url'),
    sqlc.arg('default_model'),
    sqlc.arg('allow_private_base_url'),
    sqlc.arg('max_concurrent_lanes')::integer,
    sqlc.arg('chatgpt_account_id'),
    now(), now()
FROM business b
WHERE b.id = sqlc.arg('business_id')::uuid
RETURNING *;
```

- [ ] **Step 5: Regenerate sqlc with the pinned version** (never the global one):

Run: `go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.27.0 generate`
Expected: `internal/platform/db/dbgen/models.go` gains `AiProviderOpenaiCodex` and `AiProviderCredential.ChatgptAccountID *string`; `ai.sql.go` `InsertAIProviderCredentialParams` gains `ChatgptAccountID *string` and every `row.Scan(...)` for this table gains `&i.ChatgptAccountID`. `git diff --stat` should show ONLY `models.go` + `ai.sql.go` changed (if many files churn, you used the wrong sqlc version — reset and use v1.27.0).

- [ ] **Step 6: Verify the build** (Create() still compiles because the new param defaults to nil):

Run: `go build ./...`
Expected: builds clean.

- [ ] **Step 7: Apply the migrations to the dev DB** (mf-dev on :55432, owner `manyforge`). Note (memory `manyforge-migration-dev-db-version-guard`): after this the air-run dev server refuses to serve until migrated, and you must force an air rebuild.

Run: `make migrate-up` (or the repo's documented migrate command against `postgres://manyforge@localhost:55432/...`)
Expected: `0093` and `0094` applied; `\d ai_provider_credential` shows `chatgpt_account_id`.

- [ ] **Step 8: Commit.**

```bash
git add migrations/0093_* migrations/0094_* db/schema.sql db/query/ai.sql internal/platform/db/dbgen/models.go internal/platform/db/dbgen/ai.sql.go
git commit -m "feat(codex): add openai_codex provider enum + chatgpt_account_id column (manyforge-6fx)"
```

---

### Task 2: Register `openai_codex` in the provider registry (knownProviders + factory + validation)

**Files:**
- Modify: `internal/agents/credential.go` (`knownProviders`, `validate`)
- Modify: `internal/platform/ai/factory.go` (name const, `defaultBaseURLs`, `New` guard)
- Test: `internal/platform/ai/factory_test.go`, `internal/agents/credential_test.go`

**Interfaces:**
- Consumes: `dbgen.AiProviderOpenaiCodex` (Task 1).
- Produces: `ai.ProviderOpenAICodex = "openai_codex"`; `ai.DefaultBaseURL("openai_codex") == "https://chatgpt.com/backend-api/codex", true`; `knownProviders["openai_codex"] == true`.

- [ ] **Step 1: Write the failing factory test.** Append to `internal/platform/ai/factory_test.go`:

```go
func TestOpenAICodexDefaultBaseURL(t *testing.T) {
	got, ok := DefaultBaseURL(ProviderOpenAICodex)
	if !ok || got != "https://chatgpt.com/backend-api/codex" {
		t.Fatalf("DefaultBaseURL(openai_codex) = %q,%v; want chatgpt backend,true", got, ok)
	}
}

func TestNewRejectsOpenAICodexForDirectCalls(t *testing.T) {
	// openai_codex is a sandbox/opencode-only provider (Responses wire + impersonation
	// headers); it must NOT be constructed as a direct gateway transport.
	_, err := New(Credential{Provider: ProviderOpenAICodex, APIKey: "codex-test-token", Model: "gpt-5"})
	if !errors.Is(err, ErrBadRequest) {
		t.Fatalf("New(openai_codex) err = %v; want ErrBadRequest", err)
	}
}
```

- [ ] **Step 2: Run it to verify it fails.**

Run: `go test ./internal/platform/ai/ -run 'OpenAICodex' -v`
Expected: FAIL — `ProviderOpenAICodex` undefined (compile error).

- [ ] **Step 3: Implement in `internal/platform/ai/factory.go`.** Add the constant (in the `const (...)` name block after `ProviderHuggingFace`):

```go
	// ProviderOpenAICodex is a ChatGPT-subscription credential ("Sign in with ChatGPT").
	// The key is a short-lived OAuth access token and completions go to the ChatGPT backend
	// (Responses wire) with impersonation headers — served ONLY via the code-review sandbox
	// (opencode's built-in openai provider), never as a direct gateway transport here.
	ProviderOpenAICodex = "openai_codex"
```

Add the backend URL constant near `huggingFaceBaseURL`:
```go
// openAICodexBaseURL is the ChatGPT-subscription backend (Responses wire), reached with the
// OAuth access token + ChatGPT-Account-Id/originator headers. NOT api.openai.com.
const openAICodexBaseURL = "https://chatgpt.com/backend-api/codex"
```

Add to `defaultBaseURLs`:
```go
	ProviderOpenAICodex: openAICodexBaseURL,
```

Add an explicit arm in `New(...)` BEFORE the `default:` (so it never falls into the openai-compat arm, which would speak the wrong wire with no headers):
```go
	case ProviderOpenAICodex:
		return nil, fmt.Errorf("ai: provider %q is only available via the code-review sandbox, not direct gateway calls: %w", cred.Provider, ErrBadRequest)
```

(If `errors` isn't already imported in `factory_test.go`, add it.)

- [ ] **Step 4: Run the factory tests to verify they pass.**

Run: `go test ./internal/platform/ai/ -run 'OpenAICodex' -v`
Expected: PASS.

- [ ] **Step 5: Write the failing knownProviders test.** Append to `internal/agents/credential_test.go`:

```go
func TestOpenAICodexIsKnownProvider(t *testing.T) {
	if !knownProviders[string(dbgen.AiProviderOpenaiCodex)] {
		t.Fatal("openai_codex must be a known provider")
	}
}
```

- [ ] **Step 6: Run it to verify it fails.**

Run: `go test ./internal/agents/ -run 'OpenAICodexIsKnownProvider' -v`
Expected: FAIL — key absent from map.

- [ ] **Step 7: Implement in `internal/agents/credential.go`.** Add to `knownProviders`:

```go
	string(dbgen.AiProviderOpenaiCodex): true,
```

Then extend `validate` so `openai_codex` requires an account id (add near the existing provider/base-url checks; `in.ChatGPTAccountID` is added to `CreateCredentialInput` in Task 3 — if implementing strictly in order, add this branch in Task 3's step instead and keep Task 2's `validate` change to only the known-provider path). For ordering safety, in THIS task only add the map entry; the account-id validation lands in Task 3 alongside the field it checks.

- [ ] **Step 8: Run the agents test to verify it passes.**

Run: `go test ./internal/agents/ -run 'OpenAICodexIsKnownProvider' -v`
Expected: PASS.

- [ ] **Step 9: Commit.**

```bash
git add internal/platform/ai/factory.go internal/platform/ai/factory_test.go internal/agents/credential.go internal/agents/credential_test.go
git commit -m "feat(codex): register openai_codex provider (base URL, direct-call guard, knownProviders) (manyforge-6fx)"
```

---

### Task 3: Store & resolve `chatgpt_account_id` through the credential service

**Files:**
- Modify: `internal/agents/credential.go` (`CreateCredentialInput`, `ResolvedCredential`, `CredentialView`, `storedCredential`, `Create`, `Resolve`, `resolveRow`, `validate`, `credViewFromRow`)
- Modify: `internal/agents/credential_handler.go` (parse `chatgpt_account_id` from the create request)
- Test: `internal/agents/credential_test.go`

**Interfaces:**
- Consumes: `dbgen.InsertAIProviderCredentialParams.ChatgptAccountID` (Task 1).
- Produces: `CreateCredentialInput.ChatGPTAccountID string`; `ResolvedCredential.ChatGPTAccountID string`.

- [ ] **Step 1: Write the failing round-trip test.** Append to `internal/agents/credential_test.go` (follow the file's existing harness for constructing a `CredentialService` with a test DB + Sealer; mirror an existing Create/Resolve test):

```go
func TestOpenAICodexAccountIDRoundTrips(t *testing.T) {
	svc, principal, business := newTestCredentialService(t) // existing helper
	_, err := svc.Create(ctx, principal, business, CreateCredentialInput{
		Provider:         "openai_codex",
		APIKey:           "codex-test-token", // stands in for the OAuth access token
		DefaultModel:     "gpt-5",
		ChatGPTAccountID: "acct-abc-123",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := svc.Resolve(ctx, principal, business, "openai_codex")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.APIKey != "codex-test-token" || got.ChatGPTAccountID != "acct-abc-123" {
		t.Fatalf("got key=%q acct=%q; want token + acct-abc-123", got.APIKey, got.ChatGPTAccountID)
	}
}

func TestOpenAICodexRequiresAccountID(t *testing.T) {
	svc, principal, business := newTestCredentialService(t)
	_, err := svc.Create(ctx, principal, business, CreateCredentialInput{
		Provider: "openai_codex", APIKey: "codex-test-token", DefaultModel: "gpt-5",
	})
	if !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("create without account id err = %v; want ErrValidation", err)
	}
}
```

- [ ] **Step 2: Run to verify it fails.**

Run: `go test ./internal/agents/ -run 'OpenAICodec|OpenAICodex(AccountID|Requires)' -v`
Expected: FAIL — `ChatGPTAccountID` field undefined (compile error).

- [ ] **Step 3: Add the field to the input/output/stored structs** in `internal/agents/credential.go`:

`CreateCredentialInput` — add:
```go
	// ChatGPTAccountID is the ChatGPT-Account-Id header value for openai_codex credentials
	// (non-secret). Required when Provider == "openai_codex"; ignored otherwise.
	ChatGPTAccountID string
```
`ResolvedCredential` — add:
```go
	ChatGPTAccountID string // openai_codex only; "" for other providers
```
`storedCredential` — add:
```go
	ChatGPTAccountID *string
```
`CredentialView` — add (non-secret, safe to surface):
```go
	ChatGPTAccountID string
```

- [ ] **Step 4: Wire `Create`.** Pass the field to the insert params (nil when empty so non-codex rows stay NULL) and add the validation. Inside `Create`, before building params:

```go
	var acctArg *string
	if in.ChatGPTAccountID != "" {
		acctArg = &in.ChatGPTAccountID
	}
```
add `ChatgptAccountID: acctArg,` to the `InsertAIProviderCredentialParams{...}` literal.

In `validate`, add:
```go
	if in.Provider == string(dbgen.AiProviderOpenaiCodex) && in.ChatGPTAccountID == "" {
		return fmt.Errorf("openai_codex credential requires chatgpt_account_id: %w", errs.ErrValidation)
	}
```

- [ ] **Step 5: Wire `Resolve` + `resolveRow` + `credViewFromRow`.** In `Resolve`, pass the column into `storedCredential`:
```go
		ChatGPTAccountID: row.ChatgptAccountID,
```
In `resolveRow`, map it onto `ResolvedCredential` (deref-safe):
```go
	acct := ""
	if sc.ChatGPTAccountID != nil {
		acct = *sc.ChatGPTAccountID
	}
```
and set `ChatGPTAccountID: acct` on the returned `ResolvedCredential`. In `credViewFromRow`, set `ChatGPTAccountID` from `row.ChatgptAccountID` (deref-safe, "" when nil).

- [ ] **Step 6: Parse it in the handler** `internal/agents/credential_handler.go`. Add a `ChatGPTAccountID string` json field (`chatgpt_account_id`) to the create-request DTO and pass it into `CreateCredentialInput`. (Mirror how `BaseURL`/`DefaultModel` are threaded.)

- [ ] **Step 7: Run to verify the tests pass.**

Run: `go test ./internal/agents/ -run 'OpenAICodex(AccountID|Requires)' -v`
Expected: PASS.

- [ ] **Step 8: Full package test + build.**

Run: `go build ./... && go test ./internal/agents/ ./internal/platform/ai/`
Expected: PASS.

- [ ] **Step 9: Commit.**

```bash
git add internal/agents/credential.go internal/agents/credential_handler.go internal/agents/credential_test.go
git commit -m "feat(codex): store & resolve chatgpt_account_id on the credential (manyforge-6fx)"
```

---

### Task 4: Carry the account id into the sandbox env

**Files:**
- Modify: `internal/agents/coding/credresolver.go` (`AICredential` + `Resolve` + `ResolveProvider`)
- Modify: `internal/agents/coding/service.go` (`sandboxEnv`)
- Test: `internal/agents/coding/credresolver_test.go` (or a new `sandboxenv_test.go` in package `coding`)

**Interfaces:**
- Consumes: `ResolvedCredential.ChatGPTAccountID` (Task 3).
- Produces: `AICredential.ChatGPTAccountID string`; `sandboxEnv` emits `LLM_CHATGPT_ACCOUNT_ID` (present only when non-empty).

- [ ] **Step 1: Write the failing sandboxEnv test.** Add to a test file in package `coding`:

```go
func TestSandboxEnvIncludesChatGPTAccountID(t *testing.T) {
	env := sandboxEnv(AICredential{
		APIKey: "codex-test-token", BaseURL: "https://chatgpt.com/backend-api/codex",
		Model: "gpt-5", Provider: "openai_codex", ChatGPTAccountID: "acct-abc-123",
	})
	if env["LLM_CHATGPT_ACCOUNT_ID"] != "acct-abc-123" {
		t.Fatalf("LLM_CHATGPT_ACCOUNT_ID = %q; want acct-abc-123", env["LLM_CHATGPT_ACCOUNT_ID"])
	}
	if env["LLM_PROVIDER"] != "openai_codex" || env["LLM_BASE_URL"] != "https://chatgpt.com/backend-api/codex" {
		t.Fatalf("unexpected provider/base env: %v", env)
	}
}

func TestSandboxEnvOmitsAccountIDForOtherProviders(t *testing.T) {
	env := sandboxEnv(AICredential{APIKey: "sk-x", BaseURL: "https://openrouter.ai/api/v1", Model: "m", Provider: "openrouter"})
	if _, ok := env["LLM_CHATGPT_ACCOUNT_ID"]; ok {
		t.Fatal("non-codex providers must not set LLM_CHATGPT_ACCOUNT_ID")
	}
}
```

- [ ] **Step 2: Run to verify it fails.**

Run: `go test ./internal/agents/coding/ -run 'SandboxEnv.*AccountID|SandboxEnvOmits' -v`
Expected: FAIL — `ChatGPTAccountID` field undefined (compile error).

- [ ] **Step 3: Add the field to `AICredential`** in `credresolver.go`:

```go
	// ChatGPTAccountID is the ChatGPT-Account-Id header value for openai_codex credentials
	// (non-secret). "" for every other provider. Injected as LLM_CHATGPT_ACCOUNT_ID.
	ChatGPTAccountID string
```
Set it in BOTH `Resolve` and `ResolveProvider` return literals:
```go
		ChatGPTAccountID: rc.ChatGPTAccountID,
```

- [ ] **Step 4: Emit it conditionally in `sandboxEnv`** (`service.go`):

```go
func sandboxEnv(cred AICredential) map[string]string {
	env := map[string]string{
		"LLM_API_KEY":  cred.APIKey,
		"LLM_BASE_URL": cred.BaseURL,
		"LLM_MODEL":    cred.Model,
		"LLM_PROVIDER": cred.Provider,
	}
	// openai_codex needs the ChatGPT-Account-Id header; carried as a dedicated env var so the
	// entrypoint can place it in the request headers. Omitted for every other provider.
	if cred.ChatGPTAccountID != "" {
		env["LLM_CHATGPT_ACCOUNT_ID"] = cred.ChatGPTAccountID
	}
	return env
}
```

- [ ] **Step 5: Run to verify it passes.**

Run: `go test ./internal/agents/coding/ -run 'SandboxEnv.*AccountID|SandboxEnvOmits' -v`
Expected: PASS.

- [ ] **Step 6: Commit.**

```bash
git add internal/agents/coding/credresolver.go internal/agents/coding/service.go internal/agents/coding/credresolver_test.go
git commit -m "feat(codex): inject LLM_CHATGPT_ACCOUNT_ID into the review sandbox (manyforge-6fx)"
```

---

### Task 5: Add the `openai_codex` arm to the sandbox entrypoint

> **⚠️ REVISED 2026-07-16 after the Phase 0 spike — the code below (api-key + custom baseURL/headers)
> is SUPERSEDED and DOES NOT WORK** (it made opencode send `store:true` → the ChatGPT backend 400s,
> and the config-only approach hangs). The spike proved **Approach A′**: opencode's built-in `openai`
> provider has NATIVE codex support — feed it a `type:"oauth"` auth entry and it targets the codex
> endpoint and sets `store:false` + `ChatGPT-Account-Id` + `originator:opencode` itself. **The
> authoritative task spec is the brief at `.superpowers/sdd/task-5-brief.md` and design §15a**; the
> superseded api-key code below is retained only for history. Do NOT implement it as written.

**Files:**
- Modify: `deploy/sandbox/entrypoint.sh` (add the `openai_codex → codex` mode arm + a `codex` config branch writing the oauth `auth.json`; add `LLM_CHATGPT_ACCOUNT_ID` to the metacharacter guard).
- Modify: `internal/security_regression/mf_kube_sandbox_test.go` (update the MF-KUBE-SANDBOX-22 allowlist pin to include the new arm; add source-level pins for the codex branch — matches the repo's "read entrypoint.sh + strings.Contains" test convention, hermetic, no Docker/egress).

**Interfaces:**
- Consumes: env `LLM_PROVIDER=openai_codex`, `LLM_API_KEY` (OAuth **access token**), `LLM_MODEL` (`gpt-5.5`), `LLM_CHATGPT_ACCOUNT_ID`.
- Produces: opencode `auth.json` `{"openai":{"type":"oauth","access":"<access_token>","refresh":"unused-host-side-only","expires":<far-future ms>,"accountId":"<account_id>"}}`; `OPENCODE_CONFIG` declaring `model:"openai/<slug>"` with NO baseURL/headers override; `MODEL=openai/<slug>`.

**➡️ Implement from `.superpowers/sdd/task-5-brief.md`. The steps/code below are the superseded api-key version — ignore.**

- [ ] **Step 1: Extend the provider gate.** In the `case "${LLM_PROVIDER:-}" in` block, add `openai_codex` as its own mode (it uses the built-in `openai` provider but with custom base URL + headers, so it is neither the plain `builtin` nor `compat` arm):

```sh
  openrouter|anthropic|openai)  LLM_OPENCODE_MODE=builtin ;;
  vllm|ollama|huggingface)      LLM_OPENCODE_MODE=compat ;;
  openai_codex)                 LLM_OPENCODE_MODE=codex ;;
```

- [ ] **Step 2: Extend the JSON-metacharacter guard** to include the account id (add it to the `for _mfval in ...` list):

```sh
for _mfval in "${LLM_BASE_URL:-}" "${LLM_MODEL:-}" "${LLM_API_KEY:-}" "${LLM_CHATGPT_ACCOUNT_ID:-}"; do
```

- [ ] **Step 3: Add the `codex` config arm.** Insert a new branch in the mode dispatch (before the final `fi`, as a sibling of the `compat`/`builtin` branches). Use the exact `User-Agent` string Task 0 confirmed (shown here as `codex_cli_rs/0.20.0`; replace with the recorded value):

```sh
if [ "$LLM_OPENCODE_MODE" = codex ]; then
  # ChatGPT-subscription path. Reuse opencode's BUILT-IN openai provider (Responses wire, the
  # same protocol the ChatGPT backend speaks), but override baseURL to the ChatGPT backend and
  # attach the impersonation headers Codex requires. The "key" is the OAuth access token
  # (minted host-side, injected as LLM_API_KEY). opencode provider id / auth.json key / model
  # prefix are all "openai" (the built-in provider), NOT "openai_codex" (manyforge's enum).
  # CODEX_UA must track a real codex CLI release or the backend 403s (spike-confirmed value).
  CODEX_UA='codex_cli_rs/0.20.0 (manyforge review)'
  MODEL="openai/${LLM_MODEL}"
  mkdir -p "$XDG_DATA_HOME/opencode"
  printf '{"openai":{"type":"api","key":"%s"}}\n' "$LLM_API_KEY" > "$XDG_DATA_HOME/opencode/auth.json"
  export OPENCODE_CONFIG=/tmp/opencode.json
  printf '%s\n' '{
  "$schema": "https://opencode.ai/config.json",
  "model": "'"${MODEL}"'",
  "small_model": "'"${MODEL}"'",
  "provider": {
    "openai": {
      "options": {
        "baseURL": "'"${LLM_BASE_URL}"'",
        "headers": {
          "ChatGPT-Account-Id": "'"${LLM_CHATGPT_ACCOUNT_ID}"'",
          "originator": "codex_cli_rs",
          "User-Agent": "'"${CODEX_UA}"'"
        }
      },
      "models": { "'"${LLM_MODEL}"'": { "options": { "max_tokens": 32000 } } }
    }
  },
  "permission": {
    "read": "allow", "glob": "allow", "grep": "allow",
    "edit": "deny", "bash": "deny", "webfetch": "deny", "websearch": "deny",
    "task": "deny", "external_directory": "deny"
  }
}' > "$OPENCODE_CONFIG"
elif [ "$LLM_OPENCODE_MODE" = compat ]; then
```

(Convert the existing `if [ "$LLM_OPENCODE_MODE" = compat ]; then` into the `elif` shown on the last line, so the three arms — `codex` / `compat` / `builtin` (the trailing `else`) — chain cleanly.)

- [ ] **Step 4: Write the failing entrypoint test.** Create `deploy/sandbox/entrypoint_openai_codex_test.sh` that sources the config-writing portion in isolation and asserts the emitted JSON contains the header + auth shape (keep it hermetic — no opencode run):

```sh
#!/bin/sh
# Pins the openai_codex arm: built-in openai provider, ChatGPT backend base URL, impersonation
# headers, and auth.json keyed by "openai" (NOT "openai_codex"). Regression for manyforge-6fx.
set -eu
tmp=$(mktemp -d)
export XDG_DATA_HOME="$tmp/data" XDG_CONFIG_HOME="$tmp/cfg" HOME="$tmp"
export LLM_PROVIDER=openai_codex LLM_API_KEY=codex-test-token LLM_MODEL=gpt-5 \
       LLM_BASE_URL=https://chatgpt.com/backend-api/codex LLM_CHATGPT_ACCOUNT_ID=acct-abc-123
# Run only the config-writing prefix of the entrypoint (guard: it must stop before `opencode run`).
MF_ENTRYPOINT_CONFIG_ONLY=1 sh deploy/sandbox/entrypoint.sh >/dev/null 2>&1 || true
auth="$XDG_DATA_HOME/opencode/auth.json"
cfg=/tmp/opencode.json
grep -q '"openai":{"type":"api","key":"codex-test-token"}' "$auth" || { echo "FAIL auth.json"; exit 1; }
grep -q '"ChatGPT-Account-Id": "acct-abc-123"' "$cfg" || { echo "FAIL account header"; exit 1; }
grep -q '"baseURL": "https://chatgpt.com/backend-api/codex"' "$cfg" || { echo "FAIL baseURL"; exit 1; }
grep -q '"originator": "codex_cli_rs"' "$cfg" || { echo "FAIL originator"; exit 1; }
echo PASS
```

To make the entrypoint testable without running opencode, add near the top of `entrypoint.sh` (after the config arms, before the `opencode run` invocation) a guard:
```sh
if [ -n "${MF_ENTRYPOINT_CONFIG_ONLY:-}" ]; then exit 0; fi
```
(Place it just before the `SCOPE=...`/`PROMPT=...` review section so the test exercises real config generation but skips the network run.)

- [ ] **Step 5: Run the test to verify it fails, then passes.**

Run: `sh deploy/sandbox/entrypoint_openai_codex_test.sh`
Expected: after Steps 1–3, prints `PASS`. (Before them, FAILs on the missing arm.)

- [ ] **Step 6: Commit.**

```bash
chmod +x deploy/sandbox/entrypoint_openai_codex_test.sh
git add deploy/sandbox/entrypoint.sh deploy/sandbox/entrypoint_openai_codex_test.sh
git commit -m "feat(codex): sandbox entrypoint arm for the ChatGPT backend (manyforge-6fx)"
```

---

### Task 6: Allow the ChatGPT backend host through the sandbox egress proxy

**Files:**
- Modify: `internal/platform/config/config.go` (`SandboxEgressAllow` default + doc comment)
- Modify: `charts/manyforge/values.yaml` (`egressAllow`)
- Test: `internal/platform/config/config_test.go`

**Interfaces:**
- Produces: default `SandboxEgressAllow` includes `chatgpt.com`. (Host-side `auth.openai.com` for OAuth mint/refresh is added in Increment 2, not here — the sandbox never reaches it.)

- [ ] **Step 1: Write the failing test.** Append to `internal/platform/config/config_test.go`:

```go
func TestSandboxEgressAllowsChatGPTBackend(t *testing.T) {
	cfg := Load() // or the repo's config constructor used in existing tests
	if !strings.Contains(cfg.SandboxEgressAllow, "chatgpt.com") {
		t.Fatalf("SandboxEgressAllow %q must include chatgpt.com", cfg.SandboxEgressAllow)
	}
}
```

- [ ] **Step 2: Run to verify it fails.**

Run: `go test ./internal/platform/config/ -run 'ChatGPTBackend' -v`
Expected: FAIL — host absent.

- [ ] **Step 3: Add the host to the default** in `config.go` (line ~350) and update the doc comment (line ~130):

```go
	cfg.SandboxEgressAllow = env("MANYFORGE_SANDBOX_EGRESS_ALLOW", "api.anthropic.com,openrouter.ai,api.openai.com,router.huggingface.co,chatgpt.com")
```

- [ ] **Step 4: Add it to the chart default** `charts/manyforge/values.yaml` (`egressAllow` list — append `chatgpt.com`, matching the existing entries' format).

- [ ] **Step 5: Run to verify it passes.**

Run: `go test ./internal/platform/config/ -run 'ChatGPTBackend' -v`
Expected: PASS.

- [ ] **Step 6: Commit.**

```bash
git add internal/platform/config/config.go internal/platform/config/config_test.go charts/manyforge/values.yaml
git commit -m "feat(codex): allow chatgpt.com through the sandbox egress proxy (manyforge-6fx)"
```

---

### Task 7: Add `openai_codex` to the OpenAPI contract + the create-request account-id field

**Files:**
- Modify: `specs/003-agent-runtime/contracts/openapi.yaml` (4 provider enums + `CreateAICredentialRequest`/`AICredential` `chatgpt_account_id`)
- Test: the contract test (`go test -tags contract ./cmd/...`)

**Interfaces:**
- Consumes: the handler DTO field from Task 3.
- Produces: contract parity so `go test -tags contract ./cmd/...` passes with the new provider + field.

- [ ] **Step 1: Add `openai_codex` to all four provider enums** (lines ~514, ~577, ~756, ~767). Each becomes:

```yaml
        provider: { type: string, enum: [anthropic, openai, ollama, vllm, openrouter, huggingface, openai_codex] }
```

- [ ] **Step 2: Add the optional `chatgpt_account_id` property** to `CreateAICredentialRequest` and (as a read-back, non-secret) to the `AICredential` response schema:

```yaml
        chatgpt_account_id:
          type: string
          description: ChatGPT-Account-Id for an openai_codex credential (required when provider is openai_codex).
```

- [ ] **Step 3: Run the contract test.**

Run: `go test -tags contract ./cmd/...`
Expected: PASS (no OpenAPI drift). If the handler request/response structs don't match the schema, reconcile the field names/casing until green.

- [ ] **Step 4: Commit.**

```bash
git add specs/003-agent-runtime/contracts/openapi.yaml
git commit -m "feat(codex): openai_codex + chatgpt_account_id in the OpenAPI contract (manyforge-6fx)"
```

---

### Task 8: Full-suite green + increment wrap-up

**Files:** none (verification + issue bookkeeping).

- [ ] **Step 1: Run the full verification gates** (memory `backend-verification-gates-easy-to-miss`):

```bash
go build ./...
go test ./...
go test -tags contract ./cmd/...
make lint          # staticcheck — NOT covered by per-package go test
make sec-test      # internal/security_regression
sh deploy/sandbox/entrypoint_openai_codex_test.sh
```
Expected: all PASS. Fix any failure before continuing (no "pre-existing failure" exceptions — global CLAUDE.md).

- [ ] **Step 2: Manual end-to-end confirmation in-product** (the increment's real proof, using the Task 0 token). Create an `openai_codex` credential via the API and run a review; confirm findings come back from the ChatGPT backend:

```bash
# Create the credential (access token + account id from Task 0), then trigger a review on a
# small test PR the way the code-review flow is normally exercised locally
# (see memory manyforge-live-cloud-review-repro: API on :8081, PR must be OPEN, logs → /tmp/mf-air.log).
```
Record the result (success + a sample finding, or the failure) in the epic notes.

- [ ] **Step 3: Update issue tracking.**

```bash
bd update manyforge-6fx --notes "Increment 1 landed: openai_codex provider + manual-token sandbox path proven end-to-end. Next: Increment 2 (OAuth device-code + refresh)."
bd create --type=feature --priority=2 --title="Codex Increment 2: OAuth device-code connect + token refresh (manyforge-6fx)" --description="Host-side device-code flow against auth.openai.com, sealed refresh/expiry columns, proactive+reactive refresh, per-run access-token mint. Depends on Increment 1." --deps="discovered-from:manyforge-6fx"
```

- [ ] **Step 4: Finish the branch** — invoke `superpowers:finishing-a-development-branch` to open a PR into `master` (base `master`; do NOT stack). Per memory `gh-auto-merge-races-review-fixes`, merge manyforge PRs manually (no `--auto`).

---

## Self-Review

**Spec coverage (design §-by-§):** §1–2 scope → Global Constraints + Task 8 bookkeeping. §3 findings → embedded in task rationale. §4/§4.1 Serve/Approach A → Task 0 (gate) + Task 5. §5 spike → Task 0. §6 data model (enum, account_id, sealed key reuse; refresh/expiry deferred) → Tasks 1, 3. §7 Connect UX (device-code) → **deferred to Increment 2 (out of scope, stated)**. §8 sandbox integration → Tasks 4, 5. §9 egress (sandbox=chatgpt.com; host=auth.openai.com deferred) → Task 6. §10 model catalog / `*-pro` filter → **deferred to Increment 3 (FE)**; not needed for the manual-token proof. §11 checklist → Tasks 1–7. §12 increments → this plan is Increment 1 + Phase 0. §13 testing → each task's tests + Task 8. §14 risks → Task 0 gate (fingerprint/403), Global Constraints (secrets). §15 spike items → Task 0 Step 6.

**Placeholder scan:** The only value not knowable until execution is the exact `codex_cli_rs/<ver>` User-Agent — Task 0 captures it and Task 5 transcribes it verbatim (concrete default shown). No TBD/TODO steps.

**Type consistency:** `ChatGPTAccountID` (Go field) ↔ `chatgpt_account_id` (SQL column / JSON) ↔ `dbgen.…ChatgptAccountID` (sqlc casing) ↔ `LLM_CHATGPT_ACCOUNT_ID` (env) are used consistently. Provider constant `ProviderOpenAICodex = "openai_codex"` ↔ `dbgen.AiProviderOpenaiCodex` ↔ enum literal `'openai_codex'` ↔ opencode provider id `openai` (deliberately different; flagged in Global Constraints and Task 5).

**Known ordering nuance:** Task 2 Step 7 defers the `openai_codex` account-id `validate` branch to Task 3 (where the field it reads is introduced) to keep each task independently compiling — noted inline.
