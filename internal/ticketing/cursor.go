package ticketing

import (
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/errs"
)

// keyset is the (timestamp, id) tuple that anchors a keyset page boundary. It is
// the last row of the current page; the next page selects rows strictly past it.
type keyset struct {
	ts time.Time
	id uuid.UUID
}

// Cursor tokens are opaque to clients: base64url("<kind>:<rfc3339nano>:<uuid>").
// The kind prefix binds a cursor to its endpoint so a tickets cursor cannot be
// replayed against the messages endpoint. They are NOT raw offsets (no
// whole-table skip), and decoding is defensive — a malformed token is a
// validation error (→ 400), never a 500 or an injection vector.
const (
	cursorTickets    = "t"
	cursorMessages   = "m"
	cursorRequesters = "r"
)

// sep is a separator absent from both RFC3339 timestamps (which contain ':',
// '-', 'T', '.', '+', 'Z') and UUIDs (hex + '-'), so the three fields parse
// unambiguously.
const sep = "|"

func encodeCursor(kind string, k keyset) string {
	raw := kind + sep + k.ts.UTC().Format(time.RFC3339Nano) + sep + k.id.String()
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeCursor(kind, token string) (keyset, error) {
	bad := func() (keyset, error) {
		return keyset{}, fmt.Errorf("invalid cursor: %w", errs.ErrValidation)
	}
	dec, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return bad()
	}
	parts := strings.SplitN(string(dec), sep, 3)
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
	return keyset{ts: ts, id: id}, nil
}

func encodeTicketCursor(k keyset) string    { return encodeCursor(cursorTickets, k) }
func encodeMessageCursor(k keyset) string   { return encodeCursor(cursorMessages, k) }
func encodeRequesterCursor(k keyset) string { return encodeCursor(cursorRequesters, k) }

func decodeTicketCursor(token string) (keyset, error)  { return decodeCursor(cursorTickets, token) }
func decodeMessageCursor(token string) (keyset, error) { return decodeCursor(cursorMessages, token) }
func decodeRequesterCursor(token string) (keyset, error) {
	return decodeCursor(cursorRequesters, token)
}
