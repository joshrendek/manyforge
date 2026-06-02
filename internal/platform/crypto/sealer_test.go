package crypto

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"testing"
)

func newKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatalf("rand key: %v", err)
	}
	return k
}

func newSealer(t *testing.T) *Sealer {
	t.Helper()
	s, err := NewSealer(newKey(t))
	if err != nil {
		t.Fatalf("NewSealer: %v", err)
	}
	return s
}

func TestSealOpenRoundTrip(t *testing.T) {
	s := newSealer(t)

	// A 64-byte blob mirrors an ed25519 private key (the real T055 payload).
	_, edPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519: %v", err)
	}
	if len(edPriv) != 64 {
		t.Fatalf("ed25519 priv len = %d, want 64", len(edPriv))
	}

	cases := map[string][]byte{
		"empty":        {},
		"short":        []byte("x"),
		"text":         []byte("the quick brown fox"),
		"ed25519-priv": edPriv,
	}
	for name, pt := range cases {
		t.Run(name, func(t *testing.T) {
			ref, err := s.Seal(pt)
			if err != nil {
				t.Fatalf("Seal: %v", err)
			}
			got, err := s.Open(ref)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			if !bytes.Equal(got, pt) {
				t.Fatalf("round-trip mismatch: got %x, want %x", got, pt)
			}
		})
	}
}

func TestSealNonceUniqueness(t *testing.T) {
	s := newSealer(t)
	pt := []byte("identical plaintext sealed twice")

	ref1, err := s.Seal(pt)
	if err != nil {
		t.Fatalf("Seal 1: %v", err)
	}
	ref2, err := s.Seal(pt)
	if err != nil {
		t.Fatalf("Seal 2: %v", err)
	}
	if ref1 == ref2 {
		t.Fatal("expected distinct refs for the same plaintext (random nonce reuse)")
	}

	for i, ref := range []string{ref1, ref2} {
		got, err := s.Open(ref)
		if err != nil {
			t.Fatalf("Open ref%d: %v", i+1, err)
		}
		if !bytes.Equal(got, pt) {
			t.Fatalf("Open ref%d mismatch: got %x, want %x", i+1, got, pt)
		}
	}
}

func TestOpenWrongKeyFails(t *testing.T) {
	s1 := newSealer(t)
	s2 := newSealer(t) // different random master key

	ref, err := s1.Seal([]byte("secret material"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	got, err := s2.Open(ref)
	if err == nil {
		t.Fatalf("expected GCM auth failure with wrong key, got plaintext %x", got)
	}
}

func TestOpenTamperedFails(t *testing.T) {
	s := newSealer(t)
	ref, err := s.Seal([]byte("tamper target payload"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	raw, err := base64.StdEncoding.DecodeString(ref)
	if err != nil {
		t.Fatalf("decode ref: %v", err)
	}
	// Flip a byte in the trailing ciphertext (past the nonce) to trip the auth tag.
	raw[len(raw)-1] ^= 0xFF
	tampered := base64.StdEncoding.EncodeToString(raw)

	if _, err := s.Open(tampered); err == nil {
		t.Fatal("expected Open to reject tampered ciphertext")
	}
}

func TestOpenMalformedFails(t *testing.T) {
	s := newSealer(t)

	t.Run("not-base64", func(t *testing.T) {
		if _, err := s.Open("!!! not base64 !!!"); err == nil {
			t.Fatal("expected error on non-base64 ref")
		}
	})

	t.Run("too-short", func(t *testing.T) {
		// Fewer bytes than the GCM nonce size → cannot even split nonce||ciphertext.
		short := base64.StdEncoding.EncodeToString([]byte{0x01, 0x02, 0x03})
		if _, err := s.Open(short); err == nil {
			t.Fatal("expected error on too-short blob")
		}
	})

	t.Run("empty", func(t *testing.T) {
		if _, err := s.Open(""); err == nil {
			t.Fatal("expected error on empty ref")
		}
	})
}

func TestNewSealerRejectsBadKeyLength(t *testing.T) {
	for _, n := range []int{0, 16, 31, 33} {
		key := make([]byte, n)
		if _, err := NewSealer(key); err == nil {
			t.Errorf("NewSealer(len=%d): expected error, got nil", n)
		}
	}
}
