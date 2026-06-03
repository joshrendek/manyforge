//go:build contract

package main

import (
	"os"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// inScope003Ops is the COMPLETE set of spec-003 operations served by the router so
// far (US2 agent-definition CRUD). Each entry is asserted both ways by
// TestOpenAPIDrift003 — present in the router AND documented in the 003 contract.
var inScope003Ops = []string{
	"GET /businesses/{}/agents",
	"POST /businesses/{}/agents",
	"GET /businesses/{}/agents/{}",
	"PATCH /businesses/{}/agents/{}",
	"DELETE /businesses/{}/agents/{}",
}

// is003Op reports whether a normalized "METHOD /path" belongs to the 003 surface
// (the business-nested /agents routes), as opposed to the 001/002 routes that share
// the /businesses prefix.
func is003Op(op string) bool {
	return strings.Contains(op, "/agents")
}

// TestOpenAPIDrift003 pins the spec-003 agent-runtime contract against the FULL
// production router (built via mountAPIRoutes, the same seam main uses):
//  1. Presence: every in-scope 003 operation is REGISTERED.
//  2. No drift: every registered route on the 003 (/agents) surface is documented.
func TestOpenAPIDrift003(t *testing.T) {
	routes := apiRoutes(t)
	spec003 := spec003Routes(t)

	var missing []string
	for _, op := range inScope003Ops {
		if !spec003[op] {
			t.Errorf("test bug: in-scope op %q is not declared in the 003 openapi.yaml", op)
		}
		if !routes[op] {
			missing = append(missing, op)
		}
	}
	sort.Strings(missing)
	for _, op := range missing {
		t.Errorf("003 drift: %q is in-scope (US2) and in openapi.yaml but not served by the router", op)
	}

	var undocumented []string
	for op := range routes {
		if !is003Op(op) {
			continue // 001/002 route; covered by the other drift tests.
		}
		if !spec003[op] {
			undocumented = append(undocumented, op)
		}
	}
	sort.Strings(undocumented)
	for _, op := range undocumented {
		t.Errorf("003 drift: %q is served by the router but not in 003 openapi.yaml", op)
	}
}

// TestAgentEndpointContract pins the response-code shape for the US2 agent endpoints
// in the 003 contract — a pure spec-file assertion (no DB, no router).
func TestAgentEndpointContract(t *testing.T) {
	raw, err := os.ReadFile(specPath("specs", "003-agent-runtime", "contracts", "openapi.yaml"))
	if err != nil {
		t.Fatalf("read 003 openapi: %v", err)
	}
	var doc struct {
		Paths map[string]map[string]yaml.Node `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse 003 openapi: %v", err)
	}
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
		"/businesses/{id}/agents": {
			"get":  {"200", "404"},
			"post": {"201", "400", "404", "409"},
		},
		"/businesses/{id}/agents/{agentID}": {
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
