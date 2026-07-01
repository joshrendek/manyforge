package security_regression

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/manyforge/manyforge/internal/connectors"
)

// MF007-PIN-8: ListRepoConnectors SQL must not SELECT secret_ref (credential handle
// must not reach the list response). The public summary type (RepoConnectorSummary)
// must contain no field whose name contains "Token", "Secret", or "APIKey".
// coding.CodeReview is verified via source-level check to avoid import cycles.
func TestNoSecretProjection(t *testing.T) {
	// Source-level: ListRepoConnectors SQL must not SELECT secret_ref.
	sql := mustRead(t, "../../db/query/repo_connector.sql")
	if idx := strings.Index(sql, "ListRepoConnectors"); idx >= 0 {
		block := sql[idx:]
		if selectIdx := strings.Index(block, "SELECT"); selectIdx >= 0 {
			fromBlock := block[selectIdx:]
			if endIdx := strings.Index(fromBlock, ";\n"); endIdx >= 0 {
				fromBlock = fromBlock[:endIdx]
			}
			if strings.Contains(fromBlock, "secret_ref") {
				t.Fatal("ListRepoConnectors SELECT includes secret_ref — credential handle must not reach the list response (MF007-PIN-8)")
			}
		}
	}

	// Reflection: RepoConnectorSummary must have no Token/Secret/APIKey field.
	banned := []string{"Token", "Secret", "APIKey"}
	checkNoSecretFields := func(name string, typ reflect.Type) {
		for i := 0; i < typ.NumField(); i++ {
			fname := typ.Field(i).Name
			for _, b := range banned {
				if strings.Contains(fname, b) {
					t.Fatalf("%s has field %q containing banned substring %q — credential must not leak through list/summary types (MF007-PIN-8)", name, fname, b)
				}
			}
		}
	}
	checkNoSecretFields("RepoConnectorSummary", reflect.TypeOf(connectors.RepoConnectorSummary{}))

	// Source-level: coding.CodeReview struct must have no Token/Secret/APIKey field.
	svcSrc := mustRead(t, "../agents/coding/service.go")
	if crIdx := strings.Index(svcSrc, "type CodeReview struct {"); crIdx >= 0 {
		block := svcSrc[crIdx:]
		if endIdx := strings.Index(block, "\n}"); endIdx >= 0 {
			block = block[:endIdx]
		}
		for _, b := range banned {
			if strings.Contains(block, b) {
				t.Fatalf("coding.CodeReview struct contains field with banned substring %q — credential must not leak (MF007-PIN-8)", b)
			}
		}
	}
}

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

// MF007-PIN-10 (manyforge-ht8): the sst/opencode entrypoint must run the review
// agent READ-ONLY — its opencode permission profile denies edit/bash/webfetch so a
// prompt-injected review can neither mutate the checkout nor exfiltrate via a fetch
// tool. And the provider API key is written to opencode's auth.json under the
// tmpfs data dir (XDG_DATA_HOME), OUTSIDE the reviewed cwd /tmp/src — never into the
// checkout opencode reads. If these disappear from entrypoint.sh the agent silently
// gains write/exec/network capability or leaks the key into reviewable files.
func TestSandboxEntrypointReadOnlyPinned(t *testing.T) {
	src := mustRead(t, "../../deploy/sandbox/entrypoint.sh")
	for _, frag := range []string{
		`"edit": "deny"`,
		`"bash": "deny"`,
		`"webfetch": "deny"`,
	} {
		if !strings.Contains(src, frag) {
			t.Fatalf("entrypoint.sh missing read-only permission %q — was the review agent allowed to mutate/exfiltrate? (MF007-PIN-10)", frag)
		}
	}
	if !strings.Contains(src, `"$XDG_DATA_HOME/opencode/auth.json"`) {
		t.Fatal("entrypoint.sh must write the API key to auth.json under XDG_DATA_HOME (tmpfs, outside the reviewed cwd /tmp/src) (MF007-PIN-10)")
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

// MF007-PIN-9 (manyforge-elo): the CodeReviewWorker claims principal-less (no
// manyforge.principal_id), but code_review has RLS ENABLEd (0071) and the app role
// manyforge_app is NOBYPASSRLS, so a raw claim is RLS-blocked (authorized_businesses(NULL)
// = EMPTY). The claim/requeue/fail therefore MUST go through SECURITY DEFINER
// functions with a pinned search_path (migrations/0073), mirroring the outbox drain.
// If the migration loses SECURITY DEFINER or the search_path pin, the worker either
// claims zero rows in prod (RLS block) or becomes search_path-hijackable — this test
// makes either regression a CI failure.
func TestMF007PIN9(t *testing.T) {
	matches, err := filepath.Glob("../../migrations/0073_*.up.sql")
	if err != nil {
		t.Fatalf("glob 0073 migration: %v", err)
	}
	if len(matches) == 0 {
		t.Fatal("migrations/0073_*.up.sql not found — the principal-less claim DEFINER migration is missing (MF007-PIN-9)")
	}
	src := mustRead(t, matches[0])
	if !strings.Contains(src, "SECURITY DEFINER") {
		t.Fatalf("%s missing SECURITY DEFINER — the principal-less claim would be RLS-blocked in prod (MF007-PIN-9)", matches[0])
	}
	if !strings.Contains(src, "SET search_path") {
		t.Fatalf("%s missing SET search_path — SECURITY DEFINER functions must pin search_path against hijack (MF007-PIN-9)", matches[0])
	}
}

// MF007-PIN-12 (manyforge-206 follow-on): the lease-renewal heartbeat persists
// progress + renews the lease principal-less, so it MUST be a SECURITY DEFINER
// function with a pinned search_path (migrations/0076), exactly like the 0073 claim
// functions. If 0076 loses SECURITY DEFINER or the search_path pin, the heartbeat
// either no-ops under RLS in prod (lease never renewed → long jobs re-claimed) or
// becomes search_path-hijackable — this test makes either regression a CI failure.
func TestMF007PIN12(t *testing.T) {
	matches, err := filepath.Glob("../../migrations/0076_*.up.sql")
	if err != nil {
		t.Fatalf("glob 0076 migration: %v", err)
	}
	if len(matches) == 0 {
		t.Fatal("migrations/0076_*.up.sql not found — the lease-renewal DEFINER migration is missing (MF007-PIN-12)")
	}
	src := mustRead(t, matches[0])
	if !strings.Contains(src, "renew_code_review_lease") {
		t.Fatalf("%s missing renew_code_review_lease — the heartbeat function (MF007-PIN-12)", matches[0])
	}
	if !strings.Contains(src, "SECURITY DEFINER") {
		t.Fatalf("%s missing SECURITY DEFINER — the principal-less renew would be RLS-blocked in prod (MF007-PIN-12)", matches[0])
	}
	if !strings.Contains(src, "SET search_path") {
		t.Fatalf("%s missing SET search_path — SECURITY DEFINER functions must pin search_path against hijack (MF007-PIN-12)", matches[0])
	}
}

// MF007-PIN-13 (manyforge-206 follow-up): the cloud/opencode review prompt must be
// HOST-PROVIDED at runtime (/out/review_instructions.txt), not only baked into the sandbox
// image, so local and cloud reviews share ONE prompt (localreview.go reviewInstructions) and
// a prompt change needs no image rebuild. Pins both halves — the host writes the file, and
// the entrypoint reads it — so a regression back to baked-only fails CI.
func TestMF007PIN13(t *testing.T) {
	svc := mustRead(t, "../agents/coding/service.go")
	if !strings.Contains(svc, "review_instructions.txt") {
		t.Fatal("service.go must write /out/review_instructions.txt so the cloud review uses the host prompt, not the baked default (MF007-PIN-13)")
	}
	ep := mustRead(t, "../../deploy/sandbox/entrypoint.sh")
	if !strings.Contains(ep, "/out/review_instructions.txt") {
		t.Fatal("entrypoint.sh must read /out/review_instructions.txt (host-provided prompt) before falling back to the baked default (MF007-PIN-13)")
	}
}

// MF007-PIN-7: the sandbox runs with --cap-drop ALL (no CAP_DAC_OVERRIDE), so the
// container — a different uid than the host server — can only read the /work mount
// and write the /out mount if the per-run host dirs are world-accessible. service.go
// MUST create checkout 0o755 and out 0o777; a silent regression to 0o700 breaks every
// real review on native Linux (Colima masks it via bind-mount ownership remapping).
func TestSandboxRunDirPermsPinned(t *testing.T) {
	src := mustRead(t, "../agents/coding/service.go")
	for _, frag := range []string{
		`os.Chmod(s.WorkRoot, 0o700)`, // shield: world-perm leaves unreachable by other local users
		`os.Chmod(checkout, 0o755)`,
		`os.Chmod(outDir, 0o777)`,
	} {
		if !strings.Contains(src, frag) {
			t.Fatalf("service.go missing run-dir perm %q — a capless sandbox can't access /work or /out on Linux (or the 0700 WorkRoot shield was dropped)", frag)
		}
	}
}

// MF007-PIN-14 (manyforge-5ai): the host-side local-review path dials with a plain
// (non-egress-proxied) client, so localReview's base-URL guard is the ONLY control
// keeping a run's diff from leaving the machine. It MUST classify the host via netsafe
// (shared with the create-time guard + clone path, so the IMDS/link-local screen can't
// drift) AND gate private LAN on the credential's AllowPrivateBaseURL trust opt-in.
// A regression to a hand-rolled string allowlist — or dropping the trust gate — would
// silently re-open (or over-open) the self-host egress hatch.
func TestLocalReviewBaseURLGuardPinned(t *testing.T) {
	src := mustRead(t, "../agents/coding/localreview.go")
	for _, frag := range []string{
		`func localBaseURLBlocked(`,       // the guard exists
		`netsafe.IsBlocked`,               // classification is netsafe's, not a private copy
		`cred.AllowPrivateBaseURL`,        // private LAN is gated on the trust opt-in
		`localBaseURLBlocked(cred.Host()`, // and localReview actually calls it
	} {
		if !strings.Contains(src, frag) {
			t.Fatalf("localreview.go missing local base-URL guard fragment %q — was the self-host SSRF control weakened? (MF007-PIN-14)", frag)
		}
	}
}
