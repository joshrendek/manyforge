package agents

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/errs"
)

// ---------------------------------------------------------------------------
// validate
// ---------------------------------------------------------------------------

func TestMCPServerValidate_NameRequired(t *testing.T) {
	svc := &MCPServerService{Sealer: newTestSealer(t)}
	err := svc.validate(CreateMCPServerInput{Name: "", URL: "https://mcp.example.com"})
	if err == nil {
		t.Fatal("empty name must be a validation error")
	}
	if !isValidationErr(err) {
		t.Fatalf("expected ErrValidation, got %v", err)
	}
}

func TestMCPServerValidate_URLRequired(t *testing.T) {
	svc := &MCPServerService{Sealer: newTestSealer(t)}
	err := svc.validate(CreateMCPServerInput{Name: "my-mcp", URL: ""})
	if err == nil {
		t.Fatal("empty url must be a validation error")
	}
	if !isValidationErr(err) {
		t.Fatalf("expected ErrValidation, got %v", err)
	}
}

func TestMCPServerValidate_URLSchemeMustBeHTTPOrHTTPS(t *testing.T) {
	svc := &MCPServerService{Sealer: newTestSealer(t)}

	bad := []string{"ftp://mcp.example.com", "ws://mcp.example.com", "file:///tmp/mcp"}
	for _, u := range bad {
		if err := svc.validate(CreateMCPServerInput{Name: "mcp", URL: u}); err == nil {
			t.Errorf("scheme in %q must be rejected", u)
		} else if !isValidationErr(err) {
			t.Errorf("expected ErrValidation for %q, got %v", u, err)
		}
	}

	good := []string{"http://mcp.example.com", "https://mcp.example.com"}
	for _, u := range good {
		if err := svc.validate(CreateMCPServerInput{Name: "mcp", URL: u}); err != nil {
			t.Errorf("valid URL %q unexpectedly rejected: %v", u, err)
		}
	}
}

func TestMCPServerValidate_URLMustParse(t *testing.T) {
	svc := &MCPServerService{Sealer: newTestSealer(t)}
	err := svc.validate(CreateMCPServerInput{Name: "mcp", URL: "://not-a-url"})
	if err == nil {
		t.Fatal("unparseable url must be a validation error")
	}
	if !isValidationErr(err) {
		t.Fatalf("expected ErrValidation, got %v", err)
	}
}

func TestMCPServerValidate_ValidInput(t *testing.T) {
	svc := &MCPServerService{Sealer: newTestSealer(t)}
	if err := svc.validate(CreateMCPServerInput{Name: "my-mcp", URL: "https://mcp.example.com"}); err != nil {
		t.Fatalf("valid input rejected: %v", err)
	}
}

// ---------------------------------------------------------------------------
// sealAuth / resolveAuthHeader round-trip
// ---------------------------------------------------------------------------

func TestMCPServerSealAndResolveAuthRoundTrip(t *testing.T) {
	sealer := newTestSealer(t)
	svc := &MCPServerService{Sealer: sealer}

	token := "tok-super-secret"
	ref, err := svc.sealAuth(token)
	if err != nil {
		t.Fatalf("sealAuth: %v", err)
	}
	if ref == "" || ref == token {
		t.Fatalf("ref must be a sealed, non-plaintext string, got %q", ref)
	}

	// Confirm the blob is JSON-wrapped bearer.
	plain, err := sealer.Open(ref)
	if err != nil {
		t.Fatalf("open sealed ref directly: %v", err)
	}
	var blob struct {
		Scheme string `json:"scheme"`
		Token  string `json:"token"`
	}
	if err := json.Unmarshal(plain, &blob); err != nil {
		t.Fatalf("unmarshal sealed blob: %v", err)
	}
	if blob.Scheme != "bearer" || blob.Token != token {
		t.Fatalf("blob = %+v, want scheme=bearer token=%q", blob, token)
	}

	// resolveAuthHeader should produce "Bearer <token>".
	header, err := svc.resolveAuthHeader(&ref)
	if err != nil {
		t.Fatalf("resolveAuthHeader: %v", err)
	}
	want := "Bearer " + token
	if header != want {
		t.Fatalf("header = %q, want %q", header, want)
	}
}

func TestMCPServerResolveAuthHeader_NilRef(t *testing.T) {
	svc := &MCPServerService{Sealer: newTestSealer(t)}
	header, err := svc.resolveAuthHeader(nil)
	if err != nil {
		t.Fatalf("resolveAuthHeader(nil): %v", err)
	}
	if header != "" {
		t.Fatalf("nil ref must produce empty header, got %q", header)
	}
}

func TestMCPServerResolveAuthHeader_EmptyRef(t *testing.T) {
	svc := &MCPServerService{Sealer: newTestSealer(t)}
	empty := ""
	header, err := svc.resolveAuthHeader(&empty)
	if err != nil {
		t.Fatalf("resolveAuthHeader(empty): %v", err)
	}
	if header != "" {
		t.Fatalf("empty ref must produce empty header, got %q", header)
	}
}

func TestMCPServerSealAuth_NilSealer(t *testing.T) {
	svc := &MCPServerService{Sealer: nil}
	_, err := svc.sealAuth("some-token")
	if err == nil {
		t.Fatal("nil sealer with non-empty token must return an error")
	}
}

func TestMCPServerSealAuth_EmptyTokenNoSealer(t *testing.T) {
	// Empty token with nil sealer is fine — no auth to seal.
	svc := &MCPServerService{Sealer: nil}
	ref, err := svc.sealAuth("")
	if err != nil {
		t.Fatalf("nil sealer + empty token must not error, got %v", err)
	}
	if ref != "" {
		t.Fatalf("empty token must produce empty ref, got %q", ref)
	}
}

// ---------------------------------------------------------------------------
// allPresent (set-membership backing ValidateServerIDs)
// ---------------------------------------------------------------------------

func TestMCPServerAllPresent(t *testing.T) {
	a := uuid.New()
	b := uuid.New()

	cases := []struct {
		name      string
		requested []uuid.UUID
		found     []uuid.UUID
		want      bool
	}{
		{"duplicate requested tolerated", []uuid.UUID{a, a}, []uuid.UUID{a}, true},
		{"missing id rejected", []uuid.UUID{a, b}, []uuid.UUID{a}, false},
		{"empty is present", []uuid.UUID{}, []uuid.UUID{}, true},
		{"all present", []uuid.UUID{a, b}, []uuid.UUID{a, b}, true},
		{"foreign id with nil found", []uuid.UUID{a}, nil, false},
	}
	for _, tc := range cases {
		if got := allPresent(tc.requested, tc.found); got != tc.want {
			t.Errorf("%s: allPresent(%v, %v) = %v, want %v", tc.name, tc.requested, tc.found, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func isValidationErr(err error) bool {
	return errors.Is(err, errs.ErrValidation)
}

// newTestSealer is defined in credential_test.go (same package).
