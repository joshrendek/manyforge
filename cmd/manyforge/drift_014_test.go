//go:build contract

package main

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestOpenAPIDrift014(t *testing.T) {
	routes := apiRoutes(t)
	spec := specRoutesFrom(t, specPath("specs", "014-automations", "contracts", "openapi.yaml"))
	if len(spec) != 20 {
		t.Fatalf("014 contract operations = %d, want 20", len(spec))
	}
	for operation := range spec {
		if !routes[operation] {
			t.Errorf("014 drift: %q is documented but not served", operation)
		}
	}
	for operation := range routes {
		if containsAutomationPath(operation) && !spec[operation] {
			t.Errorf("014 drift: %q is served but undocumented", operation)
		}
	}

	raw, err := os.ReadFile(specPath("specs", "014-automations", "contracts", "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Paths map[string]map[string]yaml.Node `yaml:"paths"`
	}
	if err = yaml.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	var activate struct {
		Responses map[string]yaml.Node `yaml:"responses"`
	}
	activateNode := document.Paths["/businesses/{id}/mailing/automations/{aid}/versions/{vid}/activate"]["post"]
	if err = activateNode.Decode(&activate); err != nil {
		t.Fatal(err)
	}
	if _, ok := activate.Responses["422"]; !ok {
		t.Error("activate endpoint must document AUTOMATION_INVALID as 422")
	}
}

func containsAutomationPath(operation string) bool {
	return strings.Contains(operation, "/automations")
}
