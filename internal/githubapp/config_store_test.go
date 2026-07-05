package githubapp

import (
	"testing"

	"github.com/manyforge/manyforge/internal/platform/crypto"
)

func TestSealRoundTripFields(t *testing.T) {
	s, err := crypto.NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatalf("NewSealer: %v", err)
	}
	for _, pt := range []string{"-----BEGIN KEY-----fake-----END KEY-----", "whsec_fake", ""} {
		ref, err := s.Seal([]byte(pt))
		if err != nil {
			t.Fatalf("Seal(%q): %v", pt, err)
		}
		got, err := s.Open(ref)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if string(got) != pt {
			t.Errorf("round trip = %q, want %q", got, pt)
		}
	}
}
