//go:build contract

package main

import (
	"os"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// inScope002Ops is the set of 002 operations whose handlers are implemented in the
// US1 slice (T028 inbound webhook + T031 ticketing/requester read routes). The
// remaining spec-002 operations (POST/PATCH tickets, reply, note, inbox-management)
// are documented ahead of their US2 handlers and are intentionally NOT asserted as
// served yet — add them here as their tasks land.
var inScope002Ops = []string{
	"POST /inbound/email/{}",
	"GET /businesses/{}/tickets",
	"GET /businesses/{}/tickets/{}",
	"GET /businesses/{}/tickets/{}/messages",
	"GET /businesses/{}/requesters",
	"GET /businesses/{}/requesters/{}",
}

// is002Op reports whether a normalized "METHOD /path" operation belongs to the
// 002 surface (inbound ingress or business-nested ticketing/inbox), as opposed to
// the spec-001 foundation routes that share the /businesses prefix.
func is002Op(op string, spec002 map[string]bool) bool {
	return spec002[op] || strings.HasPrefix(op, "POST /inbound/email/")
}

// TestOpenAPIDrift002 (T017) pins the 002 support-desk contract against the FULL
// production router (built via mountAPIRoutes, the same seam main uses):
//
//  1. Presence: every in-scope US1 002 operation (inbound webhook + ticketing read
//     slice) is REGISTERED — no missing handler.
//  2. No drift: every registered route that belongs to the 002 surface is PRESENT
//     in specs/002-support-desk/contracts/openapi.yaml — no undocumented 002 route.
//
// It runs under the `contract` build tag so `make contract-test` enforces it. The
// spec-001 coverage is preserved by TestOpenAPIDrift (untagged, runs everywhere).
func TestOpenAPIDrift002(t *testing.T) {
	routes := apiRoutes(t)
	spec002 := spec002Routes(t)

	// (1) Presence: in-scope 002 operations must be served.
	var missing []string
	for _, op := range inScope002Ops {
		if !spec002[op] {
			t.Errorf("test bug: in-scope op %q is not declared in the 002 openapi.yaml", op)
		}
		if !routes[op] {
			missing = append(missing, op)
		}
	}
	sort.Strings(missing)
	for _, op := range missing {
		t.Errorf("002 drift: %q is in-scope (US1) and in openapi.yaml but not served by the router", op)
	}

	// (2) No drift: any registered route on the 002 surface must be documented in
	// the 002 contract.
	var undocumented []string
	for op := range routes {
		if !is002Op(op, spec002) {
			continue // spec-001 foundation route; covered by TestOpenAPIDrift.
		}
		if !spec002[op] {
			undocumented = append(undocumented, op)
		}
	}
	sort.Strings(undocumented)
	for _, op := range undocumented {
		t.Errorf("002 drift: %q is served by the router but not in 002 openapi.yaml", op)
	}
}

// TestInboundEndpointResponseCodes is a lightweight schema pin (NOT a behavioral
// re-test — the 202/401/413 status behavior is covered by
// internal/inbox/handler_test.go). It asserts the inbound ingress operation in the
// 002 contract documents its required response codes, so a contract edit that drops
// one (e.g. 413) fails CI.
func TestInboundEndpointResponseCodes(t *testing.T) {
	raw, err := os.ReadFile(specPath("specs", "002-support-desk", "contracts", "openapi.yaml"))
	if err != nil {
		t.Fatalf("read 002 openapi: %v", err)
	}
	// Each path maps verb (and the non-verb `parameters` seq) to a raw node; decode
	// only the post operation so the sibling `parameters` sequence does not collide
	// with the operation struct.
	var doc struct {
		Paths map[string]map[string]yaml.Node `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse 002 openapi: %v", err)
	}
	postNode, ok := doc.Paths["/inbound/email/{provider}"]["post"]
	if !ok {
		t.Fatalf("002 openapi: missing POST /inbound/email/{provider}")
	}
	var post struct {
		Responses map[string]yaml.Node `yaml:"responses"`
	}
	if err := postNode.Decode(&post); err != nil {
		t.Fatalf("decode POST /inbound/email/{provider}: %v", err)
	}
	for _, code := range []string{"202", "401", "413"} {
		if _, ok := post.Responses[code]; !ok {
			t.Errorf("002 openapi: POST /inbound/email/{provider} must document response %s", code)
		}
	}
}
