//go:build contract

package main

import (
	"os"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
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

// load006Spec reads and parses the 006 openapi.yaml into a raw document, returning it for
// further inspection by the schema-pin tests below. Mirrors load002Spec (drift_002_test.go).
func load006Spec(t *testing.T) struct {
	Paths      map[string]map[string]yaml.Node `yaml:"paths"`
	Components struct {
		Schemas map[string]yaml.Node `yaml:"schemas"`
	} `yaml:"components"`
} {
	t.Helper()
	raw, err := os.ReadFile(specPath("specs", "006-feedback-boards", "contracts", "openapi.yaml"))
	if err != nil {
		t.Fatalf("read 006 openapi: %v", err)
	}
	var doc struct {
		Paths      map[string]map[string]yaml.Node `yaml:"paths"`
		Components struct {
			Schemas map[string]yaml.Node `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse 006 openapi: %v", err)
	}
	return doc
}

// paramRefNode is the minimal shape needed to identify a parameter entry in an operation's
// `parameters` sequence, whether declared inline (name) or via $ref to
// components/parameters/<Name>.
type paramRefNode struct {
	Ref  string `yaml:"$ref"`
	Name string `yaml:"name"`
}

// hasParamRef reports whether an operation's decoded parameters sequence references the named
// components/parameters entry (by $ref) or declares it inline (by name).
func hasParamRef(t *testing.T, params []yaml.Node, want string) bool {
	t.Helper()
	for _, n := range params {
		var p paramRefNode
		if err := n.Decode(&p); err != nil {
			t.Fatalf("decode parameter node: %v", err)
		}
		if p.Ref == "#/components/parameters/"+want || p.Name == want {
			return true
		}
	}
	return false
}

// operationParams decodes just the `parameters` sequence from a raw operation node.
func operationParams(t *testing.T, opNode yaml.Node, label string) []yaml.Node {
	t.Helper()
	var op struct {
		Parameters []yaml.Node `yaml:"parameters"`
	}
	if err := opNode.Decode(&op); err != nil {
		t.Fatalf("decode %s: %v", label, err)
	}
	return op.Parameters
}

// schemaProperties decodes a schema node's top-level `properties` map.
func schemaProperties(t *testing.T, node yaml.Node, label string) map[string]yaml.Node {
	t.Helper()
	var schema struct {
		Properties map[string]yaml.Node `yaml:"properties"`
	}
	if err := node.Decode(&schema); err != nil {
		t.Fatalf("decode %s schema: %v", label, err)
	}
	return schema.Properties
}

// TestFeedbackVerifiedIdentitySubmitContract pins the saz.5 verified-identity additions to the
// public submit endpoint (POST /feedback/public/{key}/posts): the optional X-Feedback-Signature
// header, the idempotency_key request field, the identity_verified/deduped response fields, and
// the 200 (dedup replay) / 409 (idempotency conflict) response codes.
func TestFeedbackVerifiedIdentitySubmitContract(t *testing.T) {
	doc := load006Spec(t)
	opNode, ok := doc.Paths["/feedback/public/{key}/posts"]["post"]
	if !ok {
		t.Fatalf("006 openapi: missing POST /feedback/public/{key}/posts")
	}

	t.Run("optional X-Feedback-Signature header", func(t *testing.T) {
		params := operationParams(t, opNode, "POST /feedback/public/{key}/posts")
		if !hasParamRef(t, params, "FeedbackSignature") {
			t.Errorf("006 openapi: POST /feedback/public/{key}/posts must document the X-Feedback-Signature header")
		}
	})

	t.Run("response codes", func(t *testing.T) {
		codes := responseCodesFor(t, opNode, "POST /feedback/public/{key}/posts")
		for _, code := range []string{"200", "201", "400", "401", "409", "413"} {
			if _, ok := codes[code]; !ok {
				t.Errorf("006 openapi: POST /feedback/public/{key}/posts must document response %s", code)
			}
		}
	})

	t.Run("PublicSubmit.idempotency_key", func(t *testing.T) {
		schemaNode, ok := doc.Components.Schemas["PublicSubmit"]
		if !ok {
			t.Fatalf("006 openapi: components/schemas/PublicSubmit not found")
		}
		props := schemaProperties(t, schemaNode, "PublicSubmit")
		if _, ok := props["idempotency_key"]; !ok {
			t.Errorf("006 openapi: PublicSubmit must document idempotency_key property")
		}
	})

	t.Run("PublicSubmitResult.identity_verified and .deduped", func(t *testing.T) {
		schemaNode, ok := doc.Components.Schemas["PublicSubmitResult"]
		if !ok {
			t.Fatalf("006 openapi: components/schemas/PublicSubmitResult not found")
		}
		props := schemaProperties(t, schemaNode, "PublicSubmitResult")
		for _, field := range []string{"identity_verified", "deduped"} {
			if _, ok := props[field]; !ok {
				t.Errorf("006 openapi: PublicSubmitResult must document %q property", field)
			}
		}
	})
}

// TestFeedbackVerifiedIdentityVoteContract pins the saz.5 verified-identity additions to the
// public vote endpoint (POST /feedback/public/{key}/posts/{postID}/votes): the optional
// X-Feedback-Signature header and the identity_verified response field.
func TestFeedbackVerifiedIdentityVoteContract(t *testing.T) {
	doc := load006Spec(t)
	opNode, ok := doc.Paths["/feedback/public/{key}/posts/{postID}/votes"]["post"]
	if !ok {
		t.Fatalf("006 openapi: missing POST /feedback/public/{key}/posts/{postID}/votes")
	}

	params := operationParams(t, opNode, "POST /feedback/public/{key}/posts/{postID}/votes")
	if !hasParamRef(t, params, "FeedbackSignature") {
		t.Errorf("006 openapi: POST /feedback/public/{key}/posts/{postID}/votes must document the X-Feedback-Signature header")
	}

	schemaNode, ok := doc.Components.Schemas["VoteResult"]
	if !ok {
		t.Fatalf("006 openapi: components/schemas/VoteResult not found")
	}
	props := schemaProperties(t, schemaNode, "VoteResult")
	if _, ok := props["identity_verified"]; !ok {
		t.Errorf("006 openapi: VoteResult must document identity_verified property")
	}
}

// TestFeedbackVerifiedIdentityListContract pins the saz.5 verified-identity additions to the
// public list endpoint (GET /feedback/public/{key}/posts): the optional voter_identity/author
// query params and the viewer_voted/identity_verified fields on each PublicPost item.
func TestFeedbackVerifiedIdentityListContract(t *testing.T) {
	doc := load006Spec(t)
	opNode, ok := doc.Paths["/feedback/public/{key}/posts"]["get"]
	if !ok {
		t.Fatalf("006 openapi: missing GET /feedback/public/{key}/posts")
	}

	params := operationParams(t, opNode, "GET /feedback/public/{key}/posts")
	for _, want := range []string{"VoterIdentityQuery", "AuthorQuery"} {
		if !hasParamRef(t, params, want) {
			t.Errorf("006 openapi: GET /feedback/public/{key}/posts must document the %s parameter", want)
		}
	}

	schemaNode, ok := doc.Components.Schemas["PublicPost"]
	if !ok {
		t.Fatalf("006 openapi: components/schemas/PublicPost not found")
	}
	props := schemaProperties(t, schemaNode, "PublicPost")
	for _, field := range []string{"viewer_voted", "identity_verified"} {
		if _, ok := props[field]; !ok {
			t.Errorf("006 openapi: PublicPost must document %q property", field)
		}
	}
}

// TestFeedbackIngestKeySecretContract pins the saz.5 write-once secret shape: secret is
// documented ONLY on the create-only IngestKeyCreated schema (mirroring the Go handler's
// createIngestKeyResp DTO split), never on the shared IngestKey schema that list/get/revoke
// responses reference. has_secret is on the shared schema and always required.
func TestFeedbackIngestKeySecretContract(t *testing.T) {
	doc := load006Spec(t)

	t.Run("shared IngestKey has no secret property", func(t *testing.T) {
		schemaNode, ok := doc.Components.Schemas["IngestKey"]
		if !ok {
			t.Fatalf("006 openapi: components/schemas/IngestKey not found")
		}
		var schema struct {
			Required   []string             `yaml:"required"`
			Properties map[string]yaml.Node `yaml:"properties"`
		}
		if err := schemaNode.Decode(&schema); err != nil {
			t.Fatalf("decode IngestKey schema: %v", err)
		}
		if _, ok := schema.Properties["secret"]; ok {
			t.Errorf("006 openapi: shared IngestKey must NOT document secret (list/get/revoke responses reference this schema; secret must live only on IngestKeyCreated)")
		}
		if _, ok := schema.Properties["has_secret"]; !ok {
			t.Errorf("006 openapi: IngestKey must document has_secret property")
		}
		required := map[string]bool{}
		for _, r := range schema.Required {
			required[r] = true
		}
		if !required["has_secret"] {
			t.Errorf("006 openapi: IngestKey.has_secret must be required (always present in the response)")
		}
	})

	t.Run("create-only IngestKeyCreated documents secret", func(t *testing.T) {
		schemaNode, ok := doc.Components.Schemas["IngestKeyCreated"]
		if !ok {
			t.Fatalf("006 openapi: components/schemas/IngestKeyCreated not found")
		}
		var schema struct {
			AllOf []struct {
				Ref        string               `yaml:"$ref"`
				Required   []string             `yaml:"required"`
				Properties map[string]yaml.Node `yaml:"properties"`
			} `yaml:"allOf"`
		}
		if err := schemaNode.Decode(&schema); err != nil {
			t.Fatalf("decode IngestKeyCreated schema: %v", err)
		}
		found := false
		requiredSecret := false
		for _, member := range schema.AllOf {
			if _, ok := member.Properties["secret"]; ok {
				found = true
			}
			for _, r := range member.Required {
				if r == "secret" {
					requiredSecret = true
				}
			}
		}
		if !found {
			t.Errorf("006 openapi: IngestKeyCreated must document secret property")
		}
		if requiredSecret {
			t.Errorf("006 openapi: IngestKeyCreated.secret must NOT be required (absent when no feedback master key is configured)")
		}
	})

	t.Run("create response uses IngestKeyCreated, not bare IngestKey", func(t *testing.T) {
		opNode, ok := doc.Paths["/businesses/{id}/feedback/boards/{bid}/keys"]["post"]
		if !ok {
			t.Fatalf("006 openapi: missing POST /businesses/{id}/feedback/boards/{bid}/keys")
		}
		var op struct {
			Responses map[string]struct {
				Content struct {
					JSON struct {
						Schema struct {
							Ref string `yaml:"$ref"`
						} `yaml:"schema"`
					} `yaml:"application/json"`
				} `yaml:"content"`
			} `yaml:"responses"`
		}
		if err := opNode.Decode(&op); err != nil {
			t.Fatalf("decode create ingest key operation: %v", err)
		}
		resp, ok := op.Responses["201"]
		if !ok {
			t.Fatalf("006 openapi: POST .../keys must document response 201")
		}
		if got := resp.Content.JSON.Schema.Ref; got != "#/components/schemas/IngestKeyCreated" {
			t.Errorf("006 openapi: POST .../keys 201 response must reference IngestKeyCreated, got %q", got)
		}
	})
}
