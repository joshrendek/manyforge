//go:build contract

package main

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestAgentEndpointContract pins the response-code shape for the US2 agent endpoints
// in the 003 contract — a pure spec-file assertion (no DB, no router).
func TestAgentEndpointContract(t *testing.T) {
	doc := loadCanonicalSpec(t)
	codesFor := func(path, verb string) map[string]yaml.Node {
		node, ok := doc.Paths[path][verb]
		if !ok {
			t.Fatalf("003 openapi: missing %s %s", strings.ToUpper(verb), path)
		}
		var op struct {
			Responses map[string]yaml.Node `yaml:"responses"`
		}
		if err := node.Decode(&op); err != nil {
			t.Fatalf("decode %s %s: %v", verb, path, err)
		}
		return op.Responses
	}
	want := map[string]map[string][]string{
		"/api/v1/businesses/{id}/agents": {
			"get":  {"200", "404"},
			"post": {"201", "400", "404", "409"},
		},
		"/api/v1/businesses/{id}/agents/{agentID}": {
			"get":    {"200", "404"},
			"patch":  {"200", "400", "404", "409"},
			"delete": {"204", "404"},
		},
		"/api/v1/businesses/{id}/mcp_servers": {
			"get":  {"200", "404"},
			"post": {"201", "400", "404", "409"},
		},
		"/api/v1/businesses/{id}/mcp_servers/{serverID}": {
			"get":    {"200", "404"},
			"patch":  {"200", "400", "404", "409"},
			"delete": {"204", "404"},
		},
	}
	for path, verbs := range want {
		for verb, codes := range verbs {
			got := codesFor(path, verb)
			for _, code := range codes {
				if _, ok := got[code]; !ok {
					t.Errorf("003 openapi: %s %s must document response %s", strings.ToUpper(verb), path, code)
				}
			}
		}
	}
}
