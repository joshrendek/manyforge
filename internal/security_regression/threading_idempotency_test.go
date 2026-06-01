// Finding MF-002-THREAD-IDEMPOTENCY (doc_support.go): a replayed Message-ID never
// double-creates a ticket/message, no inbound reply ever attaches to the wrong
// ticket (header-threaded by In-Reply-To/References, never mis-threaded), and a
// forged reply token is rejected by the constant-time HMAC verifier. The replay +
// threading behavioral assertions are //go:build integration (real Postgres via
// the inbox ingestion Service); the forged-token assertion pins the existing
// constant-time verifier and runs without infra.

package security_regression

import (
	"crypto/rand"
	"testing"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/ticketing"
)

// TestForgedReplyTokenRejected (MF-002-THREAD-IDEMPOTENCY, no infra) pins the
// constant-time reply-token verifier: a real token verifies, but a tampered,
// truncated, or garbage token presented under the same server key never does, so
// a requester cannot inject a reply into another ticket by guessing the token.
func TestForgedReplyTokenRejected(t *testing.T) {
	serverKey := make([]byte, 32)
	if _, err := rand.Read(serverKey); err != nil {
		t.Fatalf("gen server key: %v", err)
	}
	tid := uuid.New()
	good := ticketing.SignReplyToken(tid, serverKey)

	// Control: the legitimately-signed token verifies to its ticket id.
	if got, ok := ticketing.VerifyReplyToken(good, serverKey); !ok || got != tid {
		t.Fatalf("control: legitimate token must verify; got (%s,%v) want (%s,true)", got, ok, tid)
	}

	// A token signed with a DIFFERENT key must not verify under serverKey.
	otherKey := make([]byte, 32)
	if _, err := rand.Read(otherKey); err != nil {
		t.Fatalf("gen other key: %v", err)
	}
	forged := ticketing.SignReplyToken(tid, otherKey)

	forgeries := map[string]string{
		"foreign-key-signature": forged,
		"flipped-last-byte":     good[:len(good)-1] + flip(good[len(good)-1:]),
		"truncated":             good[:len(good)-4],
		"garbage":               "not-a-token",
		"empty":                 "",
		"dot-only":              ".",
		"swapped-id":            ticketing.SignReplyToken(uuid.New(), serverKey)[:8] + good[8:],
	}
	for name, tok := range forgeries {
		if _, ok := ticketing.VerifyReplyToken(tok, serverKey); ok {
			t.Errorf("forged token %q verified — reply-token forgery is possible", name)
		}
	}
}

// flip returns s with the first byte changed to a different base64url char.
func flip(s string) string {
	if s == "" {
		return "A"
	}
	b := []byte(s)
	if b[0] == 'A' {
		b[0] = 'B'
	} else {
		b[0] = 'A'
	}
	return string(b)
}
