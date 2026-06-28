# Review secret-redaction + provider generality — design

- **Date:** 2026-06-28
- **Issue:** `manyforge-fqo` items #2 (redact LLM key from review output) and #3 (provider generality beyond OpenRouter). Parent epic `manyforge-7ml`, Spec 007.
- **Status:** approved design, ready for implementation plan
- **Branch:** `feat/code-review-ui`

## Problem

Two remaining `manyforge-fqo` follow-ups on the code-review agent's **cloud
(opencode sandbox)** path:

**#2 — secrets can leak into stored/posted review output.** The LLM API key is
injected into the sandbox as `LLM_API_KEY` (written to opencode's `auth.json`).
Two untrusted-text channels flow back into our trusted output without scrubbing:
- **Sandbox stderr → stored error.** `sandboxStderrTail` reads `/out/stderr.log`
  and appends a tail to the failure error; that error string is stored in the
  `code_review.last_error` column (via `requeue_code_review`/`fail_code_review`)
  and in the audit log (`agent.coding.review.failed` output). If opencode ever
  logs an auth failure echoing the key/header, it persists at rest.
- **Model output → GitHub.** `renderReviewBody`/`renderInlineComment` post the
  model's `summary` and findings' `title`/`detail` **verbatim** to the PR. (The
  model cannot read the real key — `external_directory: deny` blocks `auth.json`
  — so this is lower-probability, but cheap to guard.)

The existing `internal/platform/observability/redact.go` is slog-attribute-key
only; it does not scrub error/findings strings.

**#3 — the sandbox/opencode mapping is OpenRouter-hardcoded.** `entrypoint.sh`
hardcodes the model prefix `openrouter/${LLM_MODEL}` and the `auth.json` provider
key `"openrouter"`, and `service.go` never forwards `cred.Provider` to the
sandbox. opencode has anthropic + openai SDKs compiled in; only the model prefix
and the `auth.json` key name change per provider.

## Goals

1. Strip secrets from (a) any error string stored in `last_error`/audit and (b)
   the review body/findings posted to GitHub.
2. Support cloud providers `anthropic`, `openai`, and `openrouter` through the
   generalized sandbox/opencode path.

## Non-goals (deferred / YAGNI)

- Custom or proxy base URLs for cloud providers (opencode's built-in SDK defaults
  are used; the egress allowlist uses the provider's default host).
- `ollama`/`vllm` — they run on the **host-side** direct-API path (`localReview`),
  not the sandbox, and are unchanged.
- Multi-call chunking of large diffs (`manyforge-206`).

## Approved decisions

| # | Decision | Choice |
|---|----------|--------|
| 1 | Redaction strategy | **Exact known-secret + key-pattern regex.** Replace literal known secret values AND scrub common key shapes. |
| 2 | Cloud providers | **anthropic + openai + openrouter.** Forward `LLM_PROVIDER`; entrypoint parameterizes model prefix + `auth.json` key. `ollama`/`vllm` stay host-side. |
| — | Scope | Both items ship together as one PR on `feat/code-review-ui`. |
| — | Exact-redact secret set | LLM key (`cred.APIKey`) **and** the GitHub connector token (`rc.Credential.APIToken`). |

## Part A — #2: secret redaction

### A1. Redactor

New focused file `internal/agents/coding/redact.go`:

```go
// redactSecrets removes secrets from text destined for storage or a posted review:
// (1) exact replacement of each known secret value, then (2) regex scrub of common
// API-key shapes for secrets we don't hold. Returns the scrubbed string.
func redactSecrets(s string, known ...string) string
```

- **Exact pass:** for each `secret` in `known` with `len(secret) >= 8` (skip empty/
  trivial values to avoid mangling unrelated text), `strings.ReplaceAll(s, secret,
  "[REDACTED]")`.
- **Pattern pass:** package-level compiled regexes, each replaced with `[REDACTED]`:
  - `sk-[A-Za-z0-9_\-]{20,}` — OpenAI / Anthropic (`sk-ant-…`) / OpenRouter style.
  - `ghp_[A-Za-z0-9]{36}` and `github_pat_[A-Za-z0-9_]{50,}` — GitHub tokens.
  - `(?i)bearer\s+[A-Za-z0-9._\-]{16,}` — bearer auth headers.

  Patterns are intentionally specific (known prefixes + length floors) to limit
  over-redaction of legitimate finding text.

### A2. Apply at the two trust boundaries (both in `runJob`, where `cred.APIKey`
and the GitHub `rc.Credential.APIToken` are in scope)

- **`sandboxStderrTail(outDir string, secrets ...string) string`** — redact the
  assembled tail before returning. The stderr tail is the only untrusted text that
  enters the failure error, so scrubbing it here makes every downstream
  `last_error`/audit write safe. All other `runJob` error messages are our own
  literals (no secrets). Update the two call sites to pass the secrets.
- **Model output** — immediately after `ParseFindings` (cloud path) redact
  `doc.Summary` and, for every finding, `Title` and `Detail`, before `buildReview`/
  `PostReview`. A small helper `redactDoc(&doc, secrets...)` keeps it local. (Apply
  on the cloud path; the local/`ollama` path's model never receives a cloud key,
  but applying uniformly after `ParseFindings` is simplest and harmless.)

### A3. Security-regression test (repo security-fix discipline)

`internal/security_regression/mf007_review_redaction_test.go` (`MF007-PIN-11`):
- An exploit-style assertion: a planted secret value embedded in a stderr tail and
  in a model finding is `[REDACTED]` — never the raw value — in the resulting error
  string and the rendered review body.
- A **source pin** (`strings.Contains` over the source) that `sandboxStderrTail`
  and the post-`ParseFindings` redaction call sites still call `redactSecrets`, so a
  future refactor that drops redaction fails CI loudly.

## Part B — #3: provider generality

- **`service.go`** — add `"LLM_PROVIDER": cred.Provider` to the sandbox `Env`
  (alongside `LLM_API_KEY`/`LLM_BASE_URL`/`LLM_MODEL`).
- **`deploy/sandbox/entrypoint.sh`** —
  - Validate `LLM_PROVIDER` against the known-good set `openrouter|anthropic|openai`;
    on anything else, write a clear error and exit non-zero (no opaque failure).
  - `MODEL="${LLM_PROVIDER}/${LLM_MODEL}"` (replaces the hardcoded `openrouter/…`).
  - `auth.json = {"${LLM_PROVIDER}":{"type":"api","key":"${LLM_API_KEY}"}}` (replaces
    the hardcoded `"openrouter"` key). Provider is a validated enum value and the
    three providers' keys are `[A-Za-z0-9_-]` → printf interpolation stays JSON-safe
    (preserve the existing safety comment, updated for the three providers).
  - Update the `LLM_*` header comments to reflect the now-used `LLM_PROVIDER`.
- **No `credresolver.go` change.** `anthropic` and `openrouter` already have
  built-in default base URLs; `openai` intentionally **requires a user-supplied
  base_url** (enforced by `internal/platform/ai/factory.go:New`, which fails closed
  on an empty openai base_url). So an openai agent's `cred.BaseURL` is already
  non-empty with host `api.openai.com`, which (a) makes `cred.Host()` resolve for
  the egress allowlist and (b) matches opencode's built-in openai SDK endpoint. No
  defaults change is needed or wanted (it would diverge from the factory).
- **Sandbox image rebuild** (`manyforge/opencode-sandbox:dev`) to bake the new
  entrypoint.

## Edge cases

- **Empty/short secret** → exact pass skips it (`len < 8` guard); no accidental
  mangling of short finding text.
- **Secret appears in both stderr and model output** → both boundaries scrub it.
- **Unknown `LLM_PROVIDER`** → entrypoint fails fast with a clear message (review
  marked failed; no opaque opencode error).
- **`ollama`/`vllm`** → never reach the entrypoint (host-side path); the redactor is
  still applied to their `ParseFindings` output (harmless, uniform).

## Test plan

Automated tests required; all green before push.

### Unit — `internal/agents/coding/redact_test.go`
- Exact: known secret replaced; `[REDACTED]` appears, raw value absent; secret
  embedded mid-sentence replaced.
- Patterns: each regex shape redacted; a legitimate non-key string (e.g. `sk`,
  short hex, a normal sentence) is NOT redacted (false-positive guard).
- `len < 8` known secret is ignored.

### Unit — `internal/agents/coding`
- `sandboxStderrTail` redacts a planted secret in a temp `stderr.log`.
- `redactDoc` scrubs `Summary`/`Title`/`Detail`.
- The sandbox `Env` built in `runJob` includes `LLM_PROVIDER=cred.Provider` (assert
  via the existing service/worker test seam or a focused spec test).

### Security-regression — `internal/security_regression`
- `MF007-PIN-11`: planted secret `[REDACTED]` in error + review body; source pins on
  the two redaction call sites. Runs under `make sec-test`.

### Shell / sandbox
- `bash -n deploy/sandbox/entrypoint.sh`; a focused check that an unknown
  `LLM_PROVIDER` exits non-zero (run the relevant entrypoint branch, or assert the
  guard text via grep pin).
- Rebuild the image.

### Gates (whole repo, before push)
- `go test ./internal/agents/coding/... ./internal/connectors/...`; `go vet ./...`;
  `make lint`; `make sec-test`; `go test -tags contract ./cmd/...`; integration
  `go test -tags integration -p 1 ./internal/agents/coding/`.

## Files touched

| File | Change |
|------|--------|
| `internal/agents/coding/redact.go` | New `redactSecrets` + patterns + `redactDoc`. |
| `internal/agents/coding/service.go` | `sandboxStderrTail` redacts (secrets param + call sites); redact `doc` after `ParseFindings`; add `LLM_PROVIDER` to sandbox `Env`. |
| `deploy/sandbox/entrypoint.sh` | Provider allowlist + parameterized `MODEL` + `auth.json`; rebuild image. |
| `internal/security_regression/mf007_review_redaction_test.go` | New `MF007-PIN-11`. |
| Test files | `redact_test.go`; stderr/doc redaction + `Env` provider tests. |

## Rollout / verification

1. Land unit + security-regression + integration green; `go vet`, `make lint`,
   contract.
2. Rebuild the sandbox image:
   `DOCKER_BUILDKIT=0 docker build --build-arg TARGETARCH=arm64 -f deploy/sandbox/Dockerfile -t manyforge/opencode-sandbox:dev .`
3. Update `HANDOFF.md`; update `manyforge-fqo` (close #2/#3; note remaining epic items).
