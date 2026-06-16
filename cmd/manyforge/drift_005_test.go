//go:build contract

package main

import (
	"sort"
	"strings"
	"testing"
)

// inScope005Ops is the COMPLETE set of spec-005 CRM operations served by the router
// (contact read + write CRUD + merge, company read + write CRUD).
// Each entry is asserted both ways by TestOpenAPIDrift005 — present in the router AND
// documented in the 005 contract.
var inScope005Ops = []string{
	"GET /businesses/{}/contacts",
	"POST /businesses/{}/contacts",
	"GET /businesses/{}/contacts/{}",
	"PATCH /businesses/{}/contacts/{}",
	"DELETE /businesses/{}/contacts/{}",
	"POST /businesses/{}/contacts/{}/merge",
	"GET /businesses/{}/companies",
	"POST /businesses/{}/companies",
	"GET /businesses/{}/companies/{}",
	"PATCH /businesses/{}/companies/{}",
	"DELETE /businesses/{}/companies/{}",
}

// is005Op reports whether a normalized "METHOD /path" belongs to the 005 CRM surface
// (the business-nested /contacts + /companies routes), as opposed to the 001/002/003
// routes that share the /businesses prefix.
func is005Op(op string) bool {
	return strings.Contains(op, "/contacts") || strings.Contains(op, "/companies")
}

// TestOpenAPIDrift005 pins the spec-005 CRM contract against the FULL production
// router (built via mountAPIRoutes, the same seam main uses):
//  1. Presence: every in-scope 005 operation is REGISTERED.
//  2. No drift: every registered route on the 005 (/contacts, /companies) surface is documented.
func TestOpenAPIDrift005(t *testing.T) {
	routes := apiRoutes(t)
	spec005 := spec005Routes(t)

	var missing []string
	for _, op := range inScope005Ops {
		if !spec005[op] {
			t.Errorf("test bug: in-scope op %q is not declared in the 005 openapi.yaml", op)
		}
		if !routes[op] {
			missing = append(missing, op)
		}
	}
	sort.Strings(missing)
	for _, op := range missing {
		t.Errorf("005 drift: %q is in-scope and in openapi.yaml but not served by the router", op)
	}

	var undocumented []string
	for op := range routes {
		if !is005Op(op) {
			continue // 001/002/003 route; covered by the other drift tests.
		}
		if !spec005[op] {
			undocumented = append(undocumented, op)
		}
	}
	sort.Strings(undocumented)
	for _, op := range undocumented {
		t.Errorf("005 drift: %q is served by the router but not in 005 openapi.yaml", op)
	}
}
