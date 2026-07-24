//go:build contract

package main

import (
	"sort"
	"strings"
	"testing"
)

// inScope006Ops is the COMPLETE set of spec-006 feedback operations served by the router:
// the authenticated board/post/key management surface (nested under /businesses/{id}) plus
// the principal-less public SDK/portal ingress (/feedback/public/{key}/...). Each entry is
// asserted both ways by TestOpenAPIDrift006 — present in the router AND documented in the 006
// contract.
var inScope006Ops = []string{
	// authenticated management
	"GET /businesses/{}/feedback/boards",
	"POST /businesses/{}/feedback/boards",
	"GET /businesses/{}/feedback/boards/{}",
	"PATCH /businesses/{}/feedback/boards/{}",
	"GET /businesses/{}/feedback/boards/{}/posts",
	"POST /businesses/{}/feedback/boards/{}/posts",
	"GET /businesses/{}/feedback/boards/{}/keys",
	"POST /businesses/{}/feedback/boards/{}/keys",
	"GET /businesses/{}/feedback/posts/{}",
	"PATCH /businesses/{}/feedback/posts/{}",
	"DELETE /businesses/{}/feedback/posts/{}",
	"POST /businesses/{}/feedback/posts/{}/vote",
	"POST /businesses/{}/feedback/posts/{}/convert",
	"POST /businesses/{}/feedback/keys/{}/revoke",
	// public SDK/portal ingress
	"GET /feedback/public/{}/posts",
	"POST /feedback/public/{}/posts",
	"POST /feedback/public/{}/posts/{}/votes",
}

// is006Op reports whether a normalized "METHOD /path" belongs to the 006 feedback surface
// (every 006 route contains /feedback), as opposed to the 001/002/005 routes that share the
// /businesses prefix.
func is006Op(op string) bool {
	return strings.Contains(op, "/feedback")
}

// TestOpenAPIDrift006 pins the spec-006 feedback contract against the FULL production router
// (built via mountAPIRoutes, the same seam main uses):
//  1. Presence: every in-scope 006 operation is REGISTERED.
//  2. No drift: every registered route on the 006 (/feedback) surface is documented.
func TestOpenAPIDrift006(t *testing.T) {
	routes := apiRoutes(t)
	spec006 := spec006Routes(t)

	var missing []string
	for _, op := range inScope006Ops {
		if !spec006[op] {
			t.Errorf("test bug: in-scope op %q is not declared in the 006 openapi.yaml", op)
		}
		if !routes[op] {
			missing = append(missing, op)
		}
	}
	sort.Strings(missing)
	for _, op := range missing {
		t.Errorf("006 drift: %q is in-scope and in openapi.yaml but not served by the router", op)
	}

	var undocumented []string
	for op := range routes {
		if is006Op(op) && !spec006[op] {
			undocumented = append(undocumented, op)
		}
	}
	sort.Strings(undocumented)
	for _, op := range undocumented {
		t.Errorf("006 drift: %q is served on the feedback surface but not documented in the 006 openapi.yaml", op)
	}
}
