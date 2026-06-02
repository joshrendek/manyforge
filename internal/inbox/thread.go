package inbox

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/ticketing"
)

// hintTicket derives the threading hint ticket id (p_hint_ticket) from an inbound
// recipient's plus/VERP token (R4 step 2). The token is the unforgeable HMAC reply
// token we minted on a prior outbound (`support+{token}@domain`); resolve.go
// stripped it from the local part. Go owns this step because it needs the server
// HMAC key — the DEFINER function only validates that the returned id belongs to
// the resolved business. A missing, malformed, or forged token verifies to
// (Nil,false) here and yields uuid.Nil, so the function treats it as "no hint" and
// falls through to header threading or a new ticket (never mis-threads, SC-003).
//
// The header match (In-Reply-To/References) is done INSIDE ingest_inbound_message
// (the reads are RLS-sensitive), so it is NOT duplicated here; this function
// contributes only the HMAC-verified plus-token hint. (A [#ref] subject match was
// specced in R4 but is deferred — not built; threading rides headers + this hint.)
func hintTicket(plusToken string, serverKey []byte) uuid.UUID {
	if plusToken == "" {
		return uuid.Nil
	}
	if tid, ok := ticketing.VerifyReplyToken(plusToken, serverKey); ok {
		return tid
	}
	return uuid.Nil
}

// resolveMessageID returns the RFC822 Message-ID to ingest under, synthesizing a
// deterministic one when the inbound message carries no usable header id (research
// R4). The synthetic id is a content hash over stable, tenant-scoped fields so a
// re-delivery of the SAME header-less message collides on the
// ticket_message(tenant_root_id, message_id) unique index and is an idempotent
// no-op (FR-005/SC-002) — while two genuinely distinct header-less messages get
// distinct ids and thread independently.
//
// We do NOT synthesize when a header Message-ID is present: that real id is the
// natural idempotency + threading key. The synthetic form is tagged with an
// @synthetic.manyforge.invalid domain so it can never collide with a real id and
// is obviously machine-minted in diagnostics.
func resolveMessageID(parsed *ParsedEmail, tenantRootID uuid.UUID, sender string) string {
	if id := strings.TrimSpace(parsed.MessageID); id != "" {
		return id
	}
	h := sha256.New()
	// Tenant-scoped: the same bytes in two tenants must not collide (idempotency is
	// per-tenant; the unique index is (tenant_root_id, message_id)). h.Write never
	// errors (hash.Hash contract), so the return is intentionally not checked.
	bh := sha256.Sum256([]byte(parsed.TextBody + "\x00" + parsed.HTMLBody))
	stable := strings.Join([]string{
		tenantRootID.String(),
		strings.ToLower(strings.TrimSpace(sender)),
		parsed.Date.UTC().Format("2006-01-02T15:04:05Z07:00"),
		parsed.Subject,
		string(bh[:]), // body hash: a changed body is a distinct message
	}, "\x00")
	_, _ = h.Write([]byte(stable))
	return "synthetic-" + hex.EncodeToString(h.Sum(nil)[:16]) + "@synthetic.manyforge.invalid"
}
