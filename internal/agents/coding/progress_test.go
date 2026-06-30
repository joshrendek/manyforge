package coding

import (
	"encoding/json"
	"strings"
	"testing"
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
