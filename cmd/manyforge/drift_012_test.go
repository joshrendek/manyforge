//go:build contract

package main

import "testing"

func TestTenantMergeEndpointContract(t *testing.T) {
	doc := loadCanonicalSpec(t)
	for _, endpoint := range []struct {
		path     string
		statuses []string
	}{
		{"/api/v1/tenant-merges/{operationId}/confirm", []string{"200", "400", "401", "404", "409", "412", "429", "503"}},
		{"/api/v1/businesses/{id}/tenant-merges", []string{"201", "400", "404", "409", "429", "503"}},
	} {
		op, ok := doc.Paths[endpoint.path]["post"]
		if !ok {
			t.Fatalf("canonical OpenAPI omits POST %s", endpoint.path)
		}
		responses := responseCodesFor(t, op, "POST "+endpoint.path)
		for _, status := range endpoint.statuses {
			if _, ok := responses[status]; !ok {
				t.Errorf("POST %s omits response %s", endpoint.path, status)
			}
		}
	}
}
