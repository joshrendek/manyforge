package crm

import (
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/errs"
)

// keyset is the (key, id) tuple that anchors a keyset page boundary — the last row of
// the current page; the next page selects rows strictly past it. Unlike ticketing's
// (timestamp, id) keyset, the CRM sort columns are textual: contact.primary_email and
// company.name. The string key is carried verbatim through the cursor so the *After
// continuation compares the exact (key, id) row-value tuple it minted.
type keyset struct {
	key string
	id  uuid.UUID
}

// Cursor tokens are opaque to clients: base64url("<kind>:<key>:<uuid>"). The kind
// prefix binds a cursor to its endpoint so a contacts cursor cannot be replayed against
// another (e.g. the companies cursor "o" added in a later task). They are NOT raw
// offsets, and decoding is defensive — a malformed token is a validation error (→ 400),
// never a 500 or an injection vector.
const cursorContacts = "c"

// cursorCompanies binds a companies cursor to CompanyService.List. The key is the
// (non-unique) company.name; the trailing UUID disambiguates ties. A distinct kind from
// cursorContacts means a contacts cursor cannot be replayed against the companies endpoint
// (and vice versa) — decodeCursor rejects a kind mismatch as a validation error.
const cursorCompanies = "co"

// cursorActivity binds an activity-timeline cursor to ActivityService.ListForContact.
// Unlike the textual contact/company sort keys, the activity sort is (occurred_at, id)
// DESC, so the keyset key carries occurred_at formatted as RFC3339Nano; the service
// parses it back to a time.Time for the strictly-less-than continuation. A distinct kind
// means a contacts/companies cursor cannot be replayed against this endpoint.
const cursorActivity = "a"

// sep delimits the token's three fields. The textual key may itself contain sep (it is
// a contact.primary_email today, but the helper is general), so decodeCursor recovers
// the trailing UUID via LastIndex(sep) rather than a fixed-arity split — any embedded
// sep stays inside the key. The leading kind is a single sep-free byte.
const sep = "|"

func encodeCursor(kind string, k keyset) string {
	raw := kind + sep + k.key + sep + k.id.String()
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
	// The id (a UUID) never contains sep, so split off the LAST sep to recover it; the
	// remainder before that is "<kind><sep><key>", and the key may itself contain sep.
	s := string(dec)
	lastSep := strings.LastIndex(s, sep)
	if lastSep < 0 {
		return bad()
	}
	head, idStr := s[:lastSep], s[lastSep+len(sep):]
	prefix := kind + sep
	if !strings.HasPrefix(head, prefix) {
		return bad()
	}
	key := head[len(prefix):]
	id, err := uuid.Parse(idStr)
	if err != nil {
		return bad()
	}
	return keyset{key: key, id: id}, nil
}

func encodeContactCursor(k keyset) string              { return encodeCursor(cursorContacts, k) }
func decodeContactCursor(token string) (keyset, error) { return decodeCursor(cursorContacts, token) }

func encodeCompanyCursor(k keyset) string              { return encodeCursor(cursorCompanies, k) }
func decodeCompanyCursor(token string) (keyset, error) { return decodeCursor(cursorCompanies, token) }

// encodeActivityCursor mints an activity cursor from the last row's (occurred_at, id).
// occurred_at is formatted as RFC3339Nano so the keyset's string key round-trips to the
// exact instant the *After continuation compares against.
func encodeActivityCursor(occurredAt time.Time, id uuid.UUID) string {
	return encodeCursor(cursorActivity, keyset{key: occurredAt.UTC().Format(time.RFC3339Nano), id: id})
}

// decodeActivityCursor parses an activity cursor back into (occurred_at, id). A malformed
// token or an unparseable occurred_at is a validation error (→ 400), never a 500.
func decodeActivityCursor(token string) (time.Time, uuid.UUID, error) {
	k, err := decodeCursor(cursorActivity, token)
	if err != nil {
		return time.Time{}, uuid.Nil, err
	}
	occurredAt, perr := time.Parse(time.RFC3339Nano, k.key)
	if perr != nil {
		return time.Time{}, uuid.Nil, fmt.Errorf("invalid cursor: %w", errs.ErrValidation)
	}
	return occurredAt, k.id, nil
}
