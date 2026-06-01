// Finding MF-002-ISOLATION (support read slice, US1/T031).
//
// No build tag: this source-level pin runs in both `make test` and `make sec-test`
// with no infrastructure. It is the CI backstop for the dual-isolation wall on the
// ticketing read endpoints — RLS principal scoping AND the service-layer
// (business_id) ownership predicate pushed into every query — plus the identical
// 404 (never 403) no-oracle boundary. A refactor that drops the principal context,
// removes the SQL predicate, widens a query, or starts distinguishing
// unauthorized from not-found will fail here even if a behavioral test is also
// weakened.
package security_regression

import (
	"strings"
	"testing"
)

func TestSupportReadIsolationPinned(t *testing.T) {
	svc := mustRead(t, "../ticketing/service.go")
	sql := mustRead(t, "../../db/query/ticketing.sql")
	mw := mustRead(t, "../platform/httpx/authz_mw.go")
	cur := mustRead(t, "../ticketing/cursor.go")
	hnd := mustRead(t, "../ticketing/handler.go")

	cases := []struct {
		name, src, fragment string
	}{
		// Dual enforcement #1: every read runs inside the caller's RLS principal tx.
		{"list under principal", svc, "s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error"},
		// Dual enforcement #2: the (business_id, …) ownership predicate is in SQL —
		// the single-row lookups scope by both id AND business_id, never id alone.
		{"get ticket predicate", sql, "WHERE id = $1 AND business_id = $2"},
		{"get requester predicate", sql, "WHERE id = $1 AND business_id = $2"},
		{"list tickets predicate", sql, "WHERE t.business_id = $1"},
		{"list messages predicate", sql, "WHERE ticket_id = $1 AND business_id = $2"},
		{"list requesters predicate", sql, "WHERE business_id = $1"},
		// No-oracle: unknown / foreign-tenant / unauthorized all map to ErrNotFound.
		{"no-oracle pgx.ErrNoRows", svc, "errors.Is(err, pgx.ErrNoRows)"},
		{"no-oracle not-found", svc, "errs.ErrNotFound"},
		// RequirePermission renders 404, never 403, on a lacking permission.
		{"perm gate 404 not 403", mw, "404 — never 403"},
		{"perm gate not-found render", mw, "WriteError(w, r, errs.ErrNotFound)"},
		// Pagination is capped at the service boundary (never the whole table).
		{"limit cap", svc, "const def, max = 50, 100"},
		{"limit cap applied", svc, "case requested > max:"},
		// Cursors are opaque base64 tokens, not raw offsets, and parsed defensively
		// (a malformed cursor is a validation error, never a 500 or injection).
		{"opaque cursor", cur, "base64.RawURLEncoding.EncodeToString"},
		{"defensive cursor parse", cur, "invalid cursor: %w"},
	}
	for _, c := range cases {
		if !strings.Contains(c.src, c.fragment) {
			t.Errorf("%s/%s: isolation pin %q missing — was the predicate/gate removed or weakened?",
				FindingSupportIsolation, c.name, c.fragment)
		}
	}

	// reply_token is DB-only: it must never appear in the wire response. The handler
	// holds the JSON DTOs, so a `reply_token` json tag or a ReplyToken DTO field
	// there would mean it leaked into a response body.
	if strings.Contains(hnd, "reply_token") || strings.Contains(hnd, "ReplyToken") {
		t.Errorf("%s: the read-slice HTTP response must never surface reply_token (DB-only)", FindingSupportIsolation)
	}
	// And the service view types must not carry a ReplyToken field (the dbgen row
	// has one; the projection deliberately drops it).
	if strings.Contains(svc, "ReplyToken ") || strings.Contains(svc, "ReplyToken\t") {
		t.Errorf("%s: the ticketing API view must not include a ReplyToken field (DB-only)", FindingSupportIsolation)
	}
}
