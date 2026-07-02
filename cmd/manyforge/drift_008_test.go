//go:build contract

package main

import (
	"sort"
	"strings"
	"testing"
)

// inScope008Ops is the COMPLETE set of spec-008 Slice 2 operations served by the router:
// the review-dimension CRUD + the review-config get/put. Each entry is asserted both ways by
// TestOpenAPIDrift008 — present in the router AND documented in the 008 contract.
var inScope008Ops = []string{
	"GET /businesses/{}/review-dimensions",
	"POST /businesses/{}/review-dimensions",
	"DELETE /businesses/{}/review-dimensions/{}",
	"GET /businesses/{}/review-config",
	"PUT /businesses/{}/review-config",
}

// is008Op reports whether a normalized "METHOD /path" belongs to the 008 surface (the
// business-nested /review-dimensions and /review-config routes).
func is008Op(op string) bool {
	return strings.Contains(op, "/review-dimensions") || strings.Contains(op, "/review-config")
}

// TestOpenAPIDrift008 pins the spec-008 config contract against the FULL production router:
//  1. Presence: every in-scope 008 operation is REGISTERED.
//  2. No drift: every registered route on the 008 surface is documented.
func TestOpenAPIDrift008(t *testing.T) {
	routes := apiRoutes(t)
	spec008 := spec008Routes(t)

	var missing []string
	for _, op := range inScope008Ops {
		if !spec008[op] {
			t.Errorf("test bug: in-scope op %q is not declared in the 008 openapi.yaml", op)
		}
		if !routes[op] {
			missing = append(missing, op)
		}
	}
	sort.Strings(missing)
	for _, op := range missing {
		t.Errorf("008 drift: %q is in-scope and in openapi.yaml but not served by the router", op)
	}

	var undocumented []string
	for op := range routes {
		if !is008Op(op) {
			continue
		}
		if !spec008[op] {
			undocumented = append(undocumented, op)
		}
	}
	sort.Strings(undocumented)
	for _, op := range undocumented {
		t.Errorf("008 drift: %q is served by the router but not in 008 openapi.yaml", op)
	}
}
