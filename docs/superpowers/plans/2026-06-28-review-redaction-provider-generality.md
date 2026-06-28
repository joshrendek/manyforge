# Review Secret-Redaction + Provider-Generality Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Strip secrets from stored/posted code-review output, and generalize the sandbox/opencode path from OpenRouter-only to anthropic + openai + openrouter.

**Architecture:** A small `redactSecrets` helper (exact known-value replacement + specific key-pattern regexes) is applied at the two trust boundaries in `runJob` — the sandbox stderr tail (→ `last_error`/audit) and the model findings doc (→ posted body + stored summary). Separately, `runJob` forwards `LLM_PROVIDER` to the sandbox and `entrypoint.sh` validates a known-good provider set and parameterizes the model prefix + `auth.json` key.

**Tech Stack:** Go (stdlib `regexp`/`strings`), POSIX sh entrypoint, Docker for the sandbox image.

## Global Constraints

- **Spec:** `docs/superpowers/specs/2026-06-28-review-redaction-provider-generality-design.md`. Issue `manyforge-fqo` #2/#3 (epic `manyforge-7ml`).
- **Redaction strategy:** exact known-secret replacement (LLM key `cred.APIKey` + GitHub token `rc.Credential.APIToken`, only values with `len ≥ 8`) PLUS regex scrub of common key shapes. Marker is the literal string `[REDACTED]`.
- **Regex patterns (specific, to limit over-redaction):** `sk-[A-Za-z0-9_\-]{20,}`, `ghp_[A-Za-z0-9]{36}`, `github_pat_[A-Za-z0-9_]{50,}`, `(?i)bearer\s+[A-Za-z0-9._\-]{16,}`.
- **Apply redaction at exactly two boundaries in `runJob`:** `sandboxStderrTail` (redacts its returned tail) and the model doc (redact `doc` right before `buildReview`). All other `runJob` errors are our own literals.
- **Cloud providers supported via the sandbox:** `openrouter`, `anthropic`, `openai`. `ollama`/`vllm` run host-side (unchanged). **No** `credresolver.go` change (openai already requires a user-supplied base_url; see `internal/platform/ai/factory.go:New`).
- **entrypoint.sh:** validate `LLM_PROVIDER` ∈ `{openrouter,anthropic,openai}` (exit non-zero otherwise); `MODEL="${LLM_PROVIDER}/${LLM_MODEL}"`; `auth.json = {"${LLM_PROVIDER}":{"type":"api","key":"…"}}`. Keep the three copies of nothing here (the review prompt is unchanged).
- **Security-fix discipline:** add `MF007-PIN-11` in `internal/security_regression/` (source pins; the next free pin number — MF007-PIN-10 is taken).
- **Verification gates (before push):** `go test ./internal/agents/coding/... ./internal/connectors/...`; `go vet ./...`; `make lint`; `make sec-test`; `go test -tags contract ./cmd/...`; integration `go test -tags integration -p 1 ./internal/agents/coding/`.
- **gopls lies after edits:** phantom `dbgen.* undefined` / `undefined` diagnostics are stale; `go build`/`go test` is truth.

---

### Task 1: `redactSecrets` + `redactDoc` helper

Pure functions in a new focused file. No dependencies on other tasks.

**Files:**
- Create: `internal/agents/coding/redact.go`
- Test: `internal/agents/coding/redact_test.go`

**Interfaces:**
- Consumes: `FindingsDoc` and `connectors.Finding` (existing).
- Produces:
  - `func redactSecrets(s string, known ...string) string`
  - `func redactDoc(doc *FindingsDoc, known ...string)`

- [ ] **Step 1: Write the failing tests**

Create `internal/agents/coding/redact_test.go`:

```go
package coding

import (
	"strings"
	"testing"

	"github.com/manyforge/manyforge/internal/connectors"
)

func TestRedactSecrets_ExactKnown(t *testing.T) {
	out := redactSecrets("auth failed for key abcd1234efgh5678 now", "abcd1234efgh5678")
	if strings.Contains(out, "abcd1234efgh5678") {
		t.Fatalf("known secret not redacted: %s", out)
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Fatalf("expected marker: %s", out)
	}
}

func TestRedactSecrets_ShortKnownIgnored(t *testing.T) {
	// A <8-char known value must not be replaced (would mangle normal text).
	if out := redactSecrets("the cat sat", "cat"); out != "the cat sat" {
		t.Fatalf("short known should be ignored: %s", out)
	}
}

func TestRedactSecrets_Patterns(t *testing.T) {
	for _, c := range []string{
		"key sk-abcdefghij0123456789ABCD end",
		"token ghp_0123456789abcdefghij0123456789abcdef end",
		"Authorization: Bearer abcdef0123456789xyz",
	} {
		if out := redactSecrets(c); !strings.Contains(out, "[REDACTED]") {
			t.Fatalf("pattern not redacted: %q -> %q", c, out)
		}
	}
}

func TestRedactSecrets_NoFalsePositive(t *testing.T) {
	in := "The function returns nil on error; consider wrapping it."
	if out := redactSecrets(in); out != in {
		t.Fatalf("false positive redaction: %q", out)
	}
}

func TestRedactDoc(t *testing.T) {
	secret := "sk-LIVEKEY0123456789abcdef"
	doc := FindingsDoc{
		Summary:  "leaked " + secret,
		Findings: []connectors.Finding{{Title: "t " + secret, Detail: "d " + secret}},
	}
	redactDoc(&doc, secret)
	if strings.Contains(doc.Summary, secret) ||
		strings.Contains(doc.Findings[0].Title, secret) ||
		strings.Contains(doc.Findings[0].Detail, secret) {
		t.Fatalf("doc not fully redacted: %+v", doc)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/agents/coding/ -run 'TestRedact' -v`
Expected: FAIL — `undefined: redactSecrets`, `undefined: redactDoc`.

- [ ] **Step 3: Implement the helper**

Create `internal/agents/coding/redact.go`:

```go
package coding

import (
	"regexp"
	"strings"
)

const redactedMarker = "[REDACTED]"

// secretPatterns scrub common API-key shapes for secrets we don't hold verbatim.
// Patterns are specific (known prefixes + length floors) to limit over-redaction.
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`sk-[A-Za-z0-9_\-]{20,}`),            // OpenAI/Anthropic/OpenRouter
	regexp.MustCompile(`ghp_[A-Za-z0-9]{36}`),               // GitHub PAT (classic)
	regexp.MustCompile(`github_pat_[A-Za-z0-9_]{50,}`),      // GitHub fine-grained PAT
	regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._\-]{16,}`), // bearer auth header
}

// redactSecrets removes secrets from text bound for storage or a posted review:
// first the exact known values (e.g. the LLM key / GitHub token we hold), then a
// regex scrub of common key shapes for secrets we don't hold. Known values shorter
// than 8 chars are ignored so a trivial value can't mangle unrelated text.
func redactSecrets(s string, known ...string) string {
	for _, k := range known {
		if len(k) >= 8 {
			s = strings.ReplaceAll(s, k, redactedMarker)
		}
	}
	for _, re := range secretPatterns {
		s = re.ReplaceAllString(s, redactedMarker)
	}
	return s
}

// redactDoc scrubs secrets from a findings doc before it is posted to the PR (and
// stored on the review row). Mutates the doc in place.
func redactDoc(doc *FindingsDoc, known ...string) {
	doc.Summary = redactSecrets(doc.Summary, known...)
	for i := range doc.Findings {
		doc.Findings[i].Title = redactSecrets(doc.Findings[i].Title, known...)
		doc.Findings[i].Detail = redactSecrets(doc.Findings[i].Detail, known...)
	}
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/agents/coding/ -run 'TestRedact' -v`
Expected: PASS (all 5).

- [ ] **Step 5: Commit**

```bash
git add internal/agents/coding/redact.go internal/agents/coding/redact_test.go
git commit -m "feat(007): redactSecrets + redactDoc helper (manyforge-fqo #2)"
```

---

### Task 2: Apply redaction at the `runJob` trust boundaries

Wire the redactor into `sandboxStderrTail` and the model doc. Changing `sandboxStderrTail`'s signature requires updating its two call sites in the same task.

**Files:**
- Modify: `internal/agents/coding/service.go` (`sandboxStderrTail` + its 2 call sites; `redactDoc` before `buildReview`)
- Test: `internal/agents/coding/service_test.go`

**Interfaces:**
- Consumes: `redactSecrets`, `redactDoc` (Task 1); `cred.APIKey`, `rc.Credential.APIToken` (in scope in `runJob`).
- Produces: `func sandboxStderrTail(outDir string, secrets ...string) string`.

- [ ] **Step 1: Write the failing test**

Append to `internal/agents/coding/service_test.go` (package `coding`). If the file's import block lacks them, add `"os"`, `"path/filepath"`, `"strings"`:

```go
func TestSandboxStderrTail_Redacts(t *testing.T) {
	dir := t.TempDir()
	secret := "sk-LIVE0123456789abcdefghij"
	if err := os.WriteFile(filepath.Join(dir, "stderr.log"),
		[]byte("Error: Unauthorized: bad key "+secret+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tail := sandboxStderrTail(dir, secret)
	if strings.Contains(tail, secret) {
		t.Fatalf("secret leaked in stderr tail: %s", tail)
	}
	if !strings.Contains(tail, "[REDACTED]") {
		t.Fatalf("expected redaction marker: %s", tail)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/agents/coding/ -run TestSandboxStderrTail_Redacts -v`
Expected: FAIL — `too many arguments in call to sandboxStderrTail` (it currently takes only `outDir`).

- [ ] **Step 3: Add the secrets param + redact in `sandboxStderrTail`**

In `internal/agents/coding/service.go`, change the signature and the final return of `sandboxStderrTail`:

```go
func sandboxStderrTail(outDir string, secrets ...string) string {
```

and its final `return` (currently `return " | sandbox stderr: " + s`):

```go
	if s == "" {
		return ""
	}
	return " | sandbox stderr: " + redactSecrets(s, secrets...)
```

(Leave the rest of the function — the read, the keep-loop, the `max` truncation — unchanged.)

- [ ] **Step 4: Update the two call sites + redact the doc**

In `internal/agents/coding/service.go`, update both `sandboxStderrTail(outDir)` call sites to pass the secrets:

```go
				fmt.Errorf("coding: no findings produced (exit %d): %w%s", res.ExitCode, ferr, sandboxStderrTail(outDir, cred.APIKey, rc.Credential.APIToken)),
```

```go
				fmt.Errorf("%w%s", perr, sandboxStderrTail(outDir, cred.APIKey, rc.Credential.APIToken)), tokensIn, tokensOut, costCents)
```

Then, immediately before the `buildReview` call (`review := buildReview(doc, changed, pr.HeadSHA, skippedFiles, omittedFiles)`), add:

```go
	// Strip any secret the sandbox/model echoed before it is stored on the review row
	// or posted to the PR (manyforge-fqo #2).
	redactDoc(&doc, cred.APIKey, rc.Credential.APIToken)
	review := buildReview(doc, changed, pr.HeadSHA, skippedFiles, omittedFiles)
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go build ./internal/agents/coding/ && go test ./internal/agents/coding/ -run 'TestSandboxStderrTail_Redacts|TestRedact' -v`
Expected: PASS. Then `go vet ./internal/agents/coding/` — clean.

- [ ] **Step 6: Commit**

```bash
git add internal/agents/coding/service.go internal/agents/coding/service_test.go
git commit -m "feat(007): redact secrets from stderr tail + posted review doc (manyforge-fqo #2)"
```

---

### Task 3: Forward `LLM_PROVIDER` + generalize the entrypoint

Extract the sandbox env into a unit-testable helper that includes `LLM_PROVIDER`, and parameterize `entrypoint.sh`.

**Files:**
- Modify: `internal/agents/coding/service.go` (`sandboxEnv` helper + use it in the spec)
- Modify: `deploy/sandbox/entrypoint.sh`
- Test: `internal/agents/coding/service_test.go`

**Interfaces:**
- Consumes: `AICredential` (existing).
- Produces: `func sandboxEnv(cred AICredential) map[string]string`.

- [ ] **Step 1: Write the failing test**

Append to `internal/agents/coding/service_test.go`:

```go
func TestSandboxEnv(t *testing.T) {
	env := sandboxEnv(AICredential{APIKey: "k", BaseURL: "https://api.openai.com", Model: "gpt-4o", Provider: "openai"})
	if env["LLM_PROVIDER"] != "openai" || env["LLM_MODEL"] != "gpt-4o" ||
		env["LLM_API_KEY"] != "k" || env["LLM_BASE_URL"] != "https://api.openai.com" {
		t.Fatalf("env = %+v", env)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/agents/coding/ -run TestSandboxEnv -v`
Expected: FAIL — `undefined: sandboxEnv`.

- [ ] **Step 3: Add `sandboxEnv` and use it**

In `internal/agents/coding/service.go`, add the helper (near `opencodeCmd`):

```go
// sandboxEnv builds the env the opencode entrypoint consumes. LLM_PROVIDER selects
// the opencode built-in provider (model prefix + auth.json key); LLM_BASE_URL is
// used only to derive the egress-allowlist host.
func sandboxEnv(cred AICredential) map[string]string {
	return map[string]string{
		"LLM_API_KEY":  cred.APIKey,
		"LLM_BASE_URL": cred.BaseURL,
		"LLM_MODEL":    cred.Model,
		"LLM_PROVIDER": cred.Provider,
	}
}
```

Then replace the inline `Env` map in the `SandboxSpec` literal:

```go
			Env:         sandboxEnv(cred),
```

(Replaces the 5-line `Env: map[string]string{ "LLM_API_KEY": …, "LLM_BASE_URL": …, "LLM_MODEL": … }`.)

- [ ] **Step 4: Run to verify it passes**

Run: `go build ./internal/agents/coding/ && go test ./internal/agents/coding/ -run TestSandboxEnv -v`
Expected: PASS.

- [ ] **Step 5: Generalize `entrypoint.sh`**

In `deploy/sandbox/entrypoint.sh`:

(a) Update the env header comment block (the `LLM_*` lines) to:

```sh
#   LLM_API_KEY   — provider API key (forwarded only to the allowlisted LLM host)
#   LLM_BASE_URL  — provider base URL (used only to derive the egress-allowlist host;
#                   opencode's built-in provider already knows its endpoint)
#   LLM_MODEL     — model slug, e.g. "google/gemini-2.5-pro" or "claude-3-5-sonnet"
#   LLM_PROVIDER  — opencode provider id: one of openrouter|anthropic|openai
```

(b) Replace the model-prefix block (the comment + `MODEL="openrouter/${LLM_MODEL}"`) with a validated, parameterized version:

```sh
# Provider selects the opencode built-in SDK (model prefix + auth.json key). Only
# these three are validated/supported via the sandbox; ollama/vllm use the host-side
# direct-API path and never reach here.
case "${LLM_PROVIDER:-}" in
  openrouter|anthropic|openai) : ;;
  *) echo "entrypoint: unsupported LLM_PROVIDER='${LLM_PROVIDER:-}'" >&2; exit 2 ;;
esac

# Model id for a built-in provider is "<provider>/<slug>"; the slug itself may
# contain a slash (e.g. openrouter/google/gemini-2.5-pro).
MODEL="${LLM_PROVIDER}/${LLM_MODEL}"
```

(c) Replace the auth.json write. Update the trailing safety comment, then the printf:

```sh
# exfiltrated, and bash/webfetch are denied. The provider is a validated enum value
# and the supported providers' keys are [A-Za-z0-9-] (no JSON metacharacters), so
# direct interpolation is safe.
mkdir -p "$XDG_DATA_HOME/opencode"
printf '{"%s":{"type":"api","key":"%s"}}\n' "$LLM_PROVIDER" "$LLM_API_KEY" > "$XDG_DATA_HOME/opencode/auth.json"
```

- [ ] **Step 6: Syntax-check the entrypoint + the provider guard**

Run: `bash -n deploy/sandbox/entrypoint.sh && echo "syntax OK"`
Expected: `syntax OK`.

Run (guard behavior, isolated):
```bash
LLM_PROVIDER=bogus sh -c 'case "${LLM_PROVIDER:-}" in openrouter|anthropic|openai) : ;; *) echo "unsupported" >&2; exit 2 ;; esac'; echo "exit=$?"
```
Expected: prints `unsupported` and `exit=2`.

- [ ] **Step 7: Commit**

```bash
git add internal/agents/coding/service.go internal/agents/coding/service_test.go deploy/sandbox/entrypoint.sh
git commit -m "feat(007): forward LLM_PROVIDER + generalize entrypoint to anthropic/openai/openrouter (manyforge-fqo #3)"
```

---

### Task 4: `MF007-PIN-11` security-regression pins

Source-level pins so a future refactor that drops redaction (or reverts the provider guard) fails CI. Must run after Tasks 1–3 (it pins their exact output).

**Files:**
- Create: `internal/security_regression/mf007_review_redaction_test.go`

**Interfaces:**
- Consumes: `mustRead(t, path)` (existing helper in package `security_regression`).
- Produces: nothing (test only).

- [ ] **Step 1: Write the pin test**

Create `internal/security_regression/mf007_review_redaction_test.go`:

```go
package security_regression

import (
	"strings"
	"testing"
)

// MF007-PIN-11: secrets must be redacted before they can reach the stored
// last_error/audit (via sandboxStderrTail) or the posted/stored review doc (via
// redactDoc), and the sandbox entrypoint must validate the provider allowlist.
// Source pins — a refactor that drops these must update this file in the same change.
func TestReviewOutputRedaction(t *testing.T) {
	svc := mustRead(t, "../agents/coding/service.go")
	if !strings.Contains(svc, "func sandboxStderrTail(outDir string, secrets ...string)") {
		t.Fatal("sandboxStderrTail must take secrets to redact (MF007-PIN-11)")
	}
	if !strings.Contains(svc, "redactSecrets(s, secrets...)") {
		t.Fatal("sandboxStderrTail must redact its tail via redactSecrets (MF007-PIN-11)")
	}
	if !strings.Contains(svc, "redactDoc(&doc, cred.APIKey, rc.Credential.APIToken)") {
		t.Fatal("model doc must be redacted before posting/storing (MF007-PIN-11)")
	}

	red := mustRead(t, "../agents/coding/redact.go")
	if !strings.Contains(red, "func redactSecrets(") || !strings.Contains(red, "func redactDoc(") {
		t.Fatal("redactSecrets/redactDoc must exist (MF007-PIN-11)")
	}

	entry := mustRead(t, "../../deploy/sandbox/entrypoint.sh")
	if !strings.Contains(entry, "openrouter|anthropic|openai) : ;;") {
		t.Fatal("entrypoint must validate the provider allowlist (MF007-PIN-11)")
	}
}
```

- [ ] **Step 2: Run to verify it passes**

Run: `go test ./internal/security_regression/ -run TestReviewOutputRedaction -v`
Expected: PASS (all four pins match the code from Tasks 1–3). (Source-only pin — no build tag needed; it also runs under `make sec-test`.)

> If a pin FAILS, the source it references drifted from this plan's exact strings — fix the source to match (or update the pin in the same commit, per the source-pin discipline). Do NOT weaken the pin to make it pass.

- [ ] **Step 3: Commit**

```bash
git add internal/security_regression/mf007_review_redaction_test.go
git commit -m "test(007): MF007-PIN-11 — pin review secret-redaction + provider allowlist (manyforge-fqo #2/#3)"
```

---

### Task 5: Rebuild image + full gate (controller-run)

No new Go code. Bakes the entrypoint and proves the whole suite.

**Files:** none (ops).

- [ ] **Step 1: Rebuild the sandbox image**

```bash
DOCKER_BUILDKIT=0 docker build --build-arg TARGETARCH=arm64 -f deploy/sandbox/Dockerfile -t manyforge/opencode-sandbox:dev .
```
Expected: builds; the `COPY deploy/sandbox/entrypoint.sh` layer re-runs.

- [ ] **Step 2: Full gate**

```bash
go test ./internal/agents/coding/... ./internal/connectors/... && \
go vet ./... && make lint && \
go test -tags contract ./cmd/... && make sec-test && \
go test -tags integration -p 1 ./internal/agents/coding/
```
Expected: all PASS.

- [ ] **Step 3: Update tracking + commit**

Update `HANDOFF.md` (note #2/#3 done) and close `manyforge-fqo` items #2/#3 in bd. Commit any doc/bd changes.

---

## Self-Review

**Spec coverage:**
- #2 redactor (exact + regex) → Task 1. ✓
- Apply at stderr tail + model doc → Task 2. ✓
- `MF007-PIN-11` → Task 4. ✓
- #3 `LLM_PROVIDER` env + entrypoint allowlist/parameterize → Task 3. ✓
- No credresolver change → honored (no task touches it). ✓
- Image rebuild + gates → Task 5. ✓

**Placeholder scan:** No TBD/TODO; every code step shows complete code. ✓

**Type consistency:** `redactSecrets(s string, known ...string) string`, `redactDoc(doc *FindingsDoc, known ...string)`, `sandboxStderrTail(outDir string, secrets ...string) string`, `sandboxEnv(cred AICredential) map[string]string` — used consistently across Tasks 1–4, and the Task 4 pins quote the exact strings Tasks 2–3 produce. ✓
```
