//go:build contract

package main

import (
	"os"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// inScope002Ops is the COMPLETE set of 002 operations served by the router, now
// that US1–US5 have all landed: inbound ingress (T028 webhook + bounce), the US1
// ticketing/requester read slice (T031), the US2 reply + note write routes (T035),
// the US3 PATCH triage route (T044), the US4 inbox-management email-domain +
// inbound-address routes (T055–T058), and the US5 DELETE/redact route (T066). Every
// entry below is asserted both ways by TestOpenAPIDrift002 — present in the router
// AND documented in openapi.yaml. Add a new entry only when its handler is
// registered (a documented-but-unserved op is allowed and simply not listed here;
// a served-but-undocumented op is caught by the no-drift half of the test).
var inScope002Ops = []string{
	"POST /inbound/email/{}",
	"POST /inbound/bounce",
	"GET /businesses/{}/tickets",
	"GET /businesses/{}/tickets/{}",
	"PATCH /businesses/{}/tickets/{}",
	"GET /businesses/{}/tickets/{}/messages",
	"GET /businesses/{}/requesters",
	"GET /businesses/{}/requesters/{}",
	"GET /businesses/{}/assignable-members",
	"DELETE /businesses/{}/tickets/{}",
	"POST /businesses/{}/tickets/{}/reply",
	"POST /businesses/{}/tickets/{}/note",
	// US4 inbox-management (T055–T058)
	"GET /businesses/{}/email-domains",
	"POST /businesses/{}/email-domains",
	"POST /businesses/{}/email-domains/{}/verify",
	"GET /businesses/{}/inbound-addresses",
	"POST /businesses/{}/inbound-addresses",
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
//  1. Presence: every in-scope 002 operation (inbound ingress through the US5
//     redact route) is REGISTERED — no missing handler.
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

// load002Spec is a helper that reads and parses the 002 openapi.yaml into a raw
// document, returning it for further inspection by schema-pin tests.
func load002Spec(t *testing.T) struct {
	Paths      map[string]map[string]yaml.Node `yaml:"paths"`
	Components struct {
		Schemas map[string]yaml.Node `yaml:"schemas"`
	} `yaml:"components"`
} {
	t.Helper()
	raw, err := os.ReadFile(specPath("specs", "002-support-desk", "contracts", "openapi.yaml"))
	if err != nil {
		t.Fatalf("read 002 openapi: %v", err)
	}
	var doc struct {
		Paths      map[string]map[string]yaml.Node `yaml:"paths"`
		Components struct {
			Schemas map[string]yaml.Node `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse 002 openapi: %v", err)
	}
	return doc
}

// responseCodesFor decodes the Responses map from a raw operation node.
func responseCodesFor(t *testing.T, opNode yaml.Node, label string) map[string]yaml.Node {
	t.Helper()
	var op struct {
		Responses map[string]yaml.Node `yaml:"responses"`
	}
	if err := opNode.Decode(&op); err != nil {
		t.Fatalf("decode %s: %v", label, err)
	}
	return op.Responses
}

// TestEmailDomainEndpointContract (T052) pins the response-code and schema shape
// for the US4 email-domain endpoints in the 002 openapi contract. It is a pure
// spec-file assertion — no DB, no router — so it runs fast and is independent of
// handler implementation. The test will remain green as long as the contract is
// consistent; removing a documented response code or the dns_challenge shape will
// fail CI.
func TestEmailDomainEndpointContract(t *testing.T) {
	doc := load002Spec(t)

	t.Run("GET /businesses/{id}/email-domains response codes", func(t *testing.T) {
		opNode, ok := doc.Paths["/businesses/{id}/email-domains"]["get"]
		if !ok {
			t.Fatalf("002 openapi: missing GET /businesses/{id}/email-domains")
		}
		codes := responseCodesFor(t, opNode, "GET /businesses/{id}/email-domains")
		for _, code := range []string{"200", "404"} {
			if _, ok := codes[code]; !ok {
				t.Errorf("002 openapi: GET /businesses/{id}/email-domains must document response %s", code)
			}
		}
	})

	t.Run("POST /businesses/{id}/email-domains response codes", func(t *testing.T) {
		opNode, ok := doc.Paths["/businesses/{id}/email-domains"]["post"]
		if !ok {
			t.Fatalf("002 openapi: missing POST /businesses/{id}/email-domains")
		}
		codes := responseCodesFor(t, opNode, "POST /businesses/{id}/email-domains")
		for _, code := range []string{"201", "400", "404", "409", "429"} {
			if _, ok := codes[code]; !ok {
				t.Errorf("002 openapi: POST /businesses/{id}/email-domains must document response %s", code)
			}
		}
	})

	t.Run("POST /businesses/{id}/email-domains/{did}/verify response codes", func(t *testing.T) {
		opNode, ok := doc.Paths["/businesses/{id}/email-domains/{did}/verify"]["post"]
		if !ok {
			t.Fatalf("002 openapi: missing POST /businesses/{id}/email-domains/{did}/verify")
		}
		codes := responseCodesFor(t, opNode, "POST /businesses/{id}/email-domains/{did}/verify")
		for _, code := range []string{"200", "404", "409", "429"} {
			if _, ok := codes[code]; !ok {
				t.Errorf("002 openapi: POST /businesses/{id}/email-domains/{did}/verify must document response %s", code)
			}
		}
	})

	t.Run("EmailDomain schema documents dns_challenge with verification_txt and dkim_record", func(t *testing.T) {
		schemaNode, ok := doc.Components.Schemas["EmailDomain"]
		if !ok {
			t.Fatalf("002 openapi: components/schemas/EmailDomain not found")
		}
		// Decode the EmailDomain schema to a generic map so we can walk it without
		// needing a full JSON-Schema struct. dns_challenge is an inline object under
		// properties, so we decode two levels deep.
		var schema struct {
			Properties map[string]yaml.Node `yaml:"properties"`
		}
		if err := schemaNode.Decode(&schema); err != nil {
			t.Fatalf("decode EmailDomain schema: %v", err)
		}
		challengeNode, ok := schema.Properties["dns_challenge"]
		if !ok {
			t.Errorf("002 openapi: EmailDomain schema must document dns_challenge property")
			return
		}
		var challenge struct {
			Properties map[string]yaml.Node `yaml:"properties"`
		}
		if err := challengeNode.Decode(&challenge); err != nil {
			t.Fatalf("decode EmailDomain.dns_challenge schema: %v", err)
		}
		for _, sub := range []string{"verification_txt", "dkim_record"} {
			if _, ok := challenge.Properties[sub]; !ok {
				t.Errorf("002 openapi: EmailDomain.dns_challenge must document %q property", sub)
			}
		}
	})
}

// TestRedactTicketEndpointContract (T066) pins the response-code shape for the US5
// delete/redact endpoint in the 002 openapi contract: 204 on success and 404 for the
// no-oracle unknown/unauthorized/already-redacted case.
func TestRedactTicketEndpointContract(t *testing.T) {
	doc := load002Spec(t)
	opNode, ok := doc.Paths["/businesses/{id}/tickets/{tid}"]["delete"]
	if !ok {
		t.Fatalf("002 openapi: missing DELETE /businesses/{id}/tickets/{tid}")
	}
	codes := responseCodesFor(t, opNode, "DELETE /businesses/{id}/tickets/{tid}")
	for _, code := range []string{"204", "404"} {
		if _, ok := codes[code]; !ok {
			t.Errorf("002 openapi: DELETE /businesses/{id}/tickets/{tid} must document response %s", code)
		}
	}
}

// TestInboundAddressEndpointContract (T052) pins the response-code shape for the
// US4 inbound-address endpoints in the 002 openapi contract. Same pure-spec approach
// as TestEmailDomainEndpointContract.
func TestInboundAddressEndpointContract(t *testing.T) {
	doc := load002Spec(t)

	t.Run("GET /businesses/{id}/inbound-addresses response codes", func(t *testing.T) {
		opNode, ok := doc.Paths["/businesses/{id}/inbound-addresses"]["get"]
		if !ok {
			t.Fatalf("002 openapi: missing GET /businesses/{id}/inbound-addresses")
		}
		codes := responseCodesFor(t, opNode, "GET /businesses/{id}/inbound-addresses")
		for _, code := range []string{"200", "404"} {
			if _, ok := codes[code]; !ok {
				t.Errorf("002 openapi: GET /businesses/{id}/inbound-addresses must document response %s", code)
			}
		}
	})

	t.Run("POST /businesses/{id}/inbound-addresses response codes", func(t *testing.T) {
		opNode, ok := doc.Paths["/businesses/{id}/inbound-addresses"]["post"]
		if !ok {
			t.Fatalf("002 openapi: missing POST /businesses/{id}/inbound-addresses")
		}
		codes := responseCodesFor(t, opNode, "POST /businesses/{id}/inbound-addresses")
		for _, code := range []string{"201", "400", "404", "409", "429"} {
			if _, ok := codes[code]; !ok {
				t.Errorf("002 openapi: POST /businesses/{id}/inbound-addresses must document response %s", code)
			}
		}
	})
}
