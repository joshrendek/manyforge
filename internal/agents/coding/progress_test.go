package coding

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestProgressSnapshotShape(t *testing.T) {
	p := &Progress{}
	p.SetPhase("reviewing")
	p.UpdateStream(42, "hello world")
	b := p.Snapshot()
	if b == nil {
		t.Fatal("Snapshot nil after SetPhase")
	}
	var s progressSnapshot
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if s.Phase != "reviewing" || s.Tokens != 42 || s.Preview != "hello world" {
		t.Fatalf("snapshot=%+v", s)
	}
}

func TestProgressSnapshotNilUntilPhase(t *testing.T) {
	p := &Progress{}
	p.UpdateStream(5, "partial")
	if p.Snapshot() != nil {
		t.Fatal("Snapshot must be nil before any phase is set (so pre-heartbeat renew leaves progress NULL)")
	}
	p.SetPhase("preparing")
	if p.Snapshot() == nil {
		t.Fatal("Snapshot must be non-nil after a phase is set")
	}
}

func TestProgressSnapshotRedactsSecret(t *testing.T) {
	secret := "sk-abcdefghijklmnopqrstuvwxyz0123456789"
	p := &Progress{}
	p.SetPhase("reviewing")
	p.SetSecrets(secret)
	p.UpdateStream(1, "the model echoed "+secret+" oops")
	var s progressSnapshot
	_ = json.Unmarshal(p.Snapshot(), &s)
	if strings.Contains(s.Preview, secret) {
		t.Fatalf("secret leaked into preview: %q", s.Preview)
	}
	if !strings.Contains(s.Preview, redactedMarker) {
		t.Fatalf("preview not redacted: %q", s.Preview)
	}
}

func TestProgressPreviewTailCapped(t *testing.T) {
	p := &Progress{}
	p.SetPhase("reviewing")
	p.UpdateStream(1, strings.Repeat("x", previewMaxBytes*3))
	var s progressSnapshot
	_ = json.Unmarshal(p.Snapshot(), &s)
	if len(s.Preview) > previewMaxBytes {
		t.Fatalf("preview not tail-capped: len=%d max=%d", len(s.Preview), previewMaxBytes)
	}
}

func TestProgressNilReceiverIsNoOp(t *testing.T) {
	var p *Progress // nil
	p.SetPhase("x")
	p.SetSecrets("y")
	p.UpdateStream(1, "z")
	if p.Snapshot() != nil {
		t.Fatal("nil *Progress Snapshot must be nil")
	}
}

func TestProgressSnapshotRedactsSecretStraddlingCap(t *testing.T) {
	// A known secret that does NOT match any redactSecrets regex pattern (no "sk-"/
	// "ghp_"/"bearer" shape), so only the exact known-value match can scrub it — the
	// path that fails on a fragment. Positioned to straddle the previewMaxBytes tail cut.
	secret := "SECRETvalue1234567890abcdefghijABCDEFGH" // 40 bytes
	prefix := strings.Repeat("a", 100)
	suffix := strings.Repeat("b", previewMaxBytes-20) // cut lands mid-secret (at offset 120 of a 140-byte head)
	raw := prefix + secret + suffix
	p := &Progress{}
	p.SetPhase("reviewing")
	p.SetSecrets(secret)
	p.UpdateStream(1, raw)
	var s progressSnapshot
	_ = json.Unmarshal(p.Snapshot(), &s)
	if strings.Contains(s.Preview, secret[20:]) { // a 20-char tail fragment of the secret
		t.Fatalf("secret fragment leaked across the tail-cap boundary: preview tail=%q", s.Preview[max(0, len(s.Preview)-60):])
	}
}

func TestTailBytesUTF8BoundaryMultibyte(t *testing.T) {
	// Cut lands mid-rune (inside a 4-byte emoji); tailBytes must drop the partial
	// leading rune and return valid UTF-8 within the cap.
	s := strings.Repeat("a", 10) + "\U0001F600" + strings.Repeat("b", previewMaxBytes-2)
	out := tailBytes(s, previewMaxBytes)
	if !utf8.ValidString(out) {
		t.Fatalf("tailBytes returned invalid UTF-8: %q", out[:min(20, len(out))])
	}
	if len(out) > previewMaxBytes {
		t.Fatalf("tailBytes exceeded cap: %d > %d", len(out), previewMaxBytes)
	}
}
