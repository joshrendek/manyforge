package feedback

import (
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/errs"
)

// Cursor tokens are opaque to clients: base64url("<kind>|<rfc3339nano>|<uuid>"). Boards and
// posts both sort by (created_at, id) DESC, so the keyset key is created_at formatted as
// RFC3339Nano (mirrors crm's activity cursor). The kind prefix binds a cursor to its endpoint
// so a boards cursor cannot be replayed against the posts endpoint. Decoding is defensive — a
// malformed token is a validation error (→ 400), never a 500 or an injection vector.
const (
	cursorBoards = "b"
	cursorPosts  = "p"
	sep          = "|"
)

func encodeTimeCursor(kind string, createdAt time.Time, id uuid.UUID) string {
	raw := kind + sep + createdAt.UTC().Format(time.RFC3339Nano) + sep + id.String()
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeTimeCursor(kind, token string) (time.Time, uuid.UUID, error) {
	bad := func() (time.Time, uuid.UUID, error) {
		return time.Time{}, uuid.Nil, fmt.Errorf("invalid cursor: %w", errs.ErrValidation)
	}
	dec, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return bad()
	}
	parts := strings.SplitN(string(dec), sep, 3)
	if len(parts) != 3 || parts[0] != kind {
		return bad()
	}
	createdAt, err := time.Parse(time.RFC3339Nano, parts[1])
	if err != nil {
		return bad()
	}
	id, err := uuid.Parse(parts[2])
	if err != nil {
		return bad()
	}
	return createdAt, id, nil
}
