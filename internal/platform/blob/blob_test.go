package blob

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestSniffAllowlist(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\n" + "rest of a png file")
	if ct, err := Sniff(png); err != nil || ct != "image/png" {
		t.Fatalf("png: ct=%q err=%v, want image/png, nil", ct, err)
	}
	if ct, err := Sniff([]byte("just some plain text, harmless")); err != nil || ct != "text/plain" {
		t.Fatalf("text: ct=%q err=%v, want text/plain, nil", ct, err)
	}
	// A Windows executable (MZ header) sniffs to application/octet-stream → rejected,
	// regardless of any declared Content-Type a caller might have claimed.
	if _, err := Sniff([]byte("MZ\x90\x00\x03\x00\x00\x00executable bytes")); !errors.Is(err, ErrUnsupportedType) {
		t.Fatalf("exe: err=%v, want ErrUnsupportedType", err)
	}
}

func TestBucketRoundTrip(t *testing.T) {
	ctx := context.Background()
	b, err := Open(ctx, "file://"+t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })

	key := Key(uuid.New(), uuid.New(), uuid.New(), uuid.New())
	want := []byte("hello attachment bytes")
	if err := b.Put(ctx, key, want, "text/plain"); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := b.Get(ctx, key)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("get = %q, want %q", got, want)
	}
	if err := b.Delete(ctx, key); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := b.Get(ctx, key); err == nil {
		t.Fatalf("get after delete: expected error, got nil")
	}
}
