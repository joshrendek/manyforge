// Pin: MF-005-approvals — the approvals wire response exposes only a redacted
// `summary`, never raw `args`, and the route stays gated by `agents.approve`.
//
// No build tag: runs in both `make test` and `make sec-test` with no
// infrastructure. Any refactor that (a) re-adds args to the wire shape,
// (b) drops the redacted summary, (c) un-gates the approvals routes from the
// agents.approve permission, or (d) wires agentsApprove with a different
// permission string will fail CI loudly.

package security_regression

import (
	"regexp"
	"strings"
	"testing"
)

// TestApprovalRespNeverExposesArgs pins the approvalResp struct shape: it must
// carry the redacted summary field, must NOT carry a json:"args" field, and the
// mapping function must populate summary via approvalSummary (not pass-through).
func TestApprovalRespNeverExposesArgs(t *testing.T) {
	src := mustRead(t, "../agents/approval_handler.go")

	// Isolate the approvalResp struct block.
	m := regexp.MustCompile(`(?s)type approvalResp struct \{.*?\}`).FindString(src)
	if m == "" {
		t.Fatal("approvalResp struct not found in approval_handler.go — struct was renamed or removed?")
	}
	if strings.Contains(m, `json:"args"`) || regexp.MustCompile(`\bArgs\b`).MatchString(m) {
		t.Fatalf("approvalResp must NOT expose args (raw planned-action payload would leak to queue UI):\n%s", m)
	}
	if !strings.Contains(m, `json:"summary"`) {
		t.Fatalf("approvalResp must expose the redacted summary field:\n%s", m)
	}

	// Pin the mapping: summary must come from approvalSummary(), not a direct
	// pass-through of a.Args.
	if !strings.Contains(src, "approvalSummary(a.Tool, a.Args)") {
		t.Fatal("toApprovalResp must populate Summary via approvalSummary(a.Tool, a.Args)")
	}
}

// TestApprovalsRouteGatedByApprovePermission pins two things in main.go:
//  1. agentsApprove middleware is wired via httpx.RequirePermission with the
//     literal string "agents.approve" — so swapping the permission string fails here.
//  2. The approvals route group uses ap.Use(h.agentsApprove) — the middleware is
//     applied to the group before ProtectedRoutes is mounted.
//
// Real gating found in main.go (lines ~475, ~767-769):
//
//	agentsApprove: httpx.RequirePermission(..., "agents.approve", ...),
//	...
//	pr.Group(func(ap chi.Router) {
//	    ap.Use(h.agentsApprove)
//	    h.approvals.ProtectedRoutes(ap)
//	})
func TestApprovalsRouteGatedByApprovePermission(t *testing.T) {
	src := mustRead(t, "../../cmd/manyforge/main.go")

	// Pin 1: agentsApprove is bound to the agents.approve permission (authz.PermAgentsApprove
	// since manyforge-xxe; TestPin_PermConstantsMatchSeededCatalog pins the constant→SQL key).
	// Whitespace-tolerant and scoped to the load-bearing tokens, so gofmt re-aligning
	// the struct literal (e.g. a longer-named sibling field) or renaming the local
	// resolver vars never false-fails this pin.
	if !regexp.MustCompile(`agentsApprove:\s+httpx\.RequirePermission\([^)]*authz\.PermAgentsApprove`).MatchString(src) {
		t.Error(`main.go: agentsApprove must be wired as httpx.RequirePermission(..., authz.PermAgentsApprove, ...) — permission gate removed or permission constant changed?`)
	}

	// Pin 2: the approvals route group applies h.agentsApprove before mounting routes.
	// We assert both tokens are present and that the middleware use appears before the
	// ProtectedRoutes mount within the same source region (position check).
	useIdx := strings.Index(src, "ap.Use(h.agentsApprove)")
	routesIdx := strings.Index(src, "h.approvals.ProtectedRoutes(ap)")
	if useIdx < 0 {
		t.Fatal("main.go: ap.Use(h.agentsApprove) not found — approvals route group no longer applies the agents.approve gate")
	}
	if routesIdx < 0 {
		t.Fatal("main.go: h.approvals.ProtectedRoutes(ap) not found — approvals routes unmounted?")
	}
	if useIdx >= routesIdx {
		t.Errorf("main.go: ap.Use(h.agentsApprove) (pos %d) must appear before h.approvals.ProtectedRoutes(ap) (pos %d)", useIdx, routesIdx)
	}
}
