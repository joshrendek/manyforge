//go:build contract

package main

import (
	"sort"
	"strings"
	"testing"
)

// inScope011Ops is the COMPLETE set of manyforge-as0 analytics operations served by the router:
// the public embed surface (mounted at the ROOT, deliberately outside /api/v1 so an embed tag
// survives API versioning) plus the authenticated dashboard read.
var inScope011Ops = []string{
	"GET /a.js",
	"POST /a/e",
	"GET /businesses/{}/analytics/summary",
}

// is011Op reports whether a normalized "METHOD /path" belongs to the analytics surface. The embed
// routes are matched exactly because "/a" is too short to substring-match safely; the read route
// is matched on /analytics.
func is011Op(op string) bool {
	return op == "GET /a.js" || op == "POST /a/e" || strings.Contains(op, "/analytics")
}

// TestOpenAPIDrift011 pins the as0 analytics contract against the FULL production router.
//
// The second direction matters most here: /a/e is a principal-less endpoint reachable by anyone
// on the internet, so a new public route shipping undocumented would be unreviewed public attack
// surface.
func TestOpenAPIDrift011(t *testing.T) {
	routes := apiRoutes(t)
	spec011 := spec011Routes(t)

	if len(spec011) == 0 {
		t.Fatal("specs/011-analytics-pageviews/contracts/openapi.yaml is missing or declares no paths")
	}

	var missing []string
	for _, op := range inScope011Ops {
		if !spec011[op] {
			t.Errorf("test bug: in-scope op %q is not declared in the 011 openapi.yaml", op)
		}
		if !routes[op] {
			missing = append(missing, op)
		}
	}
	sort.Strings(missing)
	for _, op := range missing {
		t.Errorf("011 drift: %q is in-scope and in openapi.yaml but not served by the router", op)
	}

	var undocumented []string
	for op := range routes {
		if is011Op(op) && !spec011[op] {
			undocumented = append(undocumented, op)
		}
	}
	sort.Strings(undocumented)
	for _, op := range undocumented {
		t.Errorf("011 drift: %q is served by the router but not in 011 openapi.yaml", op)
	}
}
