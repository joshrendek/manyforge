//go:build contract

package main

import (
	"sort"
	"strings"
	"testing"
)

// inScope007Ops is the COMPLETE set of spec-007 operations served by the router
// (slice 1: repo-connector creation + code-review trigger/get).
// Each entry is asserted both ways by TestOpenAPIDrift007 — present in the router AND
// documented in the 007 contract.
var inScope007Ops = []string{
	"POST /businesses/{}/repo-connectors",
	"POST /businesses/{}/code-reviews",
	"GET /businesses/{}/code-reviews/{}",
}

// is007Op reports whether a normalized "METHOD /path" belongs to the 007 surface
// (the business-nested /repo-connectors and /code-reviews routes), as opposed to the
// 001/002/003/005 routes that share the /businesses prefix.
func is007Op(op string) bool {
	return strings.Contains(op, "/repo-connectors") || strings.Contains(op, "/code-reviews")
}

// TestOpenAPIDrift007 pins the spec-007 coding-review contract against the FULL
// production router (built via mountAPIRoutes, the same seam main uses):
//  1. Presence: every in-scope 007 operation is REGISTERED.
//  2. No drift: every registered route on the 007 surface is documented.
func TestOpenAPIDrift007(t *testing.T) {
	routes := apiRoutes(t)
	spec007 := spec007Routes(t)

	var missing []string
	for _, op := range inScope007Ops {
		if !spec007[op] {
			t.Errorf("test bug: in-scope op %q is not declared in the 007 openapi.yaml", op)
		}
		if !routes[op] {
			missing = append(missing, op)
		}
	}
	sort.Strings(missing)
	for _, op := range missing {
		t.Errorf("007 drift: %q is in-scope and in openapi.yaml but not served by the router", op)
	}

	var undocumented []string
	for op := range routes {
		if !is007Op(op) {
			continue // 001/002/003/005 route; covered by the other drift tests.
		}
		if !spec007[op] {
			undocumented = append(undocumented, op)
		}
	}
	sort.Strings(undocumented)
	for _, op := range undocumented {
		t.Errorf("007 drift: %q is served by the router but not in 007 openapi.yaml", op)
	}
}
