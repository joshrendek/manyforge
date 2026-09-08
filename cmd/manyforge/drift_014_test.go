//go:build contract

package main

import "testing"

func TestAutomationActivationContract(t *testing.T) {
	doc := loadCanonicalSpec(t)
	const path = "/api/v1/businesses/{id}/mailing/automations/{aid}/versions/{vid}/activate"
	op, ok := doc.Paths[path]["post"]
	if !ok {
		t.Fatalf("canonical OpenAPI omits POST %s", path)
	}
	responses := responseCodesFor(t, op, "POST "+path)
	if _, ok := responses["422"]; !ok {
		t.Error("activate endpoint must document AUTOMATION_INVALID as 422")
	}
}
