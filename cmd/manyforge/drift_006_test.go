//go:build contract

package main

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// schemaFields follows references and composition so assertions describe the wire
// shape rather than whether the contract author used inline properties or allOf.
func schemaFields(t *testing.T, doc canonicalSpec, node yaml.Node) (map[string]yaml.Node, map[string]bool) {
	t.Helper()
	properties := map[string]yaml.Node{}
	required := map[string]bool{}
	seen := map[string]bool{}
	var visit func(yaml.Node)
	visit = func(node yaml.Node) {
		var schema struct {
			Ref        string               `yaml:"$ref"`
			Properties map[string]yaml.Node `yaml:"properties"`
			Required   []string             `yaml:"required"`
			AllOf      []yaml.Node          `yaml:"allOf"`
		}
		if err := node.Decode(&schema); err != nil {
			t.Fatalf("decode schema: %v", err)
		}
		if schema.Ref != "" && !seen[schema.Ref] {
			seen[schema.Ref] = true
			const prefix = "#/components/schemas/"
			if !strings.HasPrefix(schema.Ref, prefix) {
				t.Fatalf("unexpected schema reference %q", schema.Ref)
			}
			target, ok := doc.Components.Schemas[strings.TrimPrefix(schema.Ref, prefix)]
			if !ok {
				t.Fatalf("unresolved schema reference %q", schema.Ref)
			}
			visit(target)
		}
		for _, member := range schema.AllOf {
			visit(member)
		}
		for name, property := range schema.Properties {
			properties[name] = property
		}
		for _, name := range schema.Required {
			required[name] = true
		}
	}
	visit(node)
	return properties, required
}

func namedSchema(t *testing.T, doc canonicalSpec, name string) yaml.Node {
	t.Helper()
	node, ok := doc.Components.Schemas[name]
	if !ok {
		t.Fatalf("canonical OpenAPI omits schema %s", name)
	}
	return node
}

func contractOperation(t *testing.T, doc canonicalSpec, path, method string) yaml.Node {
	t.Helper()
	node, ok := doc.Paths[path][method]
	if !ok {
		t.Fatalf("canonical OpenAPI omits %s %s", method, path)
	}
	return node
}

func requireOptionalParameter(t *testing.T, doc canonicalSpec, operation yaml.Node, name, location string) {
	t.Helper()
	var op struct {
		Parameters []yaml.Node `yaml:"parameters"`
	}
	if err := operation.Decode(&op); err != nil {
		t.Fatal(err)
	}
	for _, node := range op.Parameters {
		var param struct {
			Ref      string `yaml:"$ref"`
			Name     string `yaml:"name"`
			In       string `yaml:"in"`
			Required bool   `yaml:"required"`
		}
		if err := node.Decode(&param); err != nil {
			t.Fatal(err)
		}
		if param.Ref != "" {
			const prefix = "#/components/parameters/"
			if !strings.HasPrefix(param.Ref, prefix) {
				t.Fatalf("unexpected parameter reference %q", param.Ref)
			}
			target, ok := doc.Components.Parameters[strings.TrimPrefix(param.Ref, prefix)]
			if !ok {
				t.Fatalf("unresolved parameter reference %q", param.Ref)
			}
			if err := target.Decode(&param); err != nil {
				t.Fatal(err)
			}
		}
		if param.Name == name && param.In == location {
			if param.Required {
				t.Errorf("%s parameter %s must remain optional", location, name)
			}
			return
		}
	}
	t.Errorf("operation omits optional %s parameter %s", location, name)
}

func requireSchemaProperties(t *testing.T, doc canonicalSpec, name string, fields ...string) {
	t.Helper()
	properties, _ := schemaFields(t, doc, namedSchema(t, doc, name))
	for _, field := range fields {
		if _, ok := properties[field]; !ok {
			t.Errorf("%s omits property %s", name, field)
		}
	}
}

func TestFeedbackVerifiedIdentitySubmitContract(t *testing.T) {
	doc := loadCanonicalSpec(t)
	op := contractOperation(t, doc, "/api/v1/feedback/public/{key}/posts", "post")
	requireOptionalParameter(t, doc, op, "X-Feedback-Signature", "header")
	responses := responseCodesFor(t, op, "public feedback submit")
	for _, code := range []string{"200", "201", "400", "401", "409", "413"} {
		if _, ok := responses[code]; !ok {
			t.Errorf("public feedback submit omits response %s", code)
		}
	}
	requireSchemaProperties(t, doc, "PublicSubmit", "idempotency_key")
	requireSchemaProperties(t, doc, "PublicSubmitResult", "identity_verified", "deduped")
}

func TestFeedbackVerifiedIdentityVoteContract(t *testing.T) {
	doc := loadCanonicalSpec(t)
	op := contractOperation(t, doc, "/api/v1/feedback/public/{key}/posts/{postID}/votes", "post")
	requireOptionalParameter(t, doc, op, "X-Feedback-Signature", "header")
	var vote struct {
		Responses map[string]struct {
			Content map[string]struct {
				Schema yaml.Node `yaml:"schema"`
			} `yaml:"content"`
		} `yaml:"responses"`
	}
	if err := op.Decode(&vote); err != nil {
		t.Fatal(err)
	}
	properties, required := schemaFields(t, doc, vote.Responses["200"].Content["application/json"].Schema)
	if _, ok := properties["identity_verified"]; !ok || !required["identity_verified"] {
		t.Error("public vote response must always expose its signature verification state")
	}
}

func TestFeedbackVerifiedIdentityListContract(t *testing.T) {
	doc := loadCanonicalSpec(t)
	op := contractOperation(t, doc, "/api/v1/feedback/public/{key}/posts", "get")
	for _, name := range []string{"voter_identity", "author"} {
		requireOptionalParameter(t, doc, op, name, "query")
	}
	requireSchemaProperties(t, doc, "PublicPost", "viewer_voted", "identity_verified")
}

func TestFeedbackAuthenticatedPostIdentityVerifiedContract(t *testing.T) {
	doc := loadCanonicalSpec(t)
	properties, required := schemaFields(t, doc, namedSchema(t, doc, "Post"))
	if _, ok := properties["identity_verified"]; !ok || !required["identity_verified"] {
		t.Error("authenticated feedback Post must always include identity_verified")
	}
}

func TestFeedbackIngestKeySecretContract(t *testing.T) {
	doc := loadCanonicalSpec(t)
	properties, required := schemaFields(t, doc, namedSchema(t, doc, "IngestKey"))
	if _, ok := properties["secret"]; ok {
		t.Error("shared IngestKey must not expose its write-once secret")
	}
	if _, ok := properties["has_secret"]; !ok || !required["has_secret"] {
		t.Error("shared IngestKey must always include has_secret")
	}
	properties, required = schemaFields(t, doc, namedSchema(t, doc, "IngestKeyCreated"))
	if _, ok := properties["secret"]; !ok {
		t.Error("created ingest key must document its write-once secret")
	}
	if required["secret"] {
		t.Error("created ingest key secret is absent when the feedback master key is not configured")
	}
	op := contractOperation(t, doc, "/api/v1/businesses/{id}/feedback/boards/{bid}/keys", "post")
	var create struct {
		Responses map[string]struct {
			Content map[string]struct {
				Schema yaml.Node `yaml:"schema"`
			} `yaml:"content"`
		} `yaml:"responses"`
	}
	if err := op.Decode(&create); err != nil {
		t.Fatal(err)
	}
	properties, required = schemaFields(t, doc, create.Responses["201"].Content["application/json"].Schema)
	if _, ok := properties["secret"]; !ok || required["secret"] {
		t.Error("create key 201 response must expose the optional write-once secret")
	}
}
