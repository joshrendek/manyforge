package security_regression

import (
	"os"
	"strings"
	"testing"
)

// manyforge-nk50 — the cross-business analytics overview.
//
// This route is the first read in the codebase that spans businesses: it takes no business id and
// returns every analytics site the caller may read. That makes it the first place where the usual
// protection — RequirePermission(..., businessIDFromPath) — structurally cannot apply, so the
// permission rule had to move into SQL. These pin the properties that make that safe.

func nk50Read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// The overview must filter on PERMISSION, not merely on RLS visibility. authorized_businesses()
// answers "is this principal a member", which is a weaker question than "may they read telemetry
// here". A member whose role lacks telemetry.read would otherwise see sites listed on the overview
// that the per-site dashboard refuses to open — the permissive answer being the one on screen.
func TestNK50_OverviewFiltersOnPermissionNotMembership(t *testing.T) {
	src := nk50Read(t, "../analytics/read.go")
	i := strings.Index(src, "func (s *Service) Overview(")
	if i < 0 {
		t.Fatal("Service.Overview is gone — if the overview moved, move this pin with it")
	}
	body := src[i:]

	if !strings.Contains(body, "businesses_with_permission(current_principal(), 'telemetry.read')") {
		t.Error("Overview no longer filters through businesses_with_permission(…, 'telemetry.read'). " +
			"Without it the overview lists every business the caller is a MEMBER of, including ones " +
			"where they lack telemetry.read and the per-site dashboard would 404.")
	}
	// Revoked sites must not appear: a revoked key has stopped collecting, and showing it implies
	// it is still live.
	if !strings.Contains(body, "c.revoked_at IS NULL") {
		t.Error("Overview no longer excludes revoked sites")
	}
}

// The SQL predicate mirrors internal/authz.Resolve. Mirrored logic drifts, so pin the three rules
// that make them equivalent — if Resolve gains a fourth, this fails and forces the question.
func TestNK50_PermissionPredicateMirrorsResolver(t *testing.T) {
	mig := nk50Read(t, "../../migrations/0110_businesses_with_permission.up.sql")

	for _, rule := range []struct{ frag, why string }{
		{"r.is_locked",
			"the locked (owner) role resolves to the whole permission catalog in authz.Resolve; " +
				"dropping this makes owners lose access they have everywhere else"},
		{"rp.permission_key = perm",
			"a non-owner role needs the permission granted explicitly; dropping this would grant " +
				"every member of a business every permission"},
		{"anc.status <> 'archived'",
			"EffectivePermissions ignores memberships held through an archived ancestor; dropping " +
				"this would resurrect access through archived businesses"},
		{"SECURITY DEFINER",
			"membership/role/role_permission are RLS-protected; without DEFINER a policy reading " +
				"them under the caller's own principal would recurse"},
		{"SET search_path = public",
			"a SECURITY DEFINER function without a pinned search_path is hijackable by a caller " +
				"who can create objects in an earlier schema"},
	} {
		if !strings.Contains(mig, rule.frag) {
			t.Errorf("businesses_with_permission lost %q — %s", rule.frag, rule.why)
		}
	}

	// The function must never be executable by PUBLIC: it answers permission questions for an
	// arbitrary principal id.
	if !strings.Contains(mig, "REVOKE ALL ON FUNCTION businesses_with_permission(uuid, text) FROM PUBLIC") {
		t.Error("businesses_with_permission is not revoked from PUBLIC")
	}
}

// The overview handler must still require an authenticated principal. It sits outside the
// telemetryRead middleware group by necessity, which removes the usual backstop, so the handler's
// own check is the only thing standing between an anonymous request and the query.
func TestNK50_OverviewRequiresPrincipal(t *testing.T) {
	src := nk50Read(t, "../analytics/handler.go")
	i := strings.Index(src, "func (h *Handler) overview(")
	if i < 0 {
		t.Fatal("overview handler is gone")
	}
	body := src[i:]
	if j := strings.Index(body, "func (h *Handler) "); j > 0 {
		if k := strings.Index(body[10:], "func (h *Handler) "); k > 0 {
			body = body[:k+10]
		}
	}
	if !strings.Contains(body, "PrincipalFromContext") {
		t.Error("the overview handler does not check for a principal. It is mounted outside the " +
			"telemetryRead group (that middleware needs a business id in the path, which this route " +
			"has none of), so this check is the only authentication gate on the route.")
	}
}

// Reads must go against rollups, never raw analytics_event: raw events are partitioned and grow
// without bound within the retention window, so a dashboard querying them gets slower every day.
func TestNK50_OverviewReadsRollupsOnly(t *testing.T) {
	src := nk50Read(t, "../analytics/read.go")
	i := strings.Index(src, "func (s *Service) Overview(")
	if i < 0 {
		t.Fatal("Service.Overview is gone")
	}
	if strings.Contains(src[i:], "FROM analytics_event") {
		t.Error("Overview queries raw analytics_event; it must read analytics_daily so response " +
			"time does not scale with traffic volume")
	}
}
