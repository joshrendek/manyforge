package agents

import (
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/manyforge/manyforge/internal/platform/errs"
)

// runKeyset is the (timestamp, id) tuple that anchors a keyset page boundary. It is
// the last row of the current page; the next page selects rows strictly past it.
type runKeyset struct {
	ts time.Time
	id uuid.UUID
}

// cursorRuns binds a cursor to the runs-list endpoint. A second agents list endpoint
// adds its own kind and reuses the encode/decode core below instead of copying the
// codec (mirrors internal/ticketing/cursor.go).
const cursorRuns = "run"

// runCursorSep is absent from RFC3339 timestamps ('-', ':', 'T', '.', '+', 'Z') and
// UUIDs (hex + '-'), so the three encoded fields parse unambiguously.
const runCursorSep = "|"

// runCursorPage1 is the page-1 keyset sentinel: a tuple strictly greater than any real
// row under (created_at, id) DESC, so an empty cursor starts the page at the newest run.
var runCursorPage1 = runKeyset{
	ts: time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC),
	id: uuid.MustParse("ffffffff-ffff-ffff-ffff-ffffffffffff"),
}

// encodeCursor renders an opaque base64url("<kind>|<rfc3339nano>|<uuid>") token. The
// kind prefix binds the cursor to its endpoint so it cannot be replayed elsewhere.
func encodeCursor(kind string, k runKeyset) string {
	raw := kind + runCursorSep + k.ts.UTC().Format(time.RFC3339Nano) + runCursorSep + k.id.String()
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// decodeCursor is the defensive inverse of encodeCursor: a malformed token (bad
// base64, wrong field count, foreign kind, unparseable ts/uuid) is a validation error
// (→ 400), never a 500 or an injection vector.
func decodeCursor(kind, token string) (runKeyset, error) {
	bad := func() (runKeyset, error) {
		return runKeyset{}, fmt.Errorf("invalid cursor: %w", errs.ErrValidation)
	}
	dec, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return bad()
	}
	parts := strings.SplitN(string(dec), runCursorSep, 3)
	if len(parts) != 3 || parts[0] != kind {
		return bad()
	}
	ts, err := time.Parse(time.RFC3339Nano, parts[1])
	if err != nil {
		return bad()
	}
	id, err := uuid.Parse(parts[2])
	if err != nil {
		return bad()
	}
	return runKeyset{ts: ts, id: id}, nil
}

func encodeRunCursor(k runKeyset) string             { return encodeCursor(cursorRuns, k) }
func decodeRunCursor(token string) (runKeyset, error) { return decodeCursor(cursorRuns, token) }
