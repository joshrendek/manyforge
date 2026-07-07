# Route Local Providers Through the opencode Sandbox — Implementation Plan (manyforge-9er)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Route local providers (vLLM / Ollama / LM Studio) through the same hardened opencode sandbox as cloud providers, delete the host-side direct-POST review path, and fall back to the dimension's cloud model at runtime when a local lane fails.

**Architecture:** `reviewLane` stops special-casing local providers — every lane builds a `SandboxSpec` and runs `opencode run`. The sandbox reaches the local LAN endpoint through the existing `mf-egress-proxy`, which gains plain-HTTP forwarding (local endpoints are plain `http://`). opencode targets the local endpoint via a bundled `@ai-sdk/openai-compatible` custom provider (verified by spike). Private-LAN egress is gated at the service layer by the credential's `AllowPrivateBaseURL`.

**Tech Stack:** Go 1.25; `sst/opencode` 1.17.11 (bundled `@ai-sdk/openai-compatible`); `cmd/mf-egress-proxy` (Go CONNECT proxy); Kubernetes KubeRunner + Helm chart `charts/manyforge`; DockerRunner for local dev.

## Global Constraints
- **One egress path:** the sandbox reaches the network ONLY via `mf-egress-proxy`; its allowlist (`EGRESS_ALLOW`, matched by `netsafe.ParseHostAllowlist`) is the sole authority. Keep this true.
- **`AllowPrivateBaseURL` gate:** private/RFC1918/ULA egress is permitted only for a credential with `AllowPrivateBaseURL == true` (`credresolver.go:26`). `AICredential` carries this field. Loopback always ok; cloud-metadata/link-local always blocked.
- **opencode local provider:** a custom provider `local` with `"npm": "@ai-sdk/openai-compatible"` + `options.baseURL` (Chat Completions). NEVER the built-in `openai` provider (Responses API `/v1/responses`, which local servers don't serve). Verified by the 2026-07-06 spike (design doc C2).
- **No silent caps / honest failure:** a lane that cannot run is skipped/failed with a recorded reason.
- **sqlc:** no schema change in this epic.
- **Gates:** `make test`, `go test -tags integration -p 1 ./internal/agents/coding/`, `go test -tags contract ./cmd/...`, `make lint`.

## File Structure
- `cmd/mf-egress-proxy/main.go` — add plain-HTTP forwarding (Task 1).
- `deploy/sandbox/entrypoint.sh` — accept `vllm`/`ollama`, emit the `local` openai-compatible provider config (Task 2).
- `internal/agents/coding/fallbackchain.go` — `laneCredFor` egress gate now applies to local, gated by `AllowPrivateBaseURL` (Task 3).
- `internal/agents/coding/service.go` — enqueue-time egress checks (`:240`, `:431`) match Task 3; `reviewLane` routes local through the sandbox (Task 4) and adds runtime cloud fallback (Task 5).
- `internal/agents/coding/localreview.go` → rename to `reviewpayload.go`; keep shared payload/prompt/budget/guard symbols, delete the direct-POST functions (Task 6).
- `internal/agents/coding/localreview_test.go` → `reviewpayload_test.go` (drop deleted-fn tests) (Task 6).
- `charts/manyforge/values.yaml` — document that `sandbox.egressAllow` may include a private host for local providers (Task 7).
- `internal/security_regression/manyforge_9er_*_test.go` — source-pins (Task 8).

Dependencies: T4 needs T2+T3; T5 needs T4; T6 needs T4+T5.

---

### Task 1: Egress proxy — plain-HTTP forwarding

**Files:**
- Modify: `cmd/mf-egress-proxy/main.go`
- Test: `cmd/mf-egress-proxy/main_test.go`

**Interfaces:**
- Consumes: `netsafe.ParseHostAllowlist(string) HostAllowlist`, `HostAllowlist.Allows(host string) bool` (existing).
- Produces: proxy now forwards plain-HTTP for allowlisted hosts (used indirectly by the sandbox at runtime).

- [ ] **Step 1: Write the failing test** — add to `cmd/mf-egress-proxy/main_test.go`. It starts a stub upstream, runs the proxy handler, and sends a plain-HTTP (non-CONNECT) request through it.

```go
func TestProxyForwardsAllowlistedPlainHTTP(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("upstream-ok"))
	}))
	defer upstream.Close()
	host := strings.TrimPrefix(upstream.URL, "http://") // 127.0.0.1:PORT

	proxy := httptest.NewServer(proxyHandler(netsafe.ParseHostAllowlist(host)))
	defer proxy.Close()

	// A client that sends every request through the proxy (plain-HTTP ⇒ absolute-form, not CONNECT).
	pu, _ := url.Parse(proxy.URL)
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(pu)}}

	resp, err := client.Get(upstream.URL)
	if err != nil {
		t.Fatalf("get through proxy: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(b) != "upstream-ok" {
		t.Fatalf("want 200/upstream-ok, got %d/%q", resp.StatusCode, b)
	}
}

func TestProxyRejectsNonAllowlistedPlainHTTP(t *testing.T) {
	proxy := httptest.NewServer(proxyHandler(netsafe.ParseHostAllowlist("api.anthropic.com")))
	defer proxy.Close()
	pu, _ := url.Parse(proxy.URL)
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(pu)}}
	resp, err := client.Get("http://198.51.100.7:9/x") // not allowlisted, never dialed
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("want 403, got %d", resp.StatusCode)
	}
}
```

- [ ] **Step 2: Run it to confirm it fails to compile** — `proxyHandler` does not exist yet.

Run: `go test ./cmd/mf-egress-proxy/ -run TestProxy -v`
Expected: FAIL — `undefined: proxyHandler`.

- [ ] **Step 3: Refactor the handler into `proxyHandler(allow)` and add the plain-HTTP branch.** Replace the body of `main()`'s handler with a call to a named constructor, and add forwarding.

```go
// proxyHandler builds the egress handler: CONNECT tunnels (HTTPS) and plain-HTTP
// forwarding, both gated by the same host allowlist. Local providers use plain HTTP,
// so a non-CONNECT request to an allowlisted host is round-tripped upstream.
func proxyHandler(allow netsafe.HostAllowlist) http.Handler {
	fwd := &http.Transport{Proxy: nil} // no upstream proxy; dial the target directly
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			if !allow.Allows(r.Host) {
				http.Error(w, "egress not allowed", http.StatusForbidden)
				return
			}
			out := r.Clone(r.Context())
			out.RequestURI = "" // required for a client (outbound) request
			resp, err := fwd.RoundTrip(out)
			if err != nil {
				http.Error(w, "upstream error", http.StatusBadGateway)
				return
			}
			defer resp.Body.Close()
			for k, vs := range resp.Header {
				for _, v := range vs {
					w.Header().Add(k, v)
				}
			}
			w.WriteHeader(resp.StatusCode)
			_, _ = io.Copy(flushWriter{w}, resp.Body) // stream (SSE) as chunks arrive
			return
		}
		if !allow.Allows(r.Host) {
			http.Error(w, "egress not allowed", http.StatusForbidden)
			return
		}
		dst, err := net.DialTimeout("tcp", r.Host, 10*time.Second)
		if err != nil {
			http.Error(w, "dial failed", http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
		hj, ok := w.(http.Hijacker)
		if !ok {
			_ = dst.Close()
			return
		}
		src, _, err := hj.Hijack()
		if err != nil {
			_ = dst.Close()
			return
		}
		go func() { _, _ = io.Copy(dst, src); _ = dst.Close() }()
		go func() { _, _ = io.Copy(src, dst); _ = src.Close() }()
	})
}

// flushWriter flushes after each write so SSE chunks reach the sandbox promptly.
type flushWriter struct{ w http.ResponseWriter }

func (f flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if fl, ok := f.w.(http.Flusher); ok {
		fl.Flush()
	}
	return n, err
}
```

Then set `main()`'s server to `srv := &http.Server{Addr: addr, Handler: proxyHandler(allow)}` (drop the inline `h` handler func).

- [ ] **Step 4: Run the tests** — `go test ./cmd/mf-egress-proxy/ -v`. Expected: PASS (forward, 403, and the existing CONNECT test).

- [ ] **Step 5: Commit**

```bash
git add cmd/mf-egress-proxy/main.go cmd/mf-egress-proxy/main_test.go
git commit -m "feat(egress): mf-egress-proxy forwards allowlisted plain-HTTP (manyforge-9er)"
```

---

### Task 2: Sandbox entrypoint — local provider → openai-compatible config

**Files:**
- Modify: `deploy/sandbox/entrypoint.sh`
- Test: `internal/agents/coding/entrypoint_config_test.go` (new — a Go test that runs the entrypoint's config generation in a shell and asserts the JSON)

**Interfaces:**
- Consumes: env `LLM_PROVIDER`, `LLM_MODEL`, `LLM_BASE_URL`, `LLM_API_KEY` (set by `sandboxEnv`, `service.go:1321`; unchanged).
- Produces: for `vllm`/`ollama`, an `opencode.json` with `provider.local.npm == "@ai-sdk/openai-compatible"`, `options.baseURL == $LLM_BASE_URL`, `model == "local/$LLM_MODEL"`, and `auth.json == {"local":{"type":"api","key":"$LLM_API_KEY"}}`.

- [ ] **Step 1: Write the failing test.** Because `entrypoint.sh` isn't factored, the test execs the whole script with `opencode` stubbed by a shell function that just prints the generated config, then asserts. Simplest robust form: a Go test that runs a trimmed copy of the provider/config block via `sh -c`. Create `internal/agents/coding/entrypoint_config_test.go`:

```go
//go:build !integration

package coding

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestEntrypointLocalProviderConfig runs entrypoint.sh up to config generation with
// opencode stubbed, and asserts a local provider maps to the bundled openai-compatible
// provider (Chat Completions), NOT the built-in openai provider (Responses API).
func TestEntrypointLocalProviderConfig(t *testing.T) {
	script, err := os.ReadFile("../../../deploy/sandbox/entrypoint.sh")
	if err != nil {
		t.Fatalf("read entrypoint: %v", err)
	}
	// Stub `opencode` to dump the resolved config+auth instead of running a review.
	harness := `opencode() { echo "CONFIG:"; cat "$OPENCODE_CONFIG"; echo "AUTH:"; cat "$XDG_DATA_HOME/opencode/auth.json"; exit 0; }
export -f opencode 2>/dev/null || true
`
	cmd := exec.Command("bash", "-c", harness+string(script))
	cmd.Env = append(os.Environ(),
		"LLM_PROVIDER=vllm", "LLM_MODEL=ornith-1.0-9b",
		"LLM_BASE_URL=http://192.168.2.241:1234/v1", "LLM_API_KEY=k",
		"MF_MARKER_NONCE=")
	out, _ := cmd.CombinedOutput()
	s := string(out)
	for _, want := range []string{
		`"npm": "@ai-sdk/openai-compatible"`,
		`"baseURL": "http://192.168.2.241:1234/v1"`,
		`"model": "local/ornith-1.0-9b"`,
		`"local":{"type":"api","key":"k"}`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("entrypoint config missing %q\n---\n%s", want, s)
		}
	}
	if strings.Contains(s, `"model": "openai/`) || strings.Contains(s, "/v1/responses") {
		t.Errorf("local provider must NOT use the built-in openai/Responses path\n%s", s)
	}
}
```

- [ ] **Step 2: Run it — fails** (`go test ./internal/agents/coding/ -run TestEntrypointLocalProviderConfig -v`): the current entrypoint `exit 2`s on `LLM_PROVIDER=vllm`.

- [ ] **Step 3: Edit `entrypoint.sh`.** Replace the provider `case` (lines ~49-52) and the config-generation block. New provider gate:

```sh
case "${LLM_PROVIDER:-}" in
  openrouter|anthropic|openai) LLM_LOCAL=0 ;;
  vllm|ollama)                 LLM_LOCAL=1 ;;
  *) echo "entrypoint: unsupported LLM_PROVIDER='${LLM_PROVIDER:-}'" >&2; exit 2 ;;
esac
```

Then branch the MODEL / auth.json / config. Keep the built-in path exactly as-is for `LLM_LOCAL=0`. For `LLM_LOCAL=1`:

```sh
if [ "$LLM_LOCAL" = 1 ]; then
  # Local OpenAI-compatible server (vLLM/Ollama/LM Studio). Use the bundled
  # @ai-sdk/openai-compatible provider (Chat Completions) — NOT the built-in openai
  # provider, which speaks the Responses API (/v1/responses) that local servers don't
  # serve. Verified: opencode loads this provider offline (no npm). LLM_BASE_URL is the
  # server's OpenAI base (e.g. http://host:1234/v1). Provider id "local" must match auth.json.
  MODEL="local/${LLM_MODEL}"
  mkdir -p "$XDG_DATA_HOME/opencode"
  printf '{"local":{"type":"api","key":"%s"}}\n' "$LLM_API_KEY" > "$XDG_DATA_HOME/opencode/auth.json"
  export OPENCODE_CONFIG=/tmp/opencode.json
  printf '%s\n' '{
  "$schema": "https://opencode.ai/config.json",
  "model": "'"${MODEL}"'",
  "provider": {
    "local": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "Local",
      "options": { "baseURL": "'"${LLM_BASE_URL}"'" },
      "models": { "'"${LLM_MODEL}"'": { "options": { "max_tokens": 8192 } } }
    }
  },
  "permission": {
    "read": "allow", "glob": "allow", "grep": "allow",
    "edit": "deny", "bash": "deny", "webfetch": "deny", "websearch": "deny",
    "task": "deny", "external_directory": "deny"
  }
}' > "$OPENCODE_CONFIG"
else
  # ... existing built-in-provider MODEL / auth.json / OPENCODE_CONFIG block unchanged ...
fi
```

`$LLM_BASE_URL` and `$LLM_MODEL` for local providers come from a validated connector/credential; `$LLM_API_KEY` is the (often placeholder) key. These are interpolated the same way the existing block interpolates `$LLM_PROVIDER`/`$LLM_MODEL`. Also update the header comment block (lines 10-15) to note `vllm|ollama` are now accepted and that `LLM_BASE_URL` is the provider base for local.

- [ ] **Step 4: Run it — passes** (`go test ./internal/agents/coding/ -run TestEntrypointLocalProviderConfig -v`). Requires `bash` (present on macOS/Linux CI).

- [ ] **Step 5: Commit**

```bash
git add deploy/sandbox/entrypoint.sh internal/agents/coding/entrypoint_config_test.go
git commit -m "feat(sandbox): entrypoint maps local providers to bundled openai-compatible (manyforge-9er)"
```

---

### Task 3: Egress gate applies to local, gated by AllowPrivateBaseURL

**Files:**
- Modify: `internal/agents/coding/fallbackchain.go` (`laneCredFor` + new helper)
- Modify: `internal/agents/coding/service.go:240`, `:431` (enqueue-time pre-checks)
- Test: `internal/agents/coding/fallbackchain_test.go` (add cases)

> **PLAN CORRECTION (do NOT reuse `localBaseURLBlocked`).** The original plan reused `localBaseURLBlocked`, but that helper is the *inverted* SSRF guard for the direct-POST path: it blocks EVERY public host and every non-`localhost` DNS name (it returns `true`/blocked for `api.anthropic.com`, `8.8.8.8`, etc.). Reusing it here would reject every cloud provider. Use a NEW narrow helper that classifies only IP-literal hosts and passes DNS/public hosts through. `localBaseURLBlocked` stays untouched by Task 3 and is deleted with the direct-POST path in Task 6.

**Interfaces:**
- Consumes: `AICredential.AllowPrivateBaseURL bool`, `AICredential.Host() string`, `s.EgressAllow.Allows`, and `netsafe.IsBlocked(ip net.IP, netsafe.Options{AllowLoopback, AllowPrivate}) bool` (metadata stays blocked even with `AllowPrivate`).
- Produces: new helper `privateBaseURLBlocked(host string, allowPrivate bool) bool` in `fallbackchain.go`; `laneCredFor` now validates ALL hosts against the allowlist, and rejects an IP-literal private/link-local/metadata host unless `AllowPrivateBaseURL`.

- [ ] **Step 1: Add the helper + its table test.** In `fallbackchain.go` add:

```go
// privateBaseURLBlocked reports whether a base-URL host must be refused for a sandbox
// lane given the credential's AllowPrivateBaseURL opt-in. Only IP-LITERAL hosts are
// classified: a DNS hostname or a public IP returns false (governed solely by the egress
// allowlist). A private/ULA IP is permitted only with the opt-in; loopback is always
// permitted; cloud-metadata/link-local stay blocked even with the opt-in. NOTE: this is
// deliberately NOT localBaseURLBlocked (the direct-POST SSRF guard, which blocks public/
// DNS hosts) — cloud providers reach opencode via the allowlist-gated egress proxy.
func privateBaseURLBlocked(host string, allowPrivate bool) bool {
	ip := net.ParseIP(host)
	if ip == nil {
		return false // DNS name / not an IP literal — egress allowlist governs it
	}
	return netsafe.IsBlocked(ip, netsafe.Options{AllowLoopback: true, AllowPrivate: allowPrivate})
}
```

Add a table test `TestPrivateBaseURLBlocked` (in `fallbackchain_test.go`) covering: `api.anthropic.com`→false (any opt-in); `8.8.8.8`→false; `192.168.2.241` opt-in=true→false, opt-in=false→true; `10.0.0.5` opt-in=false→true; `127.0.0.1`→false; `169.254.169.254` (metadata) opt-in=true→**true** (stays blocked).

- [ ] **Step 2: Write failing integration tests** in `fallbackchain_test.go`: a local (`vllm`) cred with a private host + `AllowPrivateBaseURL=true` AND the host added to the test's allowlist resolves OK; same with `AllowPrivateBaseURL=false` → error mentioning `allow_private_base_url`; a local host NOT in the allowlist → allowlist error; and confirm a cloud (`anthropic`, `api.anthropic.com`) cred with `AllowPrivateBaseURL=false` still resolves OK (regression guard — this is the case the old plan would have broken). Reuse the existing `FakeCredResolver`/allowlist scaffolding; note that `TestResolveLaneCred`'s `vllm` fixture allowlist must be extended to include its private host.

- [ ] **Step 3: Run — fails** (current code skips the check for local, so the `AllowPrivateBaseURL=false` case wrongly resolves).

- [ ] **Step 4: Rewrite the guard in `laneCredFor`** (replace the current `if !isLocalProvider(lc.Provider) && !s.EgressAllow.Allows(lc.Host())` block):

```go
	if !s.EgressAllow.Allows(lc.Host()) {
		return AICredential{}, fmt.Errorf("provider host %q not in sandbox egress allowlist", lc.Host())
	}
	// A private/RFC1918/ULA (or metadata/link-local) IP host is permitted only with the
	// credential's explicit AllowPrivateBaseURL opt-in; DNS + public hosts pass unchanged.
	if privateBaseURLBlocked(lc.Host(), lc.AllowPrivateBaseURL) {
		return AICredential{}, fmt.Errorf("host %q requires allow_private_base_url", lc.Host())
	}
```

- [ ] **Step 5: Update the two enqueue pre-checks** (`service.go:240`, `:431`) — replace `if !isLocalProvider(cred.Provider) && !s.EgressAllow.Allows(cred.Host())` with the same two-part guard (allowlist membership for all providers, then `privateBaseURLBlocked(cred.Host(), cred.AllowPrivateBaseURL)`). Keep the existing client-safe error message for the allowlist branch; add the `allow_private_base_url` branch.

- [ ] **Step 6: Run** `go test ./internal/agents/coding/ -run 'LaneCred|Egress|Enqueue|PrivateBaseURL|Trigger' -v`. Expected: PASS (incl. the cloud-provider regression case). Then `go build ./...` and `make lint` (0 issues).

- [ ] **Step 6: Commit**

```bash
git add internal/agents/coding/fallbackchain.go internal/agents/coding/service.go internal/agents/coding/fallbackchain_test.go
git commit -m "feat(review): egress gate applies to local providers, gated by allow_private_base_url (manyforge-9er)"
```

---

### Task 4: reviewLane routes local through the sandbox

**Files:**
- Modify: `internal/agents/coding/service.go:668-810` (`reviewLane`)
- Test: `internal/agents/coding/service_lane_test.go` (new; uses a fake `Sandbox` runner)

**Interfaces:**
- Consumes: `sandbox.SandboxSpec`, `sandboxEnv(cred) map[string]string`, `s.Sandbox.Run(ctx, spec)`.
- Produces: `runSandboxLane(dim Dimension, cred AICredential, laneOutDir string) laneResult` (extracted; also used by Task 5).

- [ ] **Step 1: Write failing test** — a review with a `vllm` dimension cred, driven through a fake `Sandbox` that records the spec. Assert the fake was called (i.e. local did NOT take a host-side path) and the spec has `Env["LLM_PROVIDER"]=="vllm"`, `EgressAllow==[]string{cred.Host()}`. Use the existing coding test scaffolding (`buildService`, a fake sandbox runner — see `sandbox` fakes in `service_multidim_integration_test.go` / `runner_test.go`).

- [ ] **Step 2: Run — fails**: today a `vllm` cred hits `localReview` (the `isLocalProvider` branch), so the fake sandbox is never called.

- [ ] **Step 3: Extract `runSandboxLane` and delete the `isLocalProvider` branch.** In `reviewLane`, remove lines 679-687 (the `if isLocalProvider(laneCred.Provider) { … localReview … }` block). Move the sandbox spec construction + `runLaneOnce` + retry loop + `laneResult` assembly (currently lines 689-810) into a helper:

```go
// runSandboxLane runs ONE dimension end-to-end in the opencode sandbox and returns its
// outcome. Local and cloud providers share this path (manyforge-9er): the entrypoint maps
// the provider onto opencode. Small local models keep the tighter diff budget via `maxTotal`.
runSandboxLane := func(dim Dimension, laneCred AICredential, laneOutDir string) laneResult {
	scoped := filterFilesByScope(files, dim.ScopeGlobs)
	lanePayload, _, _, _ := assembleDiffPayload(scoped, maxTotal)
	inputs := map[string][]byte{"review_instructions.txt": []byte(dim.Prompt)}
	if len(scoped) > 0 {
		inputs["review_files.txt"] = []byte(strings.Join(changedFilePaths(commentableMap(scoped)), "\n"))
	}
	if lanePayload != "" {
		inputs["review_diff.txt"] = []byte(lanePayload)
	}
	_ = s.auditStep(ctx, principalID, businessID, crID, "agent.coding.opencode.invoked",
		map[string]any{"image": s.Image, "head_sha": pr.HeadSHA, "model": laneCred.Model, "provider": laneCred.Provider, "dimension": dim.Key},
		nil, ptr("executed"))
	spec := sandbox.SandboxSpec{
		Image: s.Image, ReadOnlyDir: checkout, OutputDir: laneOutDir,
		Cmd: opencodeCmd(laneCred.Model), Env: sandboxEnv(laneCred),
		EgressAllow: []string{laneCred.Host()}, Timeout: s.timeout(),
		StreamStderr: &progressStreamWriter{prog: prog, dim: dim.Key}, Inputs: inputs,
		CloneURL: conn.CloneURL(), CloneAuthHeader: authHeader, CloneSHA: pr.HeadSHA,
		CloneAllowPrivate: rc.AllowPrivateBaseURL,
	}
	// ... existing runLaneOnce + retry loop + usage/cost + laneResult assembly (unchanged) ...
	return lr
}

reviewLane := func(dim Dimension, laneOutDir string) laneResult {
	return runSandboxLane(dim, laneCreds[dim.Key], laneOutDir)
}
```

Keep the `laneCred := laneCreds[dim.Key]` usage inside `runSandboxLane` via the passed `laneCred` param. The `maxTotal` (tighter for local, set at 610-612) is captured by closure — no change.

- [ ] **Step 4: Run** the new test + `go build ./...`. Expected: PASS; the fake sandbox is invoked for the local cred. `localReview` is now unreferenced by `reviewLane` (still compiles — deleted in Task 6).

- [ ] **Step 5: Commit**

```bash
git add internal/agents/coding/service.go internal/agents/coding/service_lane_test.go
git commit -m "feat(review): route local providers through the opencode sandbox (manyforge-9er)"
```

---

### Task 5: Runtime fallback to cloud on local lane failure

**Files:**
- Modify: `internal/agents/coding/service.go` (`reviewLane`)
- Test: `internal/agents/coding/service_lane_test.go`

**Interfaces:**
- Consumes: `runSandboxLane` (Task 4), `s.laneCredFor(ctx, principalID, businessID, cred, provider, model)`.

- [ ] **Step 1: Write failing test** — a `vllm` dimension whose sandbox run FAILS (fake runner returns an error for the vllm host) but has `FallbackProvider="openrouter"`, `FallbackModel="deepseek/..."`; the fake runner SUCCEEDS for the openrouter host. Assert the returned `laneResult` is the successful cloud one (`Provider=="openrouter"`, `Err==nil`).

- [ ] **Step 2: Run — fails**: today `reviewLane` returns the failed local result with no fallback.

- [ ] **Step 3: Add the fallback in `reviewLane`:**

```go
reviewLane := func(dim Dimension, laneOutDir string) laneResult {
	chosen := laneCreds[dim.Key]
	lr := runSandboxLane(dim, chosen, laneOutDir)
	// Runtime fallback (manyforge-9er): if the chosen lane failed and it was NOT already the
	// configured fallback, re-run once on the dimension's cloud fallback (provider, model).
	if lr.Err != nil && dim.FallbackProvider != "" && !strings.EqualFold(chosen.Provider, dim.FallbackProvider) {
		if fb, ferr := s.laneCredFor(ctx, principalID, businessID, cred, dim.FallbackProvider, dim.FallbackModel); ferr == nil {
			slog.Default().InfoContext(ctx, "code review lane: falling back to cloud after local failure",
				"dimension", dim.Key, "from", chosen.Provider, "to", fb.Provider)
			if fbResult := runSandboxLane(dim, fb, laneOutDir); fbResult.Err == nil {
				return fbResult
			}
		}
	}
	return lr
}
```

- [ ] **Step 4: Run** the test + `go build ./...`. Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/agents/coding/service.go internal/agents/coding/service_lane_test.go
git commit -m "feat(review): runtime fallback to cloud when a local lane fails (manyforge-9er)"
```

---

### Task 6: Remove the direct-POST path (split localreview.go)

**Files:**
- Rename: `internal/agents/coding/localreview.go` → `internal/agents/coding/reviewpayload.go`
- Rename: `internal/agents/coding/localreview_test.go` → `internal/agents/coding/reviewpayload_test.go`
- Modify: `internal/agents/coding/service.go` (remove `localClient` if now unused)

**Interfaces:**
- KEEP (used by the sandbox/cloud path): `reviewInstructions`, `reviewSchemaLine`, `isLocalProvider`, `reviewMaxFileBytes`, `reviewMaxTotalBytes`, `localProviderMaxTotalBytes`, `codeExt`, `isNonReviewableDoc`, `assembleDiffPayload`, `commentableMap`. (`privateBaseURLBlocked` lives in `fallbackchain.go` from Task 3 — not in this file, so the rename doesn't touch it.)
- DELETE (direct-POST only): `localReview`, `streamLocalReview`, `completionOrChunks`, `streamPreview`, `reviewResponseFormat`, `localBaseURLBlocked` (its only caller was `localReview`; Task 3 uses `privateBaseURLBlocked` instead), and constants `localReviewNumCtx`, `localReviewMaxTokens`, `localReviewMaxPlainAttempts`. Removing `localBaseURLBlocked` breaks its pins **MF007-PIN-14** and **MF008-PIN-2** — update/remove those in `internal/security_regression/` in this same commit (the direct-POST SSRF guard they pin no longer exists; the sandbox path's egress is pinned separately).

- [ ] **Step 1: Confirm the delete set is truly unreferenced.** Run: `grep -rn 'localReview\|streamLocalReview\|reviewResponseFormat\|completionOrChunks\|streamPreview\|localBaseURLBlocked\|localReviewNumCtx\|localReviewMaxTokens\|localReviewMaxPlainAttempts\|localClient' internal/agents/coding/ | grep -v _test`. After Tasks 4-5 the only hits should be their own definitions in `localreview.go` (+ possibly `localClient` in service.go, and `localBaseURLBlocked` pins in `internal/security_regression/`). If `s.localClient()` has no remaining non-test caller, delete it and its field.

- [ ] **Step 2: `git mv` and delete.**

```bash
git mv internal/agents/coding/localreview.go internal/agents/coding/reviewpayload.go
git mv internal/agents/coding/localreview_test.go internal/agents/coding/reviewpayload_test.go
```

Then delete the functions/constants in the DELETE set from `reviewpayload.go` (including `localBaseURLBlocked` + its `TestLocalBaseURLBlocked` table test), and in `reviewpayload_test.go` delete every test that calls a deleted function (the `localReview(...)`-based tests + `TestLocalBaseURLBlocked`). Keep tests for `assembleDiffPayload`, `isNonReviewableDoc`, `commentableMap`, `isLocalProvider`. Update the file header comment to describe it as the shared review-payload/prompt/budget helpers.

- [ ] **Step 3: Build + vet** — `go build ./... && go vet ./internal/agents/coding/` and `go test ./internal/security_regression/` (the MF007-PIN-14 / MF008-PIN-2 updates must pass). Expected: 0 errors (no unused symbols; no dangling references).

- [ ] **Step 4: Run the package tests** — `go test ./internal/agents/coding/`. Expected: PASS (retained payload/guard tests still pass).

- [ ] **Step 5: Commit**

```bash
git add -A internal/agents/coding/
git commit -m "refactor(review): delete direct-POST local path; keep shared payload helpers (manyforge-9er)"
```

---

### Task 7: Config + deploy documentation

**Files:**
- Modify: `charts/manyforge/values.yaml` (comment on `sandbox.egressAllow`)
- Modify: `HANDOFF.md` (deploy note)

**Interfaces:** none (docs/config only).

- [ ] **Step 1: Document `sandbox.egressAllow`.** Above the `egressAllow:` line in `values.yaml`, add a comment: local providers require their `host:port` (e.g. `192.168.2.241:1234`) added here so the egress proxy CONNECT/forward-allowlist permits it; the app reads the same value as `MANYFORGE_SANDBOX_EGRESS_ALLOW`; private hosts are additionally gated by the credential's `allow_private_base_url`. Do NOT change the default value (the specific LAN IP lives only in the deployment's Flux HelmRelease values, out of this repo).

- [ ] **Step 2: Add a deploy note** to `HANDOFF.md`: after deploy, set the hub HelmRelease `sandbox.egressAllow` to include the LM Studio host, and verify a sandbox namespace pod can route to the LAN (`kubectl -n manyforge-sandbox run … curl http://192.168.2.241:1234/v1/models` via a throwaway pod, or the first real local review).

- [ ] **Step 3: Commit**

```bash
git add charts/manyforge/values.yaml HANDOFF.md
git commit -m "docs(deploy): egressAllow includes local host for local providers (manyforge-9er)"
```

---

### Task 8: Security regression pins + full gate

**Files:**
- Create: `internal/security_regression/manyforge_9er_local_opencode_test.go`
- (No `make sec-test` change beyond adding the file — it globs the package.)

**Interfaces:** source-level pins (grep the tree) so a future refactor that reintroduces the direct-POST path or the wrong opencode provider fails CI loudly.

- [ ] **Step 1: Write the pins.**

```go
package security_regression

import (
	"os"
	"strings"
	"testing"
)

// manyforge-9er: local providers must go through the opencode sandbox, using the
// bundled openai-compatible provider — never the direct-POST path or the Responses API.
func TestLocalProvidersUseSandboxOpenAICompatible(t *testing.T) {
	entry, err := os.ReadFile("../../deploy/sandbox/entrypoint.sh")
	if err != nil {
		t.Fatal(err)
	}
	e := string(entry)
	if !strings.Contains(e, "@ai-sdk/openai-compatible") {
		t.Error("entrypoint must map local providers to @ai-sdk/openai-compatible")
	}
	if !strings.Contains(e, "vllm|ollama") {
		t.Error("entrypoint must accept vllm|ollama")
	}

	svc, err := os.ReadFile("../agents/coding/service.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(svc), "localReview(") {
		t.Error("service.go must not call localReview — the direct-POST path was removed (manyforge-9er)")
	}
}
```

- [ ] **Step 2: Run** `go test ./internal/security_regression/ -run TestLocalProvidersUseSandboxOpenAICompatible -v`. Expected: PASS.

- [ ] **Step 3: Full gate.**

```bash
make test
go test -tags integration -p 1 ./internal/agents/coding/
go test -tags contract ./cmd/...      # expect NO drift (no endpoint change)
make lint
cd web && npx ng build   # unaffected, but confirm nothing broke
```

Expected: all green. (contract has no new routes; frontend untouched.)

- [ ] **Step 4: Commit + update bd.**

```bash
git add internal/security_regression/manyforge_9er_local_opencode_test.go
git commit -m "test(review): source-pin local-through-opencode + no direct-POST (manyforge-9er)"
bd update manyforge-9er --notes "Implemented per docs/superpowers/plans/2026-07-06-opencode-local-providers.md"
```

---

## Verification / Rollout
After merge, the deploy is atomic across the app image, the `egress-proxy` image (plain-HTTP forwarding), and the chart. On the hub HelmRelease, add the LM Studio `host:port` to `sandbox.egressAllow`. Verify: trigger a local-provider review → a Job spawns in `manyforge-sandbox` (unlike today's host-side path), reaches the endpoint through the proxy, and a forced local failure falls back to the configured cloud model. Use the retry button (manyforge-7a9) to force a re-run on an unchanged head.

## Test Plan Summary
- Unit: proxy plain-HTTP forward/403/CONNECT (T1); entrypoint local-config generation (T2); `laneCredFor` local egress gating (T3); `reviewLane` builds a SandboxSpec for local (T4); runtime cloud fallback (T5).
- Refactor safety: build+vet+package tests after the split (T6).
- Regression: source-pins (T8).
- Gates: `make test`, integration, contract (no drift), `make lint`, `ng build` (T8).
