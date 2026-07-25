package feedback

import (
	"strings"
	"testing"
)

// TestNewSecretShape pins the fbs_ ingest-secret minting shape: a distinct prefix from the
// publishable fbk_ key (so the two are never confused in config) and enough entropy that the
// secret is unguessable.
func TestNewSecretShape(t *testing.T) {
	s, err := newSecret()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(s, "fbs_") {
		t.Fatalf("want fbs_ prefix, got %q", s)
	}
	if len(s) < 20 {
		t.Fatalf("secret too short: %q", s)
	}
}
