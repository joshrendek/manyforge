package security_regression

import (
	"strings"
	"testing"
)

// MF008-PIN-1: the spec-008 config tables (review_dimension, review_config) are tenant data and
// MUST be RLS-isolated exactly like ai_provider_credential (0025) — a missing policy would expose
// one business's reviewer config (prompts, model choices) to another tenant. Pins both new
// migrations: RLS ENABLEd + the business-scoped authorized_businesses policy; and pins that the
// dimension insert derives tenant_root_id from the RLS-visible business row (never a client-
// supplied tenant), so a caller can't forge cross-tenant rows.
func TestMF008PIN1(t *testing.T) {
	for _, mig := range []string{
		"../../migrations/0077_review_dimension.up.sql",
		"../../migrations/0078_review_config.up.sql",
	} {
		src := mustRead(t, mig)
		if !strings.Contains(src, "ENABLE ROW LEVEL SECURITY") {
			t.Fatalf("%s missing ENABLE ROW LEVEL SECURITY — config table is not tenant-isolated (MF008-PIN-1)", mig)
		}
		if !strings.Contains(src, "authorized_businesses(current_principal())") {
			t.Fatalf("%s missing the business-scoped RLS policy — cross-tenant reviewer config would leak (MF008-PIN-1)", mig)
		}
	}
	// InsertReviewDimension must derive tenant_root_id from the business row (ownership pushed
	// into SQL), not accept a client-supplied tenant_root_id.
	q := mustRead(t, "../../db/query/review_dimension.sql")
	if !strings.Contains(q, "FROM business b") || !strings.Contains(q, "b.tenant_root_id") {
		t.Fatal("InsertReviewDimension must derive tenant_root_id from the RLS-visible business row, not from the caller (MF008-PIN-1)")
	}
}

// MF008-PIN-2: per-dimension config must not open a NEW SSRF surface. A dimension carries a
// provider + model but NO endpoint/URL — the review endpoint always comes from the SSRF-guarded
// vault credential (create-time guard +, since manyforge-9er Tasks 4-5, the shared sandbox lane's
// privateBaseURLBlocked dial-time guard in fallbackchain.go — the direct-POST path's own
// localBaseURLBlocked guard this comment used to cite was deleted in Task 6 along with the
// path it guarded). Pins that the review_dimension schema has no base_url/endpoint column, and
// that the per-lane cloud egress stays scoped to the credential host (laneCred.Host()), so a
// per-dimension model override can't redirect a review at an internal target.
func TestMF008PIN2(t *testing.T) {
	mig := strings.ToLower(mustRead(t, "../../migrations/0077_review_dimension.up.sql"))
	for _, banned := range []string{"base_url", "endpoint"} {
		if strings.Contains(mig, banned) {
			t.Fatalf("review_dimension must not carry an endpoint column (%q) — a dimension could then redirect a review at an internal host, bypassing the credential SSRF guard (MF008-PIN-2)", banned)
		}
	}
	svc := mustRead(t, "../agents/coding/service.go")
	if !strings.Contains(svc, "EgressAllow: []string{laneCred.Host()}") {
		t.Fatal("the per-lane cloud egress must stay scoped to the credential host (laneCred.Host()) — a per-dimension override must not widen egress (MF008-PIN-2)")
	}
}

// MF008-PIN-3: the review prompt must be PER-DIMENSION, threaded from the resolved dimension
// into the review path, not the single hardcoded reviewInstructions const. A regression back to
// the const would collapse the panel to one blended prompt and defeat spec 008 — pins the
// host-written per-dimension prompt file. Local and cloud providers share ONE path since
// manyforge-9er Task 4 (runSandboxLane): reviewLane no longer branches a local credential to a
// separate host-side call, so this also pins that regression away — a reintroduced
// isLocalProvider(laneCred.Provider) branch in reviewLane would silently reopen the un-egress-
// gated direct-API path Task 3/4 closed. This test used to also assert localReview's own
// per-dimension prompt threading (`prompt + "\n\n" + reviewSchemaLine` in localreview.go); Task 6
// deleted localReview as dead code (no caller remained), so that half of the pin is gone with it
// — there is nothing left to assert about a function that no longer exists.
func TestMF008PIN3(t *testing.T) {
	svc := mustRead(t, "../agents/coding/service.go")
	if !strings.Contains(svc, "[]byte(dim.Prompt)") {
		t.Fatal("the sandbox lane must write the dimension's prompt to review_instructions.txt (per-dimension), not the const (MF008-PIN-3)")
	}
	if strings.Contains(svc, "isLocalProvider(laneCred.Provider)") {
		t.Fatal("reviewLane must not branch a local credential to a separate host-side call — local and cloud share the egress-gated sandbox path (runSandboxLane, manyforge-9er Task 4) (MF008-PIN-3)")
	}
}
