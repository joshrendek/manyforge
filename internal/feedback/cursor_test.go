package feedback

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/errs"
)

// TestTimeCursorRoundTrip asserts a cursor round-trips its (created_at, id) and that a cursor
// minted for one endpoint kind is rejected by another (no cross-endpoint replay), and that a
// malformed token is an ErrValidation (never a 500 / panic).
func TestTimeCursorRoundTrip(t *testing.T) {
	id := uuid.New()
	// Truncate to the RFC3339Nano precision the cursor carries so the round-trip is exact.
	at := time.Now().UTC().Round(0)

	tok := encodeTimeCursor(cursorPosts, at, id)
	gotAt, gotID, err := decodeTimeCursor(cursorPosts, tok)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !gotAt.Equal(at) || gotID != id {
		t.Fatalf("round-trip = (%s,%s), want (%s,%s)", gotAt, gotID, at, id)
	}

	// A posts cursor must not decode under the boards kind.
	if _, _, err := decodeTimeCursor(cursorBoards, tok); !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("cross-kind decode err = %v, want ErrValidation", err)
	}

	for _, bad := range []string{"", "!!!notbase64!!!", "Zm9v"} {
		if _, _, err := decodeTimeCursor(cursorPosts, bad); !errors.Is(err, errs.ErrValidation) {
			t.Fatalf("decode(%q) err = %v, want ErrValidation", bad, err)
		}
	}
}

func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"Feature Requests":  "feature-requests",
		"  Trim  Me  ":      "trim-me",
		"Already-slug":      "already-slug",
		"Special!@#$%Chars": "special-chars",
		"café ünïcode":      "caf-n-code", // every non-[a-z0-9] run (é, space, ü, ï) collapses to one hyphen
		"---":               "",
		"":                  "",
	}
	for in, want := range cases {
		if got := slugify(in); got != want {
			t.Errorf("slugify(%q) = %q, want %q", in, got, want)
		}
	}
}
