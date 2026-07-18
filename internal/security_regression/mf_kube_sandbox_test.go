// No build tag: this source-level guard runs in both `make test` and
// `make sec-test`, with no infrastructure. It pins KubeRunner's pod hardening,
// credential isolation, and anti-forgery properties in place so a refactor
// that silently drops one fails CI even if a behavioral test is also weakened
// or removed (CLAUDE.md: source-level pins for security).

package security_regression

import (
	"strings"
	"testing"
)

// TestKubeSandboxPodHardening pins the batchv1.Job/corev1.PodSpec hardening
// KubeRunner.buildJob emits for every sandbox review run — the Talos deploy
// target's primary containment layer, since it has no Docker daemon for
// DockerRunner's --cap-drop ALL / --read-only flags to apply to.
func TestKubeSandboxPodHardening(t *testing.T) {
	runner := mustRead(t, "../agents/coding/sandbox/kube/runner.go")

	if !strings.Contains(runner, "RunAsNonRoot: boolPtr(true)") {
		t.Error("MF-KUBE-SANDBOX-1: pod SecurityContext must set RunAsNonRoot true — pin broken, update this pin in the same change if the refactor is intentional")
	}
	if !strings.Contains(runner, "RunAsUser:    int64Ptr(65532)") {
		t.Error("MF-KUBE-SANDBOX-2: pod SecurityContext must pin RunAsUser to the non-root uid 65532 — pin broken, update this pin in the same change if the refactor is intentional")
	}
	if !strings.Contains(runner, "FSGroup:      int64Ptr(65532)") {
		t.Error("MF-KUBE-SANDBOX-3: pod SecurityContext must pin FSGroup to 65532 — pin broken, update this pin in the same change if the refactor is intentional")
	}
	if !strings.Contains(runner, "SeccompProfileTypeRuntimeDefault") {
		t.Error("MF-KUBE-SANDBOX-4: pod SecurityContext must set the RuntimeDefault seccomp profile — pin broken, update this pin in the same change if the refactor is intentional")
	}
	if !strings.Contains(runner, "ReadOnlyRootFilesystem:   boolPtr(true)") {
		t.Error("MF-KUBE-SANDBOX-5: container SecurityContext must set ReadOnlyRootFilesystem true — pin broken, update this pin in the same change if the refactor is intentional")
	}
	if !strings.Contains(runner, "AllowPrivilegeEscalation: boolPtr(false)") {
		t.Error("MF-KUBE-SANDBOX-6: container SecurityContext must set AllowPrivilegeEscalation false — pin broken, update this pin in the same change if the refactor is intentional")
	}
	if !strings.Contains(runner, `Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}`) {
		t.Error("MF-KUBE-SANDBOX-7: container SecurityContext must drop ALL Linux capabilities — pin broken, update this pin in the same change if the refactor is intentional")
	}
	if !strings.Contains(runner, "AutomountServiceAccountToken: boolPtr(false)") {
		t.Error("MF-KUBE-SANDBOX-8: pod spec must not automount the ServiceAccount token into the review/clone containers — pin broken, update this pin in the same change if the refactor is intentional")
	}
	if !strings.Contains(runner, "ActiveDeadlineSeconds:   &activeDeadline") {
		t.Error("MF-KUBE-SANDBOX-9: Job spec must bound total run time via ActiveDeadlineSeconds — pin broken, update this pin in the same change if the refactor is intentional")
	}
	if !strings.Contains(runner, "TTLSecondsAfterFinished: &ttl") {
		t.Error("MF-KUBE-SANDBOX-10: Job spec must set TTLSecondsAfterFinished so a finished run's objects don't linger indefinitely — pin broken, update this pin in the same change if the refactor is intentional")
	}
}

// TestKubeSandboxCredentialIsolation pins the property that the git clone
// credential and the LLM_* credentials only ever reach the pod via
// secretKeyRef against the per-run Secret — never as a literal env Value —
// and that the Secret is owned by the Job so Kubernetes GC reaps it even if
// this process crashes mid-run (a GitHub PAT / LLM API key must not be
// orphaned in the cluster permanently).
func TestKubeSandboxCredentialIsolation(t *testing.T) {
	runner := mustRead(t, "../agents/coding/sandbox/kube/runner.go")

	if !strings.Contains(runner, "SecretKeyRef: &corev1.SecretKeySelector{") {
		t.Error("MF-KUBE-SANDBOX-11: clone/LLM credential env vars must be wired via SecretKeyRef, not a literal Value — pin broken, update this pin in the same change if the refactor is intentional")
	}
	if !strings.Contains(runner, "OwnerReferences: owner") {
		t.Error("MF-KUBE-SANDBOX-12: the per-run Secret (and ConfigMap) must carry an OwnerReference tying its lifetime to the Job — pin broken, update this pin in the same change if the refactor is intentional")
	}
}

// TestKubeSandboxLogParseErrorPropagation pins the d2bf8a2 fix: a genuinely
// truncated or base64-garbled marker block from the pod's authoritative log
// re-read must fail Run() — a pod simply not existing (errPodNotFound, the
// unit-test/fake-clientset case) is the ONLY error class allowed to fall back
// to the best-effort follow-stream result. A refactor that widens that
// fallback to swallow every rereadPodLogs error would silently resurrect the
// truncated-usage.json-becomes-a-fake-zero-cost-result bug.
func TestKubeSandboxLogParseErrorPropagation(t *testing.T) {
	runner := mustRead(t, "../agents/coding/sandbox/kube/runner.go")

	if !strings.Contains(runner, `var errPodNotFound = errors.New("kube: run pod not found")`) {
		t.Error("MF-KUBE-SANDBOX-13: errPodNotFound sentinel must exist to distinguish a missing pod from a genuine parse error — pin broken, update this pin in the same change if the refactor is intentional")
	}
	if !strings.Contains(runner, `fmt.Errorf("kube: pod log parse: %w", followErr)`) {
		t.Error("MF-KUBE-SANDBOX-14: a genuine follow-stream parse error (when no authoritative re-read is available) must be propagated, not swallowed — pin broken, update this pin in the same change if the refactor is intentional")
	}
	if !strings.Contains(runner, `fmt.Errorf("kube: pod log parse: %w", rerr)`) {
		t.Error("MF-KUBE-SANDBOX-15: a genuine authoritative re-read parse error must be propagated (after merging whatever DID decode), not swallowed — pin broken, update this pin in the same change if the refactor is intentional")
	}
}

// TestKubeSandboxMarkerEmissionGatedOnNonce pins the anti-forgery guard: the
// entrypoint only emits the MF-REVIEW-/MF-USAGE- marker blocks (which
// KubeRunner scopes its parse to via the per-run nonce, defeating a
// prompt-injected PR that tries to print a static/guessed marker) when
// MF_MARKER_NONCE is actually set — so the DockerRunner path (which never
// sets it) is unaffected.
func TestKubeSandboxMarkerEmissionGatedOnNonce(t *testing.T) {
	entry := mustRead(t, "../../deploy/sandbox/entrypoint.sh")

	if !strings.Contains(entry, `if [ -n "${MF_MARKER_NONCE:-}" ]; then`) {
		t.Error("MF-KUBE-SANDBOX-16: marker emission must be gated on MF_MARKER_NONCE being set — pin broken, update this pin in the same change if the refactor is intentional")
	}
	if !strings.Contains(entry, `printf '===MF-REVIEW-%s-BEGIN===\n' "$MF_MARKER_NONCE"`) {
		t.Error("MF-KUBE-SANDBOX-17: the review marker block must interpolate the run's nonce — pin broken, update this pin in the same change if the refactor is intentional")
	}
	if !strings.Contains(entry, `printf '===MF-USAGE-%s-BEGIN===\n' "$MF_MARKER_NONCE"`) {
		t.Error("MF-KUBE-SANDBOX-18: the usage marker block must interpolate the run's nonce — pin broken, update this pin in the same change if the refactor is intentional")
	}
}

// TestKubeSandboxOpencodePermissionProfile pins the entrypoint's opencode
// permission profile, the sandbox's primary app-level containment under
// Flannel (no egress-proxy-independent guard otherwise stops opencode from
// running shell commands or reaching arbitrary hosts). Duplicating (part of)
// MF007-PIN-11's provider-allowlist assertion here is intentional: this test
// is scoped to the kube-sandbox finding, so it must independently catch a
// regression even if MF007's test is ever removed.
func TestKubeSandboxOpencodePermissionProfile(t *testing.T) {
	entry := mustRead(t, "../../deploy/sandbox/entrypoint.sh")

	if !strings.Contains(entry, `"bash": "deny",`) {
		t.Error("MF-KUBE-SANDBOX-19: opencode config must deny the bash tool — pin broken, update this pin in the same change if the refactor is intentional")
	}
	if !strings.Contains(entry, `"webfetch": "deny",`) {
		t.Error("MF-KUBE-SANDBOX-20: opencode config must deny the webfetch tool — pin broken, update this pin in the same change if the refactor is intentional")
	}
	if !strings.Contains(entry, `"websearch": "deny",`) {
		t.Error("MF-KUBE-SANDBOX-21: opencode config must deny the websearch tool — pin broken, update this pin in the same change if the refactor is intentional")
	}
	// MF-KUBE-SANDBOX-22: LLM_PROVIDER must be validated against a CLOSED allowlist, with every
	// other value rejected before it can reach the config/auth.json interpolation below.
	// Whitespace is collapsed first so column alignment in the case arms isn't load-bearing.
	entryNorm := collapseSpaces(entry)
	for _, arm := range []string{
		"openrouter|anthropic|openai) LLM_OPENCODE_MODE=builtin ;;",
		"vllm|ollama|huggingface) LLM_OPENCODE_MODE=compat ;;",
		"openai_codex) LLM_OPENCODE_MODE=codex ;;",
	} {
		if !strings.Contains(entryNorm, arm) {
			t.Errorf("MF-KUBE-SANDBOX-22: entrypoint must validate LLM_PROVIDER against the closed allowlist arm %q before use — pin broken, update this pin in the same change if the refactor is intentional", arm)
		}
	}
	if !strings.Contains(entryNorm, `*) echo "entrypoint: unsupported LLM_PROVIDER=`) || !strings.Contains(entryNorm, "exit 2 ;;") {
		t.Error("MF-KUBE-SANDBOX-22: entrypoint must reject any LLM_PROVIDER outside the allowlist with exit 2 — an open default would let an unvalidated provider name reach auth.json")
	}
	// manyforge-9er: entrypoint.sh interpolates LLM_BASE_URL/LLM_MODEL/LLM_API_KEY
	// into JSON string literals (and, for LLM_MODEL, a JSON object key) with no
	// escaping. A value containing a JSON metacharacter (" or \) can break out of
	// its string and inject config keys — including overriding the read-only
	// "permission" block pinned by MF-KUBE-SANDBOX-19/20/21 above. This guard must
	// run before either branch (built-in or local provider) writes config/auth.json.
	if !strings.Contains(entry, `*'"'*|*'\'*) echo "entrypoint: LLM_* value contains a JSON metacharacter" >&2; exit 2 ;;`) {
		t.Error("MF-KUBE-SANDBOX-24: entrypoint must reject any LLM_BASE_URL/LLM_MODEL/LLM_API_KEY containing a JSON metacharacter before interpolating it into config/auth.json — pin broken, update this pin in the same change if the refactor is intentional")
	}
}

// TestKubeSandboxHostSideSSRFGuardRetained pins M1 (revised for the kube-mode
// host-clone fix): runJob must ALWAYS run the SSRF guard checkCloneURL before
// the sandbox runs — regardless of which sandbox.SandboxRunner (DockerRunner
// or KubeRunner) executes the review, and regardless of whether runJob itself
// performs the host-side git clone.
//
// The host-side git clone (s.cloneFn()/CloneAtSHA) is NO LONGER entrenched for
// every runner: kube mode's app pod is gcr.io/distroless/static:nonroot (no
// git, no shell, read-only root FS), so CodeReviewService.ClonesInSandbox lets
// runJob skip the host clone entirely and rely on the KubeRunner's own
// in-cluster init-container clone (cloneScript) instead — that init container
// is a second, defense-in-depth layer, not a substitute for this check. What
// must never regress is the SSRF validation itself: checkCloneURL is pure
// (url.Parse + net.LookupIP + netsafe.IsBlocked, no git/exec) and therefore
// runs safely in EVERY mode, so it is the property this pin now enforces.
func TestKubeSandboxHostSideSSRFGuardRetained(t *testing.T) {
	svc := mustRead(t, "../agents/coding/service.go")

	if !strings.Contains(svc, "checkCloneURL(conn.CloneURL(), rc.AllowPrivateBaseURL)") {
		t.Error("MF-KUBE-SANDBOX-23: runJob must always validate the clone URL via checkCloneURL (the SSRF guard) before any runner (docker or kube) executes the review — pin broken, update this pin in the same change if the refactor is intentional")
	}
}

// TestSandboxOpenAICodexOAuthArm pins the openai_codex entrypoint arm (manyforge-6fx): opencode's
// NATIVE codex path requires a type:"oauth" auth.json (NOT api-key) so opencode sends store:false +
// the codex headers itself; the account id is metacharacter-guarded; the refresh token stays a
// host-side dummy so it never enters the sandbox.
func TestSandboxOpenAICodexOAuthArm(t *testing.T) {
	entry := mustRead(t, "../../deploy/sandbox/entrypoint.sh")
	for _, lit := range []string{
		`"type":"oauth"`,
		`"accountId":"`,
		`"refresh":"unused-host-side-only"`,
		`"${LLM_CHATGPT_ACCOUNT_ID:-}"`, // guarded alongside the other injected values
	} {
		if !strings.Contains(entry, lit) {
			t.Errorf("openai_codex codex branch must contain %q (oauth auth.json / metachar guard) — pin broken, update in the same change if intentional", lit)
		}
	}

	// MF-KUBE-SANDBOX-25 (negative pin): the codex branch must NOT set an OpenAI-compatible
	// "baseURL" — opencode's built-in oauth-driven codex path targets the ChatGPT backend
	// itself and must never be redirected via a custom base URL. The compat branch (vLLM/
	// Ollama/HuggingFace) legitimately sets "baseURL", so this assertion is scoped to ONLY the
	// codex branch's text via the if/elif markers, not the whole file.
	codexStart := strings.Index(entry, `if [ "$LLM_OPENCODE_MODE" = codex ]`)
	compatStart := strings.Index(entry, `elif [ "$LLM_OPENCODE_MODE" = compat ]`)
	if codexStart == -1 || compatStart == -1 || compatStart <= codexStart {
		t.Fatal("MF-KUBE-SANDBOX-25: could not locate the codex/compat branch markers in entrypoint.sh — pin broken, update in the same change if intentional")
	}
	codexBranch := entry[codexStart:compatStart]
	if strings.Contains(codexBranch, `"baseURL"`) {
		t.Error("MF-KUBE-SANDBOX-25: the codex branch must not set a \"baseURL\" — opencode's built-in oauth path targets the ChatGPT backend itself; a baseURL here would silently redirect codex traffic to an attacker-controlled or wrong endpoint — pin broken, update in the same change if intentional")
	}
}
