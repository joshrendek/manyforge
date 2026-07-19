package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
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
		{"codex disconnected -> 409", fmt.Errorf("mint: %w", errs.ErrCodexDisconnected), 409, "CODEX_DISCONNECTED"},
		{"upstream -> 502", fmt.Errorf("codex refresh: %w", errs.ErrUpstream), 502, "UPSTREAM"},
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

// manyforge-0yv: DecodeJSON must cap the request body (defense-in-depth) so an
// authenticated caller cannot stream an unbounded body into the JSON API. These
// pin the new 413 behavior alongside the pre-existing happy-path and malformed-body
// (400) contracts so the cap can never silently regress to "decode anything".

func TestDecodeJSONHappyPath(t *testing.T) {
	var out struct {
		Name string `json:"name"`
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/x", strings.NewReader(`{"name":"ada"}`))
	if ok := DecodeJSON(rec, req, &out); !ok {
		t.Fatalf("DecodeJSON: want ok=true for valid body, got false (status %d)", rec.Code)
	}
	if out.Name != "ada" {
		t.Errorf("decoded Name: want %q, got %q", "ada", out.Name)
	}
}

func TestDecodeJSONMalformedReturns400(t *testing.T) {
	var out map[string]any
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/x", strings.NewReader(`{not json`))
	if ok := DecodeJSON(rec, req, &out); ok {
		t.Fatalf("DecodeJSON: want ok=false for malformed body, got true")
	}
	if rec.Code != 400 {
		t.Errorf("status: want 400, got %d", rec.Code)
	}
	var body ErrorBody
	_ = json.NewDecoder(rec.Body).Decode(&body)
	if body.Code != "VALIDATION" {
		t.Errorf("code: want VALIDATION, got %s", body.Code)
	}
}

func TestDecodeJSONOversizedReturns413(t *testing.T) {
	// A syntactically-valid JSON object whose single string value alone exceeds the
	// cap, so the decoder is forced to read past MaxJSONBodyBytes and trips the cap
	// (rather than failing on JSON syntax). Without the cap this would decode fine.
	huge := strings.Repeat("A", int(MaxJSONBodyBytes)+1024)
	var out map[string]any
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/x", strings.NewReader(`{"blob":"`+huge+`"}`))
	if ok := DecodeJSON(rec, req, &out); ok {
		t.Fatalf("DecodeJSON: want ok=false for oversized body, got true")
	}
	if rec.Code != 413 {
		t.Errorf("status: want 413, got %d", rec.Code)
	}
	var body ErrorBody
	_ = json.NewDecoder(rec.Body).Decode(&body)
	if body.Code != "PAYLOAD_TOO_LARGE" {
		t.Errorf("code: want PAYLOAD_TOO_LARGE, got %s", body.Code)
	}
}

func TestDecodeJSONAtCapDecodes(t *testing.T) {
	// A body comfortably under the cap still decodes — the cap must not reject
	// legitimately-sized payloads.
	out := map[string]any{}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/x", strings.NewReader(`{"blob":"`+strings.Repeat("A", 1024)+`"}`))
	if ok := DecodeJSON(rec, req, &out); !ok {
		t.Fatalf("DecodeJSON: want ok=true for under-cap body, got false (status %d)", rec.Code)
	}
}
