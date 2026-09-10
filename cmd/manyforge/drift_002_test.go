//go:build contract

package main

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// TestInboundEndpointResponseCodes is a lightweight schema pin (NOT a behavioral
// re-test — the 202/401/413 status behavior is covered by
// internal/inbox/handler_test.go). It asserts the inbound ingress operation in the
// 002 contract documents its required response codes, so a contract edit that drops
// one (e.g. 413) fails CI.
func TestInboundEndpointResponseCodes(t *testing.T) {
	doc := loadCanonicalSpec(t)
	postNode, ok := doc.Paths["/api/v1/inbound/email/{provider}"]["post"]
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
	doc := loadCanonicalSpec(t)

	t.Run("GET /businesses/{id}/email-domains response codes", func(t *testing.T) {
		opNode, ok := doc.Paths["/api/v1/businesses/{id}/email-domains"]["get"]
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
		opNode, ok := doc.Paths["/api/v1/businesses/{id}/email-domains"]["post"]
		if !ok {
			t.Fatalf("002 openapi: missing POST /businesses/{id}/email-domains")
		}
		codes := responseCodesFor(t, opNode, "POST /businesses/{id}/email-domains")
		for _, code := range []string{"201", "400", "404", "409"} {
			if _, ok := codes[code]; !ok {
				t.Errorf("002 openapi: POST /businesses/{id}/email-domains must document response %s", code)
			}
		}
	})

	t.Run("POST /businesses/{id}/email-domains/{did}/verify response codes", func(t *testing.T) {
		opNode, ok := doc.Paths["/api/v1/businesses/{id}/email-domains/{did}/verify"]["post"]
		if !ok {
			t.Fatalf("002 openapi: missing POST /businesses/{id}/email-domains/{did}/verify")
		}
		codes := responseCodesFor(t, opNode, "POST /businesses/{id}/email-domains/{did}/verify")
		for _, code := range []string{"200", "404"} {
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
		properties, _ := schemaFields(t, doc, schemaNode)
		challengeNode, ok := properties["dns_challenge"]
		if !ok {
			t.Errorf("002 openapi: EmailDomain schema must document dns_challenge property")
			return
		}
		challenge, _ := schemaFields(t, doc, challengeNode)
		for _, sub := range []string{"verification_txt", "dkim_record"} {
			if _, ok := challenge[sub]; !ok {
				t.Errorf("002 openapi: EmailDomain.dns_challenge must document %q property", sub)
			}
		}
	})
}

// TestRedactTicketEndpointContract (T066) pins the response-code shape for the US5
// delete/redact endpoint in the 002 openapi contract: 204 on success and 404 for the
// no-oracle unknown/unauthorized/already-redacted case.
func TestRedactTicketEndpointContract(t *testing.T) {
	doc := loadCanonicalSpec(t)
	opNode, ok := doc.Paths["/api/v1/businesses/{id}/tickets/{tid}"]["delete"]
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
	doc := loadCanonicalSpec(t)

	t.Run("GET /businesses/{id}/inbound-addresses response codes", func(t *testing.T) {
		opNode, ok := doc.Paths["/api/v1/businesses/{id}/inbound-addresses"]["get"]
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
		opNode, ok := doc.Paths["/api/v1/businesses/{id}/inbound-addresses"]["post"]
		if !ok {
			t.Fatalf("002 openapi: missing POST /businesses/{id}/inbound-addresses")
		}
		codes := responseCodesFor(t, opNode, "POST /businesses/{id}/inbound-addresses")
		for _, code := range []string{"201", "400", "404", "409"} {
			if _, ok := codes[code]; !ok {
				t.Errorf("002 openapi: POST /businesses/{id}/inbound-addresses must document response %s", code)
			}
		}
	})
}
