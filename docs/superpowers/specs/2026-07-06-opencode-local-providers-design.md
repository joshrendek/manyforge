# Route Local Providers Through the opencode Sandbox — Design (manyforge-9er)

**Goal:** Every code review — local or cloud — runs the *identical* hardened-sandbox opencode flow. The host-side direct-POST `localReview` path is removed. A sandboxed local lane reaches its LAN OpenAI-compatible endpoint (vLLM / LM Studio / Ollama) through the extended egress proxy, and falls back to the dimension's configured cloud model at runtime if the local run fails.

**Architecture:** `reviewLane` stops special-casing local providers; both local and cloud build a `SandboxSpec` and run `opencode run` in the egress-restricted sandbox. The sandbox keeps exactly one egress path — the `mf-egress-proxy` — which gains plain-HTTP forwarding for allowlisted private hosts, plus a scoped NetworkPolicy exception so the proxy pod can reach the LAN. All private-LAN egress stays gated on the credential's `AllowPrivateBaseURL` flag.

**Tech stack:** Go 1.x backend; `sst/opencode` in the sandbox (compiled-in `@ai-sdk/openai` provider); `cmd/mf-egress-proxy` (Go CONNECT proxy); Kubernetes (KubeRunner + NetworkPolicy) via the `charts/manyforge` Helm chart; DockerRunner for local dev.

## Global Constraints (copied from the codebase, apply to every task)
- **sqlc pin:** none needed — no schema change in this epic.
- **Security invariant — one egress path:** the sandbox reaches the network ONLY via `mf-egress-proxy`; the allowlist (`EGRESS_ALLOW`, matched by `netsafe.ParseHostAllowlist`) is the sole authority. This must remain true after the change.
- **`AllowPrivateBaseURL` gate:** private/loopback/RFC1918 egress is permitted only for a credential whose `AllowPrivateBaseURL` is set (`credresolver.go:26`). Never widen this implicitly.
- **No silent caps / honest failure:** a lane that cannot run is skipped/failed with a recorded reason, never a false "no issues found".
- **Source-level pins for security fixes** live in `internal/security_regression/` (one file per finding/behavior).
- Verification gates: `make test`, `go test -tags integration -p 1 ./internal/agents/coding/`, `go test -tags contract ./cmd/...`, `make lint`.

## Decisions (from brainstorming, 2026-07-06)
1. **Route ALL reviews through opencode** — consistency + quality + reliability. The direct-POST path is legacy to remove.
2. **Runtime fallback to cloud on local failure** — if the local opencode lane fails (timeout / can't drive the tool loop / unparseable JSON), re-run *that lane* on the dimension's configured fallback `(provider, model)`. Reuses the existing per-dimension fallback (manyforge-azy).
3. **Egress approach: extend the proxy + NetworkPolicy** (keep the single-egress-path isolation) — chosen over "direct egress for local lanes" and "opencode in the worker".
4. **Delete `localreview.go` + `localreview_test.go`** in this epic.
5. **`sandbox.privateEgressCIDRs`** is a configurable list; default to the tightest exact host (`192.168.2.241/32`); documented to accept broader CIDRs.

## Components

### C1. Egress proxy — plain-HTTP forwarding (`cmd/mf-egress-proxy/main.go`)
Today the proxy handles only `CONNECT` (HTTPS tunneling) and 405s everything else; local endpoints are plain `http://`. Add a forward branch: a non-CONNECT request whose `r.Host` passes `allow.Allows(r.Host)` is reverse-proxied to the target (standard absolute-form proxied HTTP); non-allowlisted still 403. The allowlist stays the sole authority — no private-IP block is added or removed (the proxy already `net.Dial`s whatever is allowlisted). CONNECT behavior is unchanged.

### C2. Sandbox entrypoint — local provider mapping (`deploy/sandbox/entrypoint.sh`)
Accept `LLM_PROVIDER ∈ {vllm, ollama}` (currently `exit 2`). Map them to a **custom opencode provider backed by `@ai-sdk/openai-compatible`** (verified bundled in the opencode binary — see Spike verdict below). NOT the built-in `openai` provider, which uses the Responses API (`/v1/responses`) that local servers don't serve.
- config: `provider.local = { "npm": "@ai-sdk/openai-compatible", "options": { "baseURL": "$LLM_BASE_URL" }, "models": { "$LLM_MODEL": { "options": { "max_tokens": … } } } }`, `model = "local/$LLM_MODEL"`,
- `auth.json = {"local":{"type":"api","key":"$LLM_API_KEY"}}` (local servers accept/ignore the key; the `local` id must match the provider key),
- this drives `POST {baseURL}/chat/completions` — the OpenAI Chat Completions API that vLLM / LM Studio / Ollama all serve.
Read-only permission profile and `OPENCODE_DISABLE_*` flags unchanged. The GLM z-ai routing block stays openrouter-only.

**Spike verdict (2026-07-06, opencode 1.17.11 via `manyforge/opencode-sandbox:dev`):** Ran opencode from the real sandbox image against a stub OpenAI-compat server. (1) The built-in `openai` provider honors `options.baseURL` + `auth.json` with the catalog disabled, but calls `POST /v1/responses` — incompatible with local Chat-Completions servers. (2) A custom `@ai-sdk/openai-compatible` provider with `options.baseURL` calls `POST /v1/chat/completions`, is **bundled** (worked on a `--internal` no-internet/no-npm docker network, so no runtime npm fetch), and authed via `auth.json`. Approach (2) is adopted. Task 0 below is therefore already de-risked; it becomes a regression/characterization test rather than an open question.

### C3. Service routing + runtime fallback (`internal/agents/coding/service.go`, `fallbackchain.go`)
- `reviewLane` (`service.go:674`) drops the `isLocalProvider → localReview` branch; **both** paths build a `SandboxSpec`. Local keeps the tighter diff budget (`localProviderMaxTotalBytes`) — small models still choke on large context.
- Enqueue-time egress checks (`service.go:240,:431`, `fallbackchain.go:122`) now apply to local providers: a local host is permitted **only when the credential's `AllowPrivateBaseURL` is set**; otherwise the lane is skipped at resolve time with a recorded reason.
- **Runtime fallback:** on a local lane failure, re-run the lane once with the dimension's fallback `(provider, model)`, re-resolving that credential (today `resolveLaneCred` returns only the *chosen* cred, so the fallback must be resolved again — or `resolveLaneCred` extended to surface both). Local-then-cloud both failing = honest lane failure.
- Keep a slim `isLocalProvider` as the diff-budget / provider-mapping signal only.

### C4. Egress allowlist for the LAN endpoint (`charts/manyforge/values.yaml`, `configmap.yaml`)
**Planning correction:** the chart has **no NetworkPolicy** — sandbox isolation is enforced by forcing Job pods through the proxy (`HTTP_PROXY`) plus the proxy's static allowlist (`EGRESS_ALLOW` ← `.Values.sandbox.egressAllow`, threaded to the app as `MANYFORGE_SANDBOX_EGRESS_ALLOW`). So there is no NetworkPolicy to extend. Instead: add the local host (e.g. `192.168.2.241:1234`) to `sandbox.egressAllow` (the single value both the proxy and the app's pre-flight check read). The private-LAN reach is gated at the **service layer** — `laneCredFor` permits a local host only when the credential's `AllowPrivateBaseURL` is set. LAN routing from the sandbox namespace is a deploy-time verification (the app/worker pod already reaches the endpoint today, same cluster/CNI). No `privateEgressCIDRs` key is added.

### C5. Remove the direct-POST path
Delete `internal/agents/coding/localreview.go` and `internal/agents/coding/localreview_test.go`. Migrate any still-needed constants (diff-budget sizes, `isNonReviewableDoc`, `isLocalProvider`) to a small retained helper file.

## Error Handling
- Local lane fails → runtime fallback to the dimension's cloud model. Both fail → lane failure with reason (existing `partial success` semantics: whole review fails only if *every* lane fails).
- Local cred without `AllowPrivateBaseURL` → lane skipped at resolve time, reason recorded (`dimensions_skipped` audit).
- Proxy: non-allowlisted host → 403 (unchanged). Local-agentic timeout → sandbox timeout → treated as a lane failure → cloud fallback.

## Security Analysis
- **Isolation preserved:** the sandbox still egresses only through `mf-egress-proxy`; plain-HTTP forwarding is allowlist-enforced to the single resolved LLM host (`EgressAllow: []string{laneCred.Host()}`).
- **Private-LAN reach is doubly gated:** the credential's `AllowPrivateBaseURL` AND the scoped `privateEgressCIDRs` NetworkPolicy. Default CIDR is a single `/32`.
- **Prompt-injection containment unchanged:** opencode config denies `bash`/`webfetch`/`edit`; the proxy allows only the one LLM host; the API key lands only in the `/tmp` tmpfs, outside the reviewed cwd.
- **No new UUID/authz surface:** routing is internal; no new HTTP endpoints (no OpenAPI change).

## Test Plan
- **Task 0 — DONE (spike complete, see C2 verdict):** opencode reaches a local Chat-Completions server via a bundled custom `@ai-sdk/openai-compatible` provider + `options.baseURL`. Codify as a characterization/regression test: an `entrypoint.sh`-shape config produces a `POST /v1/chat/completions` against a stub (e.g. a sandbox-image integration test, or a shell test asserting the generated `opencode.json` uses the `@ai-sdk/openai-compatible` npm provider — never the built-in `openai`/Responses path).
- **Proxy unit tests** (`cmd/mf-egress-proxy/main_test.go`): allowlisted plain-HTTP request forwarded to a stub upstream; non-allowlisted plain-HTTP → 403; CONNECT still tunnels; allowlist parity with `netsafe`.
- **Service unit/integration** (`internal/agents/coding/...`): `reviewLane` builds a `SandboxSpec` for a local cred (a fake runner asserts the spec: provider mapping, `EgressAllow=[localHost]`, tighter budget); runtime fallback to cloud when the local lane errors; local cred without `AllowPrivateBaseURL` is skipped with a reason.
- **Regression (source-pin, `internal/security_regression/`):** assert no `isLocalProvider → localReview` branch survives, so the direct-POST path cannot silently return; assert the entrypoint rejects unknown providers.
- **Chart/NetworkPolicy:** a `helm template` assertion (or golden test) that `privateEgressCIDRs` renders into the proxy egress rule and defaults to a `/32`.
- **Remove** `localreview_test.go`.
- **Gates:** `make test`, integration tag, contract tag (expect no drift — no endpoint change), `make lint`.

## Rollout
Deploy is atomic across three images/artifacts: the app image (service routing), the `egress-proxy` image (plain-HTTP forwarding), and the chart (NetworkPolicy + `privateEgressCIDRs`). After deploy, verify with a real review against the LM Studio endpoint (`192.168.2.241`) — confirm a sandbox Job spawns (unlike today's host-side path), reaches the endpoint through the proxy, and that a forced local failure falls back to the configured cloud model.

## Out of Scope
- Changing which model the user runs locally, or model-capability tuning.
- Per-dimension config UI changes (the existing primary/fallback config already drives this).
- Any schema/migration change.
