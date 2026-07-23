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

// MF007-PIN-14 (manyforge-5ai): originally pinned localReview's localBaseURLBlocked —
// the dial-time base-URL guard for the host-side direct-POST local-review path (a
// plain, non-egress-proxied client, so that guard was the ONLY control keeping a run's
// diff from leaving the machine). manyforge-9er Tasks 4-5 routed local providers
// through the same egress-gated opencode sandbox as cloud providers, leaving the
// direct-POST path with no caller; Task 6 deleted it (localReview, streamLocalReview,
// localBaseURLBlocked, and friends) along with this pin's target. There is nothing
// left to pin here — the construct this test asserted no longer exists in source.
// The sandbox path's egress guard is pinned separately: privateBaseURLBlocked
// (fallbackchain.go) by TestGithubPRRunJobEgressPreflightPinned in
// github_pr_trigger_pin_test.go, and the sandbox's network posture by the
// MF-KUBE-SANDBOX-* pins in mf_kube_sandbox_test.go.

// MF007-PIN-15 (manyforge-7ml.1): every code-review lifecycle step must write an audit
// entry — Spec-007's "every coding action audited" regression contract. The behavior
// lives in service.go: DB-colocated steps write in-tx via audit.Write(ctx, tx, codingAudit(...)),
// and steps not co-located with a DB mutation go through s.auditStep(...) (which opens its
// own tx and calls the same codingAudit helper). Every review run reaches exactly one
// terminal outcome — "posted" (success) or "failed" — so if a refactor drops the audit
// at a terminal step the coding-action trail silently goes dark with no other test noticing.
// This pins the audit plumbing AND each lifecycle action verb; behavioral coverage needs a
// DB, so this is a source-level pin (per the security-regression source-pin discipline).
func TestCodingReviewLifecycleAudited(t *testing.T) {
	src := mustRead(t, "../agents/coding/service.go")

	// Audit sink plumbing: the helper that stamps every code_review entry, the in-tx sink,
	// and the standalone-step wrapper. If any of these is gutted the verbs below become dead
	// strings that write nothing.
	for _, frag := range []string{
		`audit.Write(ctx, tx, codingAudit(`,           // in-tx audit sink
		`func (s *CodeReviewService) auditStep(`,        // standalone-step wrapper (opens its own tx)
		`func codingAudit(`,                             // entry builder
		`tt := "code_review"`,                           // stamps TargetType = code_review
	} {
		if !strings.Contains(src, frag) {
			t.Fatalf("coding audit plumbing %q missing from service.go — is the coding-action audit trail still wired? (MF007-PIN-15)", frag)
		}
	}

	// Each lifecycle step must still emit its audit action verb. Terminal outcomes
	// (posted/failed) are load-bearing: every review run ends in exactly one of them.
	for _, action := range []string{
		"agent.coding.review.requested",          // request accepted (in-tx with the pending insert)
		"agent.coding.review.fallback_model",     // retry model downgrade
		"agent.coding.review.files_dropped",      // over-budget files shed
		"agent.coding.review.dimensions_skipped", // dimension glob-scoped out
		"agent.coding.opencode.invoked",          // the coding action itself (sandbox run)
		"agent.coding.review.posted",             // terminal success
		"agent.coding.review.failed",             // terminal failure
		"agent.coding.review.skipped_superseded", // terminal: newer review superseded this run
		"agent.coding.review.finding_dropped",    // verify pass dropped a finding / failed open (8qs.1)
	} {
		if !strings.Contains(src, `"`+action+`"`) {
			t.Fatalf("coding audit action %q missing from service.go — a review lifecycle step lost its audit entry (MF007-PIN-15)", action)
		}
	}
}

// MF007-PIN-16 (manyforge-8qs.2): the "cite rules" feature seeds the reviewed repo's OWN rule
// docs into the review prompt from inside the sandbox (the host can't read /work under KubeRunner).
// Its security property: the extractor reads ONLY a fixed allowlist of doc paths under /work —
// never a glob or directory walk — so a secret file (.env, credentials) can't be pulled into the
// model prompt. This pins the entrypoint wiring AND that rules.sh stays fixed-path.
func TestCiteRulesSandboxWiringPinned(t *testing.T) {
	ep := mustRead(t, "../../deploy/sandbox/entrypoint.sh")
	for _, frag := range []string{
		`\"rule_id\": string`, // rule_id in the baked output schema (escaped inside the bash string)
		`CITE_RULES`,          // host-set gate
		`emit_project_rules`,  // seeds via the extractor when gated on
	} {
		if !strings.Contains(ep, frag) {
			t.Fatalf("entrypoint.sh missing cite-rules wiring %q (MF007-PIN-16)", frag)
		}
	}

	rules := mustRead(t, "../../deploy/sandbox/rules.sh")
	for _, frag := range []string{"CLAUDE.md", ".specify/memory/constitution.md", "AGENTS.md"} {
		if !strings.Contains(rules, frag) {
			t.Fatalf("rules.sh missing fixed rule-doc path %q (MF007-PIN-16)", frag)
		}
	}
	// A future refactor must not turn the fixed reads into a glob/scan — that would be a
	// secret-file exfiltration surface (an .env in the repo root would enter the prompt).
	for _, banned := range []string{"find ", "*.md", "ls ", "*.txt"} {
		if strings.Contains(rules, banned) {
			t.Fatalf("rules.sh must NOT glob/scan for docs (found %q) — only fixed paths keep secrets out of the prompt (MF007-PIN-16)", banned)
		}
	}
}

// MF007-PIN-17 (manyforge-e54.1): the cross-iteration tracking table code_review_finding_seen holds
// per-business finding history — it MUST be RLS-protected with the business-scoped policy, or one
// tenant's review history leaks to another. Pins the migration (0100) that creates it.
func TestFindingSeenTableRLSPinned(t *testing.T) {
	matches, err := filepath.Glob("../../migrations/0100_*.up.sql")
	if err != nil || len(matches) == 0 {
		t.Fatalf("migration 0100 (code_review_finding_seen) not found: %v", err)
	}
	src := mustRead(t, matches[0])
	for _, frag := range []string{
		"CREATE TABLE code_review_finding_seen",
		"ENABLE ROW LEVEL SECURITY",
		"authorized_businesses(current_principal())", // the business-scoped policy predicate
		"tenant_root_id",                             // tenant column present (composite FK + immutability)
	} {
		if !strings.Contains(src, frag) {
			t.Fatalf("migration 0100 missing %q — the finding-history table must be tenant-isolated (MF007-PIN-17)", frag)
		}
	}
}

// MF007-PIN-18 (manyforge-e54.2): the per-repo dimension-override table review_dimension_repo_override
// holds per-business review config — it MUST be RLS-protected with the business-scoped policy so one
// tenant can't read/alter another's per-repo review overrides. Pins the migration (0101).
func TestRepoDimensionOverrideTableRLSPinned(t *testing.T) {
	matches, err := filepath.Glob("../../migrations/0101_*.up.sql")
	if err != nil || len(matches) == 0 {
		t.Fatalf("migration 0101 (review_dimension_repo_override) not found: %v", err)
	}
	src := mustRead(t, matches[0])
	for _, frag := range []string{
		"CREATE TABLE review_dimension_repo_override",
		"ENABLE ROW LEVEL SECURITY",
		"authorized_businesses(current_principal())",
		"tenant_root_id",
	} {
		if !strings.Contains(src, frag) {
			t.Fatalf("migration 0101 missing %q — the per-repo override table must be tenant-isolated (MF007-PIN-18)", frag)
		}
	}
}
