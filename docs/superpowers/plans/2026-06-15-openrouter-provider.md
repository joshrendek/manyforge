# OpenRouter Provider — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `openrouter` as a first-class `ai_provider` enum value end-to-end (DB → sqlc → Go allowlist → factory dispatch → frontend → OpenAPI), reusing the existing OpenAI-compatible client unchanged.

**Architecture:** OpenRouter is OpenAI-API-compatible (`https://openrouter.ai/api/v1` + `Authorization: Bearer`), so `internal/platform/ai/OpenAICompatProvider` serves it as-is. The only new logic is a factory dispatch case that defaults base_url to OpenRouter's URL when empty, plus making base_url optional for openrouter at the credential boundary, plus free-text model entry in the agent form (OpenRouter models aren't in `model_pricing`).

**Tech Stack:** Go (chi, pgx, sqlc v1.27.0 pinned), Postgres (enum `ai_provider`), Angular 21 (standalone, signals, vitest), OpenAPI contract test.

**Spec:** `docs/superpowers/specs/2026-06-15-openrouter-provider-design.md`. **bd issue:** `manyforge-eca`.

---

## Conventions for every task

- **Go env:** `export PATH="$HOME/go/bin:$PATH"`. Gates: `go build ./...` · `make test` · `make sec-test` · `make lint` (= `go vet` + staticcheck) · **`go test -tags contract ./cmd/...`** (OpenAPI drift — easy to miss; new/changed schemas must match). Integration: `go test -tags integration ./internal/agents/...`.
- **sqlc:** pinned **v1.27.0** — running the dev-global version re-churns all of dbgen. See sqlc step in Task 1 for the safe invocation + diff check. **NEVER `make generate`** (it uses the unpinned global).
- **Dev DB migrate:** `migrate -path migrations -database "postgres://manyforge:devpassword@localhost:55432/manyforge?sslmode=disable" up` (the `mf-dev` container; owner role `manyforge`).
- **Frontend (`cd web`):** `npm run build` · `npm test` (vitest, runs once) · `npm run e2e -- e2e/<file>.spec.ts` (needs :4300; page.route-mocked).
- gopls squiggles are stale right after a sqlc regen / edit — trust `go build`. Use Read/plain `grep`, not `rg` (mis-renders). Shell has `noclobber`. Conventional commits; **NO Co-Authored-By trailer**. The bd hook sweeps `.beads/issues.jsonl` into commits — leave it.

---

## File Structure

- Modify `db/schema.sql` — add `'openrouter'` to the `ai_provider` enum (the sqlc source).
- Create `migrations/0056_ai_provider_openrouter.up.sql` + `.down.sql`.
- Regen `internal/platform/db/dbgen/models.go` (adds `AiProviderOpenrouter`).
- Modify `internal/agents/credential.go` — `knownProviders` + base_url-optional for openrouter.
- Modify the `TestKnownProvidersTrackEnum` test (expects openrouter).
- Modify `internal/platform/ai/factory.go` — `ProviderOpenRouter` const + `openRouterBaseURL` + dispatch case.
- Create/modify `internal/platform/ai/factory_test.go` (or the existing one) — openrouter dispatch tests.
- Modify `specs/003-agent-runtime/contracts/openapi.yaml` — add `openrouter` to the provider enums.
- Modify `web/src/app/core/ai-credentials.service.ts` — `AIProvider` union.
- Modify `web/src/app/pages/credentials/ai/credential-form.ts` (+ `.spec.ts`) — option + base_url prefill.
- Modify `web/src/app/pages/agents/agent-form.ts` (+ `.spec.ts`) — option + free-text rename.

---

## Task 1: Add the `openrouter` enum value (DB + sqlc)

**Files:**
- Modify: `db/schema.sql` (the `CREATE TYPE ai_provider` line)
- Create: `migrations/0056_ai_provider_openrouter.up.sql`, `migrations/0056_ai_provider_openrouter.down.sql`
- Regen: `internal/platform/db/dbgen/models.go`

**Context (verified):** `ai_provider` is defined in `db/schema.sql` (`CREATE TYPE ai_provider AS ENUM ('anthropic', 'openai', 'ollama', 'vllm');`) and `migrations/0025_ai_provider_credential.up.sql:5`. It has never been `ALTER`'d. dbgen constants are at `internal/platform/db/dbgen/models.go:16-22`. Postgres `ALTER TYPE … ADD VALUE` can't be USED in the same tx that adds it (the migration must contain ONLY the ALTER), and enum values can't be removed (down = documented no-op).

- [ ] **Step 1: Add the value to `db/schema.sql`**

Find the line (it's `CREATE TYPE ai_provider AS ENUM ('anthropic', 'openai', 'ollama', 'vllm');`) and change it to:

```sql
CREATE TYPE ai_provider AS ENUM ('anthropic', 'openai', 'ollama', 'vllm', 'openrouter');
```

- [ ] **Step 2: Create the up-migration**

Create `migrations/0056_ai_provider_openrouter.up.sql`:

```sql
-- Add 'openrouter' to the ai_provider enum. OpenRouter is OpenAI-API-compatible, so it
-- reuses the OpenAICompatProvider; this just makes it a selectable first-class provider.
-- (PG: a newly added enum value cannot be USED in the same tx that adds it; nothing below
-- uses it — credentials/agents reference it post-commit — so this is safe.) Idempotent.
ALTER TYPE ai_provider ADD VALUE IF NOT EXISTS 'openrouter';
```

- [ ] **Step 3: Create the down-migration**

Create `migrations/0056_ai_provider_openrouter.down.sql`:

```sql
-- Reverse 0056. NOTE: PostgreSQL cannot remove a value from an enum type, so the
-- 'openrouter' value added to ai_provider PERSISTS after this down-migration. That is
-- acceptable and matches how every other enum addition in this schema is irreversible.
-- Nothing references 'openrouter' once the credentials/agents using it are removed.
```

- [ ] **Step 4: Apply the migration to the dev DB**

Run: `migrate -path migrations -database "postgres://manyforge:devpassword@localhost:55432/manyforge?sslmode=disable" up`
Expected: applies `0056` with no error. (If `migrate` isn't on PATH, it's at `$HOME/go/bin/migrate`.)

Verify: `psql "postgres://manyforge:devpassword@localhost:55432/manyforge?sslmode=disable" -c "SELECT unnest(enum_range(NULL::ai_provider));"` lists `openrouter`.

- [ ] **Step 5: Regenerate sqlc (pinned v1.27.0) and verify a minimal diff**

Run `sqlc generate` from the repo root using **v1.27.0**. First check the version:
```bash
sqlc version
```
- If it prints `v1.27.0`, run `sqlc generate`.
- If it prints anything else, run the pinned version instead: `go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.27.0 generate`.

Then VERIFY the diff is minimal — only the new constant, no churn:
```bash
git diff --stat internal/platform/db/dbgen/
```
Expected: only `internal/platform/db/dbgen/models.go` changed, a small (+1 line) diff adding `AiProviderOpenrouter AiProvider = "openrouter"`. **If many dbgen files churned, the wrong sqlc version ran** — recover: `git checkout internal/platform/db/dbgen/ && go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.27.0 generate` and re-check.

Confirm the constant exists:
```bash
grep -n "AiProviderOpenrouter" internal/platform/db/dbgen/models.go
```
Expected: `AiProviderOpenrouter AiProvider = "openrouter"`.

- [ ] **Step 6: Build**

Run: `export PATH="$HOME/go/bin:$PATH" && go build ./...`
Expected: exit 0.

- [ ] **Step 7: Commit**

```bash
git add db/schema.sql migrations/0056_ai_provider_openrouter.up.sql migrations/0056_ai_provider_openrouter.down.sql internal/platform/db/dbgen/models.go
git commit -m "feat(db): add openrouter to ai_provider enum"
```

---

## Task 2: Allowlist + base_url-optional for openrouter (`credential.go`)

**Files:**
- Modify: `internal/agents/credential.go` (lines ~30-35 and ~121)
- Test: the file containing `TestKnownProvidersTrackEnum` (find it) + a new validate test

**Context (verified):** `knownProviders` (credential.go:30-35) is the service-boundary allowlist, pinned by `TestKnownProvidersTrackEnum`. `validate` (credential.go:112) requires base_url for any provider except `anthropic` via `if in.Provider != "anthropic" && in.BaseURL == ""` (credential.go:121). OpenRouter's base_url is defaulted by the factory (Task 3), so it must be optional here too.

- [ ] **Step 1: Write the failing test — validate accepts openrouter with empty base_url**

Find the existing credential validate/known-providers tests: `grep -rn "TestKnownProvidersTrackEnum\|func .*validate" internal/agents/*_test.go`. Add to the appropriate test file (in-package `agents` if `validate` is unexported — it is) a test:

```go
func TestValidateOpenRouterBaseURLOptional(t *testing.T) {
	s := &CredentialService{} // validate() touches no DB/sealer fields
	// openrouter with NO base_url must be accepted (factory defaults it).
	if err := s.validate(CreateCredentialInput{Provider: "openrouter", DefaultModel: "anthropic/claude-3.5-sonnet"}); err != nil {
		t.Fatalf("openrouter empty base_url should be valid, got %v", err)
	}
	// openai with NO base_url must STILL be rejected (regression guard on the exemption).
	if err := s.validate(CreateCredentialInput{Provider: "openai", DefaultModel: "gpt-5"}); err == nil {
		t.Fatal("openai with empty base_url must still be rejected")
	}
}
```

Also update `TestKnownProvidersTrackEnum` to expect openrouter — read it first; it likely reflects over the dbgen `AiProvider*` constants or asserts a count. If it asserts a count of 4, change to 5; if it iterates the dbgen constants, it should now naturally include openrouter once `knownProviders` has it (the test's job is to catch a missing entry). Make the minimal change that keeps it pinning "every enum value is in knownProviders".

- [ ] **Step 2: Run to verify failure**

Run: `export PATH="$HOME/go/bin:$PATH" && go test ./internal/agents/ -run 'TestValidateOpenRouterBaseURLOptional|TestKnownProvidersTrackEnum' -v`
Expected: FAIL — openrouter not in knownProviders (validate returns "unknown provider"), and/or the enum-track test fails.

- [ ] **Step 3: Add openrouter to `knownProviders`**

In `internal/agents/credential.go`, change the map (lines ~30-35) to:

```go
var knownProviders = map[string]bool{
	string(dbgen.AiProviderAnthropic):  true,
	string(dbgen.AiProviderOpenai):     true,
	string(dbgen.AiProviderOllama):     true,
	string(dbgen.AiProviderVllm):       true,
	string(dbgen.AiProviderOpenrouter): true,
}
```

- [ ] **Step 4: Make base_url optional for openrouter**

In `validate` (credential.go:121), change the base_url-required line:

```go
	// openai-compat providers (openai/ollama/vllm) route through a caller-supplied base_url;
	// anthropic and openrouter have a default base_url, so theirs is optional.
	if in.Provider != "anthropic" && in.Provider != "openrouter" && in.BaseURL == "" {
		return fmt.Errorf("agents: base_url required for provider %q: %w", in.Provider, errs.ErrValidation)
	}
```

- [ ] **Step 5: Run to verify pass**

Run: `export PATH="$HOME/go/bin:$PATH" && go build ./... && go test ./internal/agents/ -run 'TestValidateOpenRouterBaseURLOptional|TestKnownProvidersTrackEnum' -v`
Expected: both PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/agents/credential.go internal/agents/*_test.go
git commit -m "feat(agents): allow openrouter provider (base_url optional)"
```

---

## Task 3: Factory dispatch (`internal/platform/ai/factory.go`)

**Files:**
- Modify: `internal/platform/ai/factory.go`
- Test: `internal/platform/ai/factory_test.go` (modify/create)

**Context (verified):** `factory.go:16-21` declares `Provider` string consts (`ProviderAnthropic`, `ProviderOpenAI`, `ProviderOllama`, `ProviderVLLM`) under a `// Keep in lockstep` comment. `New(cred Credential)` (factory.go:44-60) wraps an SSRF-guarded `*http.Client` then switches on `cred.Provider`; the `openai/ollama/vllm` case errors if `cred.BaseURL == ""`, else returns `NewOpenAICompatProvider(cred.APIKey, cred.BaseURL, cred.Model, hc)`. The OpenAI-compat client POSTs to `<base_url>/chat/completions` with `Authorization: Bearer`.

- [ ] **Step 1: Write the failing tests**

Read `internal/platform/ai/factory_test.go` (or `openaicompat_test.go`) for the existing test style + the concrete provider type name. Add (use the same package as the existing factory tests — likely in-package `ai` so it can inspect the concrete type's `baseURL` field):

```go
func TestNew_OpenRouterDefaultsBaseURL(t *testing.T) {
	p, err := New(Credential{Provider: ProviderOpenRouter, APIKey: "k", Model: "anthropic/claude-3.5-sonnet"})
	if err != nil {
		t.Fatalf("openrouter with empty base_url should be ok, got %v", err)
	}
	// Assert the default base_url was applied. Type-assert to the concrete OpenAI-compat
	// provider and read its baseURL. CONFIRM the concrete type name + field by reading
	// openaicompat.go (e.g. *openAICompatProvider with field baseURL); adjust the assertion.
	oc, ok := p.(*openAICompatProvider)
	if !ok {
		t.Fatalf("want *openAICompatProvider, got %T", p)
	}
	if oc.baseURL != openRouterBaseURL {
		t.Fatalf("baseURL = %q, want %q", oc.baseURL, openRouterBaseURL)
	}
}

func TestNew_OpenRouterRespectsCustomBaseURL(t *testing.T) {
	p, err := New(Credential{Provider: ProviderOpenRouter, APIKey: "k", Model: "m", BaseURL: "https://gw.example/v1"})
	if err != nil {
		t.Fatal(err)
	}
	oc := p.(*openAICompatProvider)
	if oc.baseURL != "https://gw.example/v1" {
		t.Fatalf("baseURL = %q, want custom", oc.baseURL)
	}
}
```

(If the existing factory tests use a different assertion mechanism — e.g. an httptest round-trip rather than field inspection — mirror that instead; the invariants to assert are: openrouter+empty-base_url returns no error, and the effective base is `openRouterBaseURL`; openrouter+custom-base_url uses the custom one. The concrete type name comes from `NewOpenAICompatProvider`'s return type in `openaicompat.go`.)

- [ ] **Step 2: Run to verify failure**

Run: `export PATH="$HOME/go/bin:$PATH" && go test ./internal/platform/ai/ -run TestNew_OpenRouter -v`
Expected: FAIL — `ProviderOpenRouter` / `openRouterBaseURL` undefined.

- [ ] **Step 3: Add the const + dispatch case**

In `internal/platform/ai/factory.go`, add to the `Provider` const block (under the `// Keep in lockstep` comment, alongside the others):

```go
	ProviderOpenRouter Provider = "openrouter"
```

Add a package-level const (near the top, or beside the anthropic default URL if one exists):

```go
// openRouterBaseURL is OpenRouter's OpenAI-compatible API base. Used when an
// openrouter credential leaves base_url empty.
const openRouterBaseURL = "https://openrouter.ai/api/v1"
```

In `New`'s switch, add a case BEFORE the `default:` (and separate from the openai/ollama/vllm case, because openrouter defaults base_url rather than requiring it):

```go
	case ProviderOpenRouter:
		base := cred.BaseURL
		if base == "" {
			base = openRouterBaseURL
		}
		return NewOpenAICompatProvider(cred.APIKey, base, cred.Model, hc), nil
```

- [ ] **Step 4: Run to verify pass**

Run: `export PATH="$HOME/go/bin:$PATH" && go build ./... && go test ./internal/platform/ai/ -run TestNew_OpenRouter -v && go test ./internal/platform/ai/`
Expected: build exit 0; new tests PASS; full ai package PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/platform/ai/factory.go internal/platform/ai/factory_test.go
git commit -m "feat(ai): openrouter provider via OpenAI-compat client (default base_url)"
```

---

## Task 4: OpenAPI — add openrouter to the provider enums

**Files:**
- Modify: `specs/003-agent-runtime/contracts/openapi.yaml`

**Context:** The credential schemas `AICredential` and `CreateAICredentialRequest` (added in the prior feature) have `provider: { type: string, enum: [anthropic, openai, ollama, vllm] }`. The contract drift test asserts route↔spec parity but the schema enum is doc-only; still, keep it accurate.

- [ ] **Step 1: Add `openrouter` to both provider enums**

In `specs/003-agent-runtime/contracts/openapi.yaml`, find the two `enum: [anthropic, openai, ollama, vllm]` lines (under `AICredential.properties.provider` and `CreateAICredentialRequest.properties.provider`) and change each to:

```yaml
        provider: { type: string, enum: [anthropic, openai, ollama, vllm, openrouter] }
```

- [ ] **Step 2: Validate + contract test**

Run:
```bash
python3 -c "import yaml; yaml.safe_load(open('specs/003-agent-runtime/contracts/openapi.yaml')); print('YAML OK')"
export PATH="$HOME/go/bin:$PATH" && go test -tags contract ./cmd/...
```
Expected: `YAML OK`; contract test `ok`.

- [ ] **Step 3: Commit**

```bash
git add specs/003-agent-runtime/contracts/openapi.yaml
git commit -m "docs(openapi): add openrouter to credential provider enum"
```

---

## Task 5: Frontend — union + credential form option + base_url prefill

**Files:**
- Modify: `web/src/app/core/ai-credentials.service.ts`
- Modify: `web/src/app/pages/credentials/ai/credential-form.ts` (+ `credential-form.spec.ts`)

**Context (verified):** `AIProvider` union is at `ai-credentials.service.ts:5` (`'anthropic' | 'openai' | 'ollama' | 'vllm'`). credential-form's provider select binds `[ngModel]="provider()" (ngModelChange)="provider.set($event)"` (line 18); `baseUrl` is a plain field `[(ngModel)]="baseUrl"` (line 42). There is NO existing `onProviderChange` handler in this form. Tests use vitest.

- [ ] **Step 1: Add `openrouter` to the union**

In `web/src/app/core/ai-credentials.service.ts`, change:

```ts
export type AIProvider = 'anthropic' | 'openai' | 'ollama' | 'vllm' | 'openrouter';
```

- [ ] **Step 2: Write the failing spec — selecting openrouter prefills base_url**

In `web/src/app/pages/credentials/ai/credential-form.spec.ts`, add (vitest — `import { ... } from 'vitest'` already present; mirror the existing tests):

```ts
it('prefills the OpenRouter base URL when openrouter is selected', () => {
  const c = fixture.componentInstance;
  expect(c.baseUrl).toBe('');
  c.onProviderChange('openrouter');
  expect(c.provider()).toBe('openrouter');
  expect(c.baseUrl).toBe('https://openrouter.ai/api/v1');
  // switching away to a provider that needs an explicit base_url clears the prefill
  c.onProviderChange('openai');
  expect(c.baseUrl).toBe('');
});
```

- [ ] **Step 3: Run to verify failure**

Run: `cd web && npm test`
Expected: FAIL — `onProviderChange` is not a function on the component.

- [ ] **Step 4: Add the option, the handler, and the const**

In `web/src/app/pages/credentials/ai/credential-form.ts`:

(a) Add the option to the provider select (after the vllm option, lines 19-22):
```html
          <option value="openrouter">OpenRouter</option>
```

(b) Change the select's change binding (line 18) from `(ngModelChange)="provider.set($event)"` to:
```html
                [ngModel]="provider()" (ngModelChange)="onProviderChange($event)" name="provider" [disabled]="submitting()">
```

(c) Add a const near the top of the file (after imports) and the handler method (near the other methods, e.g. before `submit()`):
```ts
const OPENROUTER_BASE_URL = 'https://openrouter.ai/api/v1';
```
```ts
  onProviderChange(p: AIProvider): void {
    // OpenRouter has one canonical OpenAI-compatible base URL — prefill it so the
    // user just pastes a key. Only auto-manage base_url between blank and the default,
    // so a custom value the user typed for another provider isn't clobbered silently.
    if (p === 'openrouter' && this.baseUrl === '') {
      this.baseUrl = OPENROUTER_BASE_URL;
    } else if (this.provider() === 'openrouter' && this.baseUrl === OPENROUTER_BASE_URL) {
      this.baseUrl = '';
    }
    this.provider.set(p);
  }
```

(d) Update the base_url helper text (line 43) to mention OpenRouter:
```html
        <small class="mf-hint">Defaulted for OpenRouter. Needed for OpenAI-compatible or self-hosted (Ollama/vLLM) endpoints; leave blank for the provider default.</small>
```

(e) If `reset()` (lines 112-118) sets `provider` back to a default, ensure it also resets `baseUrl = ''` (it already does per the existing reset; confirm and leave as-is).

- [ ] **Step 5: Run to verify pass**

Run: `cd web && npm test`
Expected: the new spec PASSES; suite green. Then `cd web && npm run build`.

- [ ] **Step 6: Commit**

```bash
git add web/src/app/core/ai-credentials.service.ts web/src/app/pages/credentials/ai/credential-form.ts web/src/app/pages/credentials/ai/credential-form.spec.ts
git commit -m "feat(web): OpenRouter credential option with base_url prefill"
```

---

## Task 6: Frontend — agent form option + free-text model rename

**Files:**
- Modify: `web/src/app/pages/agents/agent-form.ts` (+ `agent-form.spec.ts`)

**Context (verified):** agent-form.ts has `const SELF_HOST: AIProvider[] = ['ollama', 'vllm'];` (line 9), `isSelfHost = computed(() => SELF_HOST.includes(this.provider()));` (line 146), used by the template `@if (isSelfHost())` (line 40) to switch between the free-text `agent-model-text` input and the catalog `agent-model-select` dropdown. The provider select already calls `onProviderChange` (line 29). These are the ONLY 3 occurrences of the two symbols (all in this file). OpenRouter models aren't in `model_pricing`, so openrouter must use the free-text input.

- [ ] **Step 1: Write the failing spec — openrouter uses free-text model input**

In `web/src/app/pages/agents/agent-form.spec.ts`, add (vitest; the existing spec flushes the metadata endpoints in a `flushMetadata` helper — reuse it):

```ts
it('uses the free-text model input for openrouter (not the catalog dropdown)', () => {
  const c = fixture.componentInstance;
  c.onProviderChange('openrouter');
  expect(c.isFreeTextModel()).toBe(true);
  // anthropic stays a catalog dropdown
  c.onProviderChange('anthropic');
  expect(c.isFreeTextModel()).toBe(false);
});
```

- [ ] **Step 2: Run to verify failure**

Run: `cd web && npm test`
Expected: FAIL — `isFreeTextModel` is not a function (it's currently `isSelfHost`).

- [ ] **Step 3: Rename + add openrouter, add the option, update helper text**

In `web/src/app/pages/agents/agent-form.ts`:

(a) Line 9 — rename the const and add openrouter:
```ts
// Providers whose models are NOT in the model_pricing catalog → free-text model entry.
const FREE_TEXT_MODEL_PROVIDERS: AIProvider[] = ['ollama', 'vllm', 'openrouter'];
```

(b) Line 146 — rename the computed:
```ts
  isFreeTextModel = computed(() => FREE_TEXT_MODEL_PROVIDERS.includes(this.provider()));
```

(c) Line 40 (template) — rename the guard:
```html
        @if (isFreeTextModel()) {
```

(d) Add the option to the provider select (after the vllm option, lines 30-33):
```html
          <option value="openrouter">OpenRouter</option>
```

(e) Update the free-text input placeholder (line ~42) to hint an OpenRouter model id form:
```html
                 [(ngModel)]="model" placeholder="e.g. llama3.1:70b or anthropic/claude-3.5-sonnet" />
```

(f) `grep -n "isSelfHost\|SELF_HOST" web/src/app/pages/agents/agent-form.ts` — confirm ZERO remaining occurrences after the rename.

- [ ] **Step 4: Run to verify pass**

Run: `cd web && npm test`
Expected: the new spec PASSES; the existing agent-form specs still PASS (the rename didn't break the `modelsForProvider` dropdown path). Then `cd web && npm run build`.

- [ ] **Step 5: Commit**

```bash
git add web/src/app/pages/agents/agent-form.ts web/src/app/pages/agents/agent-form.spec.ts
git commit -m "feat(web): OpenRouter agent option with free-text model entry"
```

---

## Task 7: Full verification & PR

- [ ] **Step 1: Backend gates**

Run: `export PATH="$HOME/go/bin:$PATH" && go build ./... && make test && make sec-test && make lint && go test -tags contract ./cmd/...`
Expected: all exit 0 / `ok`.

- [ ] **Step 2: Frontend gates**

Run: `cd web && npm run build && npm test`
Then (dev server on :4300 up): `cd web && npm run e2e -- e2e/ai-credentials.spec.ts && npm run e2e -- e2e/agents.spec.ts`
Expected: build + vitest pass; the existing credential/agent e2e still pass (the new option doesn't break them).

- [ ] **Step 3: Manual smoke (real stack, optional)**

With `MANYFORGE_AI_MASTER_KEY` set on the backend, web on :4300, log in (`live-demo@manyforge.test` / `DevPassw0rd!`): on `/credentials/ai` add an **OpenRouter** credential (base_url auto-fills; paste a key + a model like `anthropic/claude-3.5-sonnet`), confirm it saves and lists. On `/agents`, create an agent with provider **OpenRouter** and confirm the model field is free-text.

- [ ] **Step 4: Open the PR / land**

```bash
git push -u origin openrouter-provider
gh pr create --base master --title "OpenRouter as a first-class AI provider" --body "Implements docs/superpowers/specs/2026-06-15-openrouter-provider-design.md (bd manyforge-eca). Adds 'openrouter' to the ai_provider enum end-to-end, reusing the OpenAI-compatible client (defaults base_url to https://openrouter.ai/api/v1). Free-text model entry; models bill \$0 until priced."
```

- [ ] **Step 5: Update bd**

Run: `export PATH="$HOME/go/bin:$PATH" && bd close manyforge-eca` then a `chore(bd): close manyforge-eca` commit.

---

## Self-Review (completed by plan author)

- **Spec coverage:** enum lockstep — schema.sql (T1), migration (T1), sqlc/dbgen (T1), factory const+dispatch (T3), knownProviders (T2), frontend union+2 forms (T5/T6), OpenAPI enum (T4) — all 8 spots have a task ✓. base_url default in factory ✓ (T3); base_url optional in credential.go ✓ (T2); free-text model rename ✓ (T6); base_url prefill ✓ (T5); $0-cost limitation noted (spec) ✓. Test plan: factory tests (T3), validate test + enum-track (T2), contract (T4), frontend specs (T5/T6) ✓.
- **Placeholder scan:** the only deferred specifics are "confirm the concrete OpenAI-compat type name from openaicompat.go" (T3) and "find the file containing TestKnownProvidersTrackEnum / adapt its assertion" (T2) — both name the exact file to read and the exact invariant. No "TBD"/"add validation"/"similar to" placeholders.
- **Type/name consistency:** `ProviderOpenRouter`/`openRouterBaseURL` (T3) referenced consistently; `AiProviderOpenrouter` (dbgen, T1) used in `knownProviders` (T2); `OPENROUTER_BASE_URL` (T5 credential-form) and `openRouterBaseURL` (T3 Go) are intentionally separate per-layer constants with the same value; `FREE_TEXT_MODEL_PROVIDERS`/`isFreeTextModel` (T6) replace `SELF_HOST`/`isSelfHost` in all 3 occurrences. The frontend `AIProvider` union (T5) is the single source of truth consumed by both forms.
- **Sequencing:** T1 (enum) must precede T2 (uses `dbgen.AiProviderOpenrouter`) and T3. T5 (union) should precede T6 (agent-form also imports `AIProvider`) — though either order compiles once the union has openrouter. Each task ends green and commits independently.
