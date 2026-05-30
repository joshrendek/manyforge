package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/manyforge/manyforge/internal/platform/errs"
)

func TestWriteErrorMapping(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"not found", errs.ErrNotFound, 404, "NOT_FOUND"},
		{"forbidden collapses to 404", fmt.Errorf("nope: %w", errs.ErrForbidden), 404, "NOT_FOUND"},
		{"validation", fmt.Errorf("bad email: %w", errs.ErrValidation), 400, "VALIDATION"},
		{"conflict", errs.ErrConflict, 409, "CONFLICT"},
		{"unknown -> 500", errors.New("boom"), 500, "INTERNAL"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest("GET", "/x", nil)
			WriteError(rec, req, c.err)
			if rec.Code != c.status {
				t.Errorf("status: want %d, got %d", c.status, rec.Code)
			}
			var body ErrorBody
			if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if body.Code != c.code {
				t.Errorf("code: want %s, got %s", c.code, body.Code)
			}
		})
	}
}
