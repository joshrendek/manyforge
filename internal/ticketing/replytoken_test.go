package ticketing

import (
	"encoding/base64"
	"testing"

	"github.com/google/uuid"
)

func TestReplyTokenRoundTrip(t *testing.T) {
	key := []byte("server-key-for-test")
	tid := uuid.New()

	got, ok := VerifyReplyToken(SignReplyToken(tid, key), key)
	if !ok {
		t.Fatalf("valid token failed to verify")
	}
	if got != tid {
		t.Fatalf("recovered ticket id = %s, want %s", got, tid)
	}
}

func TestReplyTokenRejectsForgeryAndTampering(t *testing.T) {
	key := []byte("server-key-for-test")
	tid := uuid.New()
	tok := SignReplyToken(tid, key)

	cases := map[string]string{
		"empty":            "",
		"no-dot":           "abcdef",
		"trailing-dot":     "abc.",
		"leading-dot":      ".abc",
		"bad-base64":       "@@@.@@@",
		"wrong-key-sig":    SignReplyToken(tid, []byte("different-key")),
		"flipped-sig-byte": flipLastChar(tok),
	}
	for name, bad := range cases {
		if _, ok := VerifyReplyToken(bad, key); ok {
			t.Errorf("%s: token %q verified but should have been rejected", name, bad)
		}
	}
}

func flipLastChar(s string) string {
	if s == "" {
		return s
	}
	b := []byte(s)
	if b[len(b)-1] == 'A' {
		b[len(b)-1] = 'B'
	} else {
		b[len(b)-1] = 'A'
	}
	return string(b)
}

// Resend rejects a Reply-To whose local part exceeds RFC 5321's 64 octets
// (422 validation_error), so "support+<token>" must fit.
func TestReplyTokenFitsLocalPart(t *testing.T) {
	tok := SignReplyToken(uuid.New(), []byte("server-key-for-test"))
	if len(tok) != 45 {
		t.Fatalf("token length = %d, want 45", len(tok))
	}
	if local := "support+" + tok; len(local) > 64 {
		t.Fatalf("local part %q is %d octets, over the 64-octet limit", local, len(local))
	}
}

// Tokens minted before the signature was truncated carry the full HMAC; replies
// to mail already sent with them must still thread.
func TestReplyTokenAcceptsLegacyFullSignature(t *testing.T) {
	key := []byte("server-key-for-test")
	tid := uuid.New()
	enc := base64.RawURLEncoding
	legacy := enc.EncodeToString(tid[:]) + "." + enc.EncodeToString(hmacSum(key, tid[:]))

	got, ok := VerifyReplyToken(legacy, key)
	if !ok || got != tid {
		t.Fatalf("legacy token: got (%s, %v), want (%s, true)", got, ok, tid)
	}
	if _, ok := VerifyReplyToken(legacy, []byte("different-key")); ok {
		t.Fatalf("legacy token verified under the wrong key")
	}
}

func TestReplyTokenRejectsOtherSignatureLengths(t *testing.T) {
	key := []byte("server-key-for-test")
	tid := uuid.New()
	enc := base64.RawURLEncoding
	full := hmacSum(key, tid[:])
	for _, n := range []int{0, 1, 8, 15, 17, 20, 31} {
		tok := enc.EncodeToString(tid[:]) + "." + enc.EncodeToString(full[:n])
		if _, ok := VerifyReplyToken(tok, key); ok {
			t.Errorf("%d-byte signature prefix verified; only 16 and 32 are valid", n)
		}
	}
}
