package auth

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestLoadKeyRing_RoundTrip(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal pkcs8: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})

	ring, configured, err := LoadKeyRing("manyforge", "manyforge-api", "k1", string(pemBytes), "")
	if err != nil {
		t.Fatalf("LoadKeyRing: %v", err)
	}
	if !configured {
		t.Fatal("expected configured=true")
	}
	if ring == nil {
		t.Fatal("expected non-nil ring")
	}

	pid := uuid.New()
	tok, err := ring.Sign(pid, 15*time.Minute, time.Now())
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	got, err := ring.Parse(tok)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got != pid {
		t.Errorf("subject: want %s, got %s", pid, got)
	}
}

func TestLoadKeyRing_Unconfigured(t *testing.T) {
	ring, configured, err := LoadKeyRing("manyforge", "manyforge-api", "k1", "", "")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if configured {
		t.Fatal("expected configured=false")
	}
	if ring != nil {
		t.Fatal("expected nil ring")
	}
}

func TestLoadKeyRing_BadPEM(t *testing.T) {
	_, configured, err := LoadKeyRing("manyforge", "manyforge-api", "k1", "not a pem", "")
	if err == nil {
		t.Fatal("expected error for malformed PEM")
	}
	if configured {
		t.Fatal("expected configured=false on error")
	}
}
