//go:build contract

package main

import (
	"sort"
	"strings"
	"testing"
)

// inScope010Ops is the COMPLETE set of manyforge-p20 telemetry operations served by the router:
// the authenticated client-registration surface (nested under /businesses/{id}) plus the
// principal-less public ingest endpoint. Each entry is asserted both ways by TestOpenAPIDrift010
// — present in the router AND documented in the 010 contract.
var inScope010Ops = []string{
	// authenticated client registration
	"GET /businesses/{}/telemetry/clients",
	"POST /businesses/{}/telemetry/clients",
	"POST /businesses/{}/telemetry/clients/{}/revoke",
	// public principal-less ingest
	"POST /telemetry/ingest/{}",
}

// is010Op reports whether a normalized "METHOD /path" belongs to the p20 telemetry surface.
// Every 010 route contains /telemetry, which distinguishes it from the 001/002/005/006 routes
// that share the /businesses prefix.
func is010Op(op string) bool {
	return strings.Contains(op, "/telemetry")
}

// TestOpenAPIDrift010 pins the p20 telemetry contract against the FULL production router (built
// via mountAPIRoutes, the same seam main uses):
//  1. Presence: every in-scope 010 operation is REGISTERED.
//  2. No drift: every registered route on the telemetry surface is documented.
//
// The second direction is what stops a new ingest route from shipping undocumented — which for a
// principal-less endpoint would mean an unreviewed public attack surface.
func TestOpenAPIDrift010(t *testing.T) {
	routes := apiRoutes(t)
	spec010 := spec010Routes(t)

	if len(spec010) == 0 {
		t.Fatal("specs/010-telemetry-ingest/contracts/openapi.yaml is missing or declares no paths")
	}

	var missing []string
	for _, op := range inScope010Ops {
		if !spec010[op] {
			t.Errorf("test bug: in-scope op %q is not declared in the 010 openapi.yaml", op)
		}
		if !routes[op] {
			missing = append(missing, op)
		}
	}
	sort.Strings(missing)
	for _, op := range missing {
		t.Errorf("010 drift: %q is in-scope and in openapi.yaml but not served by the router", op)
	}

	var undocumented []string
	for op := range routes {
		if is010Op(op) && !spec010[op] {
			undocumented = append(undocumented, op)
		}
	}
	sort.Strings(undocumented)
	for _, op := range undocumented {
		t.Errorf("010 drift: %q is served by the router but not in 010 openapi.yaml", op)
	}
}
