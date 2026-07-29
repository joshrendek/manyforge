//go:build contract

package main

import (
	"os"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestOpenAPIDrift012(t *testing.T) {
	routes := apiRoutes(t)
	spec := spec012Routes(t)
	if len(spec) != 5 {
		t.Fatalf("012 contract operations = %d, want 5", len(spec))
	}
	for operation := range spec {
		if !routes[operation] {
			t.Errorf("012 drift: %q is documented but not served", operation)
		}
	}

	raw, err := os.ReadFile(specPath(
		"specs", "012-tenant-merge", "contracts", "openapi.yaml",
	))
	if err != nil {
		t.Fatalf("read 012 openapi: %v", err)
	}
	var document struct {
		Paths map[string]map[string]struct {
			Responses map[string]yaml.Node `yaml:"responses"`
		} `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &document); err != nil {
		t.Fatalf("parse 012 openapi: %v", err)
	}

	confirm, ok := document.Paths["/tenant-merges/{operationId}/confirm"]["post"]
	if !ok {
		t.Fatal("012 openapi omits confirmation endpoint")
	}
	for _, status := range []string{"200", "400", "401", "404", "409", "412", "429", "503"} {
		if _, ok := confirm.Responses[status]; !ok {
			t.Errorf("confirmation endpoint omits response %s", status)
		}
	}
	create, ok := document.Paths["/businesses/{id}/tenant-merges"]["post"]
	if !ok {
		t.Fatal("012 openapi omits create/preflight endpoint")
	}
	for _, status := range []string{"201", "400", "404", "409", "429", "503"} {
		if _, ok := create.Responses[status]; !ok {
			t.Errorf("create/preflight endpoint omits response %s", status)
		}
	}
}
