// Finding MF-002-THREAD-IDEMPOTENCY (doc_support.go): a replayed Message-ID never
// double-creates a ticket/message, no inbound reply ever attaches to the wrong
// ticket (header-threaded by In-Reply-To/References, never mis-threaded), and a
// forged reply token is rejected by the constant-time HMAC verifier. The replay +
// threading behavioral assertions are //go:build integration (real Postgres via
// the inbox ingestion Service); the forged-token assertion pins the existing
// constant-time verifier and runs without infra.

package security_regression

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"strings"
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

// TestVerifyReplyTokenNonCanonicalEncoding (MF-002-THREAD-IDEMPOTENCY, no infra)
// DETERMINISTICALLY pins the base64-malleability defense in VerifyReplyToken. Go's
// RawURLEncoding decoder is non-strict: it tolerates non-zero trailing bits in a
// segment's final char, so a non-canonical re-encoding decodes to the SAME id/sig
// bytes and the HMAC would still match — making the token malleable. The verifier
// defends by re-encoding the decoded bytes and rejecting any segment not already in
// canonical form. The randomized "flipped-last-byte" case in TestForgedReplyToken
// only lands on a trailing-bit char ~6.25% of runs; here we FIX the key+uuid and
// construct a guaranteed non-canonical variant of EACH segment, so the defense is
// exercised on every run, on both the id and the sig segment.
func TestVerifyReplyTokenNonCanonicalEncoding(t *testing.T) {
	// Fixed key + uuid ⇒ fully deterministic (no rand): the constructed variants and
	// the verdict are identical on every run.
	serverKey := make([]byte, 32)
	for i := range serverKey {
		serverKey[i] = byte(i + 1)
	}
	tid := uuid.MustParse("00112233-4455-6677-8899-aabbccddeeff")
	good := ticketing.SignReplyToken(tid, serverKey)

	// Control: the canonical token verifies to its ticket id.
	if got, ok := ticketing.VerifyReplyToken(good, serverKey); !ok || got != tid {
		t.Fatalf("control: canonical token must verify; got (%s,%v) want (%s,true)", got, ok, tid)
	}

	dot := strings.IndexByte(good, '.')
	if dot <= 0 {
		t.Fatalf("control token has no segment separator: %q", good)
	}
	idPart, sigPart := good[:dot], good[dot+1:]

	ncID, ok := nonCanonicalVariant(idPart)
	if !ok {
		t.Fatalf("could not build a non-canonical id segment from %q", idPart)
	}
	ncSig, ok := nonCanonicalVariant(sigPart)
	if !ok {
		t.Fatalf("could not build a non-canonical sig segment from %q", sigPart)
	}

	// Test-bug guard: each variant must be a DIFFERENT string that decodes to the
	// SAME bytes as the canonical segment (i.e. genuinely non-canonical, not garbage).
	for _, p := range []struct{ name, orig, nc string }{
		{"id", idPart, ncID}, {"sig", sigPart, ncSig},
	} {
		if p.nc == p.orig {
			t.Fatalf("%s variant equals the original %q (no spare trailing bits?)", p.name, p.orig)
		}
		a, _ := base64.RawURLEncoding.DecodeString(p.orig)
		b, _ := base64.RawURLEncoding.DecodeString(p.nc)
		if !bytes.Equal(a, b) {
			t.Fatalf("%s variant %q decodes differently from %q (test bug)", p.name, p.nc, p.orig)
		}
	}

	// The defense must reject BOTH non-canonical forms, even though each decodes to
	// the exact id/sig bytes the valid token carries.
	malleable := map[string]string{
		"non-canonical id segment":  ncID + "." + sigPart,
		"non-canonical sig segment": idPart + "." + ncSig,
	}
	for name, tok := range malleable {
		if _, ok := ticketing.VerifyReplyToken(tok, serverKey); ok {
			t.Errorf("%s verified — reply token is malleable via base64 trailing bits", name)
		}
	}
}

// nonCanonicalVariant returns a string that base64url-decodes (non-strict) to the
// SAME bytes as seg but is NOT seg, by replacing seg's final character with an
// alphabet char whose extra trailing bits differ. ok=false if no such char exists
// (only when the segment's final char carries no spare bits — not the case for the
// 16-byte id or 32-byte HMAC segments here).
func nonCanonicalVariant(seg string) (string, bool) {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	want, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil || seg == "" {
		return "", false
	}
	last := seg[len(seg)-1]
	for i := 0; i < len(alphabet); i++ {
		alt := alphabet[i]
		if alt == last {
			continue
		}
		cand := seg[:len(seg)-1] + string(alt)
		if got, err := base64.RawURLEncoding.DecodeString(cand); err == nil && bytes.Equal(got, want) {
			return cand, true
		}
	}
	return "", false
}
