package agents

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestRunCursorRoundTrip(t *testing.T) {
	ts := time.Date(2026, 6, 5, 14, 30, 0, 123456789, time.UTC)
	id := uuid.New()
	tok := encodeRunCursor(runKeyset{ts: ts, id: id})
	got, err := decodeRunCursor(tok)
	if err != nil {
		t.Fatal(err)
	}
	if !got.ts.Equal(ts) || got.id != id {
		t.Fatalf("round-trip mismatch: got %v/%v want %v/%v", got.ts, got.id, ts, id)
	}
	if _, err := decodeRunCursor("not-base64!!"); err == nil {
		t.Fatal("expected error on garbage cursor")
	}
}
