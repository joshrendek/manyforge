// No build tag: these source-level pins run in both `make test` and
// `make sec-test` with no infrastructure. They back the Slice-2 pull_request
// trigger path (Task 4): a refactor that silently drops the fork/bot-author/
// draft filters, the per-repo installation-token scope, the DEFINER
// REVOKE/GRANT pair, the github_app secret_ref invariant, or the connector
// delete guard fails CI even if a behavioral test is also weakened.
//
// Task 5 adds the runJob egress pre-flight pin (spec §7 item 6): the shared
// review path (not just the enqueue path) must fail fast on a provider host
// outside the sandbox egress allowlist, so the app-triggered path — which
// never goes through Enqueue's check — can't launch a doomed sandbox.

package security_regression

import (
	"strings"
	"testing"
)

// TestGithubPRTriggerFiltersPinned asserts the pull_request webhook handler
// (internal/githubapp/pullrequest.go) still rejects fork PRs, bot-authored
// PRs, and draft PRs before ever resolving an installation or ingesting a
// review, and that the ingest carries the delivery id for replay dedup.
func TestGithubPRTriggerFiltersPinned(t *testing.T) {
	src := mustRead(t, "../githubapp/pullrequest.go")
	cases := []struct{ name, fragment string }{
		// A PR from a fork (or one whose head repo is gone) must never be
		// auto-reviewed — the diff could target arbitrary attacker-controlled
		// code without repo write access.
		{"fork check", "ev.PullRequest.Head.Repo == nil || ev.PullRequest.Head.Repo.ID != ev.PullRequest.Base.Repo.ID"},
		// A bot-authored PR (e.g. a dependency-bump bot) is filtered by author
		// type, not just draft state.
		{"bot author check", `ev.PullRequest.User.Type == "Bot"`},
		// Draft PRs are still in flux and shouldn't burn a review run.
		{"draft skip", "if ev.PullRequest.Draft {"},
		// The delivery id is threaded into the ingest so the DEFINER's replay
		// dedup (migrations/0084) actually has something to key on.
		{"delivery id threaded", `DeliveryID:       r.Header.Get("X-GitHub-Delivery")`},
	}
	for _, c := range cases {
		if !strings.Contains(src, c.fragment) {
			t.Errorf("%s: pin %q missing from pullrequest.go — was the filter removed or weakened?", c.name, c.fragment)
		}
	}
}

// TestGithubPRInstallationTokenScopedPinned asserts MintInstallationToken
// still mints a per-repo token (the "repositories" field), never a
// whole-installation token — a broadened token would let a single compromised
// review job read/write every repo the App is installed on.
func TestGithubPRInstallationTokenScopedPinned(t *testing.T) {
	src := mustRead(t, "../githubapp/client.go")
	if !strings.Contains(src, `map[string]any{"repositories": []string{name}}`) {
		t.Error(`MintInstallationToken pin missing: expected the access-token request body to scope to "repositories" — was it widened to a whole-installation token?`)
	}
}

// TestGithubPRReviewDefinerGrantsPinned asserts the migrations/0084
// principal-less SECURITY DEFINER functions stay locked down to PUBLIC and
// explicitly granted only to manyforge_app — without this, any role could
// call the installation-resolution/ingest DEFINER directly and bypass the
// filters/rate-cap/dedup entirely.
func TestGithubPRReviewDefinerGrantsPinned(t *testing.T) {
	up := mustRead(t, "../../migrations/0084_github_pr_review.up.sql")
	cases := []struct{ name, fragment string }{
		{"context fn revoke", "REVOKE ALL ON FUNCTION github_installation_context(bigint) FROM PUBLIC;"},
		{"context fn grant", "GRANT EXECUTE ON FUNCTION github_installation_context(bigint) TO manyforge_app;"},
		{"ingest fn revoke", "REVOKE ALL ON FUNCTION github_pr_review_ingest(bigint, text, uuid, uuid, uuid, uuid, text, int, text) FROM PUBLIC;"},
		{"ingest fn grant", "GRANT EXECUTE ON FUNCTION github_pr_review_ingest(bigint, text, uuid, uuid, uuid, uuid, text, int, text) TO manyforge_app;"},
	}
	for _, c := range cases {
		if !strings.Contains(up, c.fragment) {
			t.Errorf("%s: pin %q missing from migrations/0084 — was the DEFINER left open to PUBLIC?", c.name, c.fragment)
		}
	}
}

// TestGithubPRConnectorSecretRefNullPinned asserts migrations/0083 still
// enforces that a github_app connector (no stored PAT — auth is minted
// per-repo from the App installation) can never carry a secret_ref, and a
// plain github connector can never be missing one.
func TestGithubPRConnectorSecretRefNullPinned(t *testing.T) {
	up := mustRead(t, "../../migrations/0083_repo_connector_github_app.up.sql")
	if !strings.Contains(up, "(type = 'github_app' AND secret_ref IS NULL AND config ? 'installation_id')") {
		t.Error("repo_connector secret_ref CHECK pin missing — a github_app connector must never carry a stored secret_ref (was the CHECK loosened?)")
	}
}

// TestGithubPRConnectorDeleteRejectsAppPinned asserts RepoConnectorService.
// Delete still refuses to delete a github_app connector directly — those
// rows are lifecycle-managed by the GitHub App installation (created/removed
// via the webhook), so a direct delete would desync repo_connector from the
// installation without actually uninstalling the App.
func TestGithubPRConnectorDeleteRejectsAppPinned(t *testing.T) {
	src := mustRead(t, "../connectors/repo_service.go")
	if !strings.Contains(src, "github_app connectors are managed by the GitHub App install") {
		t.Error("repo_service.go Delete pin missing — github_app connectors must be rejected from direct deletion")
	}
}

// TestGithubPRRunJobEgressPreflightPinned asserts the SHARED code-review path
// (coding.runJob) still fails fast on a provider host outside the sandbox
// egress allowlist — BEFORE launching a sandbox the boot-static proxy would
// CONNECT-block. The app-triggered path is enqueued by the webhook DEFINER and
// never runs Enqueue's own egress check, so runJob's is the only gate it gets.
// (spec §7 item 6 / fable M5). manyforge-9er Task 3 dropped the isLocalProvider
// carve-out (local providers now route through the sandbox too, Task 4) and
// added a second gate: an IP-literal private host requires the credential's
// AllowPrivateBaseURL opt-in (privateBaseURLBlocked, fallbackchain.go) — both
// halves are pinned here so neither can silently regress.
func TestGithubPRRunJobEgressPreflightPinned(t *testing.T) {
	src := mustRead(t, "../agents/coding/service.go")
	// The allowlist gate applies to every provider now — no isLocalProvider carve-out.
	if !strings.Contains(src, "!s.EgressAllow.Allows(cred.Host())") {
		t.Error("runJob egress pre-flight pin missing — the shared review path must reject a provider host outside the sandbox egress allowlist")
	}
	if strings.Contains(src, "!isLocalProvider(cred.Provider) && !s.EgressAllow.Allows(cred.Host())") {
		t.Error("runJob egress pre-flight must NOT carve out local providers — Task 4 routes them through the sandbox too")
	}
	// The runJob-specific failJob message (distinct from Enqueue's), so the pin
	// tracks the runJob gate and not merely Enqueue's pre-existing one.
	if !strings.Contains(src, `provider host %q not in sandbox egress allowlist`) {
		t.Error("runJob egress pre-flight failJob message pin missing — was the runJob gate removed?")
	}
	// The private-base-URL opt-in gate (manyforge-9er Task 3).
	if !strings.Contains(src, "privateBaseURLBlocked(cred.Host(), cred.AllowPrivateBaseURL)") {
		t.Error("runJob private-base-URL gate pin missing — a private/IP-literal host must require allow_private_base_url")
	}
}

// TestGithubPRRunJobFinalizeNonNullJSONBPinned asserts the fable C1/C2 fixes stay
// in place: both the fail() path and finalizeSkipped write non-null dimension_runs
// ("[]"). A nil []byte encodes to SQL NULL against the jsonb NOT NULL columns → 23502
// → the UPDATE silently aborts, stranding the row 'running' (fail: last_error lost;
// finalizeSkipped: an infinite claim/re-claim loop since the worker never fails a
// nil-returning job).
func TestGithubPRRunJobFinalizeNonNullJSONBPinned(t *testing.T) {
	src := mustRead(t, "../agents/coding/service.go")
	// finalizeSkipped must exist and propagate its error (never swallow it).
	if !strings.Contains(src, "func (s *CodeReviewService) finalizeSkipped(") {
		t.Fatal("finalizeSkipped pin missing — the claim-time same-head skip must finalize 'succeeded' concretely")
	}
	// Both terminal writers (fail() and finalizeSkipped) must carry a non-nil
	// dimension_runs. Whitespace-insensitive so gofmt alignment drift doesn't break it.
	nospace := strings.NewReplacer(" ", "", "\t", "").Replace(src)
	if strings.Count(nospace, `DimensionRuns:[]byte("[]")`) < 2 {
		t.Error("fable C1/C2 pin: fail() and finalizeSkipped must both pass DimensionRuns: []byte(\"[]\") — a nil jsonb 23502-aborts the UPDATE")
	}
}
