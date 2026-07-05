package githubapp

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestStateRoundTripAndTamper(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	now := time.Unix(1_700_000_000, 0)
	p := StatePayload{Purpose: "link", BusinessID: uuid.New(), PrincipalID: uuid.New(), AgentID: uuid.New(),
		Nonce: "n1", Exp: now.Add(5 * time.Minute).Unix()}
	tok := signState(key, p)
	got, err := verifyState(key, tok, now)
	if err != nil {
		t.Fatalf("verifyState: %v", err)
	}
	if got.Purpose != "link" || got.BusinessID != p.BusinessID || got.AgentID != p.AgentID {
		t.Fatalf("mismatch: %+v", got)
	}
	if _, err := verifyState(key, tok[:len(tok)-2]+"xx", now); err == nil {
		t.Error("tampered verified")
	}
	if _, err := verifyState(key, tok, now.Add(10*time.Minute)); err == nil {
		t.Error("expired verified")
	}
	if _, err := verifyState([]byte("different-key-different-key-1234"), tok, now); err == nil {
		t.Error("wrong key verified")
	}
}
