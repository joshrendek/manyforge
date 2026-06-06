# Spec 003 — US8 Multi-Provider Coverage — Design

**Status:** approved (brainstorm) · **Date:** 2026-06-06 · **bd:** `manyforge-deo.9` (epic `manyforge-deo`)
**Builds on:** US1 (AI gateway: `openaicompat` transport, netsafe-guarded factory, golden fixtures, SSRF pins), US7 (`model_pricing` catalog / DB-backed `ai.Registry`)
**Master design:** `2026-06-02-agent-runtime-design.md` §5 US8 (P3), §3.5 (security invariants)
**Governance:** Constitution Principle II (Security by Default), IV (Bounded, Auditable AI Agents), III (Test-First), I (Tenant Isolation).

---

## 1. Problem & goal

The master design slices US8 as *"OpenAI / Ollama / vLLM exercised end-to-end (config + a recorded fixture per transport); self-host `base_url` SSRF-guard pin."* Investigation shows **most of this already shipped in US1**: the `openaicompat` transport already serves all three OpenAI-compatible backends, the `netsafe` SSRF-guarded HTTP client is already wired into the provider factory, and SSRF behavioral + source-level pins already exist (`internal/security_regression/ai_provider_ssrf_pin_test.go`). US7 added the `model_pricing` catalog.

So US8 is **small** and closes four concrete gaps — only one of which is a genuine new feature:

1. **Self-host reachability (real feature).** `netsafe` blocks loopback + RFC1918 by default — which is exactly where a self-hosted Ollama (`localhost:11434`) or vLLM (a `192.168.x.x` box) lives. As shipped, a self-hoster *cannot* point manyforge at their own model. US8 adds a **per-credential** opt-in trust flag.
2. **Per-provider coverage proof.** Today a single OpenAI fixture is reused for all three backends, so a real Ollama/vLLM wire difference would slip through. US8 adds recorded **text + tool-call** fixtures for Ollama and vLLM and an end-to-end test per provider.
3. **Self-host cost handling.** Self-hosted model IDs are user-defined and unbounded, so they can't be pre-seeded. US8 locks the behavior: unknown model ⇒ `cost_cents=0`, run proceeds.
4. **Security regression contract.** Pin the new trust flag's safe boundaries.

**Non-goal restated:** US8 does not add a credential HTTP CRUD surface, model-catalog seeding for self-host, per-business pricing overrides, or streaming.

---

## 2. Scope decisions (locked in brainstorming)

1. **Self-host reach → per-credential trust flag.** A new `allow_private_base_url bool` column on `ai_provider_credential` (default `false`). Trust is per-credential (one agent may reach a LAN Ollama; others can't), validated + audited at create time. *(Alternatives considered: a global env flag mirroring `MCPAllowLoopback` — rejected for being coarser; public-URL-only — rejected as not delivering real self-host coverage.)*
2. **Metadata IPs stay hard-blocked even when trusted.** The trust flag loosens loopback + RFC1918 only. Cloud-metadata endpoints (`169.254.169.254`, `fd00:ec2::254`) remain blocked unconditionally — there is no legitimate reason to run a model server on the IMDS address, and allowing it would turn the trust flag into a credential-theft SSRF vector.
3. **Self-host cost → unknown model = `$0`, run proceeds.** No Ollama/vLLM rows seeded. Budget cap effectively never trips for self-host (correct — zero marginal cost). A debug log notes the catalog miss so a *missing-but-paid* model is still noticeable. *(Alternatives: seed sample rows — rejected as maintenance churn; fail-loud — rejected as hostile to self-host.)*
4. **Fixture depth → text + tool-call per provider.** Hand-authored/recorded realistic responses for Ollama and vLLM capturing their real wire quirks (`usage` shape, `finish_reason` values, `tool_calls` format), proving the path agents actually depend on — tool calling. *(Alternatives: text-only — leaves tool calling unproven; reuse OpenAI fixture — proves nothing new.)*

---

## 3. Architecture & changes

### 3.1 netsafe — add `AllowPrivate`, reorder for a metadata-safe boundary
`internal/platform/netsafe/client.go`. `Options` gains `AllowPrivate bool`. `blockedWith` is rewritten with a **security-critical ordering** so no flag can ever unblock the metadata endpoint:

```go
type Options struct {
    AllowLoopback bool // permits 127/8 + ::1
    AllowPrivate  bool // permits RFC1918 + IPv6 ULA (fc00::/7)
}

func blockedWith(ip net.IP, allowLoopback, allowPrivate bool) bool {
    if ip == nil {
        return true
    }
    // (1) metadata IPs: ALWAYS blocked, before any flag — fd00:ec2::254 is itself
    //     ULA-private, so this MUST precede the IsPrivate() gate.
    for _, m := range metadataIPs {
        if ip.Equal(m) {
            return true
        }
    }
    // (2) link-local (incl. 169.254.169.254 IMDS-v4), multicast, unspecified: always blocked.
    if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
        ip.IsUnspecified() || ip.IsMulticast() {
        return true
    }
    // (3) loopback: gated.
    if ip.IsLoopback() {
        return !allowLoopback
    }
    // (4) RFC1918 + IPv6 ULA: gated.
    if ip.IsPrivate() {
        return !allowPrivate
    }
    return false // public
}
```

`Blocked(ip)` stays `blockedWith(ip, false, false)` (default locked-secure). `NewClientWithOptions` threads `o.AllowPrivate` into the dialer alongside `o.AllowLoopback`.

> Note on ordering vs. the current code: today `metadataIPs` is checked *last* and RFC1918 is *always* blocked, so the metadata loop is effectively redundant. Once `AllowPrivate` can permit `IsPrivate()`, the metadata check **must** move ahead of the `IsPrivate()` gate — otherwise `fd00:ec2::254` (an `fc00::/7` ULA) would leak through a trusted credential. This reordering is the single most important security detail in US8.

### 3.2 ai factory — thread the per-credential flag into client construction
`internal/platform/ai/factory.go`. `Credential` gains `AllowPrivateBaseURL bool`. `New` builds the client per credential:

```go
hc := netsafe.NewClientWithOptions(defaultRequestTimeout, netsafe.Options{
    AllowLoopback: cred.AllowPrivateBaseURL,
    AllowPrivate:  cred.AllowPrivateBaseURL,
})
```

One flag toggles both loopback and private — a self-hoster on `localhost` *or* a LAN box both need it, and the semantic is simply "this credential may reach private/loopback addresses." The `base_url`-required check for openai/ollama/vllm is unchanged.

### 3.3 credential service — carry, validate, resolve, audit the flag
`internal/agents/credential.go`:

- `CreateCredentialInput`, `ResolvedCredential`, `storedCredential` each gain `AllowPrivateBaseURL bool`.
- `Create` passes it into `InsertAIProviderCredentialParams`; `Resolve` / `resolveRow` carry it back into `ResolvedCredential`, and `internal/agents/run_adapters.go` passes it into `ai.Credential` when building the provider.
- **Create-time validation** in `validate()` (best-effort boundary check; dial-time netsafe remains authoritative because DNS can rebind):
  - `base_url` is **required** for `openai`/`ollama`/`vllm` (clean `ErrValidation` 400 rather than a later factory `ErrBadRequest`).
  - When set, `base_url` must parse with an `http`/`https` scheme and a non-empty host.
  - If the host is a **literal IP** that `netsafe.blockedWith(ip, flag, flag)` rejects under the credential's own flag value, reject with `ErrValidation`. (A literal private IP with `allow_private_base_url=false` ⇒ 400 at the boundary; the same IP with the flag `true` is accepted, except metadata IPs which are always rejected.) Hostnames are not resolved at create time.
  - This needs a tiny exported netsafe helper, e.g. `netsafe.IsBlocked(ip net.IP, o Options) bool`, so the service can reuse the *exact* dialer policy rather than reimplementing it.
- **Audit:** when a credential is created with `allow_private_base_url=true`, write one `audit_entry` (actor principal, business, `inputs`={provider, base_url}, `decision`="trust_private_base_url") via the existing auditor. `CredentialService` gains an optional `Auditor` dependency (the same `agents.NewDBAuditor(database)` the `Engine` already constructs); when nil (pure unit tests) the audit write is skipped. Trusting a private endpoint is a security-sensitive grant and belongs in the audit trail per the project's "admin mutation → audit" pattern.

### 3.4 DB / sqlc
- **Migration `migrations/0039_ai_credential_allow_private.{up,down}.sql`:** `ALTER TABLE ai_provider_credential ADD COLUMN allow_private_base_url boolean NOT NULL DEFAULT false;` (down drops the column).
- Mirror the column into `db/schema.sql` (sqlc reads schema.sql, not migrations — strip DEFAULT/GRANT/RLS per convention).
- Extend `db/query/ai_provider_credential.sql`: add `allow_private_base_url` to the `InsertAIProviderCredential` column list/params and to the `GetAIProviderCredential` projection.
- `make generate`; then **read** the regenerated `dbgen` struct field names and match exactly (casing is unpredictable).

### 3.5 Unknown-model cost — lock existing behavior
The cost closure already returns `0` on a registry miss (`cmd/manyforge/main.go:169–175`). US8:
- Adds a **debug log** at the miss (`logger.Debug("model not in pricing catalog; cost=0", "model", model)`), so a missing-but-paid model is noticeable.
- Adds a **regression pin** that an unseeded self-host model ⇒ `cost_cents=0` and the run reaches a **success** terminal state (not `failed`).
- Seeds **no** Ollama/vLLM rows.

---

## 4. Per-provider fixtures + end-to-end coverage

- Add four fixtures under `internal/platform/ai/testdata/`: `ollama_text.json`, `ollama_tool_calls.json`, `vllm_text.json`, `vllm_tool_calls.json`, each a realistic recorded `/v1/chat/completions` response for that backend (capture the actual `usage`, `finish_reason`, and `tool_calls` shapes — both are OpenAI-compatible but not byte-identical).
- Extend the `AI_RECORD`-gated record helpers (`internal/platform/ai/fixtures_test.go`) with `TestRecordOllamaFixture` / `TestRecordVLLMFixture` reading `OLLAMA_BASE_URL` / `VLLM_BASE_URL` (skipped in CI; used once to author the fixtures).
- A coverage test exercises the **real path per provider**: build a `Credential` → `factory.New` → drive the transport against an `httptest.Server` replaying the fixture → assert the parsed `Response` text **and** tool call. Extend the existing `openaicompat_test.go` golden-round-trip table with the new ollama/vllm cases (same transport, new fixtures), and add an integration-tagged credential→Resolve→factory path test in `internal/agents`.

---

## 5. Security invariants (regression contract)

Dedicated pins in `internal/security_regression/us8_*_test.go` (one concern per file, finding-ID header comment), plus unit/integration coverage:

1. **Metadata stays blocked under trust.** `blockedWith` (and the exported `IsBlocked`) returns `true` for `169.254.169.254` and `fd00:ec2::254` with `AllowLoopback=true, AllowPrivate=true`. Behavioral: a `allow_private_base_url=true` credential pointed at a metadata IP base_url is still refused.
2. **Trust off ⇒ refused at both layers.** `allow_private_base_url=false` + a private/loopback `base_url` ⇒ rejected at **create time** (`ErrValidation`, new) and, if it somehow reaches a run, at **dial time** (`ErrProviderUnavailable`, existing pin retained).
3. **Trust on ⇒ hatch works, safely.** `allow_private_base_url=true` reaches a private/loopback `base_url` (httptest on loopback succeeds) but a metadata base_url still fails.
4. **Source-level wiring.** The factory constructs its client via `netsafe.NewClientWithOptions` from per-credential options; no bare `http.DefaultClient`. (Extend/retain the existing `TestAIFactory_UsesNetsafeSource` AST pin.)
5. **Self-host cost.** Unseeded model ⇒ `cost_cents=0`, run succeeds (not failed).

---

## 6. Test plan

| Layer | Lives in | Covers |
|---|---|---|
| Unit | `internal/platform/netsafe/client_test.go` | `blockedWith` table: metadata always blocked under both flags; loopback/private gated; public allowed; `IsBlocked` helper parity |
| Unit | `internal/platform/ai/factory_test.go` | factory threads `AllowPrivate` from credential into `Options`; openai-compat still requires base_url |
| Unit | `internal/agents/credential_test.go` | `validate()` accept/reject matrix (scheme, host, literal-private-IP × flag); flag round-trips through seal/resolve |
| Transport e2e | `internal/platform/ai/openaicompat_test.go` | per-provider fixture round-trip (text + tool-call) for ollama & vllm via `httptest` |
| Integration (`//go:build integration`) | `internal/agents/*_integration_test.go` | credential create (trust on/off) → `Resolve` → factory → transport; cross-tenant RLS isolation; `audit_entry` written on trust grant |
| Security regression | `internal/security_regression/us8_*_test.go` | the 5 pins in §5 |
| Cost | `internal/agents` (or security_regression) | unseeded model ⇒ cost 0, run succeeds + debug log |

**CI gate (unchanged contract):** `make test` + `make int-test` + `make contract-test` + `make sec-test` + lint, all green; `gofmt -l` empty.

---

## 7. Out of scope (deferred)

- Credential HTTP CRUD / Update endpoint (none exists today; the trust flag is service/seed-set). If a credential admin UI/API is added later, surface `allow_private_base_url` there with the same validation + audit.
- Seeding a self-host model catalog or per-business pricing overrides.
- Streaming responses; per-tenant provider rate-limiting beyond the budget cap.
- Resolving hostnames at create time (DNS-rebind-proof create-time validation) — dial-time netsafe already covers this; create-time is best-effort for literal IPs only.

---

## 8. File map (anticipated)

- **netsafe:** `internal/platform/netsafe/client.go` (`Options.AllowPrivate`, reordered `blockedWith`, exported `IsBlocked`), `client_test.go`.
- **factory:** `internal/platform/ai/factory.go` (`Credential.AllowPrivateBaseURL`, per-credential client), `factory_test.go`.
- **credential:** `internal/agents/credential.go` (input/resolved/stored flag, `validate()`, optional `Auditor`), `run_adapters.go` (pass flag), `credential_test.go`.
- **DB:** `migrations/0039_ai_credential_allow_private.{up,down}.sql`, `db/schema.sql`, `db/query/ai_provider_credential.sql`, regenerated `dbgen`.
- **cost:** `cmd/manyforge/main.go` (debug log in the Cost closure).
- **fixtures/coverage:** `internal/platform/ai/testdata/{ollama,vllm}_{text,tool_calls}.json`, `fixtures_test.go` (record helpers), `openaicompat_test.go` (round-trip cases).
- **pins:** `internal/security_regression/us8_*_test.go`.
