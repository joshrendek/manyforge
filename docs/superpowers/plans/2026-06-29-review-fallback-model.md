# Code-review Fallback Model Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** On a code-review retry, switch to a faster provider-compatible fallback model so a slow model that 504'd (OpenRouter upstream idle timeout) doesn't just fail again.

**Architecture:** Two pure functions (`reviewFallbackModel` provider→fallback map; `effectiveReviewModel` applies it on `attempts >= 2`) plus one override line in `runJob` (`cred.Model = effectiveReviewModel(...)`) with an audit event when the model changes. Host-side Go only — no entrypoint or image change.

**Tech Stack:** Go (stdlib only).

## Global Constraints

- **Spec:** `docs/superpowers/specs/2026-06-29-review-fallback-model-design.md`. Issue `manyforge-206`.
- **Fallback map (exact slugs):** `openrouter`→`google/gemini-2.5-flash`; `anthropic`→`claude-sonnet-4-6`; `openai`→`gpt-4o-mini`; every other provider (`ollama`, `vllm`, unknown) → `""` (no fallback).
- **Escalation trigger:** any retry — `job.Attempts >= 2`. The first attempt (`attempts == 1`) always uses the configured model.
- **`effectiveReviewModel` returns the fallback only when** `attempts >= 2` AND the provider has a fallback AND it differs from the configured model; otherwise the configured model.
- **Audit event** when the model actually changes: `agent.coding.review.fallback_model` with inputs `{"configured": <model>, "fallback": <fb>, "attempt": <attempts>}` via the existing `s.auditStep(...)`.
- **No PR-body note, no env override, no schema change, no image rebuild** (out of scope per spec).
- **Verification gates (before push):** `go test ./internal/agents/coding/... ./internal/connectors/...`; `go vet ./...`; `make lint`; `go test -tags contract ./cmd/...`; `make sec-test`; integration `go test -tags integration -p 1 ./internal/agents/coding/`.
- **gopls lies after edits:** phantom `dbgen.* undefined` / `undefined` diagnostics are stale; `go build`/`go test` is truth.

---

### Task 1: `reviewFallbackModel` + `effectiveReviewModel`

Two pure functions in a new file. No dependencies on other tasks.

**Files:**
- Create: `internal/agents/coding/fallback.go`
- Test: `internal/agents/coding/fallback_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `func reviewFallbackModel(provider string) string`
  - `func effectiveReviewModel(provider, configuredModel string, attempts int) string`

- [ ] **Step 1: Write the failing tests**

Create `internal/agents/coding/fallback_test.go`:

```go
package coding

import "testing"

func TestReviewFallbackModel(t *testing.T) {
	cases := map[string]string{
		"openrouter": "google/gemini-2.5-flash",
		"anthropic":  "claude-sonnet-4-6",
		"openai":     "gpt-4o-mini",
		"ollama":     "",
		"vllm":       "",
		"unknown":    "",
		"":           "",
	}
	for provider, want := range cases {
		if got := reviewFallbackModel(provider); got != want {
			t.Errorf("reviewFallbackModel(%q) = %q, want %q", provider, got, want)
		}
	}
}

func TestEffectiveReviewModel(t *testing.T) {
	type tc struct {
		provider, configured string
		attempts             int
		want                 string
	}
	for _, c := range []tc{
		{"openrouter", "google/gemini-2.5-pro", 1, "google/gemini-2.5-pro"}, // first attempt → configured
		{"openrouter", "google/gemini-2.5-pro", 2, "google/gemini-2.5-flash"}, // retry → fallback
		{"openrouter", "google/gemini-2.5-pro", 3, "google/gemini-2.5-flash"}, // later retry → fallback
		{"ollama", "qwen2.5-coder:14b", 2, "qwen2.5-coder:14b"},               // no fallback → configured
		{"openrouter", "google/gemini-2.5-flash", 2, "google/gemini-2.5-flash"}, // already fallback → unchanged
	} {
		if got := effectiveReviewModel(c.provider, c.configured, c.attempts); got != c.want {
			t.Errorf("effectiveReviewModel(%q,%q,%d) = %q, want %q", c.provider, c.configured, c.attempts, got, c.want)
		}
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/agents/coding/ -run 'TestReviewFallbackModel|TestEffectiveReviewModel' -v`
Expected: FAIL — `undefined: reviewFallbackModel`, `undefined: effectiveReviewModel`.

- [ ] **Step 3: Implement the functions**

Create `internal/agents/coding/fallback.go`:

```go
package coding

// reviewFallbackModel returns a faster, provider-compatible model to retry a cloud
// review with after the configured model fails (e.g. an OpenRouter 504 from a slow
// reasoning model's long time-to-first-token). "" means no fallback for this
// provider — the retry uses the same model (today's behavior). ollama/vllm run
// host-side and never hit OpenRouter, so they have no entry (manyforge-206).
func reviewFallbackModel(provider string) string {
	switch provider {
	case "openrouter":
		return "google/gemini-2.5-flash"
	case "anthropic":
		return "claude-sonnet-4-6"
	case "openai":
		return "gpt-4o-mini"
	default:
		return ""
	}
}

// effectiveReviewModel returns the model a review attempt should use: the configured
// model on the first attempt, and (on any retry, attempts >= 2) the provider fallback
// when one exists and differs from the configured model.
func effectiveReviewModel(provider, configuredModel string, attempts int) string {
	if attempts >= 2 {
		if fb := reviewFallbackModel(provider); fb != "" && fb != configuredModel {
			return fb
		}
	}
	return configuredModel
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/agents/coding/ -run 'TestReviewFallbackModel|TestEffectiveReviewModel' -v`
Expected: PASS (both).

- [ ] **Step 5: Commit**

```bash
git add internal/agents/coding/fallback.go internal/agents/coding/fallback_test.go
git commit -m "feat(007): fallback-model selection for code-review retries (manyforge-206)"
```

---

### Task 2: Wire the override into `runJob` + integration test

Apply the fallback in `runJob` and prove end-to-end that a retry drives the sandbox with the fallback model.

**Files:**
- Modify: `internal/agents/coding/service.go` (override + audit, after line 231)
- Test: `internal/agents/coding/service_integration_test.go` (recording runner + escalation test)

**Interfaces:**
- Consumes: `effectiveReviewModel` (Task 1); existing `s.auditStep`, `ptr`, `ClaimedReview`, `FakeCredResolver`, `startCoding`, `newCodingEnv`, `createRepoConnector`, `startGitHubStub`, `buildService`.
- Produces: nothing new (behavior change only).

- [ ] **Step 1: Write the failing integration test**

Append to `internal/agents/coding/service_integration_test.go` (it already has the `//go:build integration` tag and imports `context`, `encoding/json`, `os`, `path/filepath`, `sandbox`):

```go
// recordingRunner captures the SandboxSpec.Env it was invoked with and writes a
// valid (empty-findings) review.json so the run succeeds.
type recordingRunner struct{ env map[string]string }

func (r *recordingRunner) Run(_ context.Context, spec sandbox.SandboxSpec) (sandbox.SandboxResult, error) {
	r.env = spec.Env
	data, _ := json.Marshal(map[string]any{"summary": "ok", "findings": []any{}})
	_ = os.WriteFile(filepath.Join(spec.OutputDir, "review.json"), data, 0o644)
	return sandbox.SandboxResult{ExitCode: 0}, nil
}

// TestCodeReviewFallbackModelOnRetry: a fresh attempt uses the configured model; a
// retry (Attempts>=2) drives the sandbox with the provider fallback (manyforge-206).
func TestCodeReviewFallbackModelOnRetry(t *testing.T) {
	ctx, tdb, seed := startCoding(t)
	prJSON := []byte(`{"number":1,"title":"T","state":"open","merged":false,"head":{"sha":"abc123","ref":"f"},"base":{"ref":"main"}}`)
	localSrv, _ := startGitHubStub(t, prJSON)
	fakeCred := &FakeCredResolver{Cred: AICredential{
		APIKey: "k", BaseURL: "https://openrouter.ai/api/v1", Model: "google/gemini-2.5-pro", Provider: "openrouter",
	}}
	env := newCodingEnv(t, tdb)
	connID := createRepoConnector(ctx, t, env, seed, localSrv.URL)

	run := func(pr, attempts int) string {
		rr := &recordingRunner{}
		svc := buildService(t, tdb, env, rr, fakeCred)
		cr, err := svc.Enqueue(ctx, seed.principalID, seed.businessID, seed.agentID, connID, pr)
		if err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		claimed := ClaimedReview{
			ID: cr.ID, BusinessID: seed.businessID, PrincipalID: seed.principalID,
			AgentID: seed.agentID, RepoConnectorID: connID, PRNumber: pr, Attempts: attempts,
		}
		if err := svc.runJob(ctx, claimed); err != nil {
			t.Fatalf("runJob(attempts=%d): %v", attempts, err)
		}
		return rr.env["LLM_MODEL"]
	}

	if got := run(1, 1); got != "google/gemini-2.5-pro" {
		t.Fatalf("attempts=1 should use the configured model, got %q", got)
	}
	if got := run(2, 2); got != "google/gemini-2.5-flash" {
		t.Fatalf("attempts=2 should use the fallback model, got %q", got)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test -tags integration -p 1 ./internal/agents/coding/ -run TestCodeReviewFallbackModelOnRetry -v`
Expected: FAIL at the `attempts=2` assertion — without the override, the sandbox still gets `google/gemini-2.5-pro` (got `"google/gemini-2.5-pro"`, want `"google/gemini-2.5-flash"`).

- [ ] **Step 3: Add the override + audit in `runJob`**

In `internal/agents/coding/service.go`, insert directly after the `businessID := job.BusinessID` line (currently line 231), before the `// Fetch PR metadata` comment:

```go

	// Graceful degradation (manyforge-206): on a retry, switch to a faster
	// provider-compatible fallback model so a slow model that 504'd (OpenRouter
	// upstream idle timeout) doesn't just fail again on every attempt.
	if m := effectiveReviewModel(cred.Provider, cred.Model, job.Attempts); m != cred.Model {
		_ = s.auditStep(ctx, principalID, businessID, crID,
			"agent.coding.review.fallback_model",
			map[string]any{"configured": cred.Model, "fallback": m, "attempt": job.Attempts},
			nil, ptr("executed"),
		)
		cred.Model = m
	}
```

- [ ] **Step 4: Run the integration test to verify it passes**

Run: `go build ./internal/agents/coding/ && go test -tags integration -p 1 ./internal/agents/coding/ -run TestCodeReviewFallbackModelOnRetry -v`
Expected: PASS — `attempts=1` → `google/gemini-2.5-pro`, `attempts=2` → `google/gemini-2.5-flash`.

- [ ] **Step 5: Run vet + the package unit suite**

Run: `go vet ./internal/agents/coding/ && go test ./internal/agents/coding/`
Expected: PASS, vet clean (the override is a no-op for non-fallback providers, so existing tests are unaffected).

- [ ] **Step 6: Commit**

```bash
git add internal/agents/coding/service.go internal/agents/coding/service_integration_test.go
git commit -m "feat(007): use fallback model on code-review retry + audit (manyforge-206)"
```

---

### Task 3: Full gate (controller-run)

**Files:** none.

- [ ] **Step 1: Full gate**

```bash
go test ./internal/agents/coding/... ./internal/connectors/... && \
go vet ./... && make lint && \
go test -tags contract ./cmd/... && make sec-test && \
go test -tags integration -p 1 ./internal/agents/coding/
```
Expected: all PASS.

- [ ] **Step 2: Update tracking**

Update `HANDOFF.md` and close `manyforge-206` in bd. Commit any doc/bd changes.

---

## Self-Review

**Spec coverage:**
- `reviewFallbackModel` map (3 providers + none) → Task 1. ✓
- `effectiveReviewModel` (attempts>=2 gate, differs-from-configured) → Task 1. ✓
- `runJob` override + audit event → Task 2. ✓
- Integration proof (attempts→model in Env) → Task 2. ✓
- No image rebuild / schema / env override / body note → honored (no task adds them). ✓
- Gates → Task 3. ✓

**Placeholder scan:** No TBD/TODO; every code step shows complete code. ✓

**Type consistency:** `reviewFallbackModel(provider string) string`, `effectiveReviewModel(provider, configuredModel string, attempts int) string`, override uses `cred.Provider`/`cred.Model`/`job.Attempts` (all confirmed in scope at service.go:219-231), audit via `s.auditStep(ctx, principalID, businessID, crID, action, inputs, outputs, decision)` with `ptr(...)` — all consistent with the existing code. ✓
```
