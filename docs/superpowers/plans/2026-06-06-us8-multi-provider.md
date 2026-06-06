# US8 Multi-Provider Coverage Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a self-hosted Ollama/vLLM on a private/loopback address be reached via a per-credential `allow_private_base_url` trust flag (metadata IPs stay hard-blocked), prove OpenAI/Ollama/vLLM end-to-end with recorded fixtures, and lock self-host cost + SSRF behavior with regression pins.

**Architecture:** US1 already shipped the `openaicompat` transport (serves all three OpenAI-compatible backends), the netsafe SSRF-guarded factory, and SSRF pins; US7 added the `model_pricing` catalog. This plan adds one real feature — a per-credential trust flag that loosens netsafe's loopback+RFC1918 block for *that credential only*, with the cloud-metadata block kept unconditional by reordering `blockedWith` — plus coverage fixtures, an end-to-end config test, and the US8 security-regression contract.

**Tech Stack:** Go, `pgx`, `sqlc` (generate; never hand-edit `dbgen`), golang-migrate, `httptest`, build-tagged integration tests (`//go:build integration`) against `testdb`, AST-based source pins in `internal/security_regression`.

**bd issue:** `manyforge-deo.9` (epic `manyforge-deo`). **Spec:** `docs/superpowers/specs/2026-06-06-us8-multi-provider-design.md`.

---

## Pre-flight (every session)

```bash
export PATH="$PATH:$HOME/go/bin"   # golangci-lint lives here; without it `make lint` is vet-only (false pass)
cd /Users/jigglypuff/dev/manyforge
```

- Module path is `github.com/manyforge/manyforge`.
- Fast unit loop: `go test ./internal/platform/netsafe/ ./internal/platform/ai/ ./internal/agents/`
- Integration: `go test -tags integration ./internal/agents/ -p 1` (needs Docker for `testdb`; ~slow).
- Security pins: `go test ./internal/security_regression/`
- After SQL edits: `make generate`, then **read** the regenerated `dbgen` struct to confirm field casing (it is unpredictable — e.g. `BaseUrl`, so expect `AllowPrivateBaseUrl`). Trust `go build ./...`, not IDE diagnostics (gopls lies on fresh dbgen refs).
- `gofmt -l internal/ cmd/ db/` MUST be empty before each commit (lint is not gofmt-aware).
- Commits: **no `Co-Authored-By` trailer** (project rule). The bd hook re-exports `.beads/issues.jsonl` on every commit — stage it.

---

## File structure

| File | Responsibility | Task |
|---|---|---|
| `internal/platform/netsafe/client.go` | `Options.AllowPrivate`; metadata-first `blockedWith`; exported `IsBlocked` | 1 |
| `internal/platform/netsafe/client_test.go` | private-allowed table + metadata-under-trust | 1 |
| `internal/platform/ai/factory.go` | `Credential.AllowPrivateBaseURL`; per-credential `NewClientWithOptions` | 2 |
| `internal/platform/ai/factory_test.go` | trust off→refused / on→reaches loopback | 2 |
| `internal/security_regression/ai_provider_ssrf_pin_test.go` | source pin updated for `NewClientWithOptions` | 2 |
| `migrations/0039_ai_credential_allow_private.{up,down}.sql` | add the column | 3 |
| `db/schema.sql`, `db/query/ai.sql` | sqlc mirror + Insert arg | 3 |
| `internal/agents/credential.go` | carry/validate/resolve/audit the flag | 4,5,6 |
| `internal/agents/run_adapters.go` | pass flag into `ai.Credential` | 4 |
| `internal/agents/credential_test.go` | resolveRow round-trip + validate matrix | 4,5 |
| `internal/agents/credential_integration_test.go` | trust-grant audit row | 6 |
| `internal/platform/ai/testdata/{ollama,vllm}_{text,tool_calls}.json` | recorded wire shapes | 7 |
| `internal/platform/ai/openaicompat_test.go` | golden round-trip cases | 7 |
| `internal/platform/ai/fixtures_test.go` | ollama/vllm record helpers | 7 |
| `internal/agents/multi_provider_integration_test.go` | DB→Resolve→factory→transport per provider | 8 |
| `internal/agents/model_pricing.go`, `cmd/manyforge/main.go` | `NewRegistryCostFn` (debug-log miss) | 9 |
| `internal/agents/model_pricing_test.go` | unknown→0, known→>0 | 9 |
| `internal/security_regression/us8_self_host_ssrf_pin_test.go` | metadata-under-trust + create-time pins | 10 |
| `internal/security_regression/us8_self_host_cost_pin_test.go` | unknown-model cost 0 pin | 10 |

---

## Task 1: netsafe — `AllowPrivate` option + metadata-first ordering

**Files:**
- Modify: `internal/platform/netsafe/client.go`
- Test: `internal/platform/netsafe/client_test.go`

- [ ] **Step 1: Write the failing test** — add to `client_test.go`:

```go
func TestBlockedWithPrivateAllowed(t *testing.T) {
	// RFC1918 + loopback + IPv6 ULA permitted when both flags on.
	for _, ip := range []string{"10.0.0.1", "192.168.1.1", "172.16.0.1", "127.0.0.1", "::1", "fd00::1"} {
		if blockedWith(net.ParseIP(ip), true, true) {
			t.Errorf("%s should be allowed when allowLoopback+allowPrivate", ip)
		}
	}
	// Metadata + link-local must NEVER unblock, even under full trust — fd00:ec2::254
	// is itself a ULA, so this proves the metadata check precedes the IsPrivate gate.
	for _, ip := range []string{"169.254.169.254", "fd00:ec2::254", "169.254.1.1", "0.0.0.0"} {
		if !blockedWith(net.ParseIP(ip), true, true) {
			t.Errorf("%s must stay blocked even with allowLoopback+allowPrivate", ip)
		}
	}
	// Flags are independent: allowPrivate alone does not permit loopback, and vice-versa.
	if !blockedWith(net.ParseIP("127.0.0.1"), false, true) {
		t.Error("loopback must stay blocked when only allowPrivate is set")
	}
	if !blockedWith(net.ParseIP("10.0.0.1"), true, false) {
		t.Error("RFC1918 must stay blocked when only allowLoopback is set")
	}
	// Exported IsBlocked mirrors blockedWith for the credential-create guard.
	if IsBlocked(net.ParseIP("169.254.169.254"), Options{AllowLoopback: true, AllowPrivate: true}) != true {
		t.Error("IsBlocked must block metadata under full trust")
	}
	if IsBlocked(net.ParseIP("10.0.0.1"), Options{AllowPrivate: true}) != false {
		t.Error("IsBlocked must allow RFC1918 when AllowPrivate is set")
	}
}
```

Also update the existing `TestBlockedWithLoopbackAllowed` calls from 2-arg to 3-arg:
- `blockedWith(net.ParseIP("127.0.0.1"), true)` → `blockedWith(net.ParseIP("127.0.0.1"), true, false)`
- `blockedWith(net.ParseIP("::1"), true)` → `blockedWith(net.ParseIP("::1"), true, false)`
- inside the `for _, bad` loop: `blockedWith(net.ParseIP(bad), true)` → `blockedWith(net.ParseIP(bad), true, false)`
- `blockedWith(net.ParseIP("127.0.0.1"), false)` → `blockedWith(net.ParseIP("127.0.0.1"), false, false)`

- [ ] **Step 2: Run — verify it fails to compile** (signature mismatch)

Run: `go test ./internal/platform/netsafe/`
Expected: FAIL — `too few arguments in call to blockedWith` / `undefined: IsBlocked`.

- [ ] **Step 3: Rewrite `blockedWith`, add `IsBlocked`, extend `Options`** in `client.go`:

```go
// blockedWith reports whether ip must be refused. allowLoopback permits 127/8 + ::1;
// allowPrivate permits RFC1918 + IPv6 ULA (fc00::/7). Cloud-metadata and link-local
// addresses are refused unconditionally — a trusted credential must never reach IMDS.
func blockedWith(ip net.IP, allowLoopback, allowPrivate bool) bool {
	if ip == nil {
		return true
	}
	// (1) Metadata IPs: blocked before any flag. fd00:ec2::254 is itself an fc00::/7
	// ULA, so this MUST precede the IsPrivate() gate or allowPrivate would leak IMDS.
	for _, m := range metadataIPs {
		if ip.Equal(m) {
			return true
		}
	}
	// (2) Link-local (incl. 169.254.169.254 IMDS-v4), multicast, unspecified: always blocked.
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
		return true
	}
	// (3) Loopback: permitted only when explicitly trusted.
	if ip.IsLoopback() {
		return !allowLoopback
	}
	// (4) RFC1918 + IPv6 ULA: permitted only when explicitly trusted.
	if ip.IsPrivate() {
		return !allowPrivate
	}
	return false
}

// Blocked reports whether ip is a destination outbound requests must refuse
// (loopback + private blocked — the default, locked-secure posture).
func Blocked(ip net.IP) bool { return blockedWith(ip, false, false) }

// IsBlocked reports whether ip must be refused under o. Exposed so a caller can
// pre-validate a LITERAL base_url host with the EXACT dialer policy (see the
// credential service's create-time guard) rather than reimplementing it.
func IsBlocked(ip net.IP, o Options) bool { return blockedWith(ip, o.AllowLoopback, o.AllowPrivate) }

// Options configures a guarded client.
type Options struct {
	AllowLoopback bool // permits 127/8 + ::1 (dev MCP / self-host)
	AllowPrivate  bool // permits RFC1918 + IPv6 ULA; metadata stays blocked
}
```

In `NewClientWithOptions`, update the dialer check to pass both flags:

```go
				if blockedWith(ip.IP, o.AllowLoopback, o.AllowPrivate) {
```

- [ ] **Step 4: Run — verify pass**

Run: `go test ./internal/platform/netsafe/ -v -run TestBlocked`
Expected: PASS (`TestBlocked`, `TestBlockedWithLoopbackAllowed`, `TestBlockedWithPrivateAllowed`).

- [ ] **Step 5: Commit**

```bash
gofmt -w internal/platform/netsafe/
git add internal/platform/netsafe/ .beads/issues.jsonl
git commit -m "feat(netsafe): per-call AllowPrivate option + metadata-first block ordering (manyforge-deo.9)

Adds Options.AllowPrivate (RFC1918 + IPv6 ULA) and exports IsBlocked so callers
reuse the exact dialer policy. Reorders blockedWith to check cloud-metadata IPs
first so no flag can ever unblock IMDS (fd00:ec2::254 is itself a ULA)."
```

---

## Task 2: ai factory — per-credential trust + source-pin update

**Files:**
- Modify: `internal/platform/ai/factory.go`
- Test: `internal/platform/ai/factory_test.go`
- Modify (pin): `internal/security_regression/ai_provider_ssrf_pin_test.go`

- [ ] **Step 1: Write the failing behavioral test** — add to `factory_test.go` (and add imports `context`, `net/http`, `net/http/httptest` to the existing `errors`, `testing`):

```go
func TestFactoryAllowPrivateBaseURL(t *testing.T) {
	// httptest binds 127.0.0.1 — exactly what netsafe blocks by default. This proves
	// the per-credential flag threads through factory.New into the dialer policy.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(loadGolden(t, "openai_text.json"))
	}))
	defer srv.Close()
	req := Request{Model: "m", MaxTokens: 16, Messages: []Message{{Role: RoleUser, Text: "hi"}}}

	// Trust OFF (default): loopback dial is refused.
	off, err := New(Credential{Provider: ProviderOllama, BaseURL: srv.URL + "/v1", Model: "m"})
	if err != nil {
		t.Fatalf("New(off): %v", err)
	}
	if _, err := off.Complete(context.Background(), req); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("trust off: want ErrProviderUnavailable (dial refused), got %v", err)
	}

	// Trust ON: the same loopback base_url is reachable and parses.
	on, err := New(Credential{Provider: ProviderOllama, BaseURL: srv.URL + "/v1", Model: "m", AllowPrivateBaseURL: true})
	if err != nil {
		t.Fatalf("New(on): %v", err)
	}
	resp, err := on.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("trust on: Complete: %v", err)
	}
	if resp.Text == "" {
		t.Fatal("trust on: expected a parsed response body")
	}
}
```

- [ ] **Step 2: Run — verify it fails** (unknown field)

Run: `go test ./internal/platform/ai/ -run TestFactoryAllowPrivateBaseURL`
Expected: FAIL — `unknown field 'AllowPrivateBaseURL' in struct literal`.

- [ ] **Step 3: Add the field + per-credential client** in `factory.go`:

Add to the `Credential` struct (after `Model`):

```go
	AllowPrivateBaseURL bool // self-host opt-in: permit a loopback/RFC1918 base_url for THIS credential
```

Replace the `hc := netsafe.NewClient(defaultRequestTimeout)` line in `New` with:

```go
	hc := netsafe.NewClientWithOptions(defaultRequestTimeout, netsafe.Options{
		AllowLoopback: cred.AllowPrivateBaseURL,
		AllowPrivate:  cred.AllowPrivateBaseURL,
	})
```

- [ ] **Step 4: Run — verify the new test passes; then run the existing SSRF behavioral pin (it must still pass, trust defaults off):**

Run: `go test ./internal/platform/ai/ -run TestFactory`
Expected: PASS (dispatch, unknown, requires-base-url, wires-client, allow-private).

Run: `go test ./internal/security_regression/ -run TestAIProviderFactory_RefusesPrivateBaseURL`
Expected: PASS (default trust off → private still refused).

- [ ] **Step 5: Run the source pin — verify it now FAILS, then fix it.** The factory no longer calls `netsafe.NewClient` (it calls `NewClientWithOptions`), so the AST pin breaks:

Run: `go test ./internal/security_regression/ -run TestAIFactory_UsesNetsafeSource`
Expected: FAIL — "factory.go no longer calls netsafe.NewClient".

In `ai_provider_ssrf_pin_test.go`, update the AST match to accept either constructor:

```go
			if ok && pkg.Name == "netsafe" && (sel.Sel.Name == "NewClient" || sel.Sel.Name == "NewClientWithOptions") {
				found = true
			}
```

And update the failure message:

```go
		t.Fatalf("factory.go no longer calls netsafe.NewClient/NewClientWithOptions — SSRF guard dropped")
```

- [ ] **Step 6: Run — verify pin passes**

Run: `go test ./internal/security_regression/ -run TestAIFactory`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
gofmt -w internal/platform/ai/ internal/security_regression/
git add internal/platform/ai/ internal/security_regression/ai_provider_ssrf_pin_test.go .beads/issues.jsonl
git commit -m "feat(ai): per-credential AllowPrivateBaseURL threads into netsafe options (manyforge-deo.9)

factory.New now builds the guarded client from the credential's trust flag; a
trusted credential reaches a loopback/RFC1918 base_url, an untrusted one is
refused. Updates the SSRF source pin to accept NewClientWithOptions."
```

---

## Task 3: DB — migration 0039 + sqlc mirror

**Files:**
- Create: `migrations/0039_ai_credential_allow_private.up.sql`, `migrations/0039_ai_credential_allow_private.down.sql`
- Modify: `db/schema.sql`, `db/query/ai.sql`

- [ ] **Step 1: Write the migration** — `migrations/0039_ai_credential_allow_private.up.sql`:

```sql
-- US8 (spec 003): per-credential opt-in to reach a self-hosted Ollama/vLLM on a
-- private/loopback base_url. Default false keeps every existing + new credential
-- locked to public destinations unless an operator explicitly trusts it.
ALTER TABLE ai_provider_credential
    ADD COLUMN allow_private_base_url boolean NOT NULL DEFAULT false;
```

`migrations/0039_ai_credential_allow_private.down.sql`:

```sql
ALTER TABLE ai_provider_credential DROP COLUMN allow_private_base_url;
```

- [ ] **Step 2: Mirror into `db/schema.sql`** (sqlc reads schema.sql, not migrations; strip DEFAULT per convention). In the `ai_provider_credential` table, add the column after `updated_at timestamptz NOT NULL,` (line 303) and before `UNIQUE (business_id, provider),`:

```sql
    allow_private_base_url boolean NOT NULL,
```

- [ ] **Step 3: Add the Insert arg** in `db/query/ai.sql` — `InsertAIProviderCredential` column list and SELECT:

Column list becomes:
```sql
INSERT INTO ai_provider_credential (
    id, business_id, tenant_root_id, provider, sealed_key_ref, base_url, default_model,
    allow_private_base_url, created_at, updated_at)
```

SELECT list becomes (add the arg before `now(), now()`):
```sql
    sqlc.arg('default_model'),
    sqlc.arg('allow_private_base_url'),
    now(), now()
```

(`GetAIProviderCredential` etc. are `SELECT *` and pick up the column automatically.)

- [ ] **Step 4: Regenerate + verify build**

Run: `make generate && go build ./...`
Expected: build clean.

Then **read** the generated field name (casing is unpredictable):

Run: `grep -n "AllowPrivateBaseUrl\|AllowPrivateBase" internal/platform/db/dbgen/*.go | head`
Expected: a field like `AllowPrivateBaseUrl bool` on both `InsertAIProviderCredentialParams` and the `AiProviderCredential` row. **Use that exact name in Tasks 4 & 6.** (If casing differs, adjust those tasks accordingly.)

- [ ] **Step 5: Commit**

```bash
gofmt -w ./...
git add migrations/0039_ai_credential_allow_private.up.sql migrations/0039_ai_credential_allow_private.down.sql db/schema.sql db/query/ai.sql internal/platform/db/dbgen/ .beads/issues.jsonl
git commit -m "feat(db): migration 0039 — ai_provider_credential.allow_private_base_url (manyforge-deo.9)"
```

> Dev DB note (only if running `air`): apply 0039 with the OWNER DSN before restart, since the startup guard refuses on schema drift:
> `migrate -path migrations -database "postgres://manyforge:devpassword@localhost:55432/manyforge?sslmode=disable" up`

---

## Task 4: credential service — carry the flag end-to-end

**Files:**
- Modify: `internal/agents/credential.go`, `internal/agents/run_adapters.go`
- Test: `internal/agents/credential_test.go`

- [ ] **Step 1: Write the failing unit test** — add to `credential_test.go`:

```go
func TestResolveRowCarriesAllowPrivate(t *testing.T) {
	svc := &CredentialService{} // no sealer needed when SealedKeyRef is nil
	got, err := svc.resolveRow(storedCredential{
		Provider: "ollama", SealedKeyRef: nil, DefaultModel: "llama3", AllowPrivateBaseURL: true,
	})
	if err != nil {
		t.Fatalf("resolveRow: %v", err)
	}
	if !got.AllowPrivateBaseURL {
		t.Fatal("AllowPrivateBaseURL did not round-trip through resolveRow")
	}
}
```

- [ ] **Step 2: Run — verify it fails** (unknown field)

Run: `go test ./internal/agents/ -run TestResolveRowCarriesAllowPrivate`
Expected: FAIL — `unknown field 'AllowPrivateBaseURL' in struct literal of type storedCredential`.

- [ ] **Step 3: Add the field to the three structs + thread it through** in `credential.go`:

`CreateCredentialInput` (after `DefaultModel`):
```go
	AllowPrivateBaseURL bool // self-host opt-in: permit a loopback/RFC1918 base_url
```

`ResolvedCredential` (after `Model`):
```go
	AllowPrivateBaseURL bool
```

`storedCredential` (after `DefaultModel`):
```go
	AllowPrivateBaseURL bool
```

In `resolveRow`, set it on the output (after the `Model` field is assigned):
```go
	out.AllowPrivateBaseURL = row.AllowPrivateBaseURL
```

In `Create`, add to `InsertAIProviderCredentialParams` (use the exact dbgen name from Task 3 Step 4):
```go
				AllowPrivateBaseUrl: in.AllowPrivateBaseURL,
```

In `Resolve`, map the dbgen row into `storedCredential` (add the field):
```go
		return s.resolveRow(storedCredential{
			Provider: string(row.Provider), SealedKeyRef: row.SealedKeyRef,
			BaseURL: row.BaseUrl, DefaultModel: row.DefaultModel,
			AllowPrivateBaseURL: row.AllowPrivateBaseUrl,
		})
```

- [ ] **Step 4: Pass it into the provider** in `run_adapters.go` — update the `ai.New(ai.Credential{...})` call in `NewCredentialProviderFactory`:

```go
		p, perr := ai.New(ai.Credential{
			Provider: rc.Provider, APIKey: rc.APIKey, BaseURL: rc.BaseURL,
			Model: rc.Model, AllowPrivateBaseURL: rc.AllowPrivateBaseURL,
		})
```

- [ ] **Step 5: Run — verify pass + build**

Run: `go test ./internal/agents/ -run TestResolveRowCarriesAllowPrivate && go build ./...`
Expected: PASS + clean build.

- [ ] **Step 6: Commit**

```bash
gofmt -w internal/agents/
git add internal/agents/credential.go internal/agents/run_adapters.go internal/agents/credential_test.go .beads/issues.jsonl
git commit -m "feat(agents): carry allow_private_base_url through credential resolve -> provider (manyforge-deo.9)"
```

---

## Task 5: credential service — create-time base_url validation

**Files:**
- Modify: `internal/agents/credential.go`
- Test: `internal/agents/credential_test.go`

- [ ] **Step 1: Write the failing matrix test** — add to `credential_test.go`:

```go
func TestValidateBaseURL(t *testing.T) {
	svc := &CredentialService{}
	cases := []struct {
		name    string
		in      CreateCredentialInput
		wantErr bool
	}{
		{"anthropic needs no base_url", CreateCredentialInput{Provider: "anthropic", DefaultModel: "m"}, false},
		{"openai missing base_url", CreateCredentialInput{Provider: "openai", DefaultModel: "m"}, true},
		{"openai public base_url", CreateCredentialInput{Provider: "openai", DefaultModel: "m", BaseURL: "https://api.example.com/v1"}, false},
		{"openai junk base_url", CreateCredentialInput{Provider: "openai", DefaultModel: "m", BaseURL: "not a url"}, true},
		{"openai non-http scheme", CreateCredentialInput{Provider: "openai", DefaultModel: "m", BaseURL: "ftp://x/v1"}, true},
		{"ollama private IP, trust off -> reject", CreateCredentialInput{Provider: "ollama", DefaultModel: "m", BaseURL: "http://192.168.1.10:11434/v1"}, true},
		{"ollama private IP, trust on -> ok", CreateCredentialInput{Provider: "ollama", DefaultModel: "m", BaseURL: "http://192.168.1.10:11434/v1", AllowPrivateBaseURL: true}, false},
		{"ollama loopback, trust on -> ok", CreateCredentialInput{Provider: "ollama", DefaultModel: "m", BaseURL: "http://127.0.0.1:11434/v1", AllowPrivateBaseURL: true}, false},
		{"ollama metadata IP, trust on -> STILL reject", CreateCredentialInput{Provider: "ollama", DefaultModel: "m", BaseURL: "http://169.254.169.254/v1", AllowPrivateBaseURL: true}, true},
		{"ollama hostname not resolved at create", CreateCredentialInput{Provider: "ollama", DefaultModel: "m", BaseURL: "http://my-ollama.local/v1"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := svc.validate(tc.in)
			if tc.wantErr && err == nil {
				t.Fatalf("want error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("want ok, got %v", err)
			}
		})
	}
}
```

- [ ] **Step 2: Run — verify it fails** (validation not yet present)

Run: `go test ./internal/agents/ -run TestValidateBaseURL`
Expected: FAIL — cases like "openai missing base_url" / "ollama private IP, trust off" return nil today.

- [ ] **Step 3: Extend `validate` + add `validateBaseURL`** in `credential.go`. First add imports `"net"`, `"net/url"`, and `"github.com/manyforge/manyforge/internal/platform/netsafe"`. Then replace `validate`:

```go
func (s *CredentialService) validate(in CreateCredentialInput) error {
	if !knownProviders[in.Provider] {
		return fmt.Errorf("agents: unknown provider %q: %w", in.Provider, errs.ErrValidation)
	}
	if in.DefaultModel == "" {
		return fmt.Errorf("agents: default_model required: %w", errs.ErrValidation)
	}
	// openai-compat providers (openai/ollama/vllm) route through a base_url; require
	// it at the boundary so a missing one is a clean 400, not a later factory error.
	if in.Provider != "anthropic" && in.BaseURL == "" {
		return fmt.Errorf("agents: base_url required for provider %q: %w", in.Provider, errs.ErrValidation)
	}
	if in.BaseURL != "" {
		if err := validateBaseURL(in.BaseURL, in.AllowPrivateBaseURL); err != nil {
			return err
		}
	}
	return nil
}

// validateBaseURL is a best-effort create-time guard: it pins the URL shape and,
// for a LITERAL IP host, applies the exact netsafe dialer policy. Hostnames are
// NOT resolved here (DNS can rebind) — dial-time netsafe stays authoritative.
func validateBaseURL(raw string, allowPrivate bool) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return fmt.Errorf("agents: base_url must be a valid http(s) URL: %w", errs.ErrValidation)
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil {
		if netsafe.IsBlocked(ip, netsafe.Options{AllowLoopback: allowPrivate, AllowPrivate: allowPrivate}) {
			return fmt.Errorf("agents: base_url %q is a blocked address: %w", raw, errs.ErrValidation)
		}
	}
	return nil
}
```

- [ ] **Step 4: Run — verify pass; confirm the pre-existing validate test still passes**

Run: `go test ./internal/agents/ -run 'TestValidateBaseURL|TestCredentialValidate'`
Expected: PASS (the existing anthropic-empty-model and bogus-provider cases at `credential_test.go:60-63` are unaffected).

- [ ] **Step 5: Commit**

```bash
gofmt -w internal/agents/
git add internal/agents/credential.go internal/agents/credential_test.go .beads/issues.jsonl
git commit -m "feat(agents): create-time base_url validation (require + literal-IP guard) (manyforge-deo.9)

openai-compat providers must supply a base_url; a literal private/loopback IP is
rejected unless the credential is trusted, and metadata IPs are rejected even when
trusted. Hostnames defer to dial-time netsafe (DNS rebind)."
```

---

## Task 6: audit the trust grant inside the Create transaction

**Files:**
- Modify: `internal/agents/credential.go`
- Test: `internal/agents/credential_integration_test.go`

- [ ] **Step 1: Write the failing integration test** — add to `credential_integration_test.go` (file already has `//go:build integration` + the imports/helpers used below):

```go
func TestCredentialTrustGrantAudited(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	ten := seedAgentTenant(ctx, t, tdb)
	svc := &CredentialService{DB: tdb.App, Sealer: newTestSealer(t)}

	// A trusted self-host credential writes exactly one trust-grant audit row, atomically.
	id, err := svc.Create(ctx, ten.principalID, ten.businessID, CreateCredentialInput{
		Provider: "ollama", DefaultModel: "llama3",
		BaseURL: "http://127.0.0.1:11434/v1", AllowPrivateBaseURL: true,
	})
	if err != nil {
		t.Fatalf("create trusted: %v", err)
	}
	var n int
	if err := tdb.Super.QueryRow(ctx,
		`SELECT count(*) FROM audit_entry WHERE target_id=$1 AND action='ai_credential.create' AND decision='trust_private_base_url'`,
		id).Scan(&n); err != nil {
		t.Fatalf("count trust audit: %v", err)
	}
	if n != 1 {
		t.Fatalf("want 1 trust-grant audit row, got %d", n)
	}

	// A non-trusted credential writes NO trust-grant row.
	id2, err := svc.Create(ctx, ten.principalID, ten.businessID, CreateCredentialInput{
		Provider: "openai", DefaultModel: "gpt-4o", BaseURL: "https://api.example.com/v1",
	})
	if err != nil {
		t.Fatalf("create untrusted: %v", err)
	}
	if err := tdb.Super.QueryRow(ctx,
		`SELECT count(*) FROM audit_entry WHERE target_id=$1 AND decision='trust_private_base_url'`,
		id2).Scan(&n); err != nil {
		t.Fatalf("count untrusted audit: %v", err)
	}
	if n != 0 {
		t.Fatalf("untrusted credential must write no trust-grant row, got %d", n)
	}
}
```

- [ ] **Step 2: Run — verify it fails** (no audit written yet)

Run: `go test -tags integration ./internal/agents/ -run TestCredentialTrustGrantAudited -p 1`
Expected: FAIL — got 0 trust-grant rows, want 1.

- [ ] **Step 3: Write the audit inside the Create tx** in `credential.go`. Add import `"github.com/manyforge/manyforge/internal/platform/audit"`. Replace the `WithPrincipal` closure body in `Create`:

```go
	err = s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		if _, qerr := dbgen.New(tx).InsertAIProviderCredential(ctx, dbgen.InsertAIProviderCredentialParams{
			ID:                  id,
			BusinessID:          businessID,
			Provider:            dbgen.AiProvider(in.Provider),
			SealedKeyRef:        refArg,
			BaseUrl:             baseArg,
			DefaultModel:        in.DefaultModel,
			AllowPrivateBaseUrl: in.AllowPrivateBaseURL,
		}); qerr != nil {
			return qerr
		}
		// Trusting a private/loopback endpoint is a security-sensitive grant — audit it
		// in the SAME tx as the insert so there is never a trusted credential without
		// its trail (atomicity invariant).
		if in.AllowPrivateBaseURL {
			tt := "ai_provider_credential"
			dec := "trust_private_base_url"
			return audit.Write(ctx, tx, audit.Entry{
				BusinessID:       &businessID,
				ActorPrincipalID: &principalID,
				Action:           "ai_credential.create",
				TargetType:       &tt,
				TargetID:         &id,
				Decision:         &dec,
				Inputs:           map[string]any{"provider": in.Provider, "base_url": in.BaseURL},
			})
		}
		return nil
	})
```

- [ ] **Step 4: Run — verify pass**

Run: `go test -tags integration ./internal/agents/ -run TestCredentialTrustGrantAudited -p 1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -w internal/agents/
git add internal/agents/credential.go internal/agents/credential_integration_test.go .beads/issues.jsonl
git commit -m "feat(agents): audit allow_private_base_url trust grant in the Create tx (manyforge-deo.9)"
```

---

## Task 7: Ollama/vLLM fixtures + golden round-trip + record helpers

**Files:**
- Create: `internal/platform/ai/testdata/{ollama_text,ollama_tool_calls,vllm_text,vllm_tool_calls}.json`
- Modify: `internal/platform/ai/openaicompat_test.go`, `internal/platform/ai/fixtures_test.go`

- [ ] **Step 1: Author the four fixtures** (real OpenAI-compatible wire shapes; Ollama/vLLM both expose `/v1/chat/completions` but with their own `model` ids and `usage`).

`testdata/ollama_text.json`:
```json
{
  "id": "chatcmpl-ollama-1",
  "object": "chat.completion",
  "model": "llama3.1",
  "choices": [
    {
      "index": 0,
      "message": { "role": "assistant", "content": "Sure — I can help with that ticket." },
      "finish_reason": "stop"
    }
  ],
  "usage": { "prompt_tokens": 31, "completion_tokens": 9, "total_tokens": 40 }
}
```

`testdata/ollama_tool_calls.json`:
```json
{
  "id": "chatcmpl-ollama-2",
  "object": "chat.completion",
  "model": "llama3.1",
  "choices": [
    {
      "index": 0,
      "message": {
        "role": "assistant",
        "content": "",
        "tool_calls": [
          {
            "id": "call_ollama_1",
            "type": "function",
            "function": { "name": "set_priority", "arguments": "{\"id\":\"t-7\",\"priority\":\"high\"}" }
          }
        ]
      },
      "finish_reason": "tool_calls"
    }
  ],
  "usage": { "prompt_tokens": 44, "completion_tokens": 21, "total_tokens": 65 }
}
```

`testdata/vllm_text.json`:
```json
{
  "id": "cmpl-vllm-1",
  "object": "chat.completion",
  "model": "meta-llama/Llama-3.1-8B-Instruct",
  "choices": [
    {
      "index": 0,
      "message": { "role": "assistant", "content": "Happy to help with this ticket." },
      "finish_reason": "stop"
    }
  ],
  "usage": { "prompt_tokens": 27, "completion_tokens": 7, "total_tokens": 34 }
}
```

`testdata/vllm_tool_calls.json`:
```json
{
  "id": "cmpl-vllm-2",
  "object": "chat.completion",
  "model": "meta-llama/Llama-3.1-8B-Instruct",
  "choices": [
    {
      "index": 0,
      "message": {
        "role": "assistant",
        "content": null,
        "tool_calls": [
          {
            "id": "call_vllm_1",
            "type": "function",
            "function": { "name": "set_status", "arguments": "{\"id\":\"t-9\",\"status\":\"closed\"}" }
          }
        ]
      },
      "finish_reason": "tool_calls"
    }
  ],
  "usage": { "prompt_tokens": 39, "completion_tokens": 18, "total_tokens": 57 }
}
```

- [ ] **Step 2: Add cases to the golden round-trip** in `openaicompat_test.go` — extend the `cases` slice in `TestOpenAIComplete_GoldenRoundTrip`:

```go
		{"openai_text.json", "Hello! How can I help with your ticket?", "", FinishStop},
		{"openai_tool_calls.json", "", "get_ticket", FinishToolUse},
		{"ollama_text.json", "Sure — I can help with that ticket.", "", FinishStop},
		{"ollama_tool_calls.json", "", "set_priority", FinishToolUse},
		{"vllm_text.json", "Happy to help with this ticket.", "", FinishStop},
		{"vllm_tool_calls.json", "", "set_status", FinishToolUse},
```

- [ ] **Step 3: Run — verify all six cases pass** (proves the transport parses each backend's real shape)

Run: `go test ./internal/platform/ai/ -run TestOpenAIComplete_GoldenRoundTrip -v`
Expected: PASS for all six subtests.

- [ ] **Step 4: Add record helpers** in `fixtures_test.go` (maintainer-only, `AI_RECORD`-gated; mirror `TestRecordOpenAIFixture`):

```go
// TestRecordOllamaFixture refreshes the ollama fixtures from a live Ollama server.
// Run: AI_RECORD=1 OLLAMA_BASE_URL=http://localhost:11434/v1 \
//
//	go test ./internal/platform/ai/ -run TestRecordOllamaFixture -v
func TestRecordOllamaFixture(t *testing.T) {
	if !recording() {
		t.Skip("set AI_RECORD=1 to refresh golden fixtures from a live server")
	}
	base := os.Getenv("OLLAMA_BASE_URL")
	if base == "" {
		t.Skip("OLLAMA_BASE_URL not set")
	}
	p := NewOpenAICompatProvider("", base, "llama3.1", nil)
	resp, err := p.Complete(context.Background(), Request{
		Model: "llama3.1", MaxTokens: 64,
		Messages: []Message{{Role: RoleUser, Text: "Say hello in one short sentence."}},
	})
	if err != nil {
		t.Fatalf("live call: %v", err)
	}
	t.Logf("recorded ollama response: text=%q finish=%s usage=%+v", resp.Text, resp.FinishReason, resp.Usage)
}

// TestRecordVLLMFixture mirrors the above for a live vLLM OpenAI-compatible server.
// Run: AI_RECORD=1 VLLM_BASE_URL=http://localhost:8000/v1 \
//
//	go test ./internal/platform/ai/ -run TestRecordVLLMFixture -v
func TestRecordVLLMFixture(t *testing.T) {
	if !recording() {
		t.Skip("set AI_RECORD=1 to refresh golden fixtures from a live server")
	}
	base := os.Getenv("VLLM_BASE_URL")
	if base == "" {
		t.Skip("VLLM_BASE_URL not set")
	}
	p := NewOpenAICompatProvider("", base, "meta-llama/Llama-3.1-8B-Instruct", nil)
	resp, err := p.Complete(context.Background(), Request{
		Model: "meta-llama/Llama-3.1-8B-Instruct", MaxTokens: 64,
		Messages: []Message{{Role: RoleUser, Text: "Say hello in one short sentence."}},
	})
	if err != nil {
		t.Fatalf("live call: %v", err)
	}
	t.Logf("recorded vllm response: text=%q finish=%s usage=%+v", resp.Text, resp.FinishReason, resp.Usage)
}
```

- [ ] **Step 5: Run — verify the helpers compile + skip in CI**

Run: `go test ./internal/platform/ai/ -run TestRecord -v`
Expected: all `TestRecord*` SKIP (AI_RECORD unset).

- [ ] **Step 6: Commit**

```bash
gofmt -w internal/platform/ai/
git add internal/platform/ai/testdata/ internal/platform/ai/openaicompat_test.go internal/platform/ai/fixtures_test.go .beads/issues.jsonl
git commit -m "test(ai): Ollama + vLLM golden fixtures (text + tool-call) + record helpers (manyforge-deo.9)"
```

---

## Task 8: end-to-end config — DB credential → factory → transport per provider

**Files:**
- Create: `internal/agents/multi_provider_integration_test.go`

- [ ] **Step 1: Write the integration test** (proves the *config plumbing*: a stored credential's provider + base_url + trust flag flow through `Resolve` → `NewCredentialProviderFactory` → the transport. Wire-shape fidelity is owned by Task 7's golden round-trip, so this serves a minimal inline body and binds loopback — which requires the trust flag, exercising it end-to-end):

```go
//go:build integration

package agents

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/manyforge/manyforge/internal/platform/ai"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
)

func TestMultiProviderConfigEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })

	svc := &CredentialService{DB: tdb.App, Sealer: newTestSealer(t)}
	factory := NewCredentialProviderFactory(svc)

	// A loopback OpenAI-compatible stub — what a self-hosted Ollama/vLLM looks like.
	body := `{"id":"x","object":"chat.completion","model":"m",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	for _, provider := range []string{"ollama", "vllm"} {
		t.Run(provider, func(t *testing.T) {
			ten := seedAgentTenant(ctx, t, tdb) // fresh business: one credential per (business, provider)
			if _, err := svc.Create(ctx, ten.principalID, ten.businessID, CreateCredentialInput{
				Provider: provider, DefaultModel: "m",
				BaseURL: srv.URL + "/v1", AllowPrivateBaseURL: true, // loopback stub ⇒ must be trusted
			}); err != nil {
				t.Fatalf("create %s credential: %v", provider, err)
			}

			p, model, err := factory(ctx, ten.principalID, ten.businessID, provider)
			if err != nil {
				t.Fatalf("factory(%s): %v", provider, err)
			}
			if model != "m" {
				t.Fatalf("model = %q, want m", model)
			}
			resp, err := p.Complete(ctx, ai.Request{
				Model: model, MaxTokens: 16, Messages: []ai.Message{{Role: ai.RoleUser, Text: "hi"}},
			})
			if err != nil {
				t.Fatalf("Complete(%s): %v", provider, err)
			}
			if resp.Text != "ok" {
				t.Fatalf("%s resp.Text = %q, want ok", provider, resp.Text)
			}
		})
	}
}
```

- [ ] **Step 2: Run — verify pass**

Run: `go test -tags integration ./internal/agents/ -run TestMultiProviderConfigEndToEnd -p 1 -v`
Expected: PASS for `ollama` and `vllm` subtests.

- [ ] **Step 3: Commit**

```bash
gofmt -w internal/agents/
git add internal/agents/multi_provider_integration_test.go .beads/issues.jsonl
git commit -m "test(agents): e2e config path — DB credential -> factory -> transport per provider (manyforge-deo.9)"
```

---

## Task 9: self-host cost — testable `NewRegistryCostFn` + debug log

**Files:**
- Modify: `internal/agents/model_pricing.go`, `cmd/manyforge/main.go`
- Test: `internal/agents/model_pricing_test.go` (create if absent)

- [ ] **Step 1: Write the failing unit test** — `model_pricing_test.go`:

```go
package agents

import (
	"testing"

	"github.com/manyforge/manyforge/internal/platform/ai"
)

func TestRegistryCostFn_UnknownModelIsFree(t *testing.T) {
	reg := ai.NewRegistry()
	ai.RegisterDefaults(reg) // anthropic + openai only — no self-host models
	cost := NewRegistryCostFn(reg, nil)

	// A self-hosted model absent from the catalog costs 0 — never an error/panic.
	if c := cost("llama3.1:70b", ai.Usage{InputTokens: 1000, OutputTokens: 1000}); c != 0 {
		t.Fatalf("unknown model cost = %d, want 0", c)
	}
	// Sanity: a known model still prices > 0 (the fn isn't always-zero).
	if c := cost("gpt-4o", ai.Usage{InputTokens: 1_000_000, OutputTokens: 0}); c <= 0 {
		t.Fatalf("known model cost = %d, want > 0", c)
	}
}
```

- [ ] **Step 2: Run — verify it fails** (function undefined)

Run: `go test ./internal/agents/ -run TestRegistryCostFn_UnknownModelIsFree`
Expected: FAIL — `undefined: NewRegistryCostFn`.

- [ ] **Step 3: Add `NewRegistryCostFn`** to `model_pricing.go`. Add `"log/slog"` to the import block, then:

```go
// NewRegistryCostFn returns the Engine's per-call cost function. A model absent from
// the pricing catalog (e.g. a self-hosted Ollama/vLLM tag, whose ids are user-defined
// and unbounded) costs 0 — self-hosting has no marginal token cost — and the miss is
// debug-logged so a missing-but-paid model is still noticeable. logger may be nil.
func NewRegistryCostFn(reg *ai.Registry, logger *slog.Logger) func(model string, u ai.Usage) int64 {
	return func(model string, u ai.Usage) int64 {
		m, ok := reg.Lookup(model)
		if !ok {
			if logger != nil {
				logger.Debug("model not in pricing catalog; cost=0", "model", model)
			}
			return 0
		}
		return m.CostCents(u)
	}
}
```

- [ ] **Step 4: Wire it in `main.go`** — replace the inline `Cost:` closure (currently `cmd/manyforge/main.go:169-175`) with:

```go
		Cost: agents.NewRegistryCostFn(aiReg, logger),
```

- [ ] **Step 5: Run — verify pass + build**

Run: `go test ./internal/agents/ -run TestRegistryCostFn_UnknownModelIsFree && go build ./...`
Expected: PASS + clean build.

- [ ] **Step 6: Commit**

```bash
gofmt -w internal/agents/ cmd/manyforge/
git add internal/agents/model_pricing.go internal/agents/model_pricing_test.go cmd/manyforge/main.go .beads/issues.jsonl
git commit -m "feat(agents): NewRegistryCostFn — unknown self-host model costs 0 + debug-log the miss (manyforge-deo.9)"
```

---

## Task 10: US8 security-regression pins

**Files:**
- Create: `internal/security_regression/us8_self_host_ssrf_pin_test.go`
- Create: `internal/security_regression/us8_self_host_cost_pin_test.go`

- [ ] **Step 1: Write the SSRF pin file** — `us8_self_host_ssrf_pin_test.go`:

```go
// Finding: US8 / Spec 003 §3.5 — the per-credential allow_private_base_url trust flag
// loosens netsafe for loopback + RFC1918 ONLY. Cloud-metadata IPs stay blocked even
// under full trust, and an untrusted private base_url is refused at create time.
// See manyforge-deo.9.
package security_regression

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/manyforge/manyforge/internal/platform/ai"
	"github.com/manyforge/manyforge/internal/platform/netsafe"
)

// Metadata IPs must never be reachable, even when a credential is fully trusted.
func TestUS8_MetadataBlockedUnderTrust(t *testing.T) {
	full := netsafe.Options{AllowLoopback: true, AllowPrivate: true}
	for _, ip := range []string{"169.254.169.254", "fd00:ec2::254"} {
		if !netsafe.IsBlocked(net.ParseIP(ip), full) {
			t.Fatalf("metadata %s must stay blocked under AllowLoopback+AllowPrivate", ip)
		}
	}
	// Behavioral: a trusted credential pointed at the metadata endpoint is still refused.
	p, err := ai.New(ai.Credential{
		Provider: "ollama", BaseURL: "http://169.254.169.254/v1", Model: "m", AllowPrivateBaseURL: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := p.Complete(ctx, ai.Request{MaxTokens: 8, Messages: []ai.Message{{Role: ai.RoleUser, Text: "x"}}}); !errors.Is(err, ai.ErrProviderUnavailable) {
		t.Fatalf("trusted metadata base_url -> %v, want ErrProviderUnavailable (refused)", err)
	}
}

// A trusted credential reaches a loopback base_url — the self-host escape hatch works.
func TestUS8_TrustedCredentialReachesLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer srv.Close()
	p, err := ai.New(ai.Credential{Provider: "ollama", BaseURL: srv.URL + "/v1", Model: "m", AllowPrivateBaseURL: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	resp, err := p.Complete(context.Background(), ai.Request{MaxTokens: 8, Messages: []ai.Message{{Role: ai.RoleUser, Text: "x"}}})
	if err != nil {
		t.Fatalf("trusted loopback Complete: %v", err)
	}
	if resp.Text != "ok" {
		t.Fatalf("resp.Text = %q, want ok", resp.Text)
	}
}
```

- [ ] **Step 2: Write the cost pin file** — `us8_self_host_cost_pin_test.go`:

```go
// Finding: US8 / Spec 003 §2 — a self-hosted model absent from the model_pricing
// catalog costs 0 and the run proceeds (self-hosting has no marginal token cost).
// A regression that fails-loud or mischarges unknown models breaks here.
// See manyforge-deo.9.
package security_regression

import (
	"testing"

	"github.com/manyforge/manyforge/internal/agents"
	"github.com/manyforge/manyforge/internal/platform/ai"
)

func TestUS8_UnknownSelfHostModelCostsZero(t *testing.T) {
	reg := ai.NewRegistry()
	ai.RegisterDefaults(reg)
	cost := agents.NewRegistryCostFn(reg, nil)
	if c := cost("qwen2.5:32b", ai.Usage{InputTokens: 50_000, OutputTokens: 50_000}); c != 0 {
		t.Fatalf("unknown self-host model cost = %d, want 0", c)
	}
}
```

- [ ] **Step 3: Run — verify pass**

Run: `go test ./internal/security_regression/ -run TestUS8 -v`
Expected: PASS (`TestUS8_MetadataBlockedUnderTrust`, `TestUS8_TrustedCredentialReachesLoopback`, `TestUS8_UnknownSelfHostModelCostsZero`).

- [ ] **Step 4: Commit**

```bash
gofmt -w internal/security_regression/
git add internal/security_regression/us8_self_host_ssrf_pin_test.go internal/security_regression/us8_self_host_cost_pin_test.go .beads/issues.jsonl
git commit -m "test(sec): US8 pins — metadata blocked under trust, hatch works, unknown model free (manyforge-deo.9)"
```

---

## Task 11: full gate + close

- [ ] **Step 1: Run the full backend gate**

```bash
export PATH="$PATH:$HOME/go/bin"
gofmt -l internal/ cmd/ db/        # MUST be empty
make test
make contract-test
make lint                          # MUST be 0 issues
make sec-test
make int-test                      # ~7 min; needs Docker
```
Expected: all green; `gofmt -l` empty; lint 0.

- [ ] **Step 2: Close the bd issue + file any follow-ups**

```bash
bd close manyforge-deo.9
# File follow-ups if any surfaced (e.g. "expose allow_private_base_url when a credential HTTP/admin surface lands").
```

- [ ] **Step 3: Push (session-completion protocol)**

```bash
git pull --rebase
git push
git status   # MUST show up to date with origin
```

---

## Self-review (completed during planning)

**Spec coverage:**
- §2.1 per-credential trust flag → Tasks 3,4 (column + carry-through). ✓
- §2.2 metadata hard-blocked under trust → Task 1 (reorder) + Task 10 pin. ✓
- §2.3 unknown model = $0 + debug log → Task 9 + Task 10 cost pin. ✓
- §2.4 text+tool-call fixtures per provider → Task 7. ✓
- §3.1 netsafe AllowPrivate + IsBlocked → Task 1. ✓
- §3.2 factory threads flag → Task 2. ✓
- §3.3 carry/validate/audit → Tasks 4,5,6. ✓
- §3.4 migration/schema/query/generate → Task 3. ✓
- §3.5 cost behavior → Task 9. ✓
- §4 fixtures + e2e → Tasks 7,8. ✓
- §5 five security pins → existing `ai_provider_ssrf_pin_test.go` (trust-off refused, updated source pin in Task 2) + Task 10 (metadata-under-trust, hatch-works, cost). ✓
- §6 test plan layers → Tasks 1,2,4,5,6,7,8,9,10. ✓
- §7 out of scope (no credential HTTP surface) → no contract task. ✓

**Type consistency:** `AllowPrivateBaseURL` (Go structs `ai.Credential`/`CreateCredentialInput`/`ResolvedCredential`/`storedCredential`) vs. `AllowPrivateBaseUrl` (dbgen, confirmed in Task 3 Step 4) vs. `allow_private_base_url` (SQL) — used consistently per layer. `netsafe.Options{AllowLoopback, AllowPrivate}`, `netsafe.IsBlocked`, `agents.NewRegistryCostFn`, `ai.Usage{InputTokens, OutputTokens}`, `ai.NewRegistry`/`ai.RegisterDefaults`, `ai.ErrProviderUnavailable` — all verified against source.

**Placeholder scan:** none — every code step is complete and runnable.
