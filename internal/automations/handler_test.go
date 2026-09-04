package automations

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/manyforge/manyforge/internal/platform/errs"
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

func TestWriteNotFoundIsUniform(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/businesses/unknown/mailing/automations/unknown/enrollments/unknown", nil)
	bodies := make([]string, 0, 2)
	for _, err := range []error{fmt.Errorf("unknown enrollment: %w", errs.ErrNotFound), fmt.Errorf("foreign subscriber: %w", errs.ErrNotFound)} {
		response := httptest.NewRecorder()
		write(response, request, http.StatusOK, nil, err)
		if response.Code != http.StatusNotFound {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
		bodies = append(bodies, response.Body.String())
	}
	if bodies[0] != bodies[1] {
		t.Fatalf("not-found responses differ: %q != %q", bodies[0], bodies[1])
	}
}
