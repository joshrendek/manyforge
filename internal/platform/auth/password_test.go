package auth

import (
	"errors"
	"testing"
)

func TestHashVerifyRoundTrip(t *testing.T) {
	h, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if err := VerifyPassword("correct horse battery staple", h); err != nil {
		t.Errorf("verify correct password: %v", err)
	}
	if err := VerifyPassword("wrong", h); !errors.Is(err, ErrPasswordMismatch) {
		t.Errorf("verify wrong password: want ErrPasswordMismatch, got %v", err)
	}
}

func TestVerifyRejectsMalformedHash(t *testing.T) {
	if err := VerifyPassword("x", "not-a-valid-hash"); err == nil {
		t.Error("expected error for malformed hash")
	}
}

func TestDummyVerifyDoesNotPanic(t *testing.T) {
	DummyVerify("anything") // must run a real argon2 pass without panicking
}
