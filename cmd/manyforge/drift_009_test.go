//go:build contract

package main

import (
	"sort"
	"strings"
	"testing"
)

// inScope009Ops is the COMPLETE set of spec-009 operations served by the router
// (slice 1: operator App setup, per-business install-url, authenticated link,
// and the public webhook receiver). Each entry is asserted both ways by
// TestOpenAPIDrift009 — present in the router AND documented in the 009 contract.
var inScope009Ops = []string{
	"GET /github/app/manifest",
	"POST /github/app/manifest/convert",
	"POST /github/app/installations/link",
	"GET /businesses/{}/github/app/install-url",
	"POST /github/webhook",
}

// is009Op reports whether a normalized "METHOD /path" belongs to the 009 surface
// (every spec-009 route contains "/github/"), as opposed to the 001/002/003/005/007
// routes that also share the /businesses prefix.
func is009Op(op string) bool {
	return strings.Contains(op, "/github/")
}

// TestOpenAPIDrift009 pins the spec-009 GitHub App contract against the FULL
// production router (built via mountAPIRoutes, the same seam main uses):
//  1. Presence: every in-scope 009 operation is REGISTERED.
//  2. No drift: every registered route on the 009 surface is documented.
func TestOpenAPIDrift009(t *testing.T) {
	routes := apiRoutes(t)
	spec009 := spec009Routes(t)

	var missing []string
	for _, op := range inScope009Ops {
		if !spec009[op] {
			t.Errorf("test bug: in-scope op %q is not declared in the 009 openapi.yaml", op)
		}
		if !routes[op] {
			missing = append(missing, op)
		}
	}
	sort.Strings(missing)
	for _, op := range missing {
		t.Errorf("009 drift: %q is in-scope and in openapi.yaml but not served by the router", op)
	}

	var undocumented []string
	for op := range routes {
		if !is009Op(op) {
			continue // non-009 route; covered by the other drift tests.
		}
		if !spec009[op] {
			undocumented = append(undocumented, op)
		}
	}
	sort.Strings(undocumented)
	for _, op := range undocumented {
		t.Errorf("009 drift: %q is served by the router but not in 009 openapi.yaml", op)
	}
}
