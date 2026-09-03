package automations

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWriteInvalidGraph(t *testing.T) {
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/activate", nil)
	write(response, request, http.StatusOK, nil, &InvalidGraphError{Issues: []Issue{{Code: "trigger_count", Message: "graph must contain exactly one trigger"}}})
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", response.Code)
	}
	var body invalidGraphBody
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Code != "AUTOMATION_INVALID" || len(body.Issues) != 1 || body.Issues[0].Code != "trigger_count" {
		t.Fatalf("body = %+v", body)
	}
}
