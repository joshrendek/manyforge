package security_regression

import (
	"reflect"
	"strings"
	"testing"

	"github.com/manyforge/manyforge/internal/connectors"
)

// MF007-PIN-1: the slice-1 repo connector must expose no code-write capability.
func TestRepoConnectorHasNoWriteCapability(t *testing.T) {
	typ := reflect.TypeOf((*connectors.RepoConnector)(nil)).Elem()
	banned := []string{"Push", "Commit", "CreatePR", "CreatePullRequest", "OpenPR", "Merge", "Write"}
	for i := 0; i < typ.NumMethod(); i++ {
		name := typ.Method(i).Name
		for _, b := range banned {
			if strings.Contains(name, b) {
				t.Fatalf("RepoConnector exposes write-capable method %q (banned substring %q) — slice 1 is read-only", name, b)
			}
		}
	}
}

// MF007-PIN-2: the sandbox must be read-only + drop caps + force the egress proxy.
// If any of these fragments disappear from docker.go the sandbox isolation guarantee
// is silently broken — this test makes that a CI failure.
func TestSandboxRunArgsPinned(t *testing.T) {
	src := mustRead(t, "../agents/coding/sandbox/docker.go")
	for _, frag := range []string{
		`"--read-only"`,
		`"--cap-drop", "ALL"`,
		`:/work:ro`,
		`HTTPS_PROXY=`,
		`"--network"`,
	} {
		if !strings.Contains(src, frag) {
			t.Fatalf("sandbox hardening fragment %q missing from docker.go — was isolation weakened?", frag)
		}
	}
}

// MF007-PIN-3: review posting is intentionally ungated — the service must NOT route
// the post through the approval queue (no CreatePending / approval in service.go).
func TestReviewPostingIsUngated(t *testing.T) {
	src := mustRead(t, "../agents/coding/service.go")
	for _, banned := range []string{"CreatePending", "ApprovalPending", "awaiting_approval"} {
		if strings.Contains(src, banned) {
			t.Fatalf("service.go references %q — review posting must stay ungated/advisory", banned)
		}
	}
	if !strings.Contains(src, "PostReview") {
		t.Fatal("service.go must post the review directly")
	}
}

// MF007-PIN-4: only allowlisted run-scoped secrets enter the sandbox Env — the service
// must build Env from the resolved LLM credential, never from os.Environ()/host.
func TestSandboxEnvNoHostInheritance(t *testing.T) {
	src := mustRead(t, "../agents/coding/service.go")
	if strings.Contains(src, "os.Environ()") {
		t.Fatal("service.go must not pass host environment into the sandbox spec")
	}
}

// MF007-PIN-5: clone hardening — token scope, SSRF guard, minimal git env.
// These fragments in clone.go are load-bearing security controls:
//   - http.followRedirects=false: prevents token leakage via redirect to attacker host
//   - GIT_TERMINAL_PROMPT=0: prevents git from prompting for credentials
//   - GIT_CONFIG_GLOBAL: overrides any host-level git config that could alter behavior
//   - netsafe.: confirms the SSRF pre-check against the clone URL is present
//
// Removing any of these fails CI loudly (MF007-C1, MF007-I1, MF007-I2).
func TestCloneHardeningPinned(t *testing.T) {
	src := mustRead(t, "../agents/coding/clone.go")
	for _, frag := range []string{
		`http.followRedirects=false`,
		`GIT_TERMINAL_PROMPT=0`,
		`GIT_CONFIG_GLOBAL`,
		`netsafe.`,
	} {
		if !strings.Contains(src, frag) {
			t.Fatalf("clone hardening fragment %q missing from clone.go — was the security control removed?", frag)
		}
	}
}

// MF007-PIN-6 (manyforge-0qj): the sandbox egress proxy is shared and boot-static,
// so the service must validate a run's provider host against the SAME allowlist the
// proxy enforces, up front, returning ErrValidation. Two halves are pinned:
//  1. service.go performs the pre-flight check (EgressAllow.Allows + ErrValidation);
//  2. the proxy (enforcer) and the service (validator) share netsafe's matcher and
//     neither carries a private copy — a divergent copy is exactly how the validator
//     and enforcer would drift back into silent egress blocking.
func TestEgressAllowlistValidationPinned(t *testing.T) {
	svc := mustRead(t, "../agents/coding/service.go")
	for _, frag := range []string{
		`EgressAllow`,
		`.Allows(cred.Host())`,
		`errs.ErrValidation`,
	} {
		if !strings.Contains(svc, frag) {
			t.Fatalf("service.go missing egress pre-flight fragment %q — was the manyforge-0qj guard removed?", frag)
		}
	}

	proxy := mustRead(t, "../../cmd/mf-egress-proxy/main.go")
	if !strings.Contains(proxy, "netsafe.ParseHostAllowlist") || !strings.Contains(proxy, ".Allows(") {
		t.Fatal("cmd/mf-egress-proxy/main.go must enforce egress via netsafe's shared matcher (netsafe.ParseHostAllowlist + .Allows)")
	}
	if strings.Contains(proxy, "func allowed(") {
		t.Fatal("cmd/mf-egress-proxy/main.go defines a private allow-matcher — it must share netsafe's so the validator/enforcer can't drift (manyforge-0qj)")
	}
}
