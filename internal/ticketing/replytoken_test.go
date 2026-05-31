package ticketing

import (
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
